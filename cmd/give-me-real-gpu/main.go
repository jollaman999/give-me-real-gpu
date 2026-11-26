// give-me-real-gpu is an emulator that fills GPU monitoring screens without GPU servers.
// It serves a fake OpenStack API and periodically writes GPU and system metrics built from the same topology
// to InfluxDB v1 or to an OTLP/HTTP endpoint. Settings, sinks and the topology are kept in a sqlite file
// and edited in the web settings UI; saved changes apply without a restart.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jollaman999/give-me-real-gpu/internal/api"
	"github.com/jollaman999/give-me-real-gpu/internal/editor"
	"github.com/jollaman999/give-me-real-gpu/internal/influx"
	"github.com/jollaman999/give-me-real-gpu/internal/metrics"
	"github.com/jollaman999/give-me-real-gpu/internal/openstack"
	"github.com/jollaman999/give-me-real-gpu/internal/otelgen"
	"github.com/jollaman999/give-me-real-gpu/internal/otlp"
	"github.com/jollaman999/give-me-real-gpu/internal/runtime"
	"github.com/jollaman999/give-me-real-gpu/internal/store"
	"github.com/jollaman999/give-me-real-gpu/internal/topology"
	"github.com/jollaman999/give-me-real-gpu/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// setupReason is reported by the fake OpenStack API until the settings it needs are saved.
const setupReason = "set advertise_host and the OpenStack password in the settings UI"

// sinkTimeout bounds one HTTP request to a sink.
const sinkTimeout = 30 * time.Second

func main() {
	dataDir := flag.String("data", "./data", "directory of the settings database (created if missing)")
	listen := flag.String("listen", ":15000", "listen address of the fake OpenStack API and /healthz")
	uiListen := flag.String("ui-listen", ":15080", "listen address of the settings UI and its API (no login: do not expose it to other hosts)")
	once := flag.Bool("once", false, "generate metrics once per enabled sink from the saved settings, print them to stdout and exit (nothing is written to any sink)")
	dumpTopology := flag.Bool("dump-topology", false, "print the default topology as JSON and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	switch {
	case *showVersion:
		fmt.Println(version)
		return
	case *dumpTopology:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(topology.Default()); err != nil {
			fail("failed to print the topology", err)
		}
		return
	case *once:
		if err := runOnce(*dataDir, os.Stdout); err != nil {
			fail("failed to print metrics", err)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *dataDir, *listen, *uiListen); err != nil {
		fail("exiting", err)
	}
}

func fail(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}

func run(ctx context.Context, dataDir, listen, uiListen string) error {
	st, err := store.Open(dataDir)
	if err != nil {
		return err
	}
	stClosed := false
	defer func() {
		if !stClosed {
			st.Close()
		}
	}()

	osLn, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("OpenStack API listener: %w", err)
	}
	defer osLn.Close()
	uiLn, err := net.Listen("tcp", uiListen)
	if err != nil {
		return fmt.Errorf("settings UI listener: %w", err)
	}
	defer uiLn.Close()

	snap, err := st.Snapshot()
	if err != nil {
		return err
	}
	slog.Info("give-me-real-gpu starting",
		"version", version,
		"data", dataDir,
		"hypervisors", len(snap.Topology.Hypervisors),
		"vms", len(snap.Topology.AllVMs()),
	)
	a := newApp(osLn.Addr().(*net.TCPAddr).Port)
	a.apply(snap)
	logStartup(osLn.Addr(), uiLn.Addr(), snap, a)

	osSrv := &http.Server{Handler: openStackMux(&a.sw), ReadHeaderTimeout: 10 * time.Second}
	uiSrv := &http.Server{Handler: newUI(st, a), ReadHeaderTimeout: 10 * time.Second}
	srvErr := make(chan error, 2)
	serve := func(name string, srv *http.Server, ln net.Listener) {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- fmt.Errorf("%s: %w", name, err)
		}
	}
	go serve("OpenStack API server", osSrv, osLn)
	go serve("settings UI server", uiSrv, uiLn)

	watchCtx, cancelWatch := context.WithCancel(ctx)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		a.watch(watchCtx, st)
	}()

	var runErr error
	select {
	case <-ctx.Done():
		slog.Info("received shutdown signal")
	case runErr = <-srvErr:
	}

	cancelWatch()
	<-watchDone
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := osSrv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("error while shutting down the OpenStack API server", "err", err)
	}
	if err := uiSrv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("error while shutting down the settings UI server", "err", err)
	}
	a.sup.Stop()
	stClosed = true
	if err := st.Close(); err != nil {
		slog.Warn("error while closing the database", "err", err)
	}
	slog.Info("stopped")
	return runErr
}

// openStackMux serves /healthz and hands everything else to the fake OpenStack API behind sw.
func openStackMux(sw *runtime.OpenStackSwitch) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, "ok")
	})
	mux.Handle("/", sw)
	return mux
}

// newUI serves the settings UI, its API and the topology editor endpoints.
// The editor endpoints sit under /api/v1 next to the API routes and get the same guard.
func newUI(st *store.Store, sp api.StatusProvider) *echo.Echo {
	e := echo.New()
	e.Logger = slog.Default()
	e.HTTPErrorHandler = api.ErrorHandler
	api.Register(e, st, sp)
	ed := echo.WrapHandler(editor.Handler())
	e.GET(editor.CatalogPath, ed, api.Guard)
	e.POST(editor.FillPath, ed, api.Guard)
	ui := echo.WrapHandler(web.Handler())
	e.Any("/ui", ui)
	e.Any("/ui/*", ui)
	e.GET("/", func(c *echo.Context) error { return c.Redirect(http.StatusFound, web.Prefix) })
	return e
}

// app applies snapshots of the store to the running process and reports its state to the status API.
type app struct {
	sw     runtime.OpenStackSwitch
	sup    *runtime.Supervisor
	osPort int

	mu    sync.Mutex
	snap  store.Snapshot // last applied
	osKey string         // fingerprint of the OpenStack handler in sw, "" while unavailable
}

func newApp(osPort int) *app {
	return &app{sup: runtime.NewSupervisor(), osPort: osPort}
}

// watch applies the latest snapshot after every saved change until ctx is cancelled.
func (a *app) watch(ctx context.Context, st *store.Store) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-st.Changes():
		}
		snap, err := st.Snapshot()
		if err != nil {
			slog.Error("failed to read the saved settings; keeping the running ones", "err", err)
			continue
		}
		a.apply(snap)
		var enabled []string
		for _, sk := range snap.Sinks {
			if sk.Enabled {
				enabled = append(enabled, sk.Name)
			}
		}
		slog.Info("settings applied",
			"interval", snap.Settings.Interval,
			"dry_run", snap.Settings.DryRun,
			"enabled_sinks", enabled,
		)
	}
}

func (a *app) apply(snap store.Snapshot) {
	a.mu.Lock()
	a.applyOpenStack(snap)
	a.snap = snap
	a.mu.Unlock()
	a.sup.Apply(runtimeConfig(snap))
}

// applyOpenStack swaps the fake OpenStack handler only when something it is built from changed,
// so issued tokens stay valid across unrelated changes such as the interval. a.mu must be held.
func (a *app) applyOpenStack(snap store.Snapshot) {
	s := snap.Settings
	if s.AdvertiseHost == "" || s.OpenStackPassword == "" {
		a.sw.SetUnavailable(setupReason)
		a.osKey = ""
		return
	}
	topo, _ := json.Marshal(snap.Topology)
	key := fingerprint(struct {
		Host, Password string
		Port           int
		TTL            time.Duration
		Topology       json.RawMessage
	}{s.AdvertiseHost, s.OpenStackPassword, a.osPort, s.TokenTTL, topo})
	if key == a.osKey {
		return
	}
	a.sw.Set(openstack.NewServer(snap.Topology, openstack.Config{
		Password:      s.OpenStackPassword,
		AdvertiseHost: s.AdvertiseHost,
		Port:          a.osPort,
		TokenTTL:      s.TokenTTL,
	}))
	a.osKey = key
	t := snap.Topology
	slog.Info("fake OpenStack API ready; register this credential in the management platform (the password is the one saved in the settings UI)",
		"url", authURL(s.AdvertiseHost, a.osPort),
		"user", t.UserName,
		"project_id", t.ProjectID,
		"project_name", t.ProjectName,
		"domain_id", t.DomainID,
		"region", t.Region,
	)
}

// Status implements api.StatusProvider.
func (a *app) Status() api.Status {
	a.mu.Lock()
	snap := a.snap
	a.mu.Unlock()
	ready, reason := a.sw.State()
	out := api.Status{
		Version:   version,
		OpenStack: api.OpenStackStatus{Ready: ready, Reason: reason},
	}
	if snap.Settings.AdvertiseHost != "" {
		out.OpenStack.AuthURL = authURL(snap.Settings.AdvertiseHost, a.osPort)
	}
	if t := snap.Topology; t != nil {
		out.OpenStack.User = t.UserName
		out.OpenStack.ProjectID = t.ProjectID
		out.OpenStack.ProjectName = t.ProjectName
		out.OpenStack.DomainID = t.DomainID
		out.OpenStack.Region = t.Region
	}
	sinks := a.sup.Status()
	out.Sinks = make([]api.SinkStatus, len(sinks))
	for i, s := range sinks {
		ss := api.SinkStatus{
			ID:        s.ID,
			Name:      s.Name,
			Type:      store.SinkType(s.Type),
			Enabled:   s.Enabled,
			Lines:     s.Lines,
			ElapsedMS: s.Elapsed.Milliseconds(),
			LastError: s.LastError,
		}
		if !s.LastWrite.IsZero() {
			ss.LastWrite = s.LastWrite.Format(time.RFC3339)
		}
		out.Sinks[i] = ss
	}
	return out
}

func authURL(host string, port int) string {
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/v3"
}

// runtimeConfig turns a snapshot into the desired state of the write loops.
func runtimeConfig(snap store.Snapshot) runtime.Config {
	topo, _ := json.Marshal(snap.Topology)
	specs := make([]runtime.SinkSpec, len(snap.Sinks))
	byID := make(map[uint]store.Sink, len(snap.Sinks))
	for i, sk := range snap.Sinks {
		specs[i] = runtime.SinkSpec{
			ID:      sk.ID,
			Name:    sk.Name,
			Type:    string(sk.Type),
			Enabled: sk.Enabled,
			Key:     sinkKey(sk, topo, snap.Settings),
		}
		byID[sk.ID] = sk
	}
	return runtime.Config{
		Interval: snap.Settings.Interval,
		DryRun:   snap.Settings.DryRun,
		Sinks:    specs,
		Build: func(sp runtime.SinkSpec) (runtime.Job, error) {
			sk, ok := byID[sp.ID]
			if !ok {
				return nil, fmt.Errorf("sink %d is not in the snapshot", sp.ID)
			}
			return newJob(sk, snap.Topology, snap.Settings)
		},
	}
}

// sinkKey fingerprints everything a sink's job is built from, secrets included, so a change to any of it
// restarts that sink's loop. Only the hash is kept; it is never logged.
func sinkKey(sk store.Sink, topo []byte, s store.Settings) string {
	return fingerprint(struct {
		Sink     store.Sink
		Topology json.RawMessage
		Seed     int64
		XIDRate  float64
	}{sk, topo, s.Seed, s.XIDRate})
}

func fingerprint(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("fingerprint: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func newJob(sk store.Sink, topo *topology.Topology, s store.Settings) (runtime.Job, error) {
	client := &http.Client{Timeout: sinkTimeout}
	switch sk.Type {
	case store.SinkInfluxDB:
		return &influxJob{
			gen: influxGenerator(sk, topo, s),
			sink: &influx.Sink{
				Name:     sk.Name,
				URL:      sk.URL,
				DB:       sk.DB,
				Username: sk.Username,
				Password: sk.Password,
				Client:   client,
			},
		}, nil
	case store.SinkOTLP:
		return &otlpJob{
			gen: otelGenerator(sk, topo, s),
			sink: &otlp.Sink{
				Name:     sk.Name,
				URL:      sk.URL,
				Username: sk.Username,
				Password: sk.Password,
				Token:    sk.Token,
				Client:   client,
			},
		}, nil
	default:
		return nil, fmt.Errorf("unknown sink type %q", sk.Type)
	}
}

func influxGenerator(sk store.Sink, topo *topology.Topology, s store.Settings) *metrics.Generator {
	return metrics.NewGenerator(topo, metrics.Options{
		CredentialID: sk.CredentialID,
		Seed:         s.Seed + int64(sk.ID),
		XIDRate:      s.XIDRate,
	})
}

func otelGenerator(sk store.Sink, topo *topology.Topology, s store.Settings) *otelgen.Generator {
	return otelgen.NewGenerator(topo, otelgen.Options{
		Seed:    s.Seed + int64(sk.ID),
		XIDRate: s.XIDRate,
	})
}

type influxJob struct {
	gen  *metrics.Generator
	sink *influx.Sink
}

func (j *influxJob) Collect(now time.Time) (any, int) {
	lines := j.gen.Collect(now)
	return lines, len(lines)
}

func (j *influxJob) Write(ctx context.Context, payload any) error {
	return j.sink.Write(ctx, payload.([]string))
}

type otlpJob struct {
	gen  *otelgen.Generator
	sink *otlp.Sink
}

func (j *otlpJob) Collect(now time.Time) (any, int) {
	req := j.gen.Collect(now)
	return req, req.DataPoints()
}

func (j *otlpJob) Write(ctx context.Context, payload any) error {
	return j.sink.Write(ctx, payload.(*otlp.ExportRequest))
}

// runOnce prints one collection of every enabled sink: line protocol for influxdb sinks and the number of
// data points per metric name for otlp sinks. It reads an existing database and writes to no sink.
func runOnce(dataDir string, w io.Writer) error {
	path := filepath.Join(dataDir, store.FileName)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no settings database (start the emulator and add a sink in the settings UI first): %w", err)
	}
	st, err := store.Open(dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	snap, err := st.Snapshot()
	if err != nil {
		return err
	}
	now := time.Now()
	n := 0
	for _, sk := range snap.Sinks {
		if !sk.Enabled {
			continue
		}
		n++
		switch sk.Type {
		case store.SinkInfluxDB:
			lines := influxGenerator(sk, snap.Topology, snap.Settings).Collect(now)
			slog.Info("generated metrics", "sink", sk.Name, "type", sk.Type, "lines", len(lines))
			for _, l := range lines {
				if _, err := fmt.Fprintln(w, l); err != nil {
					return err
				}
			}
		case store.SinkOTLP:
			req := otelGenerator(sk, snap.Topology, snap.Settings).Collect(now)
			slog.Info("generated metrics", "sink", sk.Name, "type", sk.Type, "data_points", req.DataPoints())
			if err := printOTLPSummary(w, sk.Name, req); err != nil {
				return err
			}
		default:
			return fmt.Errorf("sink %q: unknown type %q", sk.Name, sk.Type)
		}
	}
	if n == 0 {
		return errors.New("no enabled sink (add and enable one in the settings UI)")
	}
	return nil
}

// printOTLPSummary writes "# otlp sink <name>: <n> data points" and then "<metric name> <points>" per metric name.
func printOTLPSummary(w io.Writer, name string, req *otlp.ExportRequest) error {
	counts := map[string]int{}
	for _, rm := range req.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				counts[m.Name] += len(m.Points)
			}
		}
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)
	if _, err := fmt.Fprintf(w, "# otlp sink %s: %d data points\n", name, req.DataPoints()); err != nil {
		return err
	}
	for _, n := range names {
		if _, err := fmt.Fprintf(w, "%s %d\n", n, counts[n]); err != nil {
			return err
		}
	}
	return nil
}

func logStartup(osAddr, uiAddr net.Addr, snap store.Snapshot, a *app) {
	slog.Info("serving the fake OpenStack API", "listen", osAddr.String())
	slog.Info("serving the settings UI", "listen", uiAddr.String(), "url", uiURL(uiAddr))
	if ready, reason := a.sw.State(); !ready {
		slog.Warn("open the settings UI to finish setup", "url", uiURL(uiAddr), "reason", reason)
	}
	enabled := 0
	for _, sk := range snap.Sinks {
		if sk.Enabled {
			enabled++
		}
	}
	if enabled == 0 {
		slog.Warn("no enabled sink; no metrics are written until one is added in the settings UI")
	}
}

// uiURL is the address to open in a browser on this machine.
func uiURL(addr net.Addr) string {
	host, port := "127.0.0.1", ""
	if ta, ok := addr.(*net.TCPAddr); ok {
		port = strconv.Itoa(ta.Port)
		if !ta.IP.IsUnspecified() {
			host = ta.IP.String()
		}
	}
	return "http://" + net.JoinHostPort(host, port) + web.Prefix
}

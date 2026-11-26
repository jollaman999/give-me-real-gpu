// give-me-real-gpu is an emulator that fills GPU monitoring screens without GPU servers.
// It serves a fake OpenStack API and periodically writes GPU and system metrics built from the same topology to InfluxDB v1.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jollaman999/give-me-real-gpu/internal/config"
	"github.com/jollaman999/give-me-real-gpu/internal/influx"
	"github.com/jollaman999/give-me-real-gpu/internal/metrics"
	"github.com/jollaman999/give-me-real-gpu/internal/openstack"
	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	configPath := flag.String("config", "config.json", "path to the config file")
	dumpTopology := flag.Bool("dump-topology", false, "print the default topology as JSON and exit")
	once := flag.Bool("once", false, "generate metrics once per enabled sink, print them to stdout and exit (nothing is written to InfluxDB)")
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
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fail("failed to load the config", err)
	}
	topo, err := topology.Load(cfg.TopologyFile)
	if err != nil {
		fail("failed to load the topology", err)
	}

	if *once {
		if err := runOnce(cfg, topo); err != nil {
			fail("failed to print metrics", err)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg, topo); err != nil {
		fail("exiting", err)
	}
}

func fail(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}

func newGenerator(cfg *config.Config, topo *topology.Topology, s config.Sink, index int) *metrics.Generator {
	return metrics.NewGenerator(topo, metrics.Options{
		CredentialID: s.CredentialID,
		Seed:         cfg.Seed + int64(index),
		XIDRate:      cfg.XIDRate,
	})
}

func runOnce(cfg *config.Config, topo *topology.Topology) error {
	sinks, index := cfg.EnabledSinks()
	if len(sinks) == 0 {
		return errors.New("no enabled sink (check sinks[].enabled and credential_id)")
	}
	now := time.Now()
	w := os.Stdout
	for i, s := range sinks {
		lines := newGenerator(cfg, topo, s, index[i]).Collect(now)
		slog.Info("generated metrics", "sink", s.Name, "lines", len(lines))
		for _, l := range lines {
			if _, err := fmt.Fprintln(w, l); err != nil {
				return err
			}
		}
	}
	return nil
}

func run(ctx context.Context, cfg *config.Config, topo *topology.Topology) error {
	port, err := cfg.Port()
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, "ok")
	})
	mux.Handle("/", openstack.NewServer(topo, openstack.Config{
		Password:      cfg.OpenStack.Password,
		AdvertiseHost: cfg.AdvertiseHost,
		Port:          port,
		TokenTTL:      cfg.OpenStack.TokenTTL.Std(),
	}))
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}

	sinks, index := cfg.EnabledSinks()
	logStartup(cfg, topo, port, sinks)

	srvErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
		close(srvErr)
	}()

	loopCtx, cancelLoops := context.WithCancel(ctx)
	defer cancelLoops()
	var wg sync.WaitGroup
	for i, s := range sinks {
		gen := newGenerator(cfg, topo, s, index[i])
		sink := &influx.Sink{
			Name:     s.Name,
			URL:      s.URL,
			DB:       s.DB,
			Username: s.Username,
			Password: s.Password,
			Client:   &http.Client{Timeout: 30 * time.Second},
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			writeLoop(loopCtx, cfg.Interval.Std(), cfg.DryRun, gen, sink)
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
		slog.Info("received shutdown signal")
	case err := <-srvErr:
		runErr = fmt.Errorf("HTTP server: %w", err)
	}

	cancelLoops()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("error while shutting down the HTTP server", "err", err)
	}
	wg.Wait()
	slog.Info("stopped")
	return runErr
}

func logStartup(cfg *config.Config, topo *topology.Topology, port int, sinks []config.Sink) {
	authURL := "http://" + net.JoinHostPort(cfg.AdvertiseHost, strconv.Itoa(port)) + "/v3"
	slog.Info("give-me-real-gpu starting",
		"version", version,
		"listen", cfg.Listen,
		"hypervisors", len(topo.Hypervisors),
		"vms", len(topo.AllVMs()),
	)
	slog.Info("OpenStack credential to register in the management platform (password is openstack.password from the config)",
		"url", authURL,
		"user", topo.UserName,
		"project_id", topo.ProjectID,
		"project_name", topo.ProjectName,
		"domain_id", topo.DomainID,
		"region", topo.Region,
	)
	if len(sinks) == 0 {
		slog.Warn("no enabled sink; serving the fake OpenStack API only, no metrics are generated")
		return
	}
	names := make([]string, len(sinks))
	for i, s := range sinks {
		names[i] = s.Name
	}
	slog.Info("writing metrics",
		"sinks", strings.Join(names, ","),
		"interval", cfg.Interval.Std(),
		"dry_run", cfg.DryRun,
	)
}

// writeLoop generates metrics and writes them to the sink once immediately and then every interval.
func writeLoop(ctx context.Context, interval time.Duration, dryRun bool, gen *metrics.Generator, sink *influx.Sink) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	now := time.Now()
	for {
		lines := gen.Collect(now)
		switch {
		case dryRun:
			slog.Info("dry_run: not writing", "sink", sink.Name, "lines", len(lines))
		default:
			start := time.Now()
			if err := sink.Write(ctx, lines); err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Error("InfluxDB write failed", "sink", sink.Name, "lines", len(lines), "err", err)
			} else {
				slog.Info("InfluxDB write", "sink", sink.Name, "lines", len(lines), "elapsed", time.Since(start).Round(time.Millisecond))
			}
		}
		select {
		case <-ctx.Done():
			return
		case now = <-ticker.C:
		}
	}
}

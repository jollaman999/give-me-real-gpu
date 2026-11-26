package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jollaman999/give-me-real-gpu/internal/influx"
	"github.com/jollaman999/give-me-real-gpu/internal/store"
	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

const (
	uiHost   = "127.0.0.1:15080"
	credID   = "0b3e6a52-7f1d-4c2a-9e8b-1a2b3c4d5e6f"
	password = "<change-me>"
)

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func snapshot(t *testing.T, st *store.Store) store.Snapshot {
	t.Helper()
	snap, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func send(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = uiHost
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestUIRoutes(t *testing.T) {
	st := openStore(t, t.TempDir())
	a := newApp(15000)
	a.apply(snapshot(t, st))
	e := newUI(st, a)

	topo, err := json.Marshal(topology.Default())
	if err != nil {
		t.Fatal(err)
	}
	jsonCT := map[string]string{"Content-Type": "application/json"}
	cases := []struct {
		name         string
		method, path string
		body         string
		hdr          map[string]string
		code         int
		bodyHas      string
	}{
		{"root redirects", http.MethodGet, "/", "", nil, http.StatusFound, ""},
		{"ui index", http.MethodGet, "/ui/", "", nil, http.StatusOK, "<html"},
		{"status", http.MethodGet, "/api/v1/status", "", nil, http.StatusOK, `"ready":false`},
		{"catalog", http.MethodGet, "/api/v1/topology/catalog", "", nil, http.StatusOK, "{"},
		{"catalog rebound host", http.MethodGet, "/api/v1/topology/catalog", "", map[string]string{"Host": "evil.example:15080"},
			http.StatusForbidden, `"error":"forbidden host"`},
		{"fill", http.MethodPost, "/api/v1/topology/fill", string(topo), jsonCT, http.StatusOK, `"problems":[]`},
		{"fill cross origin", http.MethodPost, "/api/v1/topology/fill", string(topo),
			map[string]string{"Content-Type": "application/json", "Origin": "http://evil.example"},
			http.StatusForbidden, `"error":"cross-site request refused"`},
		{"fill text/plain", http.MethodPost, "/api/v1/topology/fill", string(topo), map[string]string{"Content-Type": "text/plain"},
			http.StatusUnsupportedMediaType, `"error":"content type must be application/json"`},
		{"unknown api path", http.MethodGet, "/api/v1/nope", "", nil, http.StatusNotFound, `{"error":"Not Found"}`},
	}
	for _, tc := range cases {
		rec := send(e, tc.method, tc.path, tc.body, tc.hdr)
		if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.bodyHas) {
			t.Errorf("%s: %d %.200s, want %d containing %q", tc.name, rec.Code, rec.Body.String(), tc.code, tc.bodyHas)
		}
	}
	if loc := send(e, http.MethodGet, "/", "", nil).Header().Get("Location"); loc != "/ui/" {
		t.Errorf("root redirects to %q", loc)
	}
}

func TestOpenStackFollowsSettings(t *testing.T) {
	st := openStore(t, t.TempDir())
	a := newApp(15000)
	a.apply(snapshot(t, st))
	h := openStackMux(&a.sw)

	if rec := send(h, http.MethodGet, "/healthz", "", nil); rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("healthz %d %q", rec.Code, rec.Body.String())
	}
	if rec := send(h, http.MethodGet, "/v3", "", nil); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), setupReason) {
		t.Errorf("unconfigured /v3: %d %s", rec.Code, rec.Body.String())
	}
	if s := a.Status(); s.OpenStack.Ready || s.OpenStack.Reason != setupReason || s.OpenStack.AuthURL != "" {
		t.Errorf("unconfigured status %+v", s.OpenStack)
	}

	settings := store.DefaultSettings()
	settings.AdvertiseHost = "192.0.2.10"
	settings.OpenStackPassword = password
	if err := st.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	snap := snapshot(t, st)
	a.apply(snap)
	if rec := send(h, http.MethodGet, "/v3", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("configured /v3: %d %s", rec.Code, rec.Body.String())
	}
	topo := snap.Topology
	auth := `{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"` + topo.UserName +
		`","domain":{"id":"default"},"password":"` + password + `"}}},"scope":{"project":{"id":"` + topo.ProjectID + `"}}}}`
	rec := send(h, http.MethodPost, "/v3/auth/tokens", auth, map[string]string{"Content-Type": "application/json"})
	tok := rec.Header().Get("X-Subject-Token")
	if rec.Code != http.StatusCreated || tok == "" {
		t.Fatalf("issue token: %d %s", rec.Code, rec.Body.String())
	}
	s := a.Status()
	if !s.OpenStack.Ready || s.OpenStack.AuthURL != "http://192.0.2.10:15000/v3" || s.OpenStack.User != topo.UserName ||
		s.OpenStack.ProjectID != topo.ProjectID || s.Version != version {
		t.Errorf("configured status %+v", s)
	}

	check := func() int {
		return send(h, http.MethodGet, "/v3/auth/tokens", "", map[string]string{"X-Auth-Token": tok, "X-Subject-Token": tok}).Code
	}
	settings.Interval = 3 * time.Second
	if err := st.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	a.apply(snapshot(t, st))
	if code := check(); code != http.StatusOK {
		t.Errorf("token after an interval change: %d, want 200 (handler must not be rebuilt)", code)
	}

	settings.OpenStackPassword = "<change-me-too>"
	if err := st.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	a.apply(snapshot(t, st))
	if code := check(); code == http.StatusOK {
		t.Errorf("token after a password change still valid")
	}
}

func TestSinkKey(t *testing.T) {
	sk := store.Sink{ID: 1, Name: "site-a", Type: store.SinkInfluxDB, URL: "http://192.0.2.20:8086", DB: "telegraf",
		Username: "u", Password: "<change-me-1>", CredentialID: credID, Enabled: true}
	topo, _ := json.Marshal(topology.Default())
	s := store.DefaultSettings()
	base := sinkKey(sk, topo, s)
	if base != sinkKey(sk, topo, s) {
		t.Fatal("key is not stable")
	}
	if strings.Contains(base, "<change-me-1>") || len(base) != 64 {
		t.Errorf("key %q is not a sha256 hex", base)
	}

	other := topology.Default()
	other.Region = "RegionTwo"
	otherTopo, _ := json.Marshal(other)
	changes := map[string]func() string{
		"password": func() string { c := sk; c.Password = "<change-me-2>"; return sinkKey(c, topo, s) },
		"token":    func() string { c := sk; c.Token = "tok"; return sinkKey(c, topo, s) },
		"url":      func() string { c := sk; c.URL = "http://192.0.2.21:8086"; return sinkKey(c, topo, s) },
		"version":  func() string { c := sk; c.InfluxVersion = store.InfluxV2; return sinkKey(c, topo, s) },
		"org":      func() string { c := sk; c.Org = "o"; return sinkKey(c, topo, s) },
		"cred": func() string {
			c := sk
			c.CredentialID = "1b3e6a52-7f1d-4c2a-9e8b-1a2b3c4d5e6f"
			return sinkKey(c, topo, s)
		},
		"topology": func() string { return sinkKey(sk, otherTopo, s) },
		"seed":     func() string { c := s; c.Seed = 9; return sinkKey(sk, topo, c) },
		"xid rate": func() string { c := s; c.XIDRate = 0.5; return sinkKey(sk, topo, c) },
	}
	for name, f := range changes {
		if f() == base {
			t.Errorf("%s change keeps the key", name)
		}
	}
	if c := s; sinkKey(sk, topo, func() store.Settings { c.Interval = time.Minute; return c }()) != base {
		t.Errorf("interval change alters the key; the supervisor restarts on interval by itself")
	}
}

func TestRunOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	var out bytes.Buffer
	if err := runOnce(dir, &out); err == nil {
		t.Fatal("runOnce without a database succeeded")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("runOnce created the data directory: %v", err)
	}

	st := openStore(t, dir)
	if _, err := st.CreateSink(store.Sink{Name: "site-a", Type: store.SinkInfluxDB, URL: "http://192.0.2.20:8086", DB: "telegraf",
		CredentialID: credID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSink(store.Sink{Name: "site-b", Type: store.SinkOTLP, URL: "http://192.0.2.20:8428/opentelemetry/v1/metrics",
		Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSink(store.Sink{Name: "site-off", Type: store.SinkOTLP}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	if err := runOnce(dir, &out); err != nil {
		t.Fatal(err)
	}
	var influxLines, otlpPoints, header, headerPoints int
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		switch {
		case strings.HasPrefix(l, "# otlp sink "):
			header++
			if _, err := fmt.Sscanf(l, "# otlp sink site-b: %d data points", &headerPoints); err != nil {
				t.Errorf("header %q: %v", l, err)
			}
		case strings.HasPrefix(l, "dcgm,") || strings.HasPrefix(l, "gpu_vm_mapping,") || strings.Contains(l, "credential_id="):
			influxLines++
		default:
			var name string
			var n int
			if _, err := fmt.Sscan(l, &name, &n); err != nil || n <= 0 {
				t.Errorf("unexpected line %q", l)
			}
			otlpPoints += n
		}
	}
	if header != 1 || influxLines == 0 || otlpPoints == 0 || otlpPoints != headerPoints {
		t.Errorf("headers %d, influx lines %d, otlp points %d (header says %d)\n%.500s", header, influxLines, otlpPoints, headerPoints, out.String())
	}
	if strings.Contains(out.String(), "site-off") {
		t.Error("disabled sink printed")
	}
}

func TestNewJobPassesInfluxVersion(t *testing.T) {
	sk := store.Sink{ID: 1, Name: "site-a", Type: store.SinkInfluxDB, URL: "http://192.0.2.20:8086", InfluxVersion: store.InfluxV2,
		Org: "o", DB: "bucket-a", Token: "<change-me>", CredentialID: credID, Enabled: true}
	j, err := newJob(sk, topology.Default(), store.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	got := j.(*influxJob).sink
	if got.Version != influx.V2 || got.Org != "o" || got.DB != "bucket-a" || got.Token != "<change-me>" {
		t.Errorf("influx sink %+v", got)
	}
}

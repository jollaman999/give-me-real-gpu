package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jollaman999/give-me-real-gpu/internal/store"
	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

const (
	credID       = "0b3e6a52-7f1d-4c2a-9e8b-1a2b3c4d5e6f"
	osSecret     = "os-secret-value-1"
	sinkPass     = "sink-secret-value-2"
	sinkToken    = "sink-secret-value-3"
	secretNeedle = "secret-value"
)

type fakeStatus struct{ s Status }

func (f fakeStatus) Status() Status { return f.s }

func newServer(t *testing.T, sp StatusProvider) (*echo.Echo, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := echo.New()
	Register(e, st, sp)
	return e, st
}

// do sends a request and fails the test if any stored secret shows up in the response body.
func do(t *testing.T, e *echo.Echo, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = testHost
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	out := rec.Body.String()
	if strings.Contains(out, secretNeedle) {
		t.Errorf("%s %s leaked a secret: %s", method, path, out)
	}
	return rec.Code, out
}

func decodeJSON(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, s)
	}
}

func wantError(t *testing.T, code int, body string, wantCode int, wantErr, wantProblem string) {
	t.Helper()
	if code != wantCode {
		t.Errorf("status %d, want %d: %s", code, wantCode, body)
		return
	}
	var eb struct {
		Error    string   `json:"error"`
		Problems []string `json:"problems"`
	}
	decodeJSON(t, body, &eb)
	if eb.Error != wantErr {
		t.Errorf("error %q, want %q", eb.Error, wantErr)
	}
	if wantProblem == "" {
		return
	}
	for _, p := range eb.Problems {
		if strings.Contains(p, wantProblem) {
			return
		}
	}
	t.Errorf("problems %q do not mention %q", eb.Problems, wantProblem)
}

func TestSettings(t *testing.T) {
	e, st := newServer(t, nil)

	code, body := do(t, e, http.MethodGet, "/api/v1/settings", "")
	if code != http.StatusOK {
		t.Fatalf("GET settings %d %s", code, body)
	}
	var got map[string]any
	decodeJSON(t, body, &got)
	want := map[string]any{
		"advertise_host": "", "openstack_password_set": false, "token_ttl": "1h", "interval": "10s",
		"seed": float64(1), "xid_rate": 0.002, "dry_run": false,
	}
	if len(got) != len(want) {
		t.Errorf("keys %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}

	code, body = do(t, e, http.MethodPut, "/api/v1/settings",
		`{"advertise_host":"192.0.2.5","openstack_password":"`+osSecret+`","interval":"30s","xid_rate":0.5}`)
	if code != http.StatusOK {
		t.Fatalf("PUT settings %d %s", code, body)
	}
	decodeJSON(t, body, &got)
	if got["openstack_password_set"] != true || got["interval"] != "30s" || got["token_ttl"] != "1h" || got["seed"] != float64(1) {
		t.Errorf("after PUT %v", got)
	}
	snap, _ := st.Snapshot()
	if snap.Settings.OpenStackPassword != osSecret || snap.Settings.AdvertiseHost != "192.0.2.5" {
		t.Errorf("stored settings %+v", snap.Settings)
	}

	// Sending back what GET returned keeps the password.
	code, body = do(t, e, http.MethodPut, "/api/v1/settings", body)
	if code != http.StatusOK {
		t.Fatalf("PUT echo of GET %d %s", code, body)
	}
	snap, _ = st.Snapshot()
	if snap.Settings.OpenStackPassword != osSecret {
		t.Error("password lost after sending back the GET body")
	}

	code, body = do(t, e, http.MethodPut, "/api/v1/settings", `{"interval":"500ms","xid_rate":2}`)
	wantError(t, code, body, http.StatusBadRequest, "validation failed", "xid_rate")
	code, body = do(t, e, http.MethodPut, "/api/v1/settings", `{"interval":"ten"}`)
	wantError(t, code, body, http.StatusBadRequest, "validation failed", "interval")
	code, body = do(t, e, http.MethodPut, "/api/v1/settings", `{"intervl":"10s"}`)
	wantError(t, code, body, http.StatusBadRequest, "invalid request body", "intervl")
	code, body = do(t, e, http.MethodPut, "/api/v1/settings", `{"interval":`)
	wantError(t, code, body, http.StatusBadRequest, "invalid request body", "")
	code, body = do(t, e, http.MethodPut, "/api/v1/settings", `{} {}`)
	wantError(t, code, body, http.StatusBadRequest, "invalid request body", "unexpected data")
	snap, _ = st.Snapshot()
	if snap.Settings.Interval.String() != "30s" {
		t.Errorf("rejected PUT changed interval to %s", snap.Settings.Interval)
	}
}

func TestSinks(t *testing.T) {
	e, st := newServer(t, nil)

	code, body := do(t, e, http.MethodGet, "/api/v1/sinks", "")
	if code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Fatalf("empty list %d %s", code, body)
	}

	code, body = do(t, e, http.MethodPost, "/api/v1/sinks", `{"name":"site-a","type":"influxdb","url":"http://192.0.2.10:8086",
		"db":"telegraf","username":"writer","password":"`+sinkPass+`","credential_id":"`+credID+`","enabled":true}`)
	if code != http.StatusCreated {
		t.Fatalf("POST sink %d %s", code, body)
	}
	var a map[string]any
	decodeJSON(t, body, &a)
	if a["password_set"] != true || a["token_set"] != false || a["name"] != "site-a" || a["id"] == float64(0) {
		t.Errorf("created %v", a)
	}
	if _, ok := a["password"]; ok {
		t.Error("response has a password key")
	}
	id := int(a["id"].(float64))
	path := "/api/v1/sinks/" + strconv.Itoa(id)

	code, body = do(t, e, http.MethodPost, "/api/v1/sinks", `{"name":"site-b","type":"otlp","url":"http://192.0.2.20:8429/opentelemetry/v1/metrics","token":"`+sinkToken+`","enabled":true}`)
	if code != http.StatusCreated {
		t.Fatalf("POST otlp sink %d %s", code, body)
	}

	code, body = do(t, e, http.MethodGet, "/api/v1/sinks", "")
	var list []map[string]any
	decodeJSON(t, body, &list)
	if code != http.StatusOK || len(list) != 2 || list[1]["token_set"] != true {
		t.Errorf("list %d %v", code, list)
	}

	// Sending back the GET item (no password) keeps the password.
	item, _ := json.Marshal(list[0])
	code, body = do(t, e, http.MethodPut, path, string(item))
	if code != http.StatusOK {
		t.Fatalf("PUT echo %d %s", code, body)
	}
	snap, _ := st.Snapshot()
	if snap.Sinks[0].Password != sinkPass {
		t.Error("password lost after sending back the GET item")
	}

	// Partial body: only the URL changes, so the password is dropped.
	code, body = do(t, e, http.MethodPut, path, `{"url":"http://198.51.100.10:8086"}`)
	if code != http.StatusOK {
		t.Fatalf("PUT url %d %s", code, body)
	}
	decodeJSON(t, body, &a)
	if a["password_set"] != false || a["db"] != "telegraf" || a["name"] != "site-a" {
		t.Errorf("after url change %v", a)
	}

	code, body = do(t, e, http.MethodPost, "/api/v1/sinks", `{"name":"site-a","type":"influxdb"}`)
	wantError(t, code, body, http.StatusBadRequest, "validation failed", "already used")
	code, body = do(t, e, http.MethodPost, "/api/v1/sinks", `{"name":"x","type":"influxdb","colour":"red"}`)
	wantError(t, code, body, http.StatusBadRequest, "invalid request body", "colour")
	code, body = do(t, e, http.MethodPut, path, `{"credential_id":"nope"}`)
	wantError(t, code, body, http.StatusBadRequest, "validation failed", "not a UUID")
	code, body = do(t, e, http.MethodPut, "/api/v1/sinks/999", `{}`)
	wantError(t, code, body, http.StatusNotFound, "sink not found", "")
	code, body = do(t, e, http.MethodPut, "/api/v1/sinks/abc", `{}`)
	wantError(t, code, body, http.StatusNotFound, "sink not found", "")

	code, body = do(t, e, http.MethodDelete, path, "")
	if code != http.StatusNoContent {
		t.Errorf("DELETE %d %s", code, body)
	}
	code, body = do(t, e, http.MethodDelete, path, "")
	wantError(t, code, body, http.StatusNotFound, "sink not found", "")
}

func TestTopology(t *testing.T) {
	e, _ := newServer(t, nil)

	code, body := do(t, e, http.MethodGet, "/api/v1/topology", "")
	if code != http.StatusOK {
		t.Fatalf("GET topology %d", code)
	}
	var top topology.Topology
	decodeJSON(t, body, &top)
	if len(top.Hypervisors) != len(topology.Default().Hypervisors) {
		t.Errorf("default topology has %d hypervisors", len(top.Hypervisors))
	}

	top.Hypervisors = top.Hypervisors[:1]
	b, _ := json.Marshal(top)
	code, body = do(t, e, http.MethodPut, "/api/v1/topology", string(b))
	if code != http.StatusOK {
		t.Fatalf("PUT topology %d %s", code, body)
	}
	var saved topology.Topology
	decodeJSON(t, body, &saved)
	if len(saved.Hypervisors) != 1 {
		t.Errorf("saved topology has %d hypervisors", len(saved.Hypervisors))
	}

	top.ProjectID = ""
	b, _ = json.Marshal(top)
	code, body = do(t, e, http.MethodPut, "/api/v1/topology", string(b))
	wantError(t, code, body, http.StatusBadRequest, "validation failed", "project_id is empty")
	code, body = do(t, e, http.MethodPut, "/api/v1/topology", `{"regoin":"x"}`)
	wantError(t, code, body, http.StatusBadRequest, "invalid request body", "regoin")

	code, body = do(t, e, http.MethodPost, "/api/v1/topology/reset", "")
	if code != http.StatusOK {
		t.Fatalf("reset %d %s", code, body)
	}
	decodeJSON(t, body, &saved)
	if len(saved.Hypervisors) != len(topology.Default().Hypervisors) {
		t.Errorf("after reset %d hypervisors", len(saved.Hypervisors))
	}
}

func TestStatus(t *testing.T) {
	e, _ := newServer(t, nil)
	code, body := do(t, e, http.MethodGet, "/api/v1/status", "")
	if code != http.StatusOK || !strings.Contains(body, `"sinks":[]`) {
		t.Errorf("nil provider %d %s", code, body)
	}

	sp := fakeStatus{Status{
		Version:   "v1.2.3",
		OpenStack: OpenStackStatus{Ready: true, AuthURL: "http://192.0.2.5:15000/identity/v3", User: "emu-admin"},
		Sinks:     []SinkStatus{{ID: 1, Name: "site-a", Type: store.SinkOTLP, Enabled: true, Lines: 42, ElapsedMS: 7}},
	}}
	e, _ = newServer(t, sp)
	code, body = do(t, e, http.MethodGet, "/api/v1/status", "")
	var got map[string]any
	decodeJSON(t, body, &got)
	ost := got["openstack"].(map[string]any)
	sinks := got["sinks"].([]any)
	s0 := sinks[0].(map[string]any)
	if code != http.StatusOK || got["version"] != "v1.2.3" || ost["ready"] != true || ost["auth_url"] != sp.s.OpenStack.AuthURL ||
		s0["type"] != "otlp" || s0["lines"] != float64(42) || s0["elapsed_ms"] != float64(7) || s0["last_write"] != "" {
		t.Errorf("status %d %s", code, body)
	}
}

func TestFormatDuration(t *testing.T) {
	for in, want := range map[string]string{"1h0m0s": "1h", "10s": "10s", "1m0s": "1m", "1h30m0s": "1h30m", "2h0m5s": "2h0m5s", "0s": "0s", "1.5s": "1.5s"} {
		d, err := time.ParseDuration(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestErrorHandler(t *testing.T) {
	e, _ := newServer(t, nil)
	e.HTTPErrorHandler = ErrorHandler
	e.GET("/api/v1/teapot", func(c *echo.Context) error { return echo.NewHTTPError(http.StatusTeapot, "short and stout") })
	e.GET("/outside", func(c *echo.Context) error { return c.NoContent(http.StatusNoContent) })
	e.GET("/api/v1/broken", func(c *echo.Context) error { return errors.New("internal detail " + secretNeedle) })

	cases := []struct {
		method, path string
		code         int
		errMsg       string
	}{
		{http.MethodGet, "/api/v1/nope", http.StatusNotFound, "Not Found"},
		{http.MethodGet, "/nope", http.StatusNotFound, "Not Found"},
		{http.MethodPost, "/outside", http.StatusMethodNotAllowed, "Method Not Allowed"},
		{http.MethodGet, "/api/v1/teapot", http.StatusTeapot, "short and stout"},
		{http.MethodGet, "/api/v1/broken", http.StatusInternalServerError, "Internal Server Error"},
	}
	for _, tc := range cases {
		code, body := do(t, e, tc.method, tc.path, "")
		wantError(t, code, body, tc.code, tc.errMsg, "")
		if strings.Contains(body, `"message"`) {
			t.Errorf("%s %s: echo default body: %s", tc.method, tc.path, body)
		}
	}

	req := httptest.NewRequest(http.MethodHead, "/api/v1/nope", nil)
	req.Host = testHost
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || rec.Body.Len() != 0 {
		t.Errorf("HEAD: %d %q", rec.Code, rec.Body.String())
	}
}

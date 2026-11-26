package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type received struct {
	method      string
	path        string
	contentType string
	encoding    string
	auth        string
	body        []byte
}

// recorder collects received requests and answers with the status code chosen by respond.
type recorder struct {
	mu      sync.Mutex
	reqs    []received
	respond func(n int) (int, string) // n is the 0-based request number
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	zr, err := gzip.NewReader(req.Body)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	raw, _ := io.ReadAll(zr)
	r.mu.Lock()
	n := len(r.reqs)
	r.reqs = append(r.reqs, received{
		method:      req.Method,
		path:        req.URL.RequestURI(),
		contentType: req.Header.Get("Content-Type"),
		encoding:    req.Header.Get("Content-Encoding"),
		auth:        req.Header.Get("Authorization"),
		body:        raw,
	})
	r.mu.Unlock()
	code, body := http.StatusOK, ""
	if r.respond != nil {
		code, body = r.respond(n)
	}
	w.WriteHeader(code)
	io.WriteString(w, body)
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

func init() {
	retryDelay = time.Millisecond
}

func sampleRequest() *ExportRequest {
	return &ExportRequest{ResourceMetrics: []*ResourceMetrics{{ScopeMetrics: []*ScopeMetrics{{
		Metrics: []*Metric{{Name: "system.uptime", Unit: "s", Points: []NumberDataPoint{
			{TimeUnixNano: 1700000000000000000, Double: 12.5, Attributes: []KeyValue{String("entity_id", "e1")}},
		}}},
	}}}}}
}

func TestWriteHeadersGzipBasic(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	req := sampleRequest()
	s := &Sink{Name: "t", URL: srv.URL + "/opentelemetry/v1/metrics?x=1", Username: "u1", Password: "<change-me>"}
	if err := s.Write(context.Background(), req); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("requests %d, want 1", rec.count())
	}
	got := rec.reqs[0]
	if got.method != http.MethodPost || got.path != "/opentelemetry/v1/metrics?x=1" {
		t.Errorf("request %s %s, want POST to the URL as given", got.method, got.path)
	}
	if got.contentType != "application/x-protobuf" || got.encoding != "gzip" {
		t.Errorf("Content-Type %q Content-Encoding %q", got.contentType, got.encoding)
	}
	if !bytes.Equal(got.body, req.Marshal()) {
		t.Error("gunzipped body differs from Marshal()")
	}
	hr, _ := http.NewRequest(http.MethodGet, "/", nil)
	hr.Header.Set("Authorization", got.auth)
	if u, p, ok := hr.BasicAuth(); !ok || u != "u1" || p != "<change-me>" {
		t.Errorf("basic auth %q", got.auth)
	}
}

func TestWriteAuthChoice(t *testing.T) {
	cases := []struct {
		name string
		sink Sink
		want string
	}{
		{"none", Sink{}, ""},
		{"bearer", Sink{Token: "tok"}, "Bearer tok"},
		{"bearer wins over basic", Sink{Token: "tok", Username: "u", Password: "p"}, "Bearer tok"},
		{"basic with user only", Sink{Username: "u"}, "Basic dTo="},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &recorder{}
			srv := httptest.NewServer(rec)
			defer srv.Close()
			s := c.sink
			s.URL = srv.URL + "/v1/metrics"
			if err := s.Write(context.Background(), sampleRequest()); err != nil {
				t.Fatal(err)
			}
			if got := rec.reqs[0].auth; got != c.want {
				t.Errorf("Authorization %q, want %q", got, c.want)
			}
		})
	}
}

func TestWriteEmptySendsNothing(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	s := &Sink{URL: srv.URL}
	if err := s.Write(context.Background(), &ExportRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if rec.count() != 0 {
		t.Fatalf("sent %d requests for no data points", rec.count())
	}
}

func TestWriteRetry(t *testing.T) {
	cases := []struct {
		name     string
		respond  func(n int) (int, string)
		wantReqs int
		wantErr  string
		wantCode int
	}{
		{"5xx then ok", func(n int) (int, string) {
			if n == 0 {
				return 503, "busy"
			}
			return 200, ""
		}, 2, "", 0},
		{"5xx twice", func(int) (int, string) { return 500, "down" }, 2, "retry also failed", 500},
		{"4xx not retried", func(int) (int, string) { return 400, "cannot parse" }, 1, "cannot parse", 400},
		{"401 not retried", func(int) (int, string) { return 401, "unauthorized" }, 1, "unauthorized", 401},
		{"204 is success", func(int) (int, string) { return 204, "" }, 1, "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &recorder{respond: c.respond}
			srv := httptest.NewServer(rec)
			defer srv.Close()
			s := &Sink{Name: "site-a", URL: srv.URL}
			err := s.Write(context.Background(), sampleRequest())
			if rec.count() != c.wantReqs {
				t.Errorf("requests %d, want %d", rec.count(), c.wantReqs)
			}
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("Write: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) || !strings.Contains(err.Error(), "site-a") {
				t.Fatalf("error %v, want it to contain %q and the sink name", err, c.wantErr)
			}
			var se *StatusError
			if !errors.As(err, &se) || se.Code != c.wantCode {
				t.Fatalf("status error %v, want code %d", se, c.wantCode)
			}
		})
	}
}

// TestWriteNetworkErrorRetried closes the first connection without an answer, then answers the second.
func TestWriteNetworkErrorRetried(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		io.Copy(io.Discard, r.Body)
		if n == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	s := &Sink{URL: srv.URL}
	if err := s.Write(context.Background(), sampleRequest()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls %d, want 2", calls)
	}
}

func TestWriteConnectionRefused(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	s := &Sink{Name: "gone", URL: "http://" + addr + "/v1/metrics"}
	err = s.Write(context.Background(), sampleRequest())
	if err == nil || !strings.Contains(err.Error(), "retry also failed") {
		t.Fatalf("error %v, want a failed retry", err)
	}
}

func TestWriteBadURL(t *testing.T) {
	for _, u := range []string{"ftp://192.0.2.1/x", "://bad", "http:///nohost"} {
		s := &Sink{Name: "t", URL: u}
		if err := s.Write(context.Background(), sampleRequest()); err == nil {
			t.Errorf("URL %q accepted", u)
		}
	}
}

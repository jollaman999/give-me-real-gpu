package influx

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type received struct {
	query    string
	encoding string
	user     string
	pass     string
	hasAuth  bool
	lines    []string
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
	u, p, ok := req.BasicAuth()
	r.mu.Lock()
	n := len(r.reqs)
	r.reqs = append(r.reqs, received{
		query:    req.URL.Path + "?" + req.URL.RawQuery,
		encoding: req.Header.Get("Content-Encoding"),
		user:     u,
		pass:     p,
		hasAuth:  ok,
		lines:    strings.Split(string(raw), "\n"),
	})
	r.mu.Unlock()
	code, body := http.StatusNoContent, ""
	if r.respond != nil {
		code, body = r.respond(n)
	}
	w.WriteHeader(code)
	io.WriteString(w, body)
}

func makeLines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("m,host=h v=%di %d", i, 1700000000000000000+i)
	}
	return out
}

func init() {
	retryDelay = time.Millisecond
}

func TestWriteAuthQueryGzip(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	s := &Sink{Name: "t", URL: srv.URL + "/", DB: "testdb", Username: "u1", Password: "p1"}
	lines := makeLines(3)
	if err := s.Write(context.Background(), lines); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(rec.reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(rec.reqs))
	}
	got := rec.reqs[0]
	if got.query != "/write?db=testdb&precision=ns" {
		t.Errorf("query = %q", got.query)
	}
	if got.encoding != "gzip" {
		t.Errorf("Content-Encoding = %q", got.encoding)
	}
	if !got.hasAuth || got.user != "u1" || got.pass != "p1" {
		t.Errorf("auth = %v %q %q", got.hasAuth, got.user, got.pass)
	}
	if strings.Join(got.lines, "\n") != strings.Join(lines, "\n") {
		t.Errorf("body = %q", got.lines)
	}
}

func TestWriteNoAuthWhenEmpty(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	s := &Sink{Name: "t", URL: srv.URL, DB: "testdb"}
	if err := s.Write(context.Background(), makeLines(1)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if rec.reqs[0].hasAuth {
		t.Errorf("Authorization was sent with empty credentials")
	}
}

func TestWriteEmptyLinesSendsNothing(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	s := &Sink{Name: "t", URL: srv.URL, DB: "testdb"}
	if err := s.Write(context.Background(), nil); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(rec.reqs) != 0 {
		t.Errorf("requests = %d, want 0", len(rec.reqs))
	}
}

func TestWriteChunks(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	s := &Sink{Name: "t", URL: srv.URL, DB: "testdb"}
	lines := makeLines(2*ChunkLines + 1)
	if err := s.Write(context.Background(), lines); err != nil {
		t.Fatalf("Write: %v", err)
	}
	want := []int{ChunkLines, ChunkLines, 1}
	if len(rec.reqs) != len(want) {
		t.Fatalf("requests = %d, want %d", len(rec.reqs), len(want))
	}
	var all []string
	for i, r := range rec.reqs {
		if len(r.lines) != want[i] {
			t.Errorf("chunk %d lines = %d, want %d", i, len(r.lines), want[i])
		}
		all = append(all, r.lines...)
	}
	if strings.Join(all, "\n") != strings.Join(lines, "\n") {
		t.Errorf("joined chunks differ from the original lines")
	}
}

func TestWriteBadRequestContinues(t *testing.T) {
	const msg = `partial write: field type conflict: input field "pstate" on measurement "dcgm" is type float, already exists as type integer dropped=1`
	rec := &recorder{respond: func(n int) (int, string) {
		if n == 0 {
			return http.StatusBadRequest, `{"error":"` + strings.ReplaceAll(msg, `"`, `\"`) + `"}`
		}
		return http.StatusNoContent, ""
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	s := &Sink{Name: "site-a", URL: srv.URL, DB: "testdb"}
	err := s.Write(context.Background(), makeLines(ChunkLines+2))
	if err == nil {
		t.Fatal("no error on 400")
	}
	if len(rec.reqs) != 2 {
		t.Fatalf("requests = %d, want 2 (the next chunk must be sent after a 400)", len(rec.reqs))
	}
	var be *BadRequestError
	if !errors.As(err, &be) || be.Message != msg {
		t.Errorf("BadRequestError.Message = %v, want %q", be, msg)
	}
	if !strings.Contains(err.Error(), "sink site-a") || !strings.Contains(err.Error(), "0-4999") {
		t.Errorf("error lacks sink name or line range: %v", err)
	}
}

func TestWriteRetriesOnceOn5xx(t *testing.T) {
	rec := &recorder{respond: func(n int) (int, string) {
		if n == 0 {
			return http.StatusServiceUnavailable, "busy"
		}
		return http.StatusNoContent, ""
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	s := &Sink{Name: "t", URL: srv.URL, DB: "testdb"}
	if err := s.Write(context.Background(), makeLines(2)); err != nil {
		t.Fatalf("should succeed on retry: %v", err)
	}
	if len(rec.reqs) != 2 {
		t.Errorf("requests = %d, want 2", len(rec.reqs))
	}
}

func TestWriteGivesUpAfterSecond5xx(t *testing.T) {
	rec := &recorder{respond: func(n int) (int, string) {
		return http.StatusInternalServerError, `{"error":"engine down"}`
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	s := &Sink{Name: "t", URL: srv.URL, DB: "testdb"}
	err := s.Write(context.Background(), makeLines(2*ChunkLines))
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 500 || se.Message != "engine down" {
		t.Fatalf("err = %v", err)
	}
	if len(rec.reqs) != 2 {
		t.Errorf("requests = %d, want 2 (first chunk once + one retry, next chunk not sent)", len(rec.reqs))
	}
}

func TestWriteNoRetryOn401(t *testing.T) {
	rec := &recorder{respond: func(n int) (int, string) {
		return http.StatusUnauthorized, `{"error":"authorization failed"}`
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	s := &Sink{Name: "t", URL: srv.URL, DB: "testdb", Username: "u", Password: "x"}
	err := s.Write(context.Background(), makeLines(2*ChunkLines))
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 401 {
		t.Fatalf("err = %v", err)
	}
	if len(rec.reqs) != 1 {
		t.Errorf("requests = %d, want 1", len(rec.reqs))
	}
}

func TestWriteRetriesNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close()

	s := &Sink{Name: "t", URL: addr, DB: "testdb", Client: &http.Client{Timeout: time.Second}}
	err := s.Write(context.Background(), makeLines(1))
	if err == nil || !strings.Contains(err.Error(), "retry also failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteRejectsBadURL(t *testing.T) {
	s := &Sink{Name: "t", URL: "influx:8086", DB: "testdb"}
	if err := s.Write(context.Background(), makeLines(1)); err == nil {
		t.Fatal("accepted a URL without a scheme")
	}
}

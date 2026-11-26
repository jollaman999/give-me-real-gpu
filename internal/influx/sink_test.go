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
	authz    string
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
		authz:    req.Header.Get("Authorization"),
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

func TestWriteUnprocessableContinues(t *testing.T) {
	rec := &recorder{respond: func(n int) (int, string) {
		if n == 0 {
			return http.StatusUnprocessableEntity, `{"code":"unprocessable entity","message":"partial write: field type conflict dropped=1"}`
		}
		return http.StatusNoContent, ""
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	s := &Sink{Name: "site-a", URL: srv.URL, Version: V2, Org: "o", DB: "b", Token: "t"}
	err := s.Write(context.Background(), makeLines(ChunkLines+2))
	if len(rec.reqs) != 2 {
		t.Fatalf("requests = %d, want 2 (the next chunk must be sent after a 422)", len(rec.reqs))
	}
	var be *BadRequestError
	if !errors.As(err, &be) || be.Code != 422 || be.Message != "partial write: field type conflict dropped=1" {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "InfluxDB 422: ") {
		t.Errorf("error does not show 422: %v", err)
	}
}

func TestErrorMessage(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"error":"database not found: \"x\""}`, `database not found: "x"`},
		{`{"code":"unauthorized","message":"unauthorized access"}`, "unauthorized access"},
		{`{"error":"partial write of line protocol occurred","data":[{"error_message":"invalid column type for column 'v'","line_number":3,"original_line":"m v=1.5 1"}]}`,
			"partial write of line protocol occurred: line 3: invalid column type for column 'v'"},
		{`{"error":"partial write of line protocol occurred","data":[{"error_message":"a","line_number":1},{"error_message":"b","line_number":2},{"error_message":"c","line_number":5}]}`,
			"partial write of line protocol occurred: line 1: a (and 2 more lines)"},
		{"plain text\n", "plain text"},
	}
	for _, tc := range cases {
		if got := errorMessage([]byte(tc.body)); got != tc.want {
			t.Errorf("errorMessage(%s) = %q, want %q", tc.body, got, tc.want)
		}
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

func TestWriteV2(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	s := &Sink{Name: "t", URL: srv.URL + "/", Version: V2, Org: "my org", DB: "bucket-a", Token: "tok-2",
		Username: "left-over", Password: "<change-me>"}
	if err := s.Write(context.Background(), makeLines(2)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := rec.reqs[0]
	if got.query != "/api/v2/write?bucket=bucket-a&org=my+org&precision=ns" {
		t.Errorf("query = %q", got.query)
	}
	if got.authz != "Token tok-2" || got.hasAuth {
		t.Errorf("Authorization = %q (basic %v)", got.authz, got.hasAuth)
	}
	if got.encoding != "gzip" || len(got.lines) != 2 {
		t.Errorf("encoding %q lines %d", got.encoding, len(got.lines))
	}
}

func TestWriteV3(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	s := &Sink{Name: "t", URL: srv.URL, Version: V3, DB: "db3", Token: "tok-3"}
	if err := s.Write(context.Background(), makeLines(1)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	s.Token = ""
	if err := s.Write(context.Background(), makeLines(1)); err != nil {
		t.Fatalf("Write without token: %v", err)
	}
	if got := rec.reqs[0]; got.query != "/api/v3/write_lp?db=db3&precision=nanosecond" || got.authz != "Bearer tok-3" {
		t.Errorf("with token: query %q Authorization %q", got.query, got.authz)
	}
	if got := rec.reqs[1]; got.authz != "" {
		t.Errorf("Authorization %q sent with an empty token", got.authz)
	}
}

func TestWriteAcceptsAny2xx(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusCreated, http.StatusNoContent} {
		rec := &recorder{respond: func(int) (int, string) { return code, "" }}
		srv := httptest.NewServer(rec)
		s := &Sink{Name: "t", URL: srv.URL, Version: V3, DB: "d"}
		if err := s.Write(context.Background(), makeLines(1)); err != nil {
			t.Errorf("%d: %v", code, err)
		}
		srv.Close()
	}
}

func TestWriteUnknownVersion(t *testing.T) {
	s := &Sink{Name: "t", URL: "http://127.0.0.1:1", Version: "v4", DB: "d"}
	if err := s.Write(context.Background(), makeLines(1)); err == nil || !strings.Contains(err.Error(), "v4") {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteErrorsDoNotShowToken(t *testing.T) {
	const token = "secret-token-value"
	rec := &recorder{respond: func(int) (int, string) {
		return http.StatusUnauthorized, `{"code":"unauthorized","message":"unauthorized access"}`
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	for _, v := range []string{V2, V3} {
		for _, u := range []string{srv.URL, closedURL} {
			s := &Sink{Name: "t", URL: u, Version: v, Org: "o", DB: "d", Token: token, Client: &http.Client{Timeout: time.Second}}
			err := s.Write(context.Background(), makeLines(1))
			if err == nil {
				t.Fatalf("%s %s: no error", v, u)
			}
			if strings.Contains(err.Error(), token) {
				t.Errorf("%s: error shows the token: %v", v, err)
			}
		}
	}
}

func TestV3LinesOneLine(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"shared tag dropped",
			`dcgm,host=h1,pstate=P0,gpu=0 gpu_temp=40,pstate=0i 1700000000000000000`,
			`dcgm,host=h1,gpu=0 gpu_temp=40,pstate=0i 1700000000000000000`},
		{"only tag dropped",
			`m,pstate=P8 pstate=8i`,
			`m pstate=8i`},
		{"escaped tag values and quoted field kept",
			`m,host=a\ b\,c,k\=x=v\=1,v=1 v=2i,s="x, y=\"z\" w" 1`,
			`m,host=a\ b\,c,k\=x=v\=1 v=2i,s="x, y=\"z\" w" 1`},
		{"escaped measurement and keys",
			`my\,m\ x,a\ b=1,t=2 a\ b=2i 1`,
			`my\,m\ x,t=2 a\ b=2i 1`},
		{"key only inside a quoted value is not a field",
			`m,y=1 s="a,y=2 z=3" 1`,
			`m,y=1 s="a,y=2 z=3" 1`},
		{"backslash at the end of a quoted value",
			`m,s=1,u=2 s="dir\\",u=3i 1`,
			`m s="dir\\",u=3i 1`},
		{"no shared key", `cpu,cpu=cpu0,host=h usage_idle=99.5 1`, `cpu,cpu=cpu0,host=h usage_idle=99.5 1`},
		{"no tags", `m v=1i 1`, `m v=1i 1`},
		{"no timestamp", `m,v=1 v=1i`, `m v=1i`},
		{"not line protocol", `garbage`, `garbage`},
	}
	for _, tc := range cases {
		if got := v3Lines([]string{tc.in}); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s:\n got  %s\n want %s", tc.name, got, tc.want)
		}
	}
}

func TestV3LinesAcrossLines(t *testing.T) {
	in := []string{
		`dcgm,host=h1,pstate=P0 pstate=0i,gpu_temp=40 1`,
		`dcgm,host=h1,pstate=P0,gpu_i_id=1 sm_occupancy=0.5 1`,
		`other,pstate=P0 sm_occupancy=0.5 1`,
		`my\,m,k=1 v=1i 1`,
		`my\,m,k=2,v=3 x=1i 1`,
	}
	want := []string{
		`dcgm,host=h1 pstate=0i,gpu_temp=40 1`,
		`dcgm,host=h1,gpu_i_id=1 sm_occupancy=0.5 1`,
		`other,pstate=P0 sm_occupancy=0.5 1`,
		`my\,m,k=1 v=1i 1`,
		`my\,m,k=2 x=1i 1`,
	}
	got := v3Lines(in)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n got  %s\n want %s", i, got[i], want[i])
		}
	}
	if in[0] != `dcgm,host=h1,pstate=P0 pstate=0i,gpu_temp=40 1` {
		t.Errorf("input changed: %q", in[0])
	}
}

func TestWriteDropsSharedTagsOnlyForV3(t *testing.T) {
	shared := `dcgm,host=h1,pstate=P0 pstate=0i 1700000000000000000`
	plain := `cpu,host=h1 usage_idle=99.5 1700000000000000000`
	for _, tc := range []struct {
		version string
		want    string
	}{
		{V1, shared},
		{V2, shared},
		{V3, `dcgm,host=h1 pstate=0i 1700000000000000000`},
	} {
		rec := &recorder{}
		srv := httptest.NewServer(rec)
		lines := []string{shared, plain}
		s := &Sink{Name: "t", URL: srv.URL, Version: tc.version, Org: "o", DB: "d", Token: "t"}
		if err := s.Write(context.Background(), lines); err != nil {
			t.Fatalf("%s: %v", tc.version, err)
		}
		srv.Close()
		got := rec.reqs[0].lines
		if len(got) != 2 || got[0] != tc.want || got[1] != plain {
			t.Errorf("%s: sent %q, want [%q %q]", tc.version, got, tc.want, plain)
		}
		if lines[0] != shared {
			t.Errorf("%s: the caller's slice was changed to %q", tc.version, lines[0])
		}
	}
}

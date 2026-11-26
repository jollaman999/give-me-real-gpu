package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// retryDelay is how long to wait before retrying after a 5xx or network error. Tests shorten it.
var retryDelay = time.Second

// Sink is one OTLP/HTTP metrics endpoint (VictoriaMetrics, Prometheus or an OpenTelemetry Collector).
type Sink struct {
	Name     string // name used in logs (e.g. site-a)
	URL      string // full endpoint, e.g. http://192.0.2.20:8428/opentelemetry/v1/metrics
	Username string // basic auth is omitted when both Username and Password are empty
	Password string
	Token    string       // bearer token; when set it is sent instead of basic auth
	Client   *http.Client // nil means a client with a 30s timeout
}

var defaultClient = &http.Client{Timeout: 30 * time.Second}

// StatusError is a response other than 2xx.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("OTLP endpoint %d: %s", e.Code, e.Message)
}

// Write sends req as one gzip-compressed protobuf POST to URL.
// A 5xx or network error is retried once after a second; a 4xx is not retried.
func (s *Sink) Write(ctx context.Context, req *ExportRequest) error {
	if req.DataPoints() == 0 {
		return nil
	}
	if err := s.checkURL(); err != nil {
		return err
	}
	body, err := gzipBytes(req.Marshal())
	if err != nil {
		return fmt.Errorf("sink %s: %w", s.Name, err)
	}
	if err := s.post(ctx, body); err != nil {
		return fmt.Errorf("sink %s: %w", s.Name, err)
	}
	return nil
}

func (s *Sink) checkURL() error {
	u, err := url.Parse(s.URL)
	if err != nil {
		return fmt.Errorf("sink %s: url %q: %w", s.Name, s.URL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("sink %s: url %q: scheme must be http or https", s.Name, s.URL)
	}
	if u.Host == "" {
		return fmt.Errorf("sink %s: url %q: no host", s.Name, s.URL)
	}
	return nil
}

func gzipBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// post sends the body. On a 5xx or network error it sends it once more after retryDelay.
func (s *Sink) post(ctx context.Context, body []byte) error {
	err := s.postOnce(ctx, body)
	if err == nil || !retryable(err) {
		return err
	}
	select {
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	case <-time.After(retryDelay):
	}
	if err2 := s.postOnce(ctx, body); err2 != nil {
		return fmt.Errorf("retry also failed: %w", err2)
	}
	return nil
}

func retryable(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code >= 500
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func (s *Sink) postOnce(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")
	switch {
	case s.Token != "":
		req.Header.Set("Authorization", "Bearer "+s.Token)
	case s.Username != "" || s.Password != "":
		req.SetBasicAuth(s.Username, s.Password)
	}
	client := s.Client
	if client == nil {
		client = defaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return &StatusError{Code: resp.StatusCode, Message: errorMessage(raw, resp.Header.Get("Content-Type"))}
}

// errorMessage is the response body for logs. A protobuf google.rpc.Status
// body is not decoded: only its printable bytes are kept.
func errorMessage(raw []byte, contentType string) string {
	msg := string(raw)
	if strings.Contains(contentType, "protobuf") {
		msg = strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f || r == 0xfffd {
				return -1
			}
			return r
		}, msg)
	}
	msg = strings.TrimSpace(msg)
	if len(msg) > 500 {
		msg = msg[:500] + "..."
	}
	return msg
}

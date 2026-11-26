// Package influx sends line protocol to the /write endpoint of InfluxDB v1.
package influx

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ChunkLines is the maximum number of lines sent in one request.
const ChunkLines = 5000

// retryDelay is how long to wait before retrying after a 5xx or network error. Tests shorten it.
var retryDelay = time.Second

// Sink is one InfluxDB v1 database.
type Sink struct {
	Name     string // name used in logs (e.g. site-a)
	URL      string // e.g. http://influxdb:8086
	DB       string
	Username string // basic auth is omitted when both Username and Password are empty
	Password string
	Client   *http.Client // nil means a client with a 30s timeout
}

var defaultClient = &http.Client{Timeout: 30 * time.Second}

// Write sends lines gzip-compressed in chunks of ChunkLines.
// A 400 (partial write, field type conflict) is recorded as that chunk's error and the remaining chunks are still sent.
// A 5xx or network error is retried once; if it still fails, the remaining chunks are skipped.
func (s *Sink) Write(ctx context.Context, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	endpoint, err := s.writeURL()
	if err != nil {
		return err
	}
	var errs []error
	for start := 0; start < len(lines); start += ChunkLines {
		end := min(start+ChunkLines, len(lines))
		body, err := gzipLines(lines[start:end])
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		err = s.post(ctx, endpoint, body)
		if err == nil {
			continue
		}
		err = fmt.Errorf("sink %s: lines %d-%d: %w", s.Name, start, end-1, err)
		errs = append(errs, err)
		var be *BadRequestError
		if !errors.As(err, &be) {
			break
		}
	}
	return errors.Join(errs...)
}

// BadRequestError is a chunk rejected by InfluxDB with 400. Message is the error value of the response body.
type BadRequestError struct {
	Message string
}

func (e *BadRequestError) Error() string {
	return "InfluxDB 400: " + e.Message
}

// StatusError is a response other than 204 or 400.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("InfluxDB %d: %s", e.Code, e.Message)
}

func (s *Sink) writeURL() (string, error) {
	u, err := url.Parse(strings.TrimRight(s.URL, "/") + "/write")
	if err != nil {
		return "", fmt.Errorf("sink %s: url %q: %w", s.Name, s.URL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("sink %s: url %q: scheme must be http or https", s.Name, s.URL)
	}
	q := url.Values{}
	q.Set("db", s.DB)
	q.Set("precision", "ns")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func gzipLines(lines []string) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	for i, l := range lines {
		if i > 0 {
			if _, err := io.WriteString(zw, "\n"); err != nil {
				return nil, err
			}
		}
		if _, err := io.WriteString(zw, l); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// post sends one chunk. On a 5xx or network error it sends it once more after retryDelay.
func (s *Sink) post(ctx context.Context, endpoint string, body []byte) error {
	err := s.postOnce(ctx, endpoint, body)
	if err == nil || !retryable(err) {
		return err
	}
	select {
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	case <-time.After(retryDelay):
	}
	if err2 := s.postOnce(ctx, endpoint, body); err2 != nil {
		return fmt.Errorf("retry also failed: %w", err2)
	}
	return nil
}

func retryable(err error) bool {
	var be *BadRequestError
	if errors.As(err, &be) {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code >= 500
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func (s *Sink) postOnce(ctx context.Context, endpoint string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("Content-Encoding", "gzip")
	if s.Username != "" || s.Password != "" {
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
	switch {
	case resp.StatusCode == http.StatusNoContent:
		return nil
	case resp.StatusCode == http.StatusBadRequest:
		return &BadRequestError{Message: errorMessage(raw)}
	default:
		return &StatusError{Code: resp.StatusCode, Message: errorMessage(raw)}
	}
}

// errorMessage extracts the error value from InfluxDB's {"error":"..."} body. Non-JSON bodies are used as is.
func errorMessage(raw []byte) string {
	var v struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &v) == nil && v.Error != "" {
		return v.Error
	}
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 500 {
		msg = msg[:500] + "..."
	}
	return msg
}

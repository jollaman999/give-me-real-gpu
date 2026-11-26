// Package influx sends line protocol to InfluxDB: the /write endpoint of v1, /api/v2/write of v2
// and /api/v3/write_lp of v3.
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

// InfluxDB versions. They differ in the write endpoint and the authentication.
const (
	V1 = "v1" // POST /write?db=, Basic auth
	V2 = "v2" // POST /api/v2/write?org=&bucket=, Authorization: Token
	V3 = "v3" // POST /api/v3/write_lp?db=, Authorization: Bearer
)

// Sink is one InfluxDB database (v1, v3) or bucket (v2).
type Sink struct {
	Name     string // name used in logs (e.g. site-a)
	URL      string // e.g. http://influxdb:8086
	Version  string // V1, V2 or V3. Empty means V1
	Org      string // v2 only
	DB       string // the database (v1, v3) or the bucket (v2)
	Username string // v1 only. Basic auth is omitted when both Username and Password are empty
	Password string
	Token    string       // v2 (required) and v3 (omitted when empty)
	Client   *http.Client // nil means a client with a 30s timeout
}

var defaultClient = &http.Client{Timeout: 30 * time.Second}

// Write sends lines gzip-compressed in chunks of ChunkLines.
// A 400 or 422 (partial write, field type conflict) is recorded as that chunk's error and the remaining chunks are still sent.
// A 5xx or network error is retried once; if it still fails, the remaining chunks are skipped.
// For v3 the tags that share a key with a field of the same measurement are left out (see v3Lines).
func (s *Sink) Write(ctx context.Context, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	endpoint, err := s.writeURL()
	if err != nil {
		return err
	}
	if s.Version == V3 {
		lines = v3Lines(lines)
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

// BadRequestError is a chunk rejected by InfluxDB with 400, or with 422 which v2 returns for a partial write
// (field type conflict). Message is the error text of the response body.
type BadRequestError struct {
	Code    int // 400 or 422. 0 is shown as 400
	Message string
}

func (e *BadRequestError) Error() string {
	code := e.Code
	if code == 0 {
		code = http.StatusBadRequest
	}
	return fmt.Sprintf("InfluxDB %d: %s", code, e.Message)
}

// StatusError is a response other than 2xx, 400 or 422.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("InfluxDB %d: %s", e.Code, e.Message)
}

func (s *Sink) writeURL() (string, error) {
	var path string
	q := url.Values{}
	switch s.Version {
	case "", V1:
		path = "/write"
		q.Set("db", s.DB)
		q.Set("precision", "ns")
	case V2:
		path = "/api/v2/write"
		q.Set("org", s.Org)
		q.Set("bucket", s.DB)
		q.Set("precision", "ns")
	case V3:
		path = "/api/v3/write_lp"
		q.Set("db", s.DB)
		q.Set("precision", "nanosecond")
	default:
		return "", fmt.Errorf("sink %s: unknown InfluxDB version %q", s.Name, s.Version)
	}
	u, err := url.Parse(strings.TrimRight(s.URL, "/") + path)
	if err != nil {
		return "", fmt.Errorf("sink %s: url %q: %w", s.Name, s.URL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("sink %s: url %q: scheme must be http or https", s.Name, s.URL)
	}
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
	switch s.Version {
	case V2:
		req.Header.Set("Authorization", "Token "+s.Token)
	case V3:
		if s.Token != "" {
			req.Header.Set("Authorization", "Bearer "+s.Token)
		}
	default:
		if s.Username != "" || s.Password != "" {
			req.SetBasicAuth(s.Username, s.Password)
		}
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
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity:
		return &BadRequestError{Code: resp.StatusCode, Message: errorMessage(raw)}
	default:
		return &StatusError{Code: resp.StatusCode, Message: errorMessage(raw)}
	}
}

// errorMessage extracts the error text from an InfluxDB error body: {"error":"..."} (v1),
// {"code":"...","message":"..."} (v2) or {"error":"...","data":[{"line_number":1,"error_message":"..."}]} (v3,
// where data names the rejected lines). Non-JSON bodies are used as is.
func errorMessage(raw []byte) string {
	var v struct {
		Error   string `json:"error"`
		Message string `json:"message"`
		Data    []struct {
			LineNumber   int    `json:"line_number"`
			ErrorMessage string `json:"error_message"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &v) == nil && (v.Error != "" || v.Message != "") {
		msg := v.Error
		if msg == "" {
			msg = v.Message
		}
		if len(v.Data) > 0 && v.Data[0].ErrorMessage != "" {
			msg += fmt.Sprintf(": line %d: %s", v.Data[0].LineNumber, v.Data[0].ErrorMessage)
			if len(v.Data) > 1 {
				msg += fmt.Sprintf(" (and %d more lines)", len(v.Data)-1)
			}
		}
		return msg
	}
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 500 {
		msg = msg[:500] + "..."
	}
	return msg
}

// v3Lines returns a copy of lines without the tags whose key is also a field key of the same measurement
// in any of the lines. InfluxDB 3 keeps one column per key in a table, so it rejects a line that uses a key
// as a tag when the table (or the line itself) has it as a field; v1 and v2 accept both.
// Lines without such a tag, and lines that cannot be parsed, are kept byte for byte.
func v3Lines(lines []string) []string {
	type parsed struct {
		head    []string // measurement, then the tags, escapes kept
		headEnd int      // index of the space before the field set
	}
	ps := make([]parsed, len(lines))
	fieldKeys := map[string]map[string]bool{}
	for i, line := range lines {
		headEnd := indexUnescaped(line, 0, ' ', false)
		if headEnd < 0 {
			continue
		}
		head := splitUnescaped(line[:headEnd], ',', false)
		ps[i] = parsed{head: head, headEnd: headEnd}
		fieldsEnd := indexUnescaped(line, headEnd+1, ' ', true)
		if fieldsEnd < 0 {
			fieldsEnd = len(line)
		}
		m := unescape(head[0])
		if fieldKeys[m] == nil {
			fieldKeys[m] = map[string]bool{}
		}
		for _, f := range splitUnescaped(line[headEnd+1:fieldsEnd], ',', true) {
			fieldKeys[m][unescapeKey(f)] = true
		}
	}
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = line
		p := ps[i]
		if len(p.head) < 2 {
			continue
		}
		keys := fieldKeys[unescape(p.head[0])]
		kept := p.head[:1:1]
		for _, tag := range p.head[1:] {
			if !keys[unescapeKey(tag)] {
				kept = append(kept, tag)
			}
		}
		if len(kept) != len(p.head) {
			out[i] = strings.Join(kept, ",") + line[p.headEnd:]
		}
	}
	return out
}

// indexUnescaped returns the index of the first c at or after from that is not escaped by a backslash and,
// when quotes is set, not inside a double-quoted string field value. It returns -1 when there is none.
func indexUnescaped(s string, from int, c byte, quotes bool) int {
	inQuote := false
	for i := from; i < len(s); i++ {
		switch {
		case s[i] == '\\':
			i++
		case quotes && s[i] == '"':
			inQuote = !inQuote
		case s[i] == c && !inQuote:
			return i
		}
	}
	return -1
}

// splitUnescaped splits s at every sep found by indexUnescaped. The parts keep their escapes.
func splitUnescaped(s string, sep byte, quotes bool) []string {
	var parts []string
	for {
		i := indexUnescaped(s, 0, sep, quotes)
		if i < 0 {
			return append(parts, s)
		}
		parts = append(parts, s[:i])
		s = s[i+1:]
	}
}

// unescapeKey returns the key of a "key=value" pair of a tag or field set with its escapes removed.
func unescapeKey(pair string) string {
	if i := indexUnescaped(pair, 0, '=', false); i >= 0 {
		pair = pair[:i]
	}
	return unescape(pair)
}

// unescape removes the line protocol escapes of a measurement name or a tag or field key.
func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && strings.IndexByte(`,= \`, s[i+1]) >= 0 {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

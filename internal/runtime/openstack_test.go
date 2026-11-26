package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func serve(t *testing.T, h http.Handler) (int, string, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v3/auth/tokens", nil))
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Code, rec.Header().Get("Content-Type"), string(body)
}

func checkUnavailable(t *testing.T, sw *OpenStackSwitch, wantReason string) {
	t.Helper()
	code, ct, body := serve(t, sw)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got struct{ Error, Reason string }
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body %q is not JSON: %v", body, err)
	}
	if got.Error == "" || got.Reason != wantReason {
		t.Errorf("body = %+v, want non-empty error and reason %q", got, wantReason)
	}
	if ready, reason := sw.State(); ready || reason != wantReason {
		t.Errorf("State() = %v, %q, want false, %q", ready, reason, wantReason)
	}
}

func TestOpenStackSwitch(t *testing.T) {
	var sw OpenStackSwitch
	checkUnavailable(t, &sw, "not configured yet")

	sw.SetUnavailable("advertise_host is not set")
	checkUnavailable(t, &sw, "advertise_host is not set")

	sw.Set(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "first")
	}))
	if code, _, body := serve(t, &sw); code != http.StatusCreated || body != "first" {
		t.Errorf("after Set: %d %q, want 201 first", code, body)
	}
	if ready, reason := sw.State(); !ready || reason != "" {
		t.Errorf("State() = %v, %q, want true, empty", ready, reason)
	}

	sw.Set(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "second")
	}))
	if code, _, body := serve(t, &sw); code != http.StatusOK || body != "second" {
		t.Errorf("after second Set: %d %q, want 200 second", code, body)
	}

	sw.SetUnavailable("")
	checkUnavailable(t, &sw, "not configured yet")

	sw.Set(nil)
	checkUnavailable(t, &sw, "no handler")
}

func TestOpenStackSwitchConcurrentSwap(t *testing.T) {
	var sw OpenStackSwitch
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 500 {
			sw.Set(ok)
			sw.SetUnavailable("changing")
		}
	})
	wg.Go(func() {
		for range 500 {
			code, _, _ := serve(t, &sw)
			if code != http.StatusOK && code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 200 or 503", code)
				return
			}
		}
	})
	wg.Wait()
}

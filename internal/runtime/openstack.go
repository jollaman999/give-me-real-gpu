package runtime

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
)

// defaultUnavailableReason is reported before Set or SetUnavailable is called.
const defaultUnavailableReason = "not configured yet"

// OpenStackSwitch is an http.Handler that forwards to the current fake OpenStack handler.
// The handler can be swapped at any time without restarting the listener.
// While no handler is set it answers 503 with {"error": "...", "reason": "..."}.
// The zero value is ready to use and reports the service as unavailable.
type OpenStackSwitch struct {
	cur atomic.Pointer[switchTarget]
}

type switchTarget struct {
	h      http.Handler
	reason string
}

// Set makes h serve all following requests. A nil h is the same as SetUnavailable("no handler").
func (s *OpenStackSwitch) Set(h http.Handler) {
	if h == nil {
		s.SetUnavailable("no handler")
		return
	}
	s.cur.Store(&switchTarget{h: h})
}

// SetUnavailable makes all following requests answer 503 with reason.
func (s *OpenStackSwitch) SetUnavailable(reason string) {
	if reason == "" {
		reason = defaultUnavailableReason
	}
	s.cur.Store(&switchTarget{reason: reason})
}

// State reports whether a handler is set and, if not, why.
func (s *OpenStackSwitch) State() (ready bool, reason string) {
	t := s.cur.Load()
	if t == nil {
		return false, defaultUnavailableReason
	}
	if t.h == nil {
		return false, t.reason
	}
	return true, ""
}

func (s *OpenStackSwitch) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t := s.cur.Load()
	if t != nil && t.h != nil {
		t.h.ServeHTTP(w, r)
		return
	}
	reason := defaultUnavailableReason
	if t != nil {
		reason = t.reason
	}
	body, _ := json.Marshal(struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}{"OpenStack API is not available", reason})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write(body)
}

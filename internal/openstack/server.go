// Package openstack serves a fake OpenStack API (keystone, nova, placement,
// glance, cinder, neutron, octavia) built from a topology.Topology.
//
// All services share one port and are told apart by a path prefix, because
// openstack4j clients that use endpoint NAT resolution only replace the host
// of each catalog URL and keep its port and path.
//
// The API is read-only. Responses follow the JSON shapes that openstack4j
// deserializes (field names and root keys of its domain classes).
package openstack

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

// Config holds the runtime settings of the fake API.
type Config struct {
	Password      string        // keystone password (user name and project come from the topology)
	AdvertiseHost string        // host used in catalog URLs (clients with NAT resolution replace it)
	Port          int           // port used in catalog URLs (= listen port)
	TokenTTL      time.Duration // token lifetime, 1h when zero
}

type obj = map[string]any

type server struct {
	topo   *topology.Topology
	cfg    Config
	tokens *tokenStore
	now    func() time.Time
	mux    *http.ServeMux
	net    *netModel
}

// NewServer returns an http.Handler that serves every fake OpenStack service.
func NewServer(t *topology.Topology, cfg Config) http.Handler {
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = time.Hour
	}
	s := &server{
		topo:   t,
		cfg:    cfg,
		tokens: newTokenStore(),
		now:    time.Now,
		mux:    http.NewServeMux(),
	}
	s.net = buildNetModel(t)
	s.routes()
	return s
}

func (s *server) routes() {
	s.keystoneRoutes()
	s.novaRoutes()
	s.placementRoutes()
	s.glanceRoutes()
	s.cinderRoutes()
	s.neutronRoutes()
	s.octaviaRoutes()
	s.mux.HandleFunc("/", s.fallback)
}

// publicPaths are served without a token: keystone discovery and the
// version documents at the root of every service.
var publicPaths = map[string]bool{
	"/": true, "/v3": true, "/v3/": true,
	"/compute": true, "/compute/": true,
	"/compute/v2": true, "/compute/v2/": true,
	"/compute/v2.1": true, "/compute/v2.1/": true,
	"/placement": true, "/placement/": true,
	"/image": true, "/image/": true,
	"/volume": true, "/volume/": true, "/volume/v3": true, "/volume/v3/": true,
	"/network": true, "/network/": true,
	"/load-balancer": true, "/load-balancer/": true,
}

func (s *server) isPublic(r *http.Request) bool {
	if r.Method == http.MethodPost && r.URL.Path == "/v3/auth/tokens" {
		return true
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return publicPaths[r.URL.Path]
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	if !s.isPublic(r) && !s.tokens.valid(r.Header.Get("X-Auth-Token"), s.now()) {
		w.Header().Set("WWW-Authenticate", `Keystone uri="`+s.baseURL()+`/v3"`)
		writeError(rec, http.StatusUnauthorized, "error", "The request you have made requires authentication.")
	} else {
		s.mux.ServeHTTP(rec, r)
	}
	slog.Debug("openstack api request",
		"method", r.Method, "path", r.URL.Path, "query", r.URL.RawQuery,
		"status", rec.status, "elapsed", time.Since(start))
}

// fallback answers every path no service claims. Writes are refused because
// the emulator never changes its topology.
func (s *server) fallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusForbidden, "forbidden", "The emulator is read-only: "+r.Method+" "+r.URL.Path)
		return
	}
	notFound(w, r)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// baseURL is the scheme, host and port used in catalog URLs.
func (s *server) baseURL() string {
	return "http://" + hostPort(s.cfg.AdvertiseHost, s.cfg.Port)
}

func hostPort(host string, port int) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	if port == 0 {
		return host
	}
	return host + ":" + itoa(port)
}

// requestBase is the scheme and host the client used. Version documents link
// to it so that discovery keeps working behind NAT.
func requestBase(r *http.Request) string {
	if r.Host == "" {
		return "http://localhost"
	}
	return "http://" + r.Host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		slog.Error("encode openstack api response", "err", err)
		http.Error(w, `{"error":{"code":500,"message":"encode failure"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// writeError writes the OpenStack error body {"<key>": {"code", "message"}}.
// keystone uses key "error" with a title, nova/cinder use itemNotFound etc.
func writeError(w http.ResponseWriter, status int, key, message string) {
	body := obj{"code": status, "message": message}
	if key == "error" {
		body["title"] = http.StatusText(status)
	}
	writeJSON(w, status, obj{key: body})
}

func notFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "itemNotFound", "Resource not found: "+r.URL.Path)
}

// list returns v as a JSON array that is never null.
func list[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// Package web serves the settings UI. The page is plain HTML, CSS and JavaScript
// embedded into the binary, so it works on networks without internet access.
// The page talks to the settings API (internal/api) on /api/v1 of the same origin.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed static
var staticFS embed.FS

// Prefix is the URL path the UI is served under. "/ui/" itself is index.html.
const Prefix = "/ui/"

var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".json": "application/json; charset=utf-8",
	".svg":  "image/svg+xml",
}

const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

type file struct {
	body        []byte
	contentType string
	etag        string
}

type handler struct {
	files map[string]*file // key is the path below static/, e.g. "i18n/en.json"
}

// Handler serves the embedded UI below Prefix. "/ui" redirects to "/ui/".
// Anything else, including directories, is 404.
func Handler() http.Handler {
	files, err := load(staticFS)
	if err != nil {
		panic("web: failed to read the embedded files: " + err.Error())
	}
	return &handler{files: files}
}

func load(fsys fs.FS) (map[string]*file, error) {
	files := map[string]*file{}
	err := fs.WalkDir(fsys, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		ct, ok := contentTypes[path.Ext(p)]
		if !ok {
			ct = "application/octet-stream"
		}
		sum := sha256.Sum256(body)
		files[strings.TrimPrefix(p, "static/")] = &file{
			body:        body,
			contentType: ct,
			etag:        `"` + hex.EncodeToString(sum[:8]) + `"`,
		}
		return nil
	})
	return files, err
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	if p == strings.TrimSuffix(Prefix, "/") {
		target := Prefix
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently)
		return
	}
	name, ok := strings.CutPrefix(p, Prefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if name == "" {
		name = "index.html"
	}
	if path.Clean(name) != name {
		http.NotFound(w, r)
		return
	}
	f := h.files[name]
	if f == nil {
		http.NotFound(w, r)
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", f.contentType)
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("ETag", f.etag)
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Referrer-Policy", "no-referrer")
	if f.contentType == contentTypes[".html"] {
		hdr.Set("Content-Security-Policy", contentSecurityPolicy)
		hdr.Set("X-Frame-Options", "DENY")
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(f.body))
}

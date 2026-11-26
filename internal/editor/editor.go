// Package editor serves the two helper endpoints of the topology editor: the palette catalog and
// the fill-and-check call the editor makes after every change. It never saves anything;
// saving goes through PUT /api/v1/topology of the settings API.
package editor

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

// Paths served by Handler.
const (
	CatalogPath = "/api/v1/topology/catalog"
	FillPath    = "/api/v1/topology/fill"
)

// maxBody limits the fill request body, the same limit the settings API uses.
const maxBody = 4 << 20

// FillResponse is the body of POST /api/v1/topology/fill.
// Problems holds one line per Validate error and is empty when the topology is valid.
type FillResponse struct {
	Topology *topology.Topology `json:"topology"`
	Problems []string           `json:"problems"`
}

type errorBody struct {
	Error    string   `json:"error"`
	Problems []string `json:"problems,omitempty"`
}

// Handler serves GET CatalogPath and POST FillPath. Other paths are 404.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+CatalogPath, catalog)
	mux.HandleFunc("POST "+FillPath, fill)
	return mux
}

func catalog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, topology.DefaultCatalog())
}

func fill(w http.ResponseWriter, r *http.Request) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	var t topology.Topology
	err := dec.Decode(&t)
	if err == nil && dec.Decode(&struct{}{}) != io.EOF {
		err = errors.New("unexpected data after the JSON value")
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid request body", Problems: []string{err.Error()}})
		return
	}
	topology.Fill(&t)
	resp := FillResponse{Topology: &t, Problems: []string{}}
	if err := t.Validate(); err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			if line != "" {
				resp.Problems = append(resp.Problems, line)
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("editor: write response", "err", err)
	}
}

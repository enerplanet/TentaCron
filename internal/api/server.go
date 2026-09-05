// Package api implements tentacron's public HTTP interface: job submission
// and retrieval under /v1/requests, health endpoints, API-key authentication
// and the shared JSON error envelope.
package api

import (
	"log/slog"
	"net/http"

	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/store"
)

// BuildInfo identifies the running binary on /version and /healthz.
type BuildInfo struct {
	Version  string `json:"version"`
	Go       string `json:"go,omitempty"`
	Revision string `json:"revision,omitempty"`
	Built    string `json:"built,omitempty"`
}

// Server holds the HTTP handler dependencies.
type Server struct {
	cfg    *config.Config
	store  *store.Store
	logger *slog.Logger
	nudge  chan<- struct{}
	// Build is reported by /version and /healthz; main fills it in.
	Build BuildInfo
}

// New builds the API server. nudge is signalled (non-blocking) whenever a new
// job is accepted so the worker pool wakes up immediately.
func New(cfg *config.Config, st *store.Store, logger *slog.Logger, nudge chan<- struct{}) *Server {
	return &Server{cfg: cfg, store: st, logger: logger, nudge: nudge}
}

// Handler returns the fully wired HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/requests", s.handleCreate)
	mux.HandleFunc("GET /v1/requests", s.handleList)
	mux.HandleFunc("GET /v1/requests/{id}", s.handleGet)
	mux.HandleFunc("GET /v1/requests/{id}/result", s.handleResult)
	mux.HandleFunc("GET /v1/requests/{id}/events", s.handleEvents)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /version", s.handleVersion)
	return s.withRecovery(s.withRequestLog(jsonFallback(mux)))
}

// jsonFallback keeps the mux's own distinction between an unknown route
// (404) and a known route with the wrong method (405 plus Allow) — which a
// catch-all pattern would erase — while answering both in the JSON error
// envelope instead of net/http's plain text.
func jsonFallback(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := mux.Handler(r)
		if pattern != "" {
			// Dispatch through the mux, not h directly: only ServeHTTP binds
			// the {id} path values the handlers read.
			mux.ServeHTTP(w, r)
			return
		}
		// An empty pattern means net/http's 404 or 405 handler; run it
		// against a probe to learn which, then translate.
		p := &statusProbe{header: http.Header{}}
		h.ServeHTTP(p, r)
		if p.status == http.StatusMethodNotAllowed {
			w.Header().Set("Allow", p.header.Get("Allow"))
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
				"method "+r.Method+" is not allowed for this route")
			return
		}
		writeError(w, http.StatusNotFound, CodeNotFound, "no such route")
	})
}

// statusProbe captures the status and headers a handler would send and
// discards its body.
type statusProbe struct {
	header http.Header
	status int
}

func (p *statusProbe) Header() http.Header         { return p.header }
func (p *statusProbe) Write(b []byte) (int, error) { return len(b), nil }
func (p *statusProbe) WriteHeader(status int)      { p.status = status }

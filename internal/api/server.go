// Package api implements tentacron's public HTTP interface: job submission
// and retrieval under /v1/requests, health endpoints, API-key authentication
// and the shared JSON error envelope.
package api

import (
	"log/slog"
	"net/http"

	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/notify"
	"github.com/enerplanet/tentacron/internal/store"
	"github.com/enerplanet/tentacron/internal/upstream"
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
	// upstream, when set, lets a cancellation tell a poll-mode target to
	// stop its job (cancel_url_template).
	upstream *upstream.Client
	// notifier wakes long-polling reads when a job ends; nil falls back to
	// periodic re-reads.
	notifier *notify.Hub
}

// WithNotifier wakes long-polling reads on terminal transitions instead of
// leaving them to the one-second fallback re-read.
func (s *Server) WithNotifier(h *notify.Hub) *Server {
	s.notifier = h
	return s
}

// WithUpstream enables best-effort target notifications on cancellation.
func (s *Server) WithUpstream(c *upstream.Client) *Server {
	s.upstream = c
	return s
}

// New builds the API server. nudge is signalled (non-blocking) whenever a new
// job is accepted so the worker pool wakes up immediately.
func New(cfg *config.Config, st *store.Store, logger *slog.Logger, nudge chan<- struct{}) *Server {
	return &Server{cfg: cfg, store: st, logger: logger, nudge: nudge}
}

// route is one endpoint of the API. The OpenAPI description in openapi.yaml
// lists exactly these; a test keeps the two in lockstep.
type route struct {
	method, pattern string
	handler         http.HandlerFunc
}

func (s *Server) routes() []route {
	return []route{
		{http.MethodPost, "/v1/requests", s.handleCreate},
		{http.MethodPost, "/v1/requests/validate", s.handleValidate},
		{http.MethodPost, "/v1/requests/batch", s.handleBatch},
		{http.MethodGet, "/v1/requests", s.handleList},
		{http.MethodGet, "/v1/targets", s.handleTargets},
		{http.MethodGet, "/v1/resolvents", s.handleResolvents},
		{http.MethodGet, "/v1/requests/{id}", s.handleGet},
		{http.MethodDelete, "/v1/requests/{id}", s.handleCancel},
		{http.MethodGet, "/v1/requests/{id}/result", s.handleResult},
		{http.MethodGet, "/v1/requests/{id}/events", s.handleEvents},
		{http.MethodGet, "/healthz", s.handleHealthz},
		{http.MethodGet, "/readyz", s.handleReadyz},
		{http.MethodGet, "/version", s.handleVersion},
		{http.MethodGet, "/openapi.yaml", s.handleOpenAPI},
	}
}

// Handler returns the fully wired HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		mux.HandleFunc(rt.method+" "+rt.pattern, rt.handler)
	}
	return s.withRecovery(s.withRequestLog(s.withCORS(jsonFallback(mux))))
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

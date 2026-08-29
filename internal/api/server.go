package api

import (
	"log/slog"
	"net/http"

	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/store"
)

// Server holds the HTTP handler dependencies.
type Server struct {
	cfg    *config.Config
	store  *store.Store
	logger *slog.Logger
	nudge  chan<- struct{}
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
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such route")
	})
	return s.withRecovery(s.withRequestLog(mux))
}

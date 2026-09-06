package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/enerplanet/tentacron/internal/cors"
)

// maxRequestIDLen bounds the client-supplied X-Request-ID that is echoed and
// logged; longer values are replaced by a generated id.
const maxRequestIDLen = 128

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// requestMeta collects per-request facts the handlers learn after the
// middleware started (the authenticated client) for the request log line.
type requestMeta struct{ client string }

type requestMetaKey struct{}

// noteClient records the authenticated client name for the request log.
func noteClient(r *http.Request, name string) {
	if m, ok := r.Context().Value(requestMetaKey{}).(*requestMeta); ok {
		m.client = name
	}
}

// probePaths are hit by orchestrators every few seconds; their request
// lines are logged at debug so they do not drown the log.
var probePaths = map[string]bool{"/healthz": true, "/readyz": true, "/version": true}

// withRequestLog assigns/echoes a request id and logs one line per request,
// including the client name once a handler authenticated the caller and the
// status a recovered panic turned into. Probes log at debug.
func (s *Server) withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" || len(reqID) > maxRequestIDLen {
			var b [8]byte
			_, _ = rand.Read(b[:])
			reqID = hex.EncodeToString(b[:])
		}
		w.Header().Set("X-Request-ID", reqID)
		meta := &requestMeta{}
		r = r.WithContext(context.WithValue(r.Context(), requestMetaKey{}, meta))
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		attrs := []any{
			"request_id", reqID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"client", meta.client,
			"duration_ms", time.Since(start).Milliseconds(),
		}
		// A browser's origin, and whether the policy refused it, so one
		// grep answers "is my frontend's origin configured".
		denied := false
		if origin := r.Header.Get("Origin"); origin != "" {
			attrs = append(attrs, "origin", origin)
			if h := s.cors.Load(); h != nil && h.Enabled() && !h.Allows(origin) {
				denied = true
				attrs = append(attrs, "cors", "denied")
			}
		}
		level := slog.LevelInfo
		if probePaths[r.URL.Path] || (isPreflight(r) && !denied) {
			// Probes and preflights arrive every few seconds; at info they
			// would drown the log. A refused preflight stays at info: the
			// browser sends no actual request after it, so this line is
			// the only trace of a frontend whose origin is not configured.
			level = slog.LevelDebug
		}
		s.logger.Log(r.Context(), level, "request", attrs...)
	})
}

// isPreflight reports a CORS preflight: an OPTIONS naming the method the
// browser wants to send.
func isPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
}

// withRecovery turns handler panics into a JSON 500 and a structured log
// entry; without it net/http logs the stack and just drops the connection.
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic in handler", "panic", rec, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, CodeInternal, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withCORS installs the browser policy from the configuration. It is
// always in the chain, a pass-through while no origin is allowed, so a
// reload can enable, change or disable it without a restart.
func (s *Server) withCORS(next http.Handler) http.Handler {
	h := cors.New(s.cfg().Server.CORS.Policy(), next)
	s.cors.Store(h)
	return h
}

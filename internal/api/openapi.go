package api

import (
	"net/http"
	"strconv"

	"github.com/enerplanet/tentacron/docs/openapi"
)

// handleOpenAPI serves the OpenAPI document embedded from
// docs/openapi/openapi.yaml, so a running service always describes its own
// build. Unauthenticated, like the health endpoints: it reveals the contract,
// never the configuration.
func (s *Server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Length", strconv.Itoa(len(openapi.Spec)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openapi.Spec)
}

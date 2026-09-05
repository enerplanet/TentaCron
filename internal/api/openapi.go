package api

import (
	_ "embed"
	"net/http"
	"strconv"
)

// openAPISpec is the API description shipped with the binary, so a running
// service always describes its own build.
//
//go:embed openapi.yaml
var openAPISpec []byte

// handleOpenAPI serves the OpenAPI document. Unauthenticated, like the health
// endpoints: it reveals the contract, never the configuration.
func (s *Server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Length", strconv.Itoa(len(openAPISpec)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openAPISpec)
}

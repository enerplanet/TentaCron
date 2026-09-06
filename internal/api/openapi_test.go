package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/enerplanet/tentacron/docs/openapi"
	"gopkg.in/yaml.v3"
)

// The OpenAPI document and the registered routes must agree exactly: every
// route is described, and every described operation is served. HEAD is the
// one documented method without its own route — net/http serves it for
// every GET pattern.
func TestOpenAPIDescribesExactlyTheRoutes(t *testing.T) {
	var doc struct {
		OpenAPI string                            `yaml:"openapi"`
		Paths   map[string]map[string]interface{} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(openapi.Spec, &doc); err != nil {
		t.Fatalf("openapi.yaml does not parse: %v", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		t.Errorf("openapi version = %q, want 3.1.x", doc.OpenAPI)
	}
	described := map[string]bool{}
	for path, ops := range doc.Paths {
		for method := range ops {
			if method == "parameters" {
				continue
			}
			described[strings.ToUpper(method)+" "+path] = true
		}
	}
	served := map[string]bool{}
	for _, rt := range newEnv(t).server.routes() {
		key := rt.method + " " + rt.pattern
		served[key] = true
		if !described[key] {
			t.Errorf("route %s is not described in openapi.yaml", key)
		}
	}
	for key := range described {
		if served[key] {
			continue
		}
		if method, path, _ := strings.Cut(key, " "); method == http.MethodHead && served[http.MethodGet+" "+path] {
			continue
		}
		t.Errorf("openapi.yaml describes %s, which no route serves", key)
	}
}

func TestOpenAPIEndpoint(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, "GET", "/openapi.yaml", "", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/yaml" {
		t.Fatalf("/openapi.yaml: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.HasPrefix(rec.Body.String(), "openapi: 3.1") || rec.Header().Get("Content-Length") == "" {
		t.Errorf("body/headers: %q len=%q", rec.Body.String()[:20], rec.Header().Get("Content-Length"))
	}
}

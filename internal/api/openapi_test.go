package api

import (
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/enerplanet/tentacron/docs/openapi"
	"github.com/enerplanet/tentacron/internal/plan"
	"github.com/enerplanet/tentacron/internal/store"
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

// enumAt returns the enum found at a dotted path into the parsed document.
func enumAt(t *testing.T, doc map[string]any, path string) []string {
	t.Helper()
	var cur any = doc
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			cur = node[seg]
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i >= len(node) {
				t.Fatalf("path %s: bad index %q", path, seg)
			}
			cur = node[i]
		default:
			t.Fatalf("path %s: %q is not a container", path, seg)
		}
		if cur == nil {
			t.Fatalf("path %s: %q not found", path, seg)
		}
	}
	raw, ok := cur.([]any)
	if !ok {
		t.Fatalf("path %s: not an enum list", path)
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		out = append(out, fmt.Sprint(v))
	}
	return out
}

func sortedSet(values ...[]string) []string {
	set := map[string]bool{}
	for _, list := range values {
		for _, v := range list {
			set[v] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// The description's three code enums must equal the code's lists in both
// directions: a constant the description does not know, or a documented
// code nothing produces, fails here — including codes no golden records.
func TestOpenAPIErrorCodesEqualTheConstants(t *testing.T) {
	var doc map[string]any
	if err := yaml.Unmarshal(openapi.Spec, &doc); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		want       []string
	}{
		{"Error.code", "components.schemas.Error.properties.error.properties.code.enum", sortedSet(ErrorCodes(), plan.ProblemCodes())},
		{"Job.error.code", "components.schemas.Job.properties.error.oneOf.1.properties.code.enum", sortedSet(store.JobErrorCodes())},
		{"ValidateResponse.problems.code", "components.schemas.ValidateResponse.properties.problems.items.properties.code.enum", sortedSet(plan.ProblemCodes())},
	} {
		got := sortedSet(enumAt(t, doc, tc.path))
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s enum\n  description: %v\n  code:        %v", tc.name, got, tc.want)
		}
	}
}

// Every error code the handlers answer with is a named constant, so the
// parity test above sees all of them: a quoted literal in a writeError
// call or an errorDetail literal fails here.
func TestErrorCodesAreConstantsInHandlers(t *testing.T) {
	literal := regexp.MustCompile(`(?m)(writeError\([^\n]*?,\s*"[a-z_]+"|Code:\s*"[a-z_]+")`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range literal.FindAll(src, -1) {
			t.Errorf("%s: error code given as a literal: %s", f, m)
		}
	}
}

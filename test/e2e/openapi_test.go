package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"
	"github.com/pb33f/libopenapi-validator/config"
	verrors "github.com/pb33f/libopenapi-validator/errors"
	"gopkg.in/yaml.v3"

	"github.com/enerplanet/tentacron/docs/openapi"
)

// goldenExchange is one recorded HTTP exchange of a golden transcript. The
// other step kinds (forwarded payloads, audit trails, call counts) carry no
// "request" and are not exchanges with the API.
type goldenExchange struct {
	Step        string          `json:"step"`
	Request     string          `json:"request"`
	Status      int             `json:"status"`
	ContentType string          `json:"content_type"`
	Response    json.RawMessage `json:"response"`
}

// placeholder matches the harness's «…» normalisation markers.
var placeholder = regexp.MustCompile(`«([a-z]+)(?:-([0-9]+))?»`)

// syntheticID is a request id of the documented shape (32 hex digits).
func syntheticID(n int) string { return fmt.Sprintf("%032d", n) }

// concrete replaces the transcript placeholders with values of the shape
// the description promises — a request id of 32 hex digits, an RFC 3339
// time, an opaque cursor — so format assertions stay on. The values are
// synthetic; only their shape matters here.
func concrete(s string) string {
	return placeholder.ReplaceAllStringFunc(s, func(m string) string {
		sub := placeholder.FindStringSubmatch(m)
		n, _ := strconv.Atoi(sub[2])
		switch sub[1] {
		case "job":
			return syntheticID(n)
		case "schedule":
			return "5" + syntheticID(n)[1:]
		case "ts":
			return "2026-01-01T00:00:00Z"
		case "cursor":
			return "MTc1MDAwMDAwMDAwMC4x"
		case "upstream":
			return "http://127.0.0.1:1"
		case "addr":
			return "127.0.0.1:1"
		case "dur":
			return "1s"
		}
		return m
	})
}

// newSpecValidator loads docs/openapi/openapi.yaml into a validator with
// format assertions on, and fails the test if the document itself is not a
// valid description.
func newSpecValidator(t *testing.T) validator.Validator {
	t.Helper()
	doc, err := libopenapi.NewDocument(openapi.Spec)
	if err != nil {
		t.Fatalf("openapi.yaml does not load: %v", err)
	}
	v, errs := validator.NewValidator(doc, config.WithFormatAssertions())
	if len(errs) > 0 {
		t.Fatalf("openapi.yaml does not build: %v", errs)
	}
	if ok, errs := v.ValidateDocument(); !ok {
		for _, e := range errs {
			t.Errorf("openapi.yaml: %s: %s", e.Message, e.Reason)
		}
		t.FailNow()
	}
	return v
}

// documentedPaths turns the description's path templates into matchers, so
// a deliberate probe of an unknown route can be told apart from a
// documented operation the validator should judge.
func documentedPaths(t *testing.T) []*regexp.Regexp {
	t.Helper()
	var doc struct {
		Paths map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(openapi.Spec, &doc); err != nil {
		t.Fatal(err)
	}
	param := regexp.MustCompile(`\{[^/]+\}`)
	var out []*regexp.Regexp
	for p := range doc.Paths {
		var b strings.Builder
		b.WriteString("^")
		last := 0
		for _, loc := range param.FindAllStringIndex(p, -1) {
			b.WriteString(regexp.QuoteMeta(p[last:loc[0]]))
			b.WriteString(`[^/]+`)
			last = loc[1]
		}
		b.WriteString(regexp.QuoteMeta(p[last:]))
		b.WriteString("$")
		out = append(out, regexp.MustCompile(b.String()))
	}
	return out
}

// exchange rebuilds the request and response of a recorded step for the
// validator: the request line names method and path, the body is the
// recorded JSON or, for a file download, the recorded bytes.
func exchange(st goldenExchange) (*http.Request, *http.Response) {
	method, path, _ := strings.Cut(st.Request, " ")
	path = strings.ReplaceAll(path, "{id}", syntheticID(0))
	req, err := http.NewRequest(method, "http://tentacron"+path, http.NoBody)
	if err != nil {
		panic(err)
	}
	body := []byte(st.Response)
	contentType := st.ContentType
	if bytes.HasPrefix(body, []byte(`"`)) { // a non-JSON body, recorded as a string
		var s string
		_ = json.Unmarshal(body, &s)
		body = []byte(s)
	} else if contentType == "" {
		contentType = "application/json"
	}
	resp := &http.Response{StatusCode: st.Status, Header: http.Header{}, Request: req,
		Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
	if len(body) == 0 {
		resp.Body = http.NoBody
	} else {
		resp.Header.Set("Content-Type", contentType)
	}
	return req, resp
}

// TestGoldenResponsesMatchOpenAPI validates every response the golden
// corpus records against docs/openapi/openapi.yaml: the status code must be
// documented for the operation, the media type declared, and the body must
// conform to the schema. The route test in internal/api proves the paths
// agree with the code; this proves the shapes do — a field added to a
// response, or an error code the description does not list, fails here.
// A probe of an undocumented route must answer 404 in the error shape.
func TestGoldenResponsesMatchOpenAPI(t *testing.T) {
	v := newSpecValidator(t)
	paths := documentedPaths(t)
	files, err := filepath.Glob(filepath.Join("testdata", "golden", "*.golden.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no golden files: %v", err)
	}
	checked := 0
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var steps []goldenExchange
		if err := json.Unmarshal([]byte(concrete(string(raw))), &steps); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for _, st := range steps {
			if st.Request == "" {
				continue
			}
			req, resp := exchange(st)
			if !documented(paths, req.URL.Path) {
				assertUnknownRoute(t, file, st)
				continue
			}
			checked++
			if ok, errs := v.ValidateHttpResponse(req, resp); !ok {
				for _, e := range errs {
					t.Errorf("%s: step %q (%s → %d): %s: %s%s",
						filepath.Base(file), st.Step, st.Request, st.Status, e.Message, e.Reason, schemaDetails(e))
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no recorded responses were validated")
	}
	t.Logf("%d recorded responses validated against the description", checked)
}

func documented(paths []*regexp.Regexp, path string) bool {
	for _, p := range paths {
		if p.MatchString(path) {
			return true
		}
	}
	return false
}

// assertUnknownRoute checks the contract for a route the description does
// not have: 404 in the standard error shape with code not_found.
func assertUnknownRoute(t *testing.T, file string, st goldenExchange) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(st.Response, &body); err != nil || st.Status != http.StatusNotFound ||
		body.Error.Code != "not_found" || body.Error.Message == "" {
		t.Errorf("%s: step %q (%s): an undocumented route must answer 404 not_found in the error shape, got %d %s",
			filepath.Base(file), st.Step, st.Request, st.Status, st.Response)
	}
}

func schemaDetails(e *verrors.ValidationError) string {
	var lines []string
	for _, f := range e.SchemaValidationErrors {
		lines = append(lines, "    "+f.Error())
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n" + strings.Join(lines, "\n")
}

// TestExamplesMatchOpenAPI validates every file under examples/ as the body
// of POST /v1/requests against the description's CreateRequest schema. The
// golden suite proves the examples run; this proves the description
// describes them.
func TestExamplesMatchOpenAPI(t *testing.T) {
	v := newSpecValidator(t)
	files, err := filepath.Glob(filepath.Join("..", "..", "examples", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no example files: %v", err)
	}
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodPost, "http://tentacron/v1/requests", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", clientKey)
		if ok, errs := v.ValidateHttpRequest(req); !ok {
			for _, e := range errs {
				t.Errorf("examples/%s: %s: %s%s", filepath.Base(file), e.Message, e.Reason, schemaDetails(e))
			}
		}
	}
}

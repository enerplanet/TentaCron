package cors

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(t *testing.T, cfg Config, method, origin string, extra map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	req := httptest.NewRequest(method, "/v1/requests", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	New(cfg, next).ServeHTTP(rec, req)
	return rec
}

// The "*" entry allows every origin and answers with the literal "*".
func TestWildcardOrigin(t *testing.T) {
	cfg := Config{AllowedOrigins: []string{"*"}}
	for _, origin := range []string{"https://anything.example", "http://localhost:5173", "null"} {
		rec := serve(t, cfg, http.MethodGet, origin, nil)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("origin %q: Allow-Origin = %q, want *", origin, got)
		}
	}
}

// A subdomain wildcard matches any depth below the host, never the host
// itself and never a look-alike host.
func TestSubdomainWildcard(t *testing.T) {
	cfg := Config{AllowedOrigins: []string{"https://*.example.org"}}
	for origin, want := range map[string]bool{
		"https://a.example.org":      true,
		"https://a.b.example.org":    true,
		"https://A.EXAMPLE.ORG":      true,
		"https://example.org":        false,
		"https://evil-example.org":   false,
		"http://a.example.org":       false,
		"https://a.example.org:8443": false,
		"https://.example.org":       false, // nothing between the scheme and the suffix
	} {
		rec := serve(t, cfg, http.MethodGet, origin, nil)
		if got := rec.Header().Get("Access-Control-Allow-Origin") != ""; got != want {
			t.Errorf("origin %q allowed = %v, want %v", origin, got, want)
		}
	}
}

// The literal null origin is allowed only when listed.
func TestNullOrigin(t *testing.T) {
	rec := serve(t, Config{AllowedOrigins: []string{"https://app.example.org"}}, http.MethodGet, "null", nil)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("null must not be allowed by an exact entry: %v", rec.Header())
	}
	rec = serve(t, Config{AllowedOrigins: []string{"null"}}, http.MethodGet, "null", nil)
	if rec.Header().Get("Access-Control-Allow-Origin") != "null" {
		t.Errorf("listed null origin: %v", rec.Header())
	}
}

// Exact entries match case-insensitively and never a subdomain.
func TestExactOriginMatchesCaseInsensitively(t *testing.T) {
	cfg := Config{AllowedOrigins: []string{"https://App.Example.org"}}
	for origin, want := range map[string]bool{
		"https://app.example.org":   true,
		"HTTPS://APP.EXAMPLE.ORG":   true,
		"https://a.app.example.org": false,
		"https://app.example.org/":  false,
	} {
		rec := serve(t, cfg, http.MethodGet, origin, nil)
		if got := rec.Header().Get("Access-Control-Allow-Origin") != ""; got != want {
			t.Errorf("origin %q allowed = %v, want %v", origin, got, want)
		}
	}
}

// Validate refuses what would silently match nothing or is not an origin.
func TestValidate(t *testing.T) {
	for _, ok := range []string{"*", "null", "NULL", "https://app.example.org", "http://localhost:5173", "https://*.example.org", "https://*.a.b.example.org"} {
		if err := (Config{AllowedOrigins: []string{ok}}).Validate(); err != nil {
			t.Errorf("%q must validate: %v", ok, err)
		}
	}
	for bad, reason := range map[string]string{
		"":                             "empty",
		"app.example.org":              "scheme://host",
		"https://app.example.org/":     "path",
		"https://app.example.org/x":    "path",
		"https://app.example.org?x=1":  "path",
		"ftp://app.example.org":        "http or https",
		"https://user@app.example.org": "credentials",
		"https://app.*.example.org":    "subdomain wildcard",
		"https://*example.org":         "subdomain wildcard",
		"https://*.":                   "subdomain wildcard",
		"*://app.example.org":          "http or https",
		"https://*.a.*.example.org":    "subdomain wildcard",
	} {
		err := (Config{AllowedOrigins: []string{bad}}).Validate()
		if err == nil || !strings.Contains(err.Error(), reason) {
			t.Errorf("%q: err = %v, want one naming %q", bad, err, reason)
		}
	}
}

const preflightVary = "Origin, Access-Control-Request-Method, Access-Control-Request-Headers"

func vary(rec *httptest.ResponseRecorder) string {
	return strings.Join(rec.Header().Values("Vary"), ", ")
}

// A preflight from an allowed origin is answered here, never by the next
// handler, with the allow headers and the full Vary set.
func TestPreflight(t *testing.T) {
	rec := serve(t, Config{AllowedOrigins: []string{"https://app.example.org"}}, http.MethodOptions, "https://app.example.org",
		map[string]string{"Access-Control-Request-Method": "DELETE"})
	if rec.Code != http.StatusNoContent || rec.Header().Get("Allow") != "" {
		t.Fatalf("preflight reached the next handler: %d %v", rec.Code, rec.Header())
	}
	h := rec.Header()
	for name, want := range map[string]string{
		"Access-Control-Allow-Origin":   "https://app.example.org",
		"Access-Control-Allow-Methods":  strings.Join(DefaultAllowedMethods, ", "),
		"Access-Control-Allow-Headers":  strings.Join(DefaultAllowedHeaders, ", "),
		"Access-Control-Max-Age":        "600",
		"Access-Control-Expose-Headers": "",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := vary(rec); got != preflightVary {
		t.Errorf("Vary = %q, want %q", got, preflightVary)
	}
}

// A denied preflight is answered 204 with the Vary set and nothing else:
// no allow header for the page, no Allow list from the mux.
func TestPreflightDeniedOrigin(t *testing.T) {
	rec := serve(t, Config{AllowedOrigins: []string{"https://app.example.org"}}, http.MethodOptions, "https://evil.example.org",
		map[string]string{"Access-Control-Request-Method": "POST"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204", rec.Code)
	}
	for _, name := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Methods", "Access-Control-Allow-Headers", "Access-Control-Max-Age", "Allow"} {
		if got := rec.Header().Get(name); got != "" {
			t.Errorf("%s = %q on a denied preflight", name, got)
		}
	}
	if got := vary(rec); got != preflightVary {
		t.Errorf("Vary = %q, want %q", got, preflightVary)
	}
}

// An OPTIONS without Access-Control-Request-Method is no preflight: it
// reaches the next handler with the origin marked.
func TestNonPreflightOptions(t *testing.T) {
	rec := serve(t, Config{AllowedOrigins: []string{"https://app.example.org"}}, http.MethodOptions, "https://app.example.org", nil)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") == "" {
		t.Errorf("plain OPTIONS: %d %v, want the next handler's 405 with Allow", rec.Code, rec.Header())
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example.org" || vary(rec) != "Origin" {
		t.Errorf("actual-request headers: %v", rec.Header())
	}
}

func TestWildcardsAndAllowsAny(t *testing.T) {
	cfg := Config{AllowedOrigins: []string{"https://app.example.org", "https://*.preview.example.org", "*"}}
	if cfg.Wildcards() != 2 || !cfg.AllowsAny() {
		t.Errorf("Wildcards() = %d, AllowsAny() = %v", cfg.Wildcards(), cfg.AllowsAny())
	}
	if (Config{}).Enabled() || !cfg.Enabled() {
		t.Error("Enabled follows the origin list")
	}
}

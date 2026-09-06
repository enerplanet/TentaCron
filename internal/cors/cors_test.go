package cors

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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

// Credentials: the header on preflight and response, and the specific
// origin echoed even under "*", since the Fetch specification rejects a
// wildcard there.
func TestCredentials(t *testing.T) {
	cfg := Config{AllowedOrigins: []string{"https://app.example.org"}, AllowCredentials: true}
	rec := serve(t, cfg, http.MethodOptions, "https://app.example.org", map[string]string{"Access-Control-Request-Method": "POST"})
	if rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Errorf("preflight lacks Allow-Credentials: %v", rec.Header())
	}
	rec = serve(t, cfg, http.MethodGet, "https://app.example.org", nil)
	if rec.Header().Get("Access-Control-Allow-Credentials") != "true" || rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example.org" {
		t.Errorf("response: %v", rec.Header())
	}
	rec = serve(t, Config{AllowedOrigins: []string{"*"}, AllowCredentials: true}, http.MethodGet, "https://any.example", nil)
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://any.example" {
		t.Errorf("credentialed * must echo the origin, got %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}
	rec = serve(t, Config{AllowedOrigins: []string{"https://app.example.org"}}, http.MethodGet, "https://app.example.org", nil)
	if rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Errorf("credentials off must not emit the header: %v", rec.Header())
	}
}

// Max-age: the default, a custom value in whole seconds, and omitted when
// negative.
func TestMaxAge(t *testing.T) {
	pre := func(cfg Config) string {
		cfg.AllowedOrigins = []string{"https://app.example.org"}
		rec := serve(t, cfg, http.MethodOptions, "https://app.example.org", map[string]string{"Access-Control-Request-Method": "POST"})
		return rec.Header().Get("Access-Control-Max-Age")
	}
	if got := pre(Config{}); got != "600" {
		t.Errorf("default max-age = %q, want 600", got)
	}
	if got := pre(Config{MaxAge: 90 * time.Second}); got != "90" {
		t.Errorf("custom max-age = %q, want 90", got)
	}
	if got := pre(Config{MaxAge: -1}); got != "" {
		t.Errorf("negative max-age must omit the header, got %q", got)
	}
}

// Private Network Access: answered only when asked and enabled.
func TestPrivateNetwork(t *testing.T) {
	ask := map[string]string{"Access-Control-Request-Method": "POST", "Access-Control-Request-Private-Network": "true"}
	rec := serve(t, Config{AllowedOrigins: []string{"https://app.example.org"}, AllowPrivateNetwork: true}, http.MethodOptions, "https://app.example.org", ask)
	if rec.Header().Get("Access-Control-Allow-Private-Network") != "true" {
		t.Errorf("enabled and asked: %v", rec.Header())
	}
	rec = serve(t, Config{AllowedOrigins: []string{"https://app.example.org"}, AllowPrivateNetwork: true}, http.MethodOptions, "https://app.example.org",
		map[string]string{"Access-Control-Request-Method": "POST"})
	if rec.Header().Get("Access-Control-Allow-Private-Network") != "" {
		t.Errorf("not asked: %v", rec.Header())
	}
	rec = serve(t, Config{AllowedOrigins: []string{"https://app.example.org"}}, http.MethodOptions, "https://app.example.org", ask)
	if rec.Header().Get("Access-Control-Allow-Private-Network") != "" {
		t.Errorf("disabled: %v", rec.Header())
	}
}

// Extra headers are added to the built-in sets, canonicalised and
// deduplicated; ["*"] in allowed headers echoes the preflight's request.
func TestExtraHeadersAndEchoMode(t *testing.T) {
	cfg := Config{AllowedOrigins: []string{"https://app.example.org"}, AllowedHeaders: []string{"x-proxy-user", "content-type"}, ExposeHeaders: []string{"x-proxy-trace", "retry-after"}}
	rec := serve(t, cfg, http.MethodOptions, "https://app.example.org", map[string]string{"Access-Control-Request-Method": "POST"})
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != strings.Join(DefaultAllowedHeaders, ", ")+", X-Proxy-User" {
		t.Errorf("Allow-Headers = %q", got)
	}
	rec = serve(t, cfg, http.MethodGet, "https://app.example.org", nil)
	if got := rec.Header().Get("Access-Control-Expose-Headers"); got != strings.Join(DefaultExposeHeaders, ", ")+", X-Proxy-Trace" {
		t.Errorf("Expose-Headers = %q", got)
	}
	echo := Config{AllowedOrigins: []string{"https://app.example.org"}, AllowedHeaders: []string{"*"}}
	rec = serve(t, echo, http.MethodOptions, "https://app.example.org", map[string]string{"Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "x-one, x-two"})
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "x-one, x-two" {
		t.Errorf("echo mode: Allow-Headers = %q", got)
	}
	rec = serve(t, echo, http.MethodOptions, "https://app.example.org", map[string]string{"Access-Control-Request-Method": "GET"})
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "" {
		t.Errorf("echo mode with nothing asked: Allow-Headers = %q", got)
	}
}

// The Fetch specification's forbidden combinations are refused.
func TestValidateCredentialCombinations(t *testing.T) {
	for name, cfg := range map[string]Config{
		"* origin":         {AllowedOrigins: []string{"*"}, AllowCredentials: true},
		"* expose header":  {AllowedOrigins: []string{"https://app.example.org"}, ExposeHeaders: []string{"*"}, AllowCredentials: true},
		"* allowed method": {AllowedOrigins: []string{"https://app.example.org"}, AllowedMethods: []string{"*"}, AllowCredentials: true},
	} {
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "allow_credentials") {
			t.Errorf("%s: err = %v, want a credentials error", name, err)
		}
	}
	ok := Config{AllowedOrigins: []string{"https://app.example.org"}, AllowedHeaders: []string{"*"}, ExposeHeaders: []string{"X-Trace"}, AllowCredentials: true}
	if err := ok.Validate(); err != nil {
		t.Errorf("echo-mode headers with credentials are fine: %v", err)
	}
}

// Update swaps the policy under concurrent requests: disabled to enabled,
// one origin to another, and back to a pass-through, with every response
// answered by one whole policy.
func TestUpdateSwapsThePolicy(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusMethodNotAllowed) })
	h := New(Config{}, next)
	preflight := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodOptions, "/v1/requests", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := preflight("https://a.example"); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Vary") != "" {
		t.Fatalf("disabled handler must pass through untouched: %d %v", rec.Code, rec.Header())
	}
	h.Update(Config{AllowedOrigins: []string{"https://a.example"}})
	if rec := preflight("https://a.example"); rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "https://a.example" {
		t.Fatalf("enabled by Update: %d %v", rec.Code, rec.Header())
	}
	if !h.Enabled() || !h.Allows("https://a.example") || h.Allows("https://b.example") {
		t.Errorf("Enabled/Allows do not reflect the policy")
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rec := preflight("https://b.example")
				if rec.Code != http.StatusNoContent && rec.Code != http.StatusMethodNotAllowed {
					t.Errorf("status %d during a swap", rec.Code)
					return
				}
				if allow := rec.Header().Get("Access-Control-Allow-Origin"); allow != "" && rec.Code != http.StatusNoContent {
					t.Errorf("an allow header on a %d: the response mixed two policies", rec.Code)
					return
				}
			}
		}()
	}
	for i := range 50 {
		if i%2 == 0 {
			h.Update(Config{AllowedOrigins: []string{"https://b.example"}})
		} else {
			h.Update(Config{})
		}
	}
	close(stop)
	wg.Wait()
	h.Update(Config{AllowedOrigins: []string{"https://b.example"}})
	if rec := preflight("https://b.example"); rec.Header().Get("Access-Control-Allow-Origin") != "https://b.example" {
		t.Errorf("the last policy wins: %v", rec.Header())
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

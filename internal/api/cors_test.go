package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/enerplanet/tentacron/internal/config"
)

// The CORS matrix mirrors the sibling service meme's cors_test.go case for
// case, so a reviewer of both repositories reads one list. Each test records
// what the middleware does today; a case that a 0.4.0-alpha item changes says
// so in a comment and is flipped in that item's commit.

const (
	allowedOrigin = "https://app.example.org"
	foreignOrigin = "https://evil.example.org"
)

// corsEnv is a test server that allows origins.
func corsEnv(t *testing.T, origins ...string) *testEnv {
	t.Helper()
	return newEnvWith(t, func(c *config.Config) { c.Server.CORS.AllowedOrigins = origins }, nil)
}

func withOrigin(origin string, extra map[string]string) map[string]string {
	h := map[string]string{"Origin": origin}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func assertHeader(t *testing.T, h http.Header, name, want string) {
	t.Helper()
	if got := h.Get(name); got != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

// With no origins configured the middleware is absent: no CORS header, no
// Vary, and a preflight keeps the mux's 405 for OPTIONS.
func TestCORSDisabledByDefault(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, http.MethodGet, "/healthz", "", withOrigin(allowedOrigin, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	assertHeader(t, rec.Header(), "Access-Control-Allow-Origin", "")
	assertHeader(t, rec.Header(), "Vary", "")
	rec = e.do(t, http.MethodOptions, "/v1/requests", "", withOrigin(allowedOrigin, map[string]string{"Access-Control-Request-Method": "POST"}))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") == "" {
		t.Errorf("preflight without CORS: %d %v, want the mux's 405 with Allow", rec.Code, rec.Header())
	}
}

// A preflight from an allowed origin is answered by the middleware, before
// the mux and before authentication, with every method the API has.
func TestCORSPreflight(t *testing.T) {
	e := corsEnv(t, allowedOrigin)
	rec := e.do(t, http.MethodOptions, "/v1/requests", "", withOrigin(allowedOrigin, map[string]string{"Access-Control-Request-Method": "POST"}))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204", rec.Code)
	}
	h := rec.Header()
	assertHeader(t, h, "Access-Control-Allow-Origin", allowedOrigin)
	assertHeader(t, h, "Access-Control-Allow-Methods", "GET, HEAD, POST, DELETE, OPTIONS")
	assertHeader(t, h, "Access-Control-Allow-Headers", "Content-Type, X-API-Key, Idempotency-Key, X-Request-ID, Range")
	assertHeader(t, h, "Access-Control-Max-Age", "600")
	// Item 1.2 adds Access-Control-Request-Method and -Headers to Vary.
	assertHeader(t, h, "Vary", "Origin")
	// A browser cancelling a request preflights DELETE on the request URL;
	// the answer must allow it, or the frontend can never cancel.
	rec = e.do(t, http.MethodOptions, "/v1/requests/0123456789abcdef0123456789abcdef", "", withOrigin(allowedOrigin, map[string]string{"Access-Control-Request-Method": "DELETE"}))
	if rec.Code != http.StatusNoContent || !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "DELETE") {
		t.Errorf("cancel preflight: %d %v", rec.Code, rec.Header())
	}
}

// A preflight from an origin that is not allowed falls through to the mux
// today, which answers 405 with its Allow list. Item 1.2 changes this to a
// 204 that carries the Vary set and nothing else.
func TestCORSPreflightDeniedOrigin(t *testing.T) {
	e := corsEnv(t, allowedOrigin)
	rec := e.do(t, http.MethodOptions, "/v1/requests", "", withOrigin(foreignOrigin, map[string]string{"Access-Control-Request-Method": "POST"}))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405 (today's behaviour)", rec.Code)
	}
	assertHeader(t, rec.Header(), "Access-Control-Allow-Origin", "")
	assertHeader(t, rec.Header(), "Access-Control-Allow-Methods", "")
	assertHeader(t, rec.Header(), "Vary", "Origin")
	if rec.Header().Get("Allow") == "" {
		t.Errorf("the mux's Allow list is handed to the refused origin today: %v", rec.Header())
	}
}

// The actual request from an allowed origin carries the allow-origin and
// the exposed headers on every outcome; a foreign origin gets Vary only.
func TestCORSActualRequest(t *testing.T) {
	e := corsEnv(t, allowedOrigin)
	auth := map[string]string{"X-API-Key": "valid-key"}
	rec := e.do(t, http.MethodGet, "/v1/requests", "", withOrigin(allowedOrigin, auth))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	assertHeader(t, rec.Header(), "Access-Control-Allow-Origin", allowedOrigin)
	assertHeader(t, rec.Header(), "Vary", "Origin")
	if rec.Header().Get("Access-Control-Expose-Headers") == "" {
		t.Errorf("exposed headers missing: %v", rec.Header())
	}
	rec = e.do(t, http.MethodGet, "/v1/requests", "", withOrigin(foreignOrigin, auth))
	if rec.Code != http.StatusOK {
		t.Fatalf("foreign origin status %d", rec.Code)
	}
	assertHeader(t, rec.Header(), "Access-Control-Allow-Origin", "")
	assertHeader(t, rec.Header(), "Access-Control-Expose-Headers", "")
	assertHeader(t, rec.Header(), "Vary", "Origin")
}

// Every response varies on Origin once CORS is on, even without an Origin
// header, so a shared cache never serves one origin's answer to another.
func TestCORSVaryWithoutOrigin(t *testing.T) {
	e := corsEnv(t, allowedOrigin)
	rec := e.do(t, http.MethodGet, "/healthz", "", nil)
	assertHeader(t, rec.Header(), "Vary", "Origin")
	assertHeader(t, rec.Header(), "Access-Control-Allow-Origin", "")
}

// An exact entry matches case-insensitively and never a subdomain, a
// different scheme or a trailing slash.
func TestCORSExactOrigin(t *testing.T) {
	e := corsEnv(t, allowedOrigin)
	rec := e.do(t, http.MethodGet, "/healthz", "", withOrigin("HTTPS://APP.EXAMPLE.ORG", nil))
	assertHeader(t, rec.Header(), "Access-Control-Allow-Origin", "HTTPS://APP.EXAMPLE.ORG")
	for _, origin := range []string{"https://a.app.example.org", "https://app.example.org/", "http://app.example.org"} {
		rec := e.do(t, http.MethodGet, "/healthz", "", withOrigin(origin, nil))
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("origin %q allowed as %q; only the exact entry matches", origin, got)
		}
	}
}

// The "*" entry admits every origin and answers with the literal "*".
func TestCORSWildcardOrigin(t *testing.T) {
	e := corsEnv(t, "*")
	rec := e.do(t, http.MethodGet, "/healthz", "", withOrigin(foreignOrigin, nil))
	assertHeader(t, rec.Header(), "Access-Control-Allow-Origin", "*")
	rec = e.do(t, http.MethodOptions, "/v1/requests", "", withOrigin(foreignOrigin, map[string]string{"Access-Control-Request-Method": "POST"}))
	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight under *: %d", rec.Code)
	}
	assertHeader(t, rec.Header(), "Access-Control-Allow-Origin", "*")
}

// A subdomain wildcard admits any depth below the host, never the host
// itself and never a look-alike.
func TestCORSSubdomainWildcard(t *testing.T) {
	e := corsEnv(t, "https://*.preview.example.org")
	for origin, want := range map[string]string{
		"https://pr-12.preview.example.org":   "https://pr-12.preview.example.org",
		"https://a.b.preview.example.org":     "https://a.b.preview.example.org",
		"https://preview.example.org":         "",
		"https://evil-preview.example.org":    "",
		"http://pr-12.preview.example.org":    "",
		"https://preview.example.org.evil.io": "",
	} {
		rec := e.do(t, http.MethodGet, "/healthz", "", withOrigin(origin, nil))
		assertHeader(t, rec.Header(), "Access-Control-Allow-Origin", want)
	}
}

// The literal null origin, which sandboxed iframes and file:// pages send,
// is admitted only when listed.
func TestCORSNullOrigin(t *testing.T) {
	rec := corsEnv(t, allowedOrigin).do(t, http.MethodGet, "/healthz", "", withOrigin("null", nil))
	assertHeader(t, rec.Header(), "Access-Control-Allow-Origin", "")
	rec = corsEnv(t, "null").do(t, http.MethodGet, "/healthz", "", withOrigin("null", nil))
	assertHeader(t, rec.Header(), "Access-Control-Allow-Origin", "null")
}

// Error envelopes carry the CORS headers too: the middleware wraps the
// fallback and the mux, so a page can read a 401, a 413 and a 429.
func TestCORSHeadersOnAuthAndSizeErrors(t *testing.T) {
	e := newEnvWith(t, func(c *config.Config) {
		c.Server.CORS.AllowedOrigins = []string{allowedOrigin}
		c.Server.MaxBodyBytes = 48
		c.Auth.APIKeys[0].MaxQueued = 1
	}, nil)
	small := `{"target":"meme","payload":{}}`
	rec := e.do(t, http.MethodPost, "/v1/requests", small, withOrigin(allowedOrigin, map[string]string{"X-API-Key": "wrong"}))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("401 case: %d %s", rec.Code, rec.Body.String())
	}
	assertHeader(t, rec.Header(), "Access-Control-Allow-Origin", allowedOrigin)
	rec = e.do(t, http.MethodPost, "/v1/requests", validBody, withOrigin(allowedOrigin, nil))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("413 case: %d %s", rec.Code, rec.Body.String())
	}
	assertHeader(t, rec.Header(), "Access-Control-Allow-Origin", allowedOrigin)
	auth := map[string]string{"X-API-Key": "valid-key"}
	if rec := e.do(t, http.MethodPost, "/v1/requests", small, withOrigin(allowedOrigin, auth)); rec.Code != http.StatusAccepted {
		t.Fatalf("first submission: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, http.MethodPost, "/v1/requests", small, withOrigin(allowedOrigin, auth))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("429 case: %d %s", rec.Code, rec.Body.String())
	}
	assertHeader(t, rec.Header(), "Access-Control-Allow-Origin", allowedOrigin)
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("the 429 must carry Retry-After for the page to read: %v", rec.Header())
	}
}

// The exposed set today. Retry-After is missing from it, so a page cannot
// read when to retry a 429; item 1.3 adds it.
func TestCORSExposeHeaders(t *testing.T) {
	e := corsEnv(t, allowedOrigin)
	rec := e.do(t, http.MethodGet, "/healthz", "", withOrigin(allowedOrigin, nil))
	assertHeader(t, rec.Header(), "Access-Control-Expose-Headers", "X-Request-ID, Allow, Content-Disposition, Content-Length, Content-Range, Accept-Ranges")
}

// Preflights carry no key and are answered before authentication.
func TestCORSPreflightWithoutAPIKey(t *testing.T) {
	e := corsEnv(t, allowedOrigin)
	rec := e.do(t, http.MethodOptions, "/v1/requests", "", withOrigin(allowedOrigin, map[string]string{"Access-Control-Request-Method": "POST"}))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight without a key: %d %s, want 204", rec.Code, rec.Body.String())
	}
}

// An OPTIONS without Access-Control-Request-Method is not a preflight: it
// reaches the mux and keeps its 405 with the Allow list.
func TestCORSNonPreflightOptions(t *testing.T) {
	e := corsEnv(t, allowedOrigin)
	rec := e.do(t, http.MethodOptions, "/v1/requests", "", withOrigin(allowedOrigin, nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") == "" {
		t.Errorf("plain OPTIONS: %d %v, want 405 with Allow", rec.Code, rec.Header())
	}
	assertHeader(t, rec.Header(), "Access-Control-Allow-Origin", allowedOrigin)
}

// The preflight max-age is fixed at ten minutes today; item 1.4 makes it
// configurable.
func TestCORSMaxAge(t *testing.T) {
	e := corsEnv(t, allowedOrigin)
	rec := e.do(t, http.MethodOptions, "/v1/requests", "", withOrigin(allowedOrigin, map[string]string{"Access-Control-Request-Method": "POST"}))
	assertHeader(t, rec.Header(), "Access-Control-Max-Age", "600")
}

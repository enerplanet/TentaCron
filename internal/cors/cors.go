// Package cors answers browsers: it decides which origins may read the
// API's responses and answers their preflights. The shape follows the
// sibling service meme's internal/api/cors.go so a reviewer of both reads
// one design; the header sets are TentaCron's own, because they name what
// this API's handlers set and read.
//
// CORS is browser policy, not access control: a client that is not a
// browser ignores it, and the API key remains the authentication.
package cors

import (
	"fmt"
	"net/http"
	"strings"
)

// Defaults applied when the corresponding Config list is empty. The methods
// are every method this API serves, cancellation and schedule deletion
// included. The allowed request headers are every header the handlers
// read, plus Authorization for an authenticating proxy in front of the
// API; the exposed response headers are every header the handlers set
// that the Fetch specification does not safelist, so browser JS can read
// a result's filename and ranges, the request id, the Allow list of a 405
// and the Retry-After of a 429. A test in internal/api scans the handlers
// and fails when a header is added to them but not here.
var (
	DefaultAllowedMethods = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodDelete, http.MethodOptions}
	DefaultAllowedHeaders = []string{"Content-Type", "X-API-Key", "Idempotency-Key", "X-Request-ID", "Range", "Authorization"}
	DefaultExposeHeaders  = []string{"X-Request-ID", "Allow", "Retry-After", "Content-Disposition", "Content-Length", "Content-Range", "Accept-Ranges"}
)

// defaultMaxAge caps preflight caching. Ten minutes is Chromium's upper
// bound, so a larger value buys nothing.
const defaultMaxAge = 600

// Config is the browser policy. The zero value disables CORS entirely: no
// CORS header is emitted and OPTIONS keeps its 405 from the method-scoped
// mux patterns. CORS is on when AllowedOrigins is non-empty.
type Config struct {
	// AllowedOrigins lists origins allowed to read responses: exact origins
	// ("https://app.example.org"), the wildcard "*" (any origin), or a
	// subdomain wildcard ("https://*.example.org", matching any subdomain
	// depth — https://a.example.org, https://a.b.example.org — but neither
	// https://example.org itself nor https://evil-example.org). Matching is
	// case-insensitive. The literal origin "null" (sandboxed iframes,
	// file:// pages) is only allowed when listed explicitly or via "*".
	AllowedOrigins []string
}

// Enabled reports whether the policy answers browsers at all.
func (c Config) Enabled() bool { return len(c.AllowedOrigins) > 0 }

// Validate rejects entries that are malformed or would silently match
// nothing, such as a trailing slash. It returns nil on the zero value.
func (c Config) Validate() error {
	for _, o := range c.AllowedOrigins {
		if err := ValidateOrigin(o); err != nil {
			return err
		}
	}
	return nil
}

// ValidateOrigin accepts "*", "null", an exact scheme://host[:port] or a
// subdomain wildcard scheme://*.host, and nothing else.
func ValidateOrigin(o string) error {
	if o == "" {
		return fmt.Errorf("cors: allowed_origins contains an empty entry")
	}
	if o == "*" || strings.EqualFold(o, "null") {
		return nil
	}
	scheme, host, ok := strings.Cut(o, "://")
	if !ok || scheme == "" || host == "" {
		return fmt.Errorf("cors: origin %q must be \"*\", \"null\", or scheme://host[:port]", o)
	}
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("cors: origin %q must use http or https", o)
	}
	if strings.ContainsAny(host, "/?#") {
		return fmt.Errorf("cors: origin %q must not contain a path (origins are scheme://host[:port], no trailing slash)", o)
	}
	if strings.Contains(host, "@") {
		return fmt.Errorf("cors: origin %q must not carry credentials", o)
	}
	if strings.Contains(host, "*") && (!strings.HasPrefix(host, "*.") || len(host) < len("*.x") || strings.Contains(host[1:], "*")) {
		return fmt.Errorf("cors: origin %q: \"*\" is only valid on its own or as a subdomain wildcard like https://*.example.org", o)
	}
	return nil
}

// Wildcards reports how many entries are patterns: the "*" origin and the
// subdomain wildcards. Start-up and validation print it, since a pattern
// admits pages the operator did not name.
func (c Config) Wildcards() int {
	n := 0
	for _, o := range c.AllowedOrigins {
		if o == "*" || strings.Contains(o, "://*.") {
			n++
		}
	}
	return n
}

// AllowsAny reports whether the "*" origin is listed.
func (c Config) AllowsAny() bool {
	for _, o := range c.AllowedOrigins {
		if o == "*" {
			return true
		}
	}
	return false
}

// wildcard matches origins of the form prefix + <at least one character> +
// suffix; https://*.example.org becomes {"https://", ".example.org"}.
type wildcard struct{ prefix, suffix string }

func (w wildcard) match(origin string) bool {
	return len(origin) > len(w.prefix)+len(w.suffix) &&
		strings.HasPrefix(origin, w.prefix) && strings.HasSuffix(origin, w.suffix)
}

// policy is a Config normalised into ready-to-emit header values at
// construction, so the per-request work is a map lookup plus header writes.
type policy struct {
	allowAll  bool            // "*" listed
	exact     map[string]bool // lowercased exact origins (may include "null")
	wildcards []wildcard      // lowercased subdomain patterns

	methods string // Access-Control-Allow-Methods value
	headers string // Access-Control-Allow-Headers value
	expose  string // Access-Control-Expose-Headers value
	maxAge  string // Access-Control-Max-Age in seconds
}

func newPolicy(cfg Config) *policy {
	p := &policy{exact: map[string]bool{}}
	for _, o := range cfg.AllowedOrigins {
		o = strings.ToLower(o)
		if o == "*" {
			p.allowAll = true
		} else if i := strings.Index(o, "://*."); i >= 0 {
			p.wildcards = append(p.wildcards, wildcard{prefix: o[:i+len("://")], suffix: o[i+len("://*"):]})
		} else {
			p.exact[o] = true
		}
	}
	p.methods = strings.Join(DefaultAllowedMethods, ", ")
	p.headers = strings.Join(DefaultAllowedHeaders, ", ")
	p.expose = strings.Join(DefaultExposeHeaders, ", ")
	p.maxAge = fmt.Sprint(defaultMaxAge)
	return p
}

func (p *policy) originAllowed(origin string) bool {
	if p.allowAll {
		return true
	}
	o := strings.ToLower(origin)
	if p.exact[o] {
		return true
	}
	for _, w := range p.wildcards {
		if w.match(o) {
			return true
		}
	}
	return false
}

// allowOriginValue is the Access-Control-Allow-Origin value for an allowed
// origin: the literal "*" when every origin is allowed, otherwise the
// request origin echoed back.
func (p *policy) allowOriginValue(origin string) string {
	if p.allowAll {
		return "*"
	}
	return origin
}

// Handler wraps the API with the browser policy.
type Handler struct {
	policy *policy
	next   http.Handler
}

// New builds the handler; call it only when cfg.Enabled(), since a
// disabled policy must not even add Vary.
func New(cfg Config, next http.Handler) *Handler {
	return &Handler{policy: newPolicy(cfg), next: next}
}

// ServeHTTP answers every preflight itself and marks the actual request's
// response for its origin. Every response varies on Origin, so a shared
// cache never serves one origin's headers to another.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := h.policy
	origin := r.Header.Get("Origin")
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		h.preflight(w, p, origin)
		return
	}

	// Actual request (a non-preflight OPTIONS falls through to the mux and
	// keeps its 405). Headers are set before the handler runs so that every
	// outcome carries them: error envelopes, 413s, the streamed result.
	hdr := w.Header()
	hdr.Add("Vary", "Origin")
	if origin != "" && p.originAllowed(origin) {
		hdr.Set("Access-Control-Allow-Origin", p.allowOriginValue(origin))
		hdr.Set("Access-Control-Expose-Headers", p.expose)
	}
	h.next.ServeHTTP(w, r)
}

// preflight answers without consulting the mux (whose method-scoped
// patterns would 405 it) or authentication: preflights carry no key, the
// browser sends the credentialless OPTIONS on its own. A denied origin
// gets the Vary headers and nothing else; the browser then blocks the
// actual request. The method and header lists are emitted as-is: the
// Fetch specification has the browser compare the request against them
// and fail the fetch itself.
func (h *Handler) preflight(w http.ResponseWriter, p *policy, origin string) {
	hdr := w.Header()
	hdr.Add("Vary", "Origin")
	hdr.Add("Vary", "Access-Control-Request-Method")
	hdr.Add("Vary", "Access-Control-Request-Headers")
	if origin != "" && p.originAllowed(origin) {
		hdr.Set("Access-Control-Allow-Origin", p.allowOriginValue(origin))
		hdr.Set("Access-Control-Allow-Methods", p.methods)
		hdr.Set("Access-Control-Allow-Headers", p.headers)
		hdr.Set("Access-Control-Max-Age", p.maxAge)
	}
	w.WriteHeader(http.StatusNoContent)
}

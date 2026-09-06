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
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
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

// DefaultMaxAge caps preflight caching when Config.MaxAge is zero. Ten
// minutes is Chromium's upper bound, so a larger default buys nothing.
const DefaultMaxAge = 10 * time.Minute

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

	// AllowedMethods a cross-origin request may use. Empty means every
	// method this API serves (DefaultAllowedMethods). A literal "*" is
	// emitted as-is and only acts as a wildcard on credentialless
	// requests; Validate rejects it in combination with AllowCredentials.
	AllowedMethods []string

	// AllowedHeaders a preflight may request, in addition to the ones the
	// handlers read (DefaultAllowedHeaders): what a proxy in front adds.
	// The single entry "*" echoes whatever the preflight asks for — a
	// literal "*" would be read as a header named "*" on credentialed
	// requests.
	AllowedHeaders []string

	// ExposeHeaders lists response headers browser JS may read in addition
	// to the ones the handlers set (DefaultExposeHeaders): what a proxy in
	// front adds. Like AllowedMethods, a literal "*" is only a wildcard
	// without credentials; Validate rejects the credentialed combination.
	ExposeHeaders []string

	// AllowCredentials permits cookies and TLS client certificates on
	// cross-origin requests. Forbidden together with the "*" origin:
	// Validate rejects the combination, and the handler echoes the
	// specific origin rather than "*" regardless.
	AllowCredentials bool

	// MaxAge bounds how long browsers may cache a preflight answer. The
	// zero value means DefaultMaxAge; a negative value omits the header
	// entirely (browsers then fall back to their 5-second default).
	// Emitted as whole seconds.
	MaxAge time.Duration

	// AllowPrivateNetwork answers Chrome's Private Network Access
	// preflights (Access-Control-Request-Private-Network) affirmatively —
	// needed when a public page calls an API on a LAN or localhost. Off
	// by default.
	AllowPrivateNetwork bool
}

// Enabled reports whether the policy answers browsers at all.
func (c Config) Enabled() bool { return len(c.AllowedOrigins) > 0 }

// Validate rejects configurations that are forbidden by the Fetch
// specification or — like a trailing slash on an origin — silently match
// nothing. It returns nil on the zero value. The configuration calls it
// at start; the handler itself accepts whatever it is given.
func (c Config) Validate() error {
	if c.AllowCredentials {
		// On credentialed requests browsers read "*" in these headers as a
		// literal token, not a wildcard — the config would silently not do
		// what it says. (AllowedHeaders "*" is exempt: it is echo mode.)
		if slices.Contains(c.AllowedMethods, "*") {
			return fmt.Errorf("cors: allowed_methods \"*\" cannot be combined with allow_credentials (browsers treat it as a literal method name on credentialed requests)")
		}
		if slices.Contains(c.ExposeHeaders, "*") {
			return fmt.Errorf("cors: expose_headers \"*\" cannot be combined with allow_credentials (browsers treat it as a literal header name on credentialed requests)")
		}
		if c.AllowsAny() {
			return fmt.Errorf("cors: the \"*\" origin cannot be combined with allow_credentials (the Fetch specification forbids wildcard origins on credentialed requests)")
		}
	}
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
	enabled   bool            // any origin listed; false makes the handler a pass-through
	allowAll  bool            // "*" listed
	exact     map[string]bool // lowercased exact origins (may include "null")
	wildcards []wildcard      // lowercased subdomain patterns

	credentials    bool   // Access-Control-Allow-Credentials: true
	privateNetwork bool   // answer Access-Control-Request-Private-Network
	methods        string // Access-Control-Allow-Methods value
	headers        string // Access-Control-Allow-Headers value ("" with headersAny)
	headersAny     bool   // AllowedHeaders is "*": echo the requested headers
	expose         string // Access-Control-Expose-Headers value
	maxAge         string // Access-Control-Max-Age in seconds; "" omits the header
}

func newPolicy(cfg Config) *policy {
	p := &policy{enabled: cfg.Enabled(), exact: map[string]bool{}, credentials: cfg.AllowCredentials, privateNetwork: cfg.AllowPrivateNetwork}
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
	methods := cfg.AllowedMethods
	if len(methods) == 0 {
		methods = DefaultAllowedMethods
	}
	p.methods = joinHeaderList(nil, methods, strings.ToUpper)
	if len(cfg.AllowedHeaders) == 1 && cfg.AllowedHeaders[0] == "*" {
		p.headersAny = true
	} else {
		p.headers = joinHeaderList(DefaultAllowedHeaders, cfg.AllowedHeaders, http.CanonicalHeaderKey)
	}
	p.expose = joinHeaderList(DefaultExposeHeaders, cfg.ExposeHeaders, http.CanonicalHeaderKey)
	switch {
	case cfg.MaxAge < 0: // omit
	case cfg.MaxAge == 0:
		p.maxAge = strconv.Itoa(int(DefaultMaxAge / time.Second))
	default:
		p.maxAge = strconv.Itoa(int(cfg.MaxAge / time.Second))
	}
	return p
}

// joinHeaderList emits base as spelled (the documented names), followed by
// the canonicalised extras it does not already hold, as one header value.
func joinHeaderList(base, extra []string, canon func(string) string) string {
	out := slices.Clone(base)
	for _, v := range extra {
		v = canon(v)
		if !slices.ContainsFunc(out, func(have string) bool { return strings.EqualFold(have, v) }) {
			out = append(out, v)
		}
	}
	return strings.Join(out, ", ")
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
// request origin echoed back. Credentialed responses always echo the
// specific origin — the Fetch specification rejects "*" there — even if
// the caller skipped Validate.
func (p *policy) allowOriginValue(origin string) string {
	if p.allowAll && !p.credentials {
		return "*"
	}
	return origin
}

// Handler wraps the API with the browser policy. The policy can be swapped
// while requests are in flight: each request reads it once, so a request
// finishes with the policy it started with.
type Handler struct {
	policy atomic.Pointer[policy]
	next   http.Handler
}

// New builds the handler. A disabled cfg makes it a pass-through that adds
// nothing, not even Vary, until Update enables it.
func New(cfg Config, next http.Handler) *Handler {
	h := &Handler{next: next}
	h.Update(cfg)
	return h
}

// Update swaps the policy for cfg, enabling or disabling the handler as
// the configuration says; a reload applies it without a restart.
func (h *Handler) Update(cfg Config) { h.policy.Store(newPolicy(cfg)) }

// Enabled reports whether the current policy answers browsers at all.
func (h *Handler) Enabled() bool { return h.policy.Load().enabled }

// Allows reports whether the current policy admits origin.
func (h *Handler) Allows(origin string) bool {
	p := h.policy.Load()
	return p.enabled && p.originAllowed(origin)
}

// ServeHTTP answers every preflight itself and marks the actual request's
// response for its origin. Every response varies on Origin, so a shared
// cache never serves one origin's headers to another.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := h.policy.Load()
	if !p.enabled {
		h.next.ServeHTTP(w, r)
		return
	}
	origin := r.Header.Get("Origin")
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		h.preflight(w, r, p, origin)
		return
	}

	// Actual request (a non-preflight OPTIONS falls through to the mux and
	// keeps its 405). Headers are set before the handler runs so that every
	// outcome carries them: error envelopes, 413s, the streamed result.
	hdr := w.Header()
	hdr.Add("Vary", "Origin")
	if origin != "" && p.originAllowed(origin) {
		hdr.Set("Access-Control-Allow-Origin", p.allowOriginValue(origin))
		if p.credentials {
			hdr.Set("Access-Control-Allow-Credentials", "true")
		}
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
func (h *Handler) preflight(w http.ResponseWriter, r *http.Request, p *policy, origin string) {
	hdr := w.Header()
	hdr.Add("Vary", "Origin")
	hdr.Add("Vary", "Access-Control-Request-Method")
	hdr.Add("Vary", "Access-Control-Request-Headers")
	if origin != "" && p.originAllowed(origin) {
		hdr.Set("Access-Control-Allow-Origin", p.allowOriginValue(origin))
		hdr.Set("Access-Control-Allow-Methods", p.methods)
		if p.headersAny {
			if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
				hdr.Set("Access-Control-Allow-Headers", req)
			}
		} else {
			hdr.Set("Access-Control-Allow-Headers", p.headers)
		}
		if p.credentials {
			hdr.Set("Access-Control-Allow-Credentials", "true")
		}
		if p.maxAge != "" {
			hdr.Set("Access-Control-Max-Age", p.maxAge)
		}
		if p.privateNetwork && r.Header.Get("Access-Control-Request-Private-Network") == "true" {
			hdr.Set("Access-Control-Allow-Private-Network", "true")
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/enerplanet/tentacron/internal/cors"
)

// Describe renders a loaded configuration as the operator-facing summary
// `tentacron validate` prints: every target and resolvent with the knobs
// that matter for routing, and the worker settings. Credentials never appear.
func Describe(c *Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "configuration ok: %d client key(s), %d target(s), %d resolvent type(s)\n",
		len(c.Auth.APIKeys), len(c.Targets), len(c.Resolvents))
	fmt.Fprintf(&b, "server  %s  request body limit %d bytes\n", c.Server.Addr, c.Server.MaxBodyBytes)
	fmt.Fprintf(&b, "limits  upstream response %d bytes, result download %d bytes\n", c.Upstream.MaxResponseBytes, c.Storage.MaxResultBytes)
	fmt.Fprintf(&b, "worker  %d worker(s), %d resolvent fetch(es) per job, %d attempt(s), job timeout %s\n",
		c.Worker.Count, c.Worker.ResolventConcurrency, c.Worker.MaxAttempts, c.Worker.JobTimeout.Std())
	fmt.Fprintf(&b, "storage %s  results %s  retention %s\n", c.Storage.Path, c.Storage.ResultsDir, c.Storage.Retention.Std())
	b.WriteString("cors    " + DescribeCORS(c.Server.CORS) + "\n")
	b.WriteString("targets\n")
	for _, name := range slices.Sorted(maps.Keys(c.Targets)) {
		fmt.Fprintf(&b, "  %-18s %s\n", name, describeTarget(c, name, c.Targets[name]))
	}
	b.WriteString("resolvents\n")
	for _, name := range slices.Sorted(maps.Keys(c.Resolvents)) {
		fmt.Fprintf(&b, "  %-24s %s\n", name, describeResolvent(c.Resolvents[name]))
	}
	return b.String()
}

// DescribeCORS is the browser policy in one line, for the validate summary
// and the start-up log, with a warning when the "*" origin is set: a key in
// a page on any origin is a key on every origin.
func DescribeCORS(c CORS) string {
	p := c.Policy()
	if !p.Enabled() {
		return "off (no browser origin allowed)"
	}
	onOff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	maxAge := cors.DefaultMaxAge.String()
	switch {
	case p.MaxAge < 0:
		maxAge = "omitted"
	case p.MaxAge > 0:
		maxAge = p.MaxAge.String()
	}
	s := fmt.Sprintf("%d origin(s), %d wildcard(s), credentials %s, private network %s, preflight max-age %s",
		len(c.AllowedOrigins), p.Wildcards(), onOff(p.AllowCredentials), onOff(p.AllowPrivateNetwork), maxAge)
	if p.AllowsAny() {
		s += `; WARNING: "*" lets any page that holds a key use it`
	}
	return s
}

func describeTarget(c *Config, name string, t Target) string {
	parts := []string{t.Method + " " + t.URL, t.Response.Mode}
	if t.Proxy {
		parts = append(parts, "proxy")
	} else {
		parts = append(parts, "resolvents in "+t.TimeseriesPath)
	}
	parts = append(parts, fmt.Sprintf("timeout %s", t.Timeout.Std()),
		fmt.Sprintf("job timeout %s", c.JobTimeoutFor(name)),
		fmt.Sprintf("attempts %d", c.MaxAttemptsFor(name)))
	if !t.RetriesOnTimeout() {
		parts = append(parts, "not retried on timeout")
	}
	if t.APIKeyInject != InjectNone {
		parts = append(parts, "key via "+t.APIKeyInject)
	}
	return strings.Join(parts, ", ")
}

func describeResolvent(r Resolvent) string {
	parts := []string{}
	if r.Target != "" {
		parts = append(parts, "via target "+r.Target)
	} else {
		parts = append(parts, r.Method+" "+r.URL, fmt.Sprintf("timeout %s", r.Timeout.Std()))
	}
	if r.PayloadField != "" {
		parts = append(parts, "payload field "+r.PayloadField)
	}
	if r.ResponsePath != "" {
		parts = append(parts, "response path "+r.ResponsePath)
	}
	parts = append(parts, fmt.Sprintf("cache %s", r.CacheTTL.Std()))
	if r.APIKey != "" {
		parts = append(parts, "key via header "+r.APIKeyHeader)
	}
	return strings.Join(parts, ", ")
}

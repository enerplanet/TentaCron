package config

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"strings"

	"github.com/enerplanet/tentacron/internal/resolver"
)

// ResolventTypePrefix is the required prefix of resolvent type identifiers.
// It aliases resolver.TypePrefix so config validation and payload resolution
// can never disagree on what marks a resolvent object.
const ResolventTypePrefix = resolver.TypePrefix

// Validate checks the configuration for consistency. All problems are
// reported at once so the operator can fix a config file in one pass.
func (c *Config) Validate() error {
	v := &validator{}
	v.server(c.Server)
	v.storage(c.Storage)
	v.worker(c.Worker)
	v.cache(c.Cache)
	v.upstream(c.Upstream)
	v.auth(c.Auth)
	v.targets(c.Targets, c.Resolvents, c.Worker)
	v.resolvents(c.Resolvents, c.Targets)
	return errors.Join(v.errs...)
}

// validator accumulates every problem found in a configuration.
type validator struct {
	errs []error
}

func (v *validator) fail(format string, args ...any) {
	v.errs = append(v.errs, fmt.Errorf(format, args...))
}

// positiveDur rejects zero and negative durations. Defaults only replace
// zero, so a negative value written by the operator would otherwise survive
// into the runtime (a negative retention prunes everything at once, a
// negative timeout expires every call immediately).
func (v *validator) positiveDur(field string, d Duration) {
	if d <= 0 {
		v.fail("%s: must be a positive duration (got %s)", field, d.Std())
	}
}

// positiveInt rejects zero and negative counts; a negative worker count
// would silently start no workers at all.
func (v *validator) positiveInt(field string, n int64) {
	if n <= 0 {
		v.fail("%s: must be a positive integer (got %d)", field, n)
	}
}

func (v *validator) server(s Server) {
	v.positiveDur("server.read_timeout", s.ReadTimeout)
	v.positiveDur("server.write_timeout", s.WriteTimeout)
	v.positiveDur("server.shutdown_grace", s.ShutdownGrace)
	v.positiveInt("server.max_body_bytes", s.MaxBodyBytes)
	switch s.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		v.fail("server.log_level: must be one of debug, info, warn, error (got %q)", s.LogLevel)
	}
	if s.MetricsAddr != "" {
		if _, _, err := net.SplitHostPort(s.MetricsAddr); err != nil {
			v.fail("server.metrics_addr: %q is not a host:port listen address", s.MetricsAddr)
		}
	}
}

func (v *validator) storage(s Storage) {
	v.positiveDur("storage.retention", s.Retention)
	v.positiveInt("storage.max_result_bytes", s.MaxResultBytes)
}

func (v *validator) upstream(u Upstream) {
	v.positiveInt("upstream.max_response_bytes", u.MaxResponseBytes)
}

func (v *validator) worker(w Worker) {
	v.positiveInt("worker.count", int64(w.Count))
	v.positiveInt("worker.resolvent_concurrency", int64(w.ResolventConcurrency))
	v.positiveInt("worker.max_attempts", int64(w.MaxAttempts))
	v.positiveDur("worker.poll_interval", w.PollInterval)
	v.positiveDur("worker.backoff_base", w.BackoffBase)
	v.positiveDur("worker.backoff_max", w.BackoffMax)
	v.positiveDur("worker.job_timeout", w.JobTimeout)
	if w.BackoffBase > 0 && w.BackoffMax > 0 && w.BackoffBase > w.BackoffMax {
		v.fail("worker.backoff_base (%s) must not exceed worker.backoff_max (%s)", w.BackoffBase.Std(), w.BackoffMax.Std())
	}
}

func (v *validator) cache(c Cache) {
	v.positiveDur("cache.default_ttl", c.DefaultTTL)
	v.positiveDur("cache.cleanup_interval", c.CleanupInterval)
}

func (v *validator) auth(a Auth) {
	if len(a.APIKeys) == 0 {
		v.fail("auth.api_keys: at least one API key is required")
	}
	seen := map[string]bool{}
	for i, k := range a.APIKeys {
		if k.Name == "" {
			v.fail("auth.api_keys[%d]: name is required", i)
		}
		if k.Key == "" {
			v.fail("auth.api_keys[%d] (%s): key is required", i, k.Name)
		}
		if seen[k.Name] {
			v.fail("auth.api_keys: duplicate name %q", k.Name)
		}
		seen[k.Name] = true
		if k.Role != RoleClient && k.Role != RoleAdmin {
			v.fail("auth.api_keys[%d] (%s): role must be %q or %q (got %q)", i, k.Name, RoleClient, RoleAdmin, k.Role)
		}
	}
}

func (v *validator) targets(targets map[string]Target, resolvents map[string]Resolvent, w Worker) {
	if len(targets) == 0 {
		v.fail("targets: at least one target is required")
	}
	longest, longestName := longestResolventTimeout(resolvents, targets)
	for name, t := range targets {
		v.target("targets."+name, t)
		v.timeoutBudget("targets."+name, t, w, longest, longestName)
	}
}

// longestResolventTimeout returns the longest time a single resolvent call
// may take — a target-backed resolvent counts with its backing target's
// timeout — and which resolvent it belongs to (sorted name order breaks
// ties so messages are stable).
func longestResolventTimeout(resolvents map[string]Resolvent, targets map[string]Target) (Duration, string) {
	var longest Duration
	var longestName string
	for _, name := range slices.Sorted(maps.Keys(resolvents)) {
		r := resolvents[name]
		d := r.Timeout
		if r.Target != "" {
			d = targets[r.Target].Timeout
		}
		if d > longest {
			longest, longestName = d, name
		}
	}
	return longest, longestName
}

// timeoutBudget requires a target's attempt deadline to cover its own call
// timeout plus the longest resolvent timeout. Below that, a forward that is
// merely slow is cut off by the job deadline, classified transient, and the
// target's work is submitted again — the most expensive failure mode for a
// synchronous simulation target.
func (v *validator) timeoutBudget(p string, t Target, w Worker, longest Duration, longestName string) {
	budget, source := t.JobTimeout, p+".job_timeout"
	if budget == 0 {
		budget, source = w.JobTimeout, "worker.job_timeout"
	}
	if budget <= 0 || t.Timeout <= 0 {
		return // both already reported as invalid
	}
	need, detail := t.Timeout, fmt.Sprintf("its timeout (%s)", t.Timeout.Std())
	if !t.Proxy && longest > 0 {
		need += longest
		detail += fmt.Sprintf(" plus the longest resolvent timeout (%s, %s)", longest.Std(), longestName)
	}
	if budget < need {
		v.fail("%s: %s (%s) is shorter than %s; a forward cut off by the job deadline would be retried and re-submit the target's work — raise it to at least %s",
			p, source, budget.Std(), detail, need.Std())
	}
}

func (v *validator) target(p string, t Target) {
	v.url(p+".url", t.URL)
	if !validMethod(t.Method) {
		v.fail("%s.method: %q is not a supported HTTP method", p, t.Method)
	}
	v.positiveDur(p+".timeout", t.Timeout)
	if t.JobTimeout < 0 {
		v.fail("%s.job_timeout: must be a positive duration or omitted (got %s)", p, t.JobTimeout.Std())
	}
	if t.MaxAttempts < 0 {
		v.fail("%s.max_attempts: must be a positive integer or omitted (got %d)", p, t.MaxAttempts)
	}
	v.targetAuth(p, t)
	if t.Proxy && (t.TimeseriesPath != "" || t.AttachResolvent != nil) {
		v.fail("%s: timeseries_path/attach_resolvent have no effect on a proxy target", p)
	}
	switch t.Response.Mode {
	case ModeDirect:
	case ModePoll:
		v.poll(p+".response.poll", t.Response.Poll)
	default:
		v.fail("%s.response.mode: must be \"direct\" or \"poll\" (got %q)", p, t.Response.Mode)
	}
}

func (v *validator) targetAuth(p string, t Target) {
	switch t.APIKeyInject {
	case InjectNone, InjectBodyField, InjectHeader:
	default:
		v.fail("%s.api_key_inject: must be one of none, body_field, header (got %q)", p, t.APIKeyInject)
	}
	if t.APIKeyInject != InjectNone && t.APIKey == "" {
		v.fail("%s.api_key: required when api_key_inject is %q", p, t.APIKeyInject)
	}
}

func (v *validator) poll(p string, pl *Poll) {
	if pl == nil {
		v.fail("%s: required when mode is \"poll\"", p)
		return
	}
	if pl.IDJSONPath == "" {
		v.fail("%s.id_json_path: required", p)
	}
	if !strings.Contains(pl.URLTemplate, "{id}") {
		v.fail("%s.url_template: must contain the {id} placeholder", p)
	}
	if u, err := url.Parse(strings.ReplaceAll(pl.URLTemplate, "{id}", "x")); err != nil ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		v.fail("%s.url_template: %q is not a valid http(s) URL", p, pl.URLTemplate)
	}
	if pl.StatusJSONPath == "" {
		v.fail("%s.status_json_path: required", p)
	}
	if len(pl.DoneValues) == 0 {
		v.fail("%s.done_values: at least one value is required", p)
	}
	v.positiveDur(p+".interval", pl.Interval)
	v.positiveDur(p+".timeout", pl.Timeout)
	if pl.Interval > 0 && pl.Timeout > 0 && pl.Interval >= pl.Timeout {
		v.fail("%s.interval (%s) must be shorter than %s.timeout (%s)", p, pl.Interval.Std(), p, pl.Timeout.Std())
	}
	// A status in both sets would fail every successful job: the worker
	// checks Failed before Done.
	if overlap := intersect(pl.DoneValues, pl.FailedValues); len(overlap) > 0 {
		v.fail("%s: done_values and failed_values overlap on %q", p, overlap)
	}
}

func (v *validator) resolvents(resolvents map[string]Resolvent, targets map[string]Target) {
	for name, r := range resolvents {
		p := "resolvents." + name
		if !strings.HasPrefix(name, ResolventTypePrefix) {
			v.fail("%s: resolvent type must start with %q", p, ResolventTypePrefix)
		}
		v.positiveDur(p+".cache_ttl", r.CacheTTL)
		switch {
		case r.Target != "" && r.URL != "":
			v.fail("%s: url and target are mutually exclusive", p)
		case r.Target != "":
			v.targetBacked(p, r, targets)
		default:
			v.url(p+".url", r.URL)
			if !validMethod(r.Method) {
				v.fail("%s.method: %q is not a supported HTTP method", p, r.Method)
			}
			v.positiveDur(p+".timeout", r.Timeout)
		}
	}
}

func (v *validator) targetBacked(p string, r Resolvent, targets map[string]Target) {
	if tgt, ok := targets[r.Target]; !ok {
		v.fail("%s.target: %q is not a configured target", p, r.Target)
	} else if tgt.Response.Mode != ModeDirect {
		// A poll-mode backing target would make resolution itself
		// asynchronous; only synchronous targets can back a resolvent.
		v.fail("%s.target: %q uses response mode %q; only direct-mode targets can back a resolvent",
			p, r.Target, tgt.Response.Mode)
	}
	if r.APIKey != "" || r.Method != "" || r.Timeout != 0 {
		v.fail("%s: method/api_key/timeout belong to the backing target %q, not the resolvent", p, r.Target)
	}
}

// url records a problem unless raw is an absolute http(s) URL.
func (v *validator) url(field, raw string) {
	if raw == "" {
		v.fail("%s: required", field)
		return
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		v.fail("%s: %q is not a valid http(s) URL", field, raw)
	}
}

func intersect(a, b []string) []string {
	inA := make(map[string]bool, len(a))
	for _, v := range a {
		inA[v] = true
	}
	var out []string
	for _, v := range b {
		if inA[v] {
			out = append(out, v)
		}
	}
	return out
}

func validMethod(m string) bool {
	switch m {
	case "GET", "POST", "PUT", "PATCH":
		return true
	}
	return false
}

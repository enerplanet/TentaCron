package config

import (
	"errors"
	"fmt"
	"net/url"
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
	v.auth(c.Auth)
	v.targets(c.Targets)
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
	}
}

func (v *validator) targets(targets map[string]Target) {
	if len(targets) == 0 {
		v.fail("targets: at least one target is required")
	}
	for name, t := range targets {
		v.target("targets."+name, t)
	}
}

func (v *validator) target(p string, t Target) {
	v.url(p+".url", t.URL)
	if !validMethod(t.Method) {
		v.fail("%s.method: %q is not a supported HTTP method", p, t.Method)
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
	v.url(p+".url_template", strings.ReplaceAll(pl.URLTemplate, "{id}", "x"))
	if pl.StatusJSONPath == "" {
		v.fail("%s.status_json_path: required", p)
	}
	if len(pl.DoneValues) == 0 {
		v.fail("%s.done_values: at least one value is required", p)
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

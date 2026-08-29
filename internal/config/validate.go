package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ResolventTypePrefix is the required prefix of resolvent type identifiers.
const ResolventTypePrefix = "resolvent-"

// Validate checks the configuration for consistency. All problems are
// reported at once so the operator can fix a config file in one pass.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if len(c.Auth.APIKeys) == 0 {
		fail("auth.api_keys: at least one API key is required")
	}
	seenNames := map[string]bool{}
	for i, k := range c.Auth.APIKeys {
		if k.Name == "" {
			fail("auth.api_keys[%d]: name is required", i)
		}
		if k.Key == "" {
			fail("auth.api_keys[%d] (%s): key is required", i, k.Name)
		}
		if seenNames[k.Name] {
			fail("auth.api_keys: duplicate name %q", k.Name)
		}
		seenNames[k.Name] = true
	}

	if len(c.Targets) == 0 {
		fail("targets: at least one target is required")
	}
	for name, t := range c.Targets {
		p := "targets." + name
		checkURL(&errs, p+".url", t.URL)
		if !validMethod(t.Method) {
			fail("%s.method: %q is not a supported HTTP method", p, t.Method)
		}
		switch t.APIKeyInject {
		case InjectNone, InjectBodyField, InjectHeader:
		default:
			fail("%s.api_key_inject: must be one of none, body_field, header (got %q)", p, t.APIKeyInject)
		}
		if t.APIKeyInject != InjectNone && t.APIKey == "" {
			fail("%s.api_key: required when api_key_inject is %q", p, t.APIKeyInject)
		}
		switch t.Response.Mode {
		case ModeDirect:
		case ModePoll:
			if t.Response.Poll == nil {
				fail("%s.response.poll: required when mode is \"poll\"", p)
				continue
			}
			pl := t.Response.Poll
			if pl.IDJSONPath == "" {
				fail("%s.response.poll.id_json_path: required", p)
			}
			if !strings.Contains(pl.URLTemplate, "{id}") {
				fail("%s.response.poll.url_template: must contain the {id} placeholder", p)
			}
			checkURL(&errs, p+".response.poll.url_template", strings.ReplaceAll(pl.URLTemplate, "{id}", "x"))
			if pl.StatusJSONPath == "" {
				fail("%s.response.poll.status_json_path: required", p)
			}
			if len(pl.DoneValues) == 0 {
				fail("%s.response.poll.done_values: at least one value is required", p)
			}
		default:
			fail("%s.response.mode: must be \"direct\" or \"poll\" (got %q)", p, t.Response.Mode)
		}
	}

	for name, r := range c.Resolvents {
		p := "resolvents." + name
		if !strings.HasPrefix(name, ResolventTypePrefix) {
			fail("%s: resolvent type must start with %q", p, ResolventTypePrefix)
		}
		checkURL(&errs, p+".url", r.URL)
		if !validMethod(r.Method) {
			fail("%s.method: %q is not a supported HTTP method", p, r.Method)
		}
	}

	return errors.Join(errs...)
}

func checkURL(errs *[]error, field, raw string) {
	if raw == "" {
		*errs = append(*errs, fmt.Errorf("%s: required", field))
		return
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		*errs = append(*errs, fmt.Errorf("%s: %q is not a valid http(s) URL", field, raw))
	}
}

func validMethod(m string) bool {
	switch m {
	case "GET", "POST", "PUT", "PATCH":
		return true
	}
	return false
}

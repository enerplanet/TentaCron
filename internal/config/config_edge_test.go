package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// expandEnv must report every unset variable at once — sorted and
// deduplicated — so an operator fixes a config file in one pass.
func TestExpandEnvReportsAllMissingVariablesOnce(t *testing.T) {
	_, err := expandEnv("a=${TC_EDGE_ZZZ} b=${TC_EDGE_AAA} c=${TC_EDGE_ZZZ} d=$TC_EDGE_MMM")
	if err == nil {
		t.Fatal("want error for unset variables")
	}
	msg := err.Error()
	if !strings.Contains(msg, "TC_EDGE_AAA, TC_EDGE_MMM, TC_EDGE_ZZZ") {
		t.Errorf("missing variables must be listed sorted: %s", msg)
	}
	if strings.Count(msg, "TC_EDGE_ZZZ") != 1 {
		t.Errorf("a variable referenced twice must be reported once: %s", msg)
	}
}

// The documented escaping rules, plus the os.Expand corner cases an operator
// can trip over: a trailing or free-standing dollar stays literal, "${}" is
// swallowed as invalid syntax, and the bare $VAR form interpolates too.
func TestExpandEnvDollarSemantics(t *testing.T) {
	t.Setenv("TC_EDGE_VAL", "v")
	cases := map[string]string{
		"$$":               "$",
		"x$$y":             "x$y",
		"${TC_EDGE_VAL}":   "v",
		"$TC_EDGE_VAL":     "v",
		"$$${TC_EDGE_VAL}": "$v",
		"cost 5$":          "cost 5$",
		"a $ b":            "a $ b",
		"${}":              "",
	}
	for in, want := range cases {
		got, err := expandEnv(in)
		if err != nil {
			t.Errorf("expandEnv(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("expandEnv(%q) = %q, want %q", in, got, want)
		}
	}
}

// A variable that is set but empty passes interpolation; validation must
// still refuse the empty credential so the service never boots open.
func TestEmptyEnvValueStillFailsValidation(t *testing.T) {
	t.Setenv("TC_EDGE_EMPTY", "")
	_, err := Load(writeConfig(t, `
auth:
  api_keys: [{name: t, key: "${TC_EDGE_EMPTY}"}]
targets:
  demo:
    url: "https://demo.example.com/run"
`))
	if err == nil || !strings.Contains(err.Error(), "key is required") {
		t.Fatalf("want 'key is required', got %v", err)
	}
}

// Interpolation runs over the whole file, comments included — the reference
// config documents this; an unset variable in a comment aborts startup.
func TestUnsetVariableInCommentAbortsStartup(t *testing.T) {
	_, err := Load(writeConfig(t, minimalYAML+"\n# see ${TC_EDGE_UNSET_IN_COMMENT}\n"))
	if err == nil || !strings.Contains(err.Error(), "TC_EDGE_UNSET_IN_COMMENT") {
		t.Fatalf("want unset-variable error from a comment, got %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("want read error, got %v", err)
	}
}

func TestDurationParsing(t *testing.T) {
	cases := []struct{ name, yaml, wantErr string }{
		{"bare number has no unit", "worker:\n  job_timeout: 30\n", "invalid duration"},
		{"non-scalar node", "worker:\n  job_timeout: [1]\n", "duration must be a string"},
		{"compound value", "worker:\n  job_timeout: 1m30s\n", ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, minimalYAML+tt.yaml))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Worker.JobTimeout.Std() != 90*time.Second {
				t.Errorf("job_timeout = %v, want 1m30s", cfg.Worker.JobTimeout.Std())
			}
		})
	}
}

func TestTargetAuthDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
auth:
  api_keys: [{name: t, key: k}]
targets:
  hdr:
    url: "https://hdr.example.com/run"
    api_key: secret
  body:
    url: "https://body.example.com/run"
    api_key: secret
    api_key_inject: body_field
`))
	if err != nil {
		t.Fatal(err)
	}
	hdr := cfg.Targets["hdr"]
	if hdr.APIKeyInject != InjectHeader || hdr.APIKeyHeader != "X-API-Key" {
		t.Errorf("a configured key defaults to header injection via X-API-Key, got %q/%q", hdr.APIKeyInject, hdr.APIKeyHeader)
	}
	if hdr.Timeout.Std() != 60*time.Second {
		t.Errorf("target timeout default = %v, want 60s", hdr.Timeout.Std())
	}
	if body := cfg.Targets["body"]; body.APIKeyField != "api_key" {
		t.Errorf("body_field default field = %q, want api_key", body.APIKeyField)
	}
}

func TestPollDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
auth:
  api_keys: [{name: t, key: k}]
targets:
  meme:
    url: "https://meme.example.com/simulate"
    response:
      mode: poll
      poll:
        id_json_path: id
        url_template: "https://meme.example.com/jobs/{id}"
        status_json_path: state
        done_values: [succeeded]
`))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Targets["meme"].Response.Poll
	if p.Interval.Std() != 10*time.Second || p.Timeout.Std() != 30*time.Minute {
		t.Errorf("poll defaults = %v/%v, want 10s/30m", p.Interval.Std(), p.Timeout.Std())
	}
	if p.ResultURLTemplate != p.URLTemplate {
		t.Errorf("result_url_template must default to url_template, got %q", p.ResultURLTemplate)
	}
}

func TestProxyTargetHasNoResolutionDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
auth:
  api_keys: [{name: t, key: k}]
targets:
  ignis-calculate:
    url: "https://ignis.example.com/api/v1/calculate/{code}"
    proxy: true
`))
	if err != nil {
		t.Fatal(err)
	}
	tgt := cfg.Targets["ignis-calculate"]
	if tgt.TimeseriesPath != "" || tgt.AttachResolvent != nil {
		t.Errorf("proxy target must not receive resolution defaults: path=%q attach=%v", tgt.TimeseriesPath, tgt.AttachResolvent)
	}
	// Even the default value, spelled out explicitly, is rejected: it would
	// suggest resolution happens.
	_, err = Load(writeConfig(t, `
auth:
  api_keys: [{name: t, key: k}]
targets:
  ignis-calculate:
    url: "https://ignis.example.com/api/v1/calculate/{code}"
    proxy: true
    attach_resolvent: true
`))
	if err == nil || !strings.Contains(err.Error(), "no effect on a proxy target") {
		t.Fatalf("explicit attach_resolvent on a proxy target must be rejected, got %v", err)
	}
}

// Validation collects every problem instead of stopping at the first.
func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	_, err := Load(writeConfig(t, `
auth:
  api_keys: [{key: k}]
targets:
  demo:
    url: "https://demo.example.com/run"
    api_key_inject: query
resolvents:
  pv1:
    method: POST
`))
	if err == nil {
		t.Fatal("want validation error")
	}
	for _, want := range []string{"name is required", "api_key_inject", "must start with", "url: required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention %q:\n%v", want, err)
		}
	}
}

func TestMethodValidation(t *testing.T) {
	for _, m := range []string{"GET", "POST", "PUT", "PATCH"} {
		_, err := Load(writeConfig(t, `
auth:
  api_keys: [{name: t, key: k}]
targets:
  demo:
    url: "https://demo.example.com/run"
    method: `+m+`
resolvents:
  resolvent-x:
    url: "https://x.example.com/q"
    method: `+m+`
`))
		if err != nil {
			t.Errorf("method %s must be accepted: %v", m, err)
		}
	}
	for _, m := range []string{"post", "DELETE", "HEAD", "OPTIONS"} {
		_, err := Load(writeConfig(t, `
auth:
  api_keys: [{name: t, key: k}]
targets:
  demo:
    url: "https://demo.example.com/run"
    method: `+m+`
resolvents:
  resolvent-x:
    url: "https://x.example.com/q"
    method: `+m+`
`))
		if err == nil || strings.Count(err.Error(), "not a supported HTTP method") != 2 {
			t.Errorf("method %q must be rejected for target and resolvent, got %v", m, err)
		}
	}
}

func TestURLValidation(t *testing.T) {
	for _, u := range []string{"https://", "example.com/run", "/relative", "mailto:x@y", "http:///path"} {
		_, err := Load(writeConfig(t, `
auth:
  api_keys: [{name: t, key: k}]
targets:
  demo:
    url: "`+u+`"
`))
		if err == nil || !strings.Contains(err.Error(), "not a valid http(s) URL") {
			t.Errorf("url %q must be rejected, got %v", u, err)
		}
	}
	for _, u := range []string{"http://host", "https://host:8443/a?b=c", "http://127.0.0.1:8080"} {
		_, err := Load(writeConfig(t, `
auth:
  api_keys: [{name: t, key: k}]
targets:
  demo:
    url: "`+u+`"
`))
		if err != nil {
			t.Errorf("url %q must be accepted: %v", u, err)
		}
	}
	_, err := Load(writeConfig(t, minimalYAML+`
resolvents:
  resolvent-x:
    method: GET
`))
	if err == nil || !strings.Contains(err.Error(), "resolvents.resolvent-x.url: required") {
		t.Errorf("a URL-backed resolvent without url must be rejected, got %v", err)
	}
}

func TestPollValidationRules(t *testing.T) {
	base := `
auth:
  api_keys: [{name: t, key: k}]
targets:
  meme:
    url: "https://meme.example.com/simulate"
    response:
      mode: poll
      poll:
`
	cases := []struct{ name, poll, wantErr string }{
		{"missing id path", `
        url_template: "https://meme.example.com/jobs/{id}"
        status_json_path: state
        done_values: [done]
`, "id_json_path: required"},
		{"missing status path", `
        id_json_path: id
        url_template: "https://meme.example.com/jobs/{id}"
        done_values: [done]
`, "status_json_path: required"},
		{"empty done values", `
        id_json_path: id
        url_template: "https://meme.example.com/jobs/{id}"
        status_json_path: state
        done_values: []
`, "done_values: at least one"},
		{"relative url template", `
        id_json_path: id
        url_template: "jobs/{id}"
        status_json_path: state
        done_values: [done]
`, "url_template: \"jobs/{id}\" is not a valid http(s) URL"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, base+tt.poll))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
	_, err := Load(writeConfig(t, `
auth:
  api_keys: [{name: t, key: k}]
targets:
  meme:
    url: "https://meme.example.com/simulate"
    response:
      mode: async
`))
	if err == nil || !strings.Contains(err.Error(), `response.mode: must be "direct" or "poll"`) {
		t.Errorf("unknown response mode must be rejected, got %v", err)
	}
}

func TestAuthKeyValidation(t *testing.T) {
	_, err := Load(writeConfig(t, `
auth:
  api_keys: [{name: t}]
targets:
  demo:
    url: "https://demo.example.com/run"
`))
	if err == nil || !strings.Contains(err.Error(), "(t): key is required") {
		t.Errorf("missing key must name the entry, got %v", err)
	}
}

// Secrets are redacted longest-first so a shorter credential can never
// split a longer one; ties are ordered lexically for determinism.
func TestUpstreamSecretsOrdering(t *testing.T) {
	cfg := &Config{
		Targets: map[string]Target{
			"a": {APIKey: "abc"}, "b": {APIKey: ""}, "c": {APIKey: "abcdef"},
		},
		Resolvents: map[string]Resolvent{
			"resolvent-x": {APIKey: "zz"}, "resolvent-y": {APIKey: "abcdef"}, "resolvent-z": {},
		},
	}
	got := cfg.UpstreamSecrets()
	want := []string{"abcdef", "abcdef", "abc", "zz"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("UpstreamSecrets = %v, want %v", got, want)
	}
	if (&Config{}).UpstreamSecrets() != nil {
		t.Error("no credentials must yield an empty list")
	}
}

func TestResolventCacheTTLDefaultsToConfiguredDefault(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalYAML+`
cache:
  default_ttl: 1h
resolvents:
  resolvent-x:
    url: "https://x.example.com/q"
  resolvent-y:
    url: "https://y.example.com/q"
    cache_ttl: 2h
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Resolvents["resolvent-x"].CacheTTL.Std(); got != time.Hour {
		t.Errorf("resolvent-x cache_ttl = %v, want the 1h default", got)
	}
	if got := cfg.Resolvents["resolvent-y"].CacheTTL.Std(); got != 2*time.Hour {
		t.Errorf("resolvent-y cache_ttl = %v, want its own 2h", got)
	}
	x := cfg.Resolvents["resolvent-x"]
	if x.Method != "POST" || x.APIKeyHeader != "X-API-Key" || x.Timeout.Std() != 30*time.Second {
		t.Errorf("URL-backed resolvent defaults = %+v", x)
	}
}

// The containerized config must load exactly like the reference config: a
// drift between the two would only surface at container start.
func TestEnvironmentConfigLoads(t *testing.T) {
	t.Setenv("PORT", "9999")
	for _, v := range []string{
		"TENTACRON_KEY_FRONTEND", "TENTACRON_KEY_BATCH",
		"MEME_API_KEY", "BUEM_API_KEY", "PV1_API_KEY", "WIND_API_KEY",
		"WEATHER_API_KEY", "IGNIS_API_KEY",
	} {
		t.Setenv(v, "test-"+v)
	}
	cfg, err := Load("../../environment/config.yaml")
	if err != nil {
		t.Fatalf("environment/config.yaml must load cleanly: %v", err)
	}
	if cfg.Server.Addr != ":9999" {
		t.Errorf("addr = %q, want the PORT interpolation", cfg.Server.Addr)
	}
	for _, name := range []string{"meme", "buem", "buem-building", "ignis-calculate"} {
		if _, ok := cfg.Targets[name]; !ok {
			t.Errorf("target %q missing from the environment config", name)
		}
	}
	for _, name := range []string{"resolvent-pv1", "resolvent-wind", "resolvent-weather",
		"resolvent-city2tabula", "resolvent-ignis", "resolvent-buem"} {
		if _, ok := cfg.Resolvents[name]; !ok {
			t.Errorf("resolvent %q missing from the environment config", name)
		}
	}
}

// Validate is also called on hand-built configs (tests, embedders) that
// never went through applyDefaults; every zero value must be diagnosed.
func TestValidateHandBuiltConfigWithoutDefaults(t *testing.T) {
	cfg := &Config{
		Auth:    Auth{APIKeys: []APIKey{{Name: "t", Key: "k"}}},
		Targets: map[string]Target{"x": {URL: "https://x.example.com"}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("want validation errors for zero-valued target")
	}
	for _, want := range []string{"not a supported HTTP method", "api_key_inject: must be one of", `response.mode: must be "direct" or "poll"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention %q:\n%v", want, err)
		}
	}
}

// Defaults only replace zero values, so every numeric and duration setting
// must reject negatives (and zero where zero cannot mean "default") with a
// message naming the key; a hand-built config skipping applyDefaults must
// be diagnosed the same way.
func TestNumericAndDurationValidation(t *testing.T) {
	cases := []struct{ name, yaml, wantErr string }{
		{"negative worker count", "worker:\n  count: -1\n", "worker.count: must be a positive integer (got -1)"},
		{"negative resolvent concurrency", "worker:\n  resolvent_concurrency: -2\n", "worker.resolvent_concurrency: must be a positive integer"},
		{"negative max attempts", "worker:\n  max_attempts: -1\n", "worker.max_attempts: must be a positive integer"},
		{"negative body limit", "server:\n  max_body_bytes: -1\n", "server.max_body_bytes: must be a positive integer"},
		{"negative job timeout", "worker:\n  job_timeout: -5s\n", "worker.job_timeout: must be a positive duration (got -5s)"},
		{"negative poll interval", "worker:\n  poll_interval: -1s\n", "worker.poll_interval: must be a positive duration"},
		{"negative read timeout", "server:\n  read_timeout: -1s\n", "server.read_timeout: must be a positive duration"},
		{"negative retention", "storage:\n  retention: -1h\n", "storage.retention: must be a positive duration"},
		{"negative cleanup interval", "cache:\n  cleanup_interval: -1m\n", "cache.cleanup_interval: must be a positive duration"},
		{"backoff base above max", "worker:\n  backoff_base: 2m\n  backoff_max: 1m\n", "worker.backoff_base (2m0s) must not exceed worker.backoff_max (1m0s)"},
		{"negative target timeout", "auth:\n  api_keys: [{name: t, key: k}]\ntargets:\n  buem:\n    url: \"https://buem.example.com/run\"\n    timeout: -1s\n", "targets.buem.timeout: must be a positive duration"},
		{"negative resolvent timeout", "resolvents:\n  resolvent-x:\n    url: \"https://x.example.com/q\"\n    timeout: -1s\n", "resolvents.resolvent-x.timeout: must be a positive duration"},
		{"negative cache ttl", "resolvents:\n  resolvent-x:\n    url: \"https://x.example.com/q\"\n    cache_ttl: -1h\n", "resolvents.resolvent-x.cache_ttl: must be a positive duration"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			doc := tt.yaml
			if !strings.HasPrefix(doc, "auth:") {
				doc = minimalYAML + doc
			}
			_, err := Load(writeConfig(t, doc))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
	// Zero still means "use the default", so the documented defaults apply.
	cfg, err := Load(writeConfig(t, minimalYAML+"worker:\n  count: 0\n"))
	if err != nil || cfg.Worker.Count != 4 {
		t.Errorf("zero count must fall back to the default 4, got %d (err %v)", cfg.Worker.Count, err)
	}
	hand := &Config{
		Auth:    Auth{APIKeys: []APIKey{{Name: "t", Key: "k"}}},
		Worker:  Worker{Count: -3},
		Targets: map[string]Target{"x": {URL: "https://x.example.com", Method: "POST", APIKeyInject: InjectNone, Timeout: Duration(time.Second), Response: Response{Mode: ModeDirect}}},
	}
	if err := hand.Validate(); err == nil || !strings.Contains(err.Error(), "worker.count: must be a positive integer (got -3)") {
		t.Errorf("hand-built config must be diagnosed too, got %v", err)
	}
}

func TestPollIntervalMustBeShorterThanTimeout(t *testing.T) {
	_, err := Load(writeConfig(t, `
auth:
  api_keys: [{name: t, key: k}]
targets:
  meme:
    url: "https://meme.example.com/simulate"
    response:
      mode: poll
      poll:
        id_json_path: id
        url_template: "https://meme.example.com/jobs/{id}"
        status_json_path: state
        done_values: [succeeded]
        interval: 1h
        timeout: 1m
`))
	if err == nil || !strings.Contains(err.Error(), "poll.interval (1h0m0s) must be shorter than targets.meme.response.poll.timeout (1m0s)") {
		t.Fatalf("want interval/timeout error, got %v", err)
	}
}

// The attempt deadline must cover a target's own timeout plus the longest
// resolvent timeout; otherwise a slow forward is cut off, classified
// transient, and the target's work is submitted again.
func TestTimeoutBudgetValidation(t *testing.T) {
	base := `
auth:
  api_keys: [{name: t, key: k}]
worker:
  job_timeout: 5m
targets:
  buem:
    url: "https://buem.example.com/run"
    timeout: 300s
  buem-building:
    url: "https://buem.example.com/building"
    timeout: 200s
  ignis:
    url: "https://ignis.example.com/calc/{code}"
    timeout: 60s
    proxy: true
resolvents:
  resolvent-weather:
    url: "https://weather.example.com/point"
    timeout: 60s
  resolvent-buem:
    target: buem-building
`
	_, err := Load(writeConfig(t, base))
	if err == nil {
		t.Fatal("want a budget error")
	}
	want := "targets.buem: worker.job_timeout (5m0s) is shorter than its timeout (5m0s) plus the longest resolvent timeout (3m20s, resolvent-buem)"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error must read\n  %s\ngot\n  %v", want, err)
	}
	if !strings.Contains(err.Error(), "raise it to at least 8m20s") {
		t.Errorf("error must name the required budget, got %v", err)
	}
	if strings.Contains(err.Error(), "targets.ignis") {
		t.Errorf("a proxy target only needs its own timeout (60s < 5m), got %v", err)
	}
	// buem-building needs 200s + 200s = 6m40s and is reported as well, with
	// its own required budget.
	if !strings.Contains(err.Error(), "targets.buem-building: worker.job_timeout (5m0s) is shorter than its timeout (3m20s)") ||
		!strings.Contains(err.Error(), "raise it to at least 6m40s") {
		t.Errorf("buem-building must be reported with its own budget, got %v", err)
	}
	// A per-target job_timeout satisfies the rule for that target only.
	fixed := strings.Replace(base, "    timeout: 300s\n", "    timeout: 300s\n    job_timeout: 10m\n", 1)
	fixed = strings.Replace(fixed, "    timeout: 200s\n", "    timeout: 200s\n    job_timeout: 7m\n", 1)
	if _, err := Load(writeConfig(t, fixed)); err != nil {
		t.Errorf("per-target job_timeout must satisfy the budget: %v", err)
	}
	// Raising the worker default fixes every target at once.
	if _, err := Load(writeConfig(t, strings.Replace(base, "job_timeout: 5m", "job_timeout: 15m", 1))); err != nil {
		t.Errorf("worker.job_timeout 15m must satisfy every target: %v", err)
	}
}

func TestPerTargetOverridesAndRetryPolicy(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
auth:
  api_keys: [{name: t, key: k}]
worker:
  job_timeout: 5m
  max_attempts: 5
targets:
  tuned:
    url: "https://tuned.example.com/run"
    job_timeout: 2m
    max_attempts: 2
    retry_on_timeout: false
  plain:
    url: "https://plain.example.com/run"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.JobTimeoutFor("tuned"); got != 2*time.Minute {
		t.Errorf("tuned job timeout = %v, want 2m", got)
	}
	if got := cfg.JobTimeoutFor("plain"); got != 5*time.Minute {
		t.Errorf("plain job timeout = %v, want the worker default 5m", got)
	}
	if got := cfg.JobTimeoutFor("gone"); got != 5*time.Minute {
		t.Errorf("unknown target job timeout = %v, want the worker default", got)
	}
	if cfg.MaxAttemptsFor("tuned") != 2 || cfg.MaxAttemptsFor("plain") != 5 || cfg.MaxAttemptsFor("gone") != 5 {
		t.Errorf("max attempts = %d/%d/%d, want 2/5/5", cfg.MaxAttemptsFor("tuned"), cfg.MaxAttemptsFor("plain"), cfg.MaxAttemptsFor("gone"))
	}
	if cfg.Targets["tuned"].RetriesOnTimeout() || !cfg.Targets["plain"].RetriesOnTimeout() || !(Target{}).RetriesOnTimeout() {
		t.Error("retry_on_timeout must default to true and honour an explicit false")
	}
	for _, tt := range []struct{ yaml, wantErr string }{
		{"    job_timeout: -1m\n", "targets.plain.job_timeout: must be a positive duration or omitted (got -1m0s)"},
		{"    max_attempts: -2\n", "targets.plain.max_attempts: must be a positive integer or omitted (got -2)"},
	} {
		_, err := Load(writeConfig(t, minimalYAML+"\n"+`  plain:
    url: "https://plain.example.com/run"
`+tt.yaml))
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("want %q, got %v", tt.wantErr, err)
		}
	}
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimalYAML = `
auth:
  api_keys:
    - name: test
      key: secret
targets:
  buem:
    url: "https://buem.example.com/run"
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMinimalAppliesDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":8080" {
		t.Errorf("addr = %q, want :8080", cfg.Server.Addr)
	}
	if cfg.Server.MaxBodyBytes != 10<<20 {
		t.Errorf("max_body_bytes = %d, want %d", cfg.Server.MaxBodyBytes, 10<<20)
	}
	if cfg.Worker.MaxAttempts != 5 {
		t.Errorf("max_attempts = %d, want 5", cfg.Worker.MaxAttempts)
	}
	tgt := cfg.Targets["buem"]
	if tgt.Method != "POST" {
		t.Errorf("target method = %q, want POST", tgt.Method)
	}
	if tgt.TimeseriesPath != "time-series" {
		t.Errorf("timeseries_path = %q, want time-series", tgt.TimeseriesPath)
	}
	if tgt.APIKeyInject != InjectNone {
		t.Errorf("api_key_inject = %q, want none (no key configured)", tgt.APIKeyInject)
	}
	if tgt.Response.Mode != ModeDirect {
		t.Errorf("response mode = %q, want direct", tgt.Response.Mode)
	}
	if tgt.AttachResolvent == nil || !*tgt.AttachResolvent {
		t.Errorf("attach_resolvent must default to true, got %v", tgt.AttachResolvent)
	}
	if cfg.Cache.DefaultTTL.Std() != 6*time.Hour {
		t.Errorf("default_ttl = %v, want 6h", cfg.Cache.DefaultTTL.Std())
	}
}

func TestLoadEnvInterpolation(t *testing.T) {
	t.Setenv("TC_TEST_KEY", "from-env")
	cfg, err := Load(writeConfig(t, `
auth:
  api_keys:
    - name: test
      key: "${TC_TEST_KEY}"
targets:
  buem:
    url: "https://buem.example.com/run"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Auth.APIKeys[0].Key; got != "from-env" {
		t.Errorf("key = %q, want from-env", got)
	}
}

func TestLoadUnsetEnvFails(t *testing.T) {
	_, err := Load(writeConfig(t, `
auth:
  api_keys:
    - name: test
      key: "${TC_DEFINITELY_UNSET_VAR}"
targets:
  buem:
    url: "https://buem.example.com/run"
`))
	if err == nil || !strings.Contains(err.Error(), "TC_DEFINITELY_UNSET_VAR") {
		t.Fatalf("want unset-env error naming the variable, got %v", err)
	}
}

func TestLoadRootPathAndAttachResolvent(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
auth:
  api_keys:
    - name: test
      key: secret
targets:
  buem:
    url: "https://buem-gateway.example.com/api/v1/buem/buildings"
    timeseries_path: "."
    attach_resolvent: false
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tgt := cfg.Targets["buem"]
	if tgt.TimeseriesPath != RootTimeseriesPath {
		t.Errorf("timeseries_path = %q, want %q", tgt.TimeseriesPath, RootTimeseriesPath)
	}
	if tgt.AttachResolvent == nil || *tgt.AttachResolvent {
		t.Errorf("attach_resolvent = %v, want false", tgt.AttachResolvent)
	}
}

func TestLoadTargetBackedResolvent(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
auth:
  api_keys: [{name: t, key: k}]
targets:
  buem-building:
    url: "https://buem-gateway.example.com/api/v1/buem/building"
resolvents:
  resolvent-buem:
    target: buem-building
    payload_field: payload
    response_path: "buem.thermal_load_profile.timeseries"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := cfg.Resolvents["resolvent-buem"]
	if r.Target != "buem-building" || r.PayloadField != "payload" ||
		r.ResponsePath != "buem.thermal_load_profile.timeseries" {
		t.Errorf("resolvent = %+v", r)
	}
	// URL-call defaults must not be applied to a target-backed resolvent.
	if r.Method != "" || r.Timeout != 0 {
		t.Errorf("dead URL-call defaults applied: method=%q timeout=%v", r.Method, r.Timeout)
	}
	if r.CacheTTL == 0 {
		t.Error("cache_ttl default must still apply")
	}
}

func TestValidateTargetBackedResolventRules(t *testing.T) {
	base := `
auth:
  api_keys: [{name: t, key: k}]
targets:
  direct-t:
    url: "https://direct.example.com/run"
  poll-t:
    url: "https://poll.example.com/simulate"
    response:
      mode: poll
      poll:
        id_json_path: job_id
        url_template: "https://poll.example.com/jobs/{id}"
        status_json_path: status
        done_values: [done]
`
	tests := []struct {
		name, resolvents, wantErr string
	}{
		{"url and target together", `
resolvents:
  resolvent-x:
    url: "https://x.example.com"
    target: direct-t
`, "mutually exclusive"},
		{"unknown backing target", `
resolvents:
  resolvent-x:
    target: nope
`, "not a configured target"},
		{"poll-mode backing target", `
resolvents:
  resolvent-x:
    target: poll-t
`, "only direct-mode targets can back a resolvent"},
		{"url-call fields on target-backed", `
resolvents:
  resolvent-x:
    target: direct-t
    api_key: leak
`, "belong to the backing target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, base+tt.resolvents))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestLoadDollarEscape(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
auth:
  api_keys:
    - name: test
      key: "lit$$eral"
targets:
  buem:
    url: "https://buem.example.com/run"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Auth.APIKeys[0].Key; got != "lit$eral" {
		t.Errorf("key = %q, want lit$eral", got)
	}
}

func TestLoadUnknownFieldFails(t *testing.T) {
	_, err := Load(writeConfig(t, minimalYAML+`
server:
  listen_addr: ":9090"
`))
	if err == nil {
		t.Fatal("want error for unknown field, got nil")
	}
}

func TestLoadBadDurationFails(t *testing.T) {
	_, err := Load(writeConfig(t, minimalYAML+`
worker:
  job_timeout: soon
`))
	if err == nil || !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("want duration parse error, got %v", err)
	}
}

func TestValidateRules(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "no auth keys",
			yaml: `
targets:
  buem:
    url: "https://buem.example.com/run"
`,
			wantErr: "auth.api_keys",
		},
		{
			name: "no targets",
			yaml: `
auth:
  api_keys: [{name: t, key: k}]
`,
			wantErr: "targets",
		},
		{
			name: "bad target url",
			yaml: `
auth:
  api_keys: [{name: t, key: k}]
targets:
  buem:
    url: "ftp://buem.example.com"
`,
			wantErr: "not a valid http(s) URL",
		},
		{
			name: "bad inject mode",
			yaml: `
auth:
  api_keys: [{name: t, key: k}]
targets:
  buem:
    url: "https://buem.example.com/run"
    api_key: k
    api_key_inject: query
`,
			wantErr: "api_key_inject",
		},
		{
			name: "inject without key",
			yaml: `
auth:
  api_keys: [{name: t, key: k}]
targets:
  buem:
    url: "https://buem.example.com/run"
    api_key_inject: header
`,
			wantErr: "required when api_key_inject",
		},
		{
			name: "poll without poll block",
			yaml: `
auth:
  api_keys: [{name: t, key: k}]
targets:
  meme:
    url: "https://meme.example.com/simulate"
    response:
      mode: poll
`,
			wantErr: "response.poll: required",
		},
		{
			name: "poll url without id placeholder",
			yaml: `
auth:
  api_keys: [{name: t, key: k}]
targets:
  meme:
    url: "https://meme.example.com/simulate"
    response:
      mode: poll
      poll:
        id_json_path: job_id
        url_template: "https://meme.example.com/jobs"
        status_json_path: status
        done_values: [done]
`,
			wantErr: "{id} placeholder",
		},
		{
			name: "poll done and failed values overlap",
			yaml: `
auth:
  api_keys: [{name: t, key: k}]
targets:
  meme:
    url: "https://meme.example.com/simulate"
    response:
      mode: poll
      poll:
        id_json_path: job_id
        url_template: "https://meme.example.com/jobs/{id}"
        status_json_path: status
        done_values: [finished, done]
        failed_values: [error, finished]
`,
			wantErr: `overlap on ["finished"]`,
		},
		{
			name: "proxy target with resolution knobs",
			yaml: `
auth:
  api_keys: [{name: t, key: k}]
targets:
  ignis-calculate:
    url: "https://ignis.example.com/api/v1/calculate/{code}"
    proxy: true
    timeseries_path: "time-series"
`,
			wantErr: "no effect on a proxy target",
		},
		{
			name: "resolvent bad prefix",
			yaml: `
auth:
  api_keys: [{name: t, key: k}]
targets:
  buem:
    url: "https://buem.example.com/run"
resolvents:
  pv1:
    url: "https://pvsim.example.com/gen"
`,
			wantErr: "must start with",
		},
		{
			name: "duplicate key names",
			yaml: `
auth:
  api_keys: [{name: t, key: k1}, {name: t, key: k2}]
targets:
  buem:
    url: "https://buem.example.com/run"
`,
			wantErr: "duplicate name",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestLoadFullExample(t *testing.T) {
	for _, v := range []string{
		"TENTACRON_KEY_FRONTEND", "TENTACRON_KEY_BATCH",
		"MEME_API_KEY", "BUEM_API_KEY", "PV1_API_KEY", "WIND_API_KEY",
		"WEATHER_API_KEY", "IGNIS_API_KEY", "CITY2TABULA_API_KEY",
	} {
		t.Setenv(v, "test-"+v)
	}
	cfg, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatalf("config.example.yaml must load cleanly: %v", err)
	}
	buem := cfg.Targets["buem"]
	if buem.TimeseriesPath != RootTimeseriesPath {
		t.Errorf("buem timeseries_path = %q, want %q (weather at the payload root)", buem.TimeseriesPath, RootTimeseriesPath)
	}
	if buem.AttachResolvent == nil || *buem.AttachResolvent {
		t.Errorf("buem attach_resolvent = %v, want false", buem.AttachResolvent)
	}
	if buem.APIKeyHeader != "X-Api-Key" {
		t.Errorf("buem api_key_header = %q, want the gateway's X-Api-Key", buem.APIKeyHeader)
	}
	if _, ok := cfg.Targets["demo"]; !ok {
		t.Error("demo target missing — the generic examples point at it")
	}
	if ic := cfg.Targets["ignis-calculate"]; !ic.Proxy || !strings.Contains(ic.URL, "{code}") {
		t.Errorf("ignis-calculate must be a proxy target with a {code} url template: %+v", ic)
	}
	if _, ok := cfg.Resolvents["resolvent-weather"]; !ok {
		t.Error("resolvent-weather missing — buem payloads resolve weather through it")
	}
	rb := cfg.Resolvents["resolvent-buem"]
	if rb.Target != "buem-building" || rb.PayloadField != "payload" || rb.ResponsePath == "" {
		t.Errorf("resolvent-buem must be backed by the buem-building target: %+v", rb)
	}
	// The meme poll block mirrors meme's verified contract: id in "id",
	// status at /jobs/{id}/status with a "state" of queued|running|
	// succeeded|failed, the zip bundle at /jobs/{id}.
	meme := cfg.Targets["meme"]
	if meme.Response.Mode != ModePoll {
		t.Errorf("meme response mode = %q, want poll", meme.Response.Mode)
	}
	poll := meme.Response.Poll
	if poll.IDJSONPath != "id" || poll.StatusJSONPath != "state" {
		t.Errorf("meme poll paths = %q/%q, want id/state", poll.IDJSONPath, poll.StatusJSONPath)
	}
	if len(poll.DoneValues) != 1 || poll.DoneValues[0] != "succeeded" {
		t.Errorf("meme done_values = %v, want [succeeded]", poll.DoneValues)
	}
	if !strings.HasSuffix(poll.URLTemplate, "/jobs/{id}/status") ||
		!strings.HasSuffix(poll.ResultURLTemplate, "/jobs/{id}") {
		t.Errorf("meme poll urls = %q / %q", poll.URLTemplate, poll.ResultURLTemplate)
	}
	if meme.TimeseriesPath != "model.timeseries" {
		t.Errorf("meme timeseries_path = %q", meme.TimeseriesPath)
	}
	// GET resolvents against the verified weather/city2tabula/ignis APIs.
	if w := cfg.Resolvents["resolvent-weather"]; w.Method != "GET" || !strings.Contains(w.URL, "format=json") {
		t.Errorf("resolvent-weather = %+v, want GET point query with format=json", w)
	}
	if c := cfg.Resolvents["resolvent-city2tabula"]; c.Method != "GET" || c.ResponsePath != "0" || c.APIKey != "" {
		t.Errorf("resolvent-city2tabula = %+v", c)
	}
	if i := cfg.Resolvents["resolvent-ignis"]; i.Method != "GET" ||
		!strings.Contains(i.URL, "{code}") || i.APIKeyHeader != "X-Api-Key" {
		t.Errorf("resolvent-ignis = %+v", i)
	}
	if got := cfg.Resolvents["resolvent-pv1"].CacheTTL.Std(); got != 24*time.Hour {
		t.Errorf("pv1 cache_ttl = %v, want 24h", got)
	}
}

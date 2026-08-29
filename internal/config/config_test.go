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
	} {
		t.Setenv(v, "test-"+v)
	}
	cfg, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatalf("config.example.yaml must load cleanly: %v", err)
	}
	meme := cfg.Targets["meme"]
	if meme.Response.Mode != ModePoll {
		t.Errorf("meme response mode = %q, want poll", meme.Response.Mode)
	}
	if meme.Response.Poll.ResultURLTemplate != meme.Response.Poll.URLTemplate {
		t.Errorf("result_url_template should default to url_template")
	}
	if meme.TimeseriesPath != "model.timeseries" {
		t.Errorf("meme timeseries_path = %q", meme.TimeseriesPath)
	}
	if got := cfg.Resolvents["resolvent-pv1"].CacheTTL.Std(); got != 24*time.Hour {
		t.Errorf("pv1 cache_ttl = %v, want 24h", got)
	}
}

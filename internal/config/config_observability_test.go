package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestObservabilityDefaultsAndValidation(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.LogLevel != "info" || cfg.Server.SlogLevel() != slog.LevelInfo || cfg.Server.MetricsAddr != "" {
		t.Errorf("defaults: level=%q metrics_addr=%q", cfg.Server.LogLevel, cfg.Server.MetricsAddr)
	}
	cfg, err = Load(writeConfig(t, minimalYAML+"server:\n  log_level: debug\n  metrics_addr: \"127.0.0.1:9090\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.SlogLevel() != slog.LevelDebug || cfg.Server.MetricsAddr != "127.0.0.1:9090" {
		t.Errorf("explicit values not applied: %+v", cfg.Server)
	}
	for _, tt := range []struct{ yaml, wantErr string }{
		{"server:\n  log_level: verbose\n", `server.log_level: must be one of debug, info, warn, error (got "verbose")`},
		{"server:\n  metrics_addr: \"9090\"\n", `server.metrics_addr: "9090" is not a host:port listen address`},
	} {
		_, err := Load(writeConfig(t, minimalYAML+tt.yaml))
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("want %q, got %v", tt.wantErr, err)
		}
	}
	for level, want := range map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError, "": slog.LevelInfo} {
		if got := (Server{LogLevel: level}).SlogLevel(); got != want {
			t.Errorf("SlogLevel(%q) = %v, want %v", level, got, want)
		}
	}
}

// The knobs load with their defaults, map onto the policy, and the Fetch
// combinations are refused with the key named.
func TestCORSKnobs(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalYAML+`server:
  cors:
    allowed_origins: ["https://app.example.org"]
    allowed_headers: ["X-Proxy-User"]
    expose_headers: ["X-Proxy-Trace"]
    allow_credentials: true
    allow_private_network: true
    max_age: 2m
`))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Server.CORS.Policy()
	if !p.AllowCredentials || !p.AllowPrivateNetwork || p.MaxAge != 2*time.Minute || len(p.AllowedHeaders) != 1 || len(p.ExposeHeaders) != 1 {
		t.Errorf("policy = %+v", p)
	}
	plain, err := Load(writeConfig(t, minimalYAML+"server:\n  cors:\n    allowed_origins: [\"https://app.example.org\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p := plain.Server.CORS.Policy(); p.AllowCredentials || p.AllowPrivateNetwork || p.MaxAge != 0 {
		t.Errorf("defaults: %+v", p)
	}
	omitted, err := Load(writeConfig(t, minimalYAML+"server:\n  cors:\n    allowed_origins: [\"https://app.example.org\"]\n    max_age: 0s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if omitted.Server.CORS.Policy().MaxAge >= 0 {
		t.Errorf("max_age 0s must omit the header (negative policy value), got %v", omitted.Server.CORS.Policy().MaxAge)
	}
	for name, doc := range map[string]string{
		"credentials with *":        "server:\n  cors:\n    allowed_origins: [\"*\"]\n    allow_credentials: true\n",
		"credentials with * expose": "server:\n  cors:\n    allowed_origins: [\"https://app.example.org\"]\n    expose_headers: [\"*\"]\n    allow_credentials: true\n",
		"negative max_age":          "server:\n  cors:\n    allowed_origins: [\"https://app.example.org\"]\n    max_age: -1s\n",
	} {
		if _, err := Load(writeConfig(t, minimalYAML+doc)); err == nil || !strings.Contains(err.Error(), "server.cors") {
			t.Errorf("%s: err = %v, want one naming server.cors", name, err)
		}
	}
}

func TestCORSOriginValidation(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalYAML+"server:\n  cors:\n    allowed_origins: [\"https://app.example.org\", \"http://localhost:5173\"]\n"))
	if err != nil || len(cfg.Server.CORS.AllowedOrigins) != 2 {
		t.Fatalf("valid origins: %v (err %v)", cfg.Server.CORS.AllowedOrigins, err)
	}
	for _, origin := range []string{"*", "https://*.example.org", "null"} {
		if _, err := Load(writeConfig(t, minimalYAML+"server:\n  cors:\n    allowed_origins: [\""+origin+"\"]\n")); err != nil {
			t.Errorf("origin %q must be accepted: %v", origin, err)
		}
	}
	for _, origin := range []string{"", "app.example.org", "https://app.example.org/", "https://app.example.org/path", "https://app.*.example.org", "ftp://x", "https://user@app.example.org"} {
		_, err := Load(writeConfig(t, minimalYAML+"server:\n  cors:\n    allowed_origins: [\""+origin+"\"]\n"))
		if err == nil || !strings.Contains(err.Error(), "server.cors.allowed_origins[0]") {
			t.Errorf("origin %q must be rejected, got %v", origin, err)
		}
	}
}

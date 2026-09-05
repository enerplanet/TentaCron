package config

import (
	"strings"
	"testing"
)

// The inbound body cap, the upstream JSON response cap and the result
// download cap are three independent knobs with their own defaults.
func TestSizeLimitsDefaultsAndValidation(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.MaxBodyBytes != 10<<20 || cfg.Upstream.MaxResponseBytes != 10<<20 || cfg.Storage.MaxResultBytes != 1<<30 {
		t.Errorf("defaults: body=%d response=%d result=%d", cfg.Server.MaxBodyBytes, cfg.Upstream.MaxResponseBytes, cfg.Storage.MaxResultBytes)
	}
	cfg, err = Load(writeConfig(t, minimalYAML+"upstream:\n  max_response_bytes: 2048\nstorage:\n  max_result_bytes: 4096\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstream.MaxResponseBytes != 2048 || cfg.Storage.MaxResultBytes != 4096 || cfg.Server.MaxBodyBytes != 10<<20 {
		t.Errorf("explicit limits not applied independently: %+v %+v", cfg.Upstream, cfg.Storage)
	}
	for _, tt := range []struct{ yaml, wantErr string }{
		{"upstream:\n  max_response_bytes: -1\n", "upstream.max_response_bytes: must be a positive integer"},
		{"storage:\n  max_result_bytes: -5\n", "storage.max_result_bytes: must be a positive integer"},
	} {
		if _, err := Load(writeConfig(t, minimalYAML+tt.yaml)); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("want %q, got %v", tt.wantErr, err)
		}
	}
}

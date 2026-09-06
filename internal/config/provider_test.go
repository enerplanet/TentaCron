package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestProviderReloadSwapsValidAndKeepsOldOnError(t *testing.T) {
	path := writeConfig(t, minimalYAML)
	p, err := NewProvider(path)
	if err != nil {
		t.Fatal(err)
	}
	first := p.Current()
	if p.Hash() == "" || len(p.Hash()) != 64 {
		t.Errorf("hash = %q", p.Hash())
	}
	// Identical bytes: nothing changed.
	res, err := p.Reload()
	if err != nil || res.Changed || len(res.RestartRequired) != 0 || res.Hash != p.Hash() {
		t.Errorf("no-op reload = %+v %v", res, err)
	}
	// A hot change (a new key) swaps; a startup-only one is reported.
	changed := strings.Replace(minimalYAML, "api_keys:", "api_keys:\n    - name: rotated\n      key: new-key\n      previous_key: old-key", 1)
	changed += "worker:\n  count: 9\n"
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = p.Reload()
	if err != nil {
		t.Fatalf("reload: %v\n%s", err, changed)
	}
	if !res.Changed || !reflect.DeepEqual(res.RestartRequired, []string{"worker.count"}) {
		t.Errorf("reload = %+v", res)
	}
	cur := p.Current()
	if cur == first || len(cur.Auth.APIKeys) != len(first.Auth.APIKeys)+1 || cur.Auth.APIKeys[0].PreviousKey != "old-key" {
		t.Errorf("current after reload = %+v", cur.Auth)
	}
	// An invalid file keeps the running configuration and reports the error.
	if err := os.WriteFile(path, []byte("server: [not a mapping"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Reload(); err == nil {
		t.Fatal("an invalid file must fail the reload")
	}
	if p.Current() != cur {
		t.Error("an invalid file must leave the running configuration in place")
	}
	// A file that parses but fails validation is refused the same way.
	if err := os.WriteFile(path, []byte(strings.Replace(minimalYAML, "api_keys:", "api_keys: []\n  ignored:", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Reload(); err == nil || p.Current() != cur {
		t.Errorf("invalid content: err = %v", err)
	}
	// A missing file too.
	_ = os.Remove(path)
	if _, err := p.Reload(); err == nil || p.Current() != cur {
		t.Errorf("missing file: err = %v", err)
	}
}

func TestStaticProviderNeverReloads(t *testing.T) {
	cfg := &Config{}
	p := Static(cfg)
	res, err := p.Reload()
	if err != nil || res.Changed || p.Current() != cfg {
		t.Errorf("static reload = %+v %v", res, err)
	}
}

func TestRestartRequiredNamesStartupOnlySettings(t *testing.T) {
	old, _ := parse([]byte(minimalYAML))
	cur, _ := parse([]byte(minimalYAML))
	cur.Server.LogLevel = "debug"
	cur.Server.CORS.AllowedOrigins = []string{"https://app.example.org"}
	cur.Auth.APIKeys = append(cur.Auth.APIKeys, APIKey{Name: "x", Key: "y"})
	cur.Cache.DefaultTTL++
	if got := restartRequired(old, cur); len(got) != 0 {
		t.Errorf("hot changes must not require a restart: %v", got)
	}
	cur.Server.Addr = ":9"
	cur.Storage.Path = "/elsewhere"
	cur.Worker.PollInterval++
	cur.Cache.CleanupInterval++
	want := []string{"server", "storage", "worker.poll_interval", "cache.cleanup_interval"}
	if got := restartRequired(old, cur); !reflect.DeepEqual(got, want) {
		t.Errorf("restart required = %v, want %v", got, want)
	}
}

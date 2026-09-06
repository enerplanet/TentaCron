package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/metrics"
	"github.com/enerplanet/tentacron/internal/upstream"
)

func TestMetricsServerOnlyWhenConfigured(t *testing.T) {
	m := metrics.New(nil)
	if srv := newMetricsServer(&config.Config{}, m); srv != nil {
		t.Fatal("no metrics listener without server.metrics_addr")
	}
	cfg := &config.Config{Server: config.Server{MetricsAddr: "127.0.0.1:0"}}
	srv := newMetricsServer(cfg, m)
	if srv == nil || srv.Addr != "127.0.0.1:0" {
		t.Fatalf("metrics server = %+v", srv)
	}
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "go_goroutines") {
		t.Errorf("/metrics: %d %s", rec.Code, rec.Body.String()[:min(200, rec.Body.Len())])
	}
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 404 {
		t.Errorf("the metrics listener must serve nothing but /metrics, got %d for /healthz", rec.Code)
	}
}

func TestBuildInfoCarriesVersionAndGo(t *testing.T) {
	b := buildInfo()
	if b.Version != version || !strings.HasPrefix(b.Go, "go") {
		t.Errorf("buildInfo = %+v", b)
	}
}

// A reload applies the response cap to the outbound client, next to the
// log level and the redaction list.
func TestReloadAppliesTheResponseCap(t *testing.T) {
	dir := t.TempDir()
	cfgText := func(cap int) string {
		return fmt.Sprintf("auth:\n  api_keys: [{name: t, key: k}]\nstorage:\n  path: %q\nupstream:\n  max_response_bytes: %d\ntargets:\n  demo:\n    url: \"https://demo.example.org/run\"\n", filepath.Join(dir, "t.db"), cap)
	}
	path := writeConfig(t, cfgText(1000))
	provider, err := config.NewProvider(path)
	if err != nil {
		t.Fatal(err)
	}
	client := upstream.New(provider.Current().Upstream.MaxResponseBytes, nil)
	if err := os.WriteFile(path, []byte(cfgText(2000)), 0o600); err != nil {
		t.Fatal(err)
	}
	reloadConfig(provider, slog.New(slog.NewTextHandler(io.Discard, nil)), new(slog.LevelVar), client)
	if client.MaxBody() != 2000 {
		t.Fatalf("cap after reload = %d, want 2000", client.MaxBody())
	}
}

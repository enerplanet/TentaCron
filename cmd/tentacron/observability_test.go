package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/metrics"
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

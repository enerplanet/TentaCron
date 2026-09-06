package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// lifecycleConfig is a complete configuration on a free port whose one
// target refuses connections, so a request stays queued behind a long
// backoff and a long-poll on it has something to wait for.
func lifecycleConfig(dir, level string) string {
	return fmt.Sprintf(`
server:
  addr: "127.0.0.1:0"
  write_timeout: 8s
  shutdown_grace: 3s
  log_level: %s
auth:
  api_keys: [{name: t, key: k}]
storage:
  path: %q
  results_dir: %q
worker:
  count: 1
  poll_interval: 100ms
  backoff_base: 30s
  backoff_max: 60s
targets:
  demo:
    url: "http://127.0.0.1:1/run"
    timeout: 1s
`, level, filepath.Join(dir, "t.db"), filepath.Join(dir, "results"))
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The process lifecycle, driven the way runServe drives it: listen on a
// free port, answer readiness, reload the configuration on request, serve
// a long-poll, and stop in the documented order within the grace window.
func TestProcessLifecycle(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, lifecycleConfig(dir, "info"))
	level := new(slog.LevelVar)
	p, err := newProcess(path, slog.New(slog.NewTextHandler(io.Discard, nil)), level)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reloads := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() { done <- p.run(ctx, reloads) }()
	<-p.started
	base := "http://" + p.bound[0]
	client := &http.Client{Timeout: 15 * time.Second}
	get := func(url, key string) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		return client.Do(req)
	}

	waitUntil(t, "readiness", func() bool {
		resp, err := get(base+"/readyz", "")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	// A reload swaps the file in and applies the log level.
	hashBefore := p.provider.Hash()
	if err := os.WriteFile(path, []byte(lifecycleConfig(dir, "debug")), 0o600); err != nil {
		t.Fatal(err)
	}
	reloads <- struct{}{}
	waitUntil(t, "the reload", func() bool { return p.provider.Hash() != hashBefore && level.Level() == slog.LevelDebug })

	// A request that stays queued: its target refuses connections and the
	// backoff is long.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, base+"/v1/requests", strings.NewReader(`{"target":"demo","payload":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "k")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var accepted struct{ ID string }
	_ = json.NewDecoder(resp.Body).Decode(&accepted)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || accepted.ID == "" {
		t.Fatalf("submit: %d %+v", resp.StatusCode, accepted)
	}
	waitDone := make(chan int, 1)
	go func() {
		resp, err := get(base+"/v1/requests/"+accepted.ID+"?wait=2s", "k")
		if err != nil {
			waitDone <- 0
			return
		}
		resp.Body.Close()
		waitDone <- resp.StatusCode
	}()
	time.Sleep(200 * time.Millisecond) // the long-poll is parked

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("the process did not stop")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("shutdown took %s, want within the grace window", elapsed)
	}
	if code := <-waitDone; code != http.StatusOK {
		t.Fatalf("the parked long-poll answered %d, want 200 with the current state", code)
	}
	if _, err := get(base+"/healthz", ""); err == nil {
		t.Fatal("the listener is still open after run returned")
	}
}

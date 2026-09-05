// Package live is the env-gated live-integration tier: it boots the real
// tentacron stack from an operator-supplied config — real upstream URLs,
// real credentials via ${ENV} interpolation — sends one real request through
// it, and follows the job to a terminal state. Nothing here runs in CI: the
// suite skips unless both environment variables are set.
//
//	TENTACRON_LIVE_CONFIG=./config.yaml \
//	TENTACRON_LIVE_REQUEST=./examples/buem-buildings.json \
//	make live
//
// The request file must be a complete POST /v1/requests body whose api_key
// matches the config's auth.api_keys. The job's full journey (states, audit
// trail, final result) is logged, so a failing run shows exactly where the
// real integration broke.
package live

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enerplanet/tentacron/internal/api"
	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/store"
	"github.com/enerplanet/tentacron/internal/upstream"
	"github.com/enerplanet/tentacron/internal/worker"
)

func TestLiveRequest(t *testing.T) {
	cfg, request := liveInputs(t)
	st, srv := bootLiveStack(t, cfg)
	id := submitLive(t, srv.URL, request)
	followLiveJob(t, st, id)
}

// liveInputs loads the operator-supplied config and request file, skipping
// the tier when the gating variables are unset.
func liveInputs(t *testing.T) (*config.Config, []byte) {
	t.Helper()
	configPath := os.Getenv("TENTACRON_LIVE_CONFIG")
	requestPath := os.Getenv("TENTACRON_LIVE_REQUEST")
	if configPath == "" || requestPath == "" {
		t.Skip("live tier disabled: set TENTACRON_LIVE_CONFIG and TENTACRON_LIVE_REQUEST to run against real upstreams")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load %s: %v", configPath, err)
	}
	request, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatalf("read %s: %v", requestPath, err)
	}
	var reqDoc struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal(request, &reqDoc); err != nil {
		t.Fatalf("request file is not a JSON object: %v", err)
	}
	if _, ok := cfg.Targets[reqDoc.Target]; !ok {
		t.Fatalf("request targets %q, which the config does not define", reqDoc.Target)
	}
	return cfg, request
}

// bootLiveStack runs the real stack in process. Only the store lives in a
// temp dir, so a live run never touches an existing deployment database.
func bootLiveStack(t *testing.T, cfg *config.Config) (*store.Store, *httptest.Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Storage.ResultsDir = t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	nudge := make(chan struct{}, 1)
	prov := config.Static(cfg)
	pool := worker.New(prov, st, upstream.New(cfg.Upstream.MaxResponseBytes, cfg.UpstreamSecrets()), logger, nudge)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		pool.Run(ctx)
	}()
	srv := httptest.NewServer(api.New(prov, st, logger, nudge).Handler())
	t.Cleanup(func() {
		srv.Close()
		cancel()
		<-done
		_ = st.Close()
	})
	return st, srv
}

// submitLive posts the request and returns the accepted job id.
func submitLive(t *testing.T, base string, request []byte) string {
	t.Helper()
	resp, err := http.Post(base+"/v1/requests", "application/json", strings.NewReader(string(request)))
	if err != nil {
		t.Fatal(err)
	}
	acceptBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	t.Logf("accept: %d %s", resp.StatusCode, acceptBody)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("request rejected with %d — fix the request file or config before looking at upstreams", resp.StatusCode)
	}
	var accepted struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(acceptBody, &accepted); err != nil || accepted.ID == "" {
		t.Fatalf("no job id in accept response: %s", acceptBody)
	}
	return accepted.ID
}

// followLiveJob logs every state change until the job is terminal and fails
// the test on a failed job.
func followLiveJob(t *testing.T, st *store.Store, id string) {
	t.Helper()
	deadline := time.Now().Add(liveDeadline(t))
	lastState := ""
	for {
		if time.Now().After(deadline) {
			t.Fatalf("job %s still %q at the live deadline — raise TENTACRON_LIVE_TIMEOUT if the upstream is just slow", id, lastState)
		}
		job, err := st.GetJob(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if job.State != lastState {
			t.Logf("state: %s", job.State)
			lastState = job.State
		}
		if job.State == store.StateCompleted || job.State == store.StateFailed {
			logOutcome(t, st, job)
			if job.State != store.StateCompleted {
				t.Fatalf("live job failed: %s: %s", job.ErrorCode, job.ErrorMessage)
			}
			return
		}
		time.Sleep(time.Second)
	}
}

func liveDeadline(t *testing.T) time.Duration {
	t.Helper()
	raw := os.Getenv("TENTACRON_LIVE_TIMEOUT")
	if raw == "" {
		return 10 * time.Minute
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("TENTACRON_LIVE_TIMEOUT %q: %v", raw, err)
	}
	return d
}

func logOutcome(t *testing.T, st *store.Store, job *store.Job) {
	t.Helper()
	events, err := st.ListEvents(context.Background(), job.ID)
	if err == nil {
		for _, e := range events {
			t.Logf("audit: %s -> %s: %s", e.FromState, e.ToState, e.Detail)
		}
	}
	switch {
	case job.ResultPath != "":
		t.Logf("result: file %s (%s)", job.ResultPath, job.ResultContentType)
	case len(job.TargetResponse) > 0:
		body := job.TargetResponse
		if len(body) > 2048 {
			body = body[:2048]
		}
		t.Logf("result: %s", body)
	}
}

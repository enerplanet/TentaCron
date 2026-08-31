package worker

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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/store"
	"github.com/enerplanet/tentacron/internal/upstream"
)

func dur(d time.Duration) config.Duration { return config.Duration(d) }

// baseConfig returns a config tuned for fast tests. Targets/resolvents are
// filled in per test.
func baseConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Server:  config.Server{MaxBodyBytes: 1 << 20},
		Storage: config.Storage{ResultsDir: t.TempDir(), Retention: dur(720 * time.Hour)},
		Worker: config.Worker{
			Count:                2,
			ResolventConcurrency: 4,
			PollInterval:         dur(15 * time.Millisecond),
			MaxAttempts:          3,
			BackoffBase:          dur(10 * time.Millisecond),
			BackoffMax:           dur(40 * time.Millisecond),
			JobTimeout:           dur(5 * time.Second),
		},
		Cache:      config.Cache{DefaultTTL: dur(time.Hour), CleanupInterval: dur(time.Hour)},
		Targets:    map[string]config.Target{},
		Resolvents: map[string]config.Resolvent{},
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// startPool runs the worker pool until test cleanup.
func startPool(t *testing.T, cfg *config.Config, st *store.Store) chan struct{} {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	nudge := make(chan struct{}, 1)
	pool := New(cfg, st, upstream.New(cfg.Server.MaxBodyBytes, nil), logger, nudge)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		pool.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return nudge
}

func createJob(t *testing.T, st *store.Store, target string, payload string, maxAttempts int) string {
	t.Helper()
	id, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := st.CreateJob(context.Background(), &store.Job{
		ID: id, Target: target, MaxAttempts: maxAttempts, Payload: []byte(payload),
	})
	if err != nil || !created {
		t.Fatalf("CreateJob: created=%v err=%v", created, err)
	}
	return id
}

func waitForTerminal(t *testing.T, st *store.Store, id string) *store.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, err := st.GetJob(context.Background(), id)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if job.State == store.StateCompleted || job.State == store.StateFailed {
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	job, _ := st.GetJob(context.Background(), id)
	events, _ := st.ListEvents(context.Background(), id)
	for _, e := range events {
		t.Logf("event: %s -> %s (%s)", e.FromState, e.ToState, e.Detail)
	}
	t.Fatalf("job %s did not reach a terminal state (state %s)", id, job.State)
	return nil
}

const seriesResponse = `{"type":"time-series","unit":"kW","values":[0.1,0.2,0.3]}`

// fakeResource serves a resource API, counting calls and optionally failing
// the first n requests with HTTP 500.
func fakeResource(t *testing.T, failFirst int64) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= failFirst {
			http.Error(w, `{"error":"flaky"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, seriesResponse)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// fakeDirectTarget records forwarded bodies and answers with respBody.
func fakeDirectTarget(t *testing.T, status int, respBody string) (*httptest.Server, func() []byte) {
	t.Helper()
	var mu sync.Mutex
	var lastBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastBody = body
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return lastBody
	}
}

func resolventCfg(url string) config.Resolvent {
	return config.Resolvent{URL: url, Method: "POST", APIKeyHeader: "X-API-Key",
		Timeout: dur(2 * time.Second), CacheTTL: dur(time.Hour)}
}

func directTargetCfg(url string) config.Target {
	return config.Target{URL: url, Method: "POST", Timeout: dur(2 * time.Second),
		TimeseriesPath: "time-series", APIKeyInject: config.InjectNone,
		Response: config.Response{Mode: config.ModeDirect}}
}

const payloadWithResolvents = `{
	"scenario": "rooftop",
	"time-series": [
		{"name": "pv", "type": "resolvent-pv1", "capacity_kw": 12.5},
		{"name": "load", "type": "time-series", "values": [1, 2]},
		{"name": "wind", "type": "resolvent-wind", "hub_height": 120}
	]
}`

func TestHappyPathDirectTarget(t *testing.T) {
	cfg := baseConfig(t)
	resource, calls := fakeResource(t, 0)
	target, lastBody := fakeDirectTarget(t, 200, `{"ok":true}`)
	cfg.Resolvents["resolvent-pv1"] = resolventCfg(resource.URL)
	cfg.Resolvents["resolvent-wind"] = resolventCfg(resource.URL)
	cfg.Targets["buem"] = directTargetCfg(target.URL)

	st := openStore(t)
	id := createJob(t, st, "buem", payloadWithResolvents, 3)
	startPool(t, cfg, st)

	job := waitForTerminal(t, st, id)
	if job.State != store.StateCompleted {
		t.Fatalf("state = %s, error = %s: %s", job.State, job.ErrorCode, job.ErrorMessage)
	}
	if job.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", job.Attempts)
	}
	if calls.Load() != 2 {
		t.Errorf("resource calls = %d, want 2 (one per distinct resolvent)", calls.Load())
	}

	var doc struct {
		Scenario string `json:"scenario"`
		Series   []struct {
			Name      string         `json:"name"`
			Type      string         `json:"type"`
			Values    []float64      `json:"values"`
			Resolvent map[string]any `json:"resolvent"`
		} `json:"time-series"`
	}
	if err := json.Unmarshal(lastBody(), &doc); err != nil {
		t.Fatalf("target received invalid JSON: %v", err)
	}
	if doc.Scenario != "rooftop" || len(doc.Series) != 3 {
		t.Fatalf("forwarded doc = %+v", doc)
	}
	pv := doc.Series[0]
	if pv.Type != "time-series" || len(pv.Values) != 3 || pv.Resolvent["type"] != "resolvent-pv1" {
		t.Errorf("pv slot not resolved correctly: %+v", pv)
	}
	if pass := doc.Series[1]; pass.Resolvent != nil || len(pass.Values) != 2 {
		t.Errorf("pass-through series was modified: %+v", pass)
	}
	if wind := doc.Series[2]; wind.Type != "time-series" || wind.Resolvent["hub_height"] != float64(120) {
		t.Errorf("wind slot not resolved correctly: %+v", wind)
	}
	if job.TargetStatus == nil || *job.TargetStatus != 200 || string(job.TargetResponse) != `{"ok":true}` {
		t.Errorf("stored result: status=%v body=%s", job.TargetStatus, job.TargetResponse)
	}
}

func TestPollModeTarget(t *testing.T) {
	cfg := baseConfig(t)
	resource, _ := fakeResource(t, 0)

	var polls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /simulate", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"job_id":"m-1"}`)
	})
	mux.HandleFunc("GET /jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "m-1" {
			http.NotFound(w, r)
			return
		}
		if polls.Add(1) < 3 {
			_, _ = io.WriteString(w, `{"status":"running"}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"done","objective":42.5}`)
	})
	targetSrv := httptest.NewServer(mux)
	t.Cleanup(targetSrv.Close)

	cfg.Resolvents["resolvent-pv1"] = resolventCfg(resource.URL)
	cfg.Resolvents["resolvent-wind"] = resolventCfg(resource.URL)
	cfg.Targets["meme"] = config.Target{
		URL: targetSrv.URL + "/simulate", Method: "POST", Timeout: dur(2 * time.Second),
		TimeseriesPath: "time-series", APIKeyInject: config.InjectNone,
		Response: config.Response{Mode: config.ModePoll, Poll: &config.Poll{
			IDJSONPath:        "job_id",
			URLTemplate:       targetSrv.URL + "/jobs/{id}",
			ResultURLTemplate: targetSrv.URL + "/jobs/{id}",
			StatusJSONPath:    "status",
			DoneValues:        []string{"done"},
			FailedValues:      []string{"failed"},
			Interval:          dur(10 * time.Millisecond),
			Timeout:           dur(5 * time.Second),
		}},
	}

	st := openStore(t)
	id := createJob(t, st, "meme", payloadWithResolvents, 3)
	startPool(t, cfg, st)

	job := waitForTerminal(t, st, id)
	if job.State != store.StateCompleted {
		t.Fatalf("state = %s, error = %s: %s", job.State, job.ErrorCode, job.ErrorMessage)
	}
	if job.TargetJobID != "m-1" {
		t.Errorf("target job id = %q", job.TargetJobID)
	}
	if polls.Load() < 3 {
		t.Errorf("polls = %d, want >= 3", polls.Load())
	}
	if !strings.Contains(string(job.TargetResponse), `"objective":42.5`) {
		t.Errorf("final result not stored: %s", job.TargetResponse)
	}
}

func TestSeriesCacheAvoidsSecondResourceCall(t *testing.T) {
	cfg := baseConfig(t)
	resource, calls := fakeResource(t, 0)
	target, _ := fakeDirectTarget(t, 200, `{"ok":true}`)
	cfg.Resolvents["resolvent-pv1"] = resolventCfg(resource.URL)
	cfg.Targets["buem"] = directTargetCfg(target.URL)

	st := openStore(t)
	payload := `{"time-series":[{"type":"resolvent-pv1","lat":48.8}]}`
	id1 := createJob(t, st, "buem", payload, 3)
	startPool(t, cfg, st)
	if job := waitForTerminal(t, st, id1); job.State != store.StateCompleted {
		t.Fatalf("job1 failed: %s %s", job.ErrorCode, job.ErrorMessage)
	}

	id2 := createJob(t, st, "buem", payload, 3)
	if job := waitForTerminal(t, st, id2); job.State != store.StateCompleted {
		t.Fatalf("job2 failed: %s %s", job.ErrorCode, job.ErrorMessage)
	}
	if calls.Load() != 1 {
		t.Errorf("resource calls = %d, want 1 (second job must hit the cache)", calls.Load())
	}
}

func TestTransientResourceErrorRetries(t *testing.T) {
	cfg := baseConfig(t)
	resource, calls := fakeResource(t, 2) // 500, 500, then success
	target, _ := fakeDirectTarget(t, 200, `{"ok":true}`)
	cfg.Resolvents["resolvent-pv1"] = resolventCfg(resource.URL)
	cfg.Targets["buem"] = directTargetCfg(target.URL)

	st := openStore(t)
	id := createJob(t, st, "buem", `{"time-series":[{"type":"resolvent-pv1"}]}`, 3)
	startPool(t, cfg, st)

	job := waitForTerminal(t, st, id)
	if job.State != store.StateCompleted {
		t.Fatalf("state = %s: %s", job.State, job.ErrorMessage)
	}
	if job.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", job.Attempts)
	}
	if calls.Load() != 3 {
		t.Errorf("resource calls = %d, want 3", calls.Load())
	}
}

func TestUnknownResolventFailsFast(t *testing.T) {
	cfg := baseConfig(t)
	resource, calls := fakeResource(t, 0)
	target, _ := fakeDirectTarget(t, 200, `{"ok":true}`)
	cfg.Resolvents["resolvent-pv1"] = resolventCfg(resource.URL)
	cfg.Targets["buem"] = directTargetCfg(target.URL)

	st := openStore(t)
	id := createJob(t, st, "buem",
		`{"time-series":[{"type":"resolvent-pv1"},{"type":"resolvent-tidal"}]}`, 3)
	startPool(t, cfg, st)

	job := waitForTerminal(t, st, id)
	if job.State != store.StateFailed || job.ErrorCode != errUnknownResolvent {
		t.Fatalf("state=%s code=%s, want failed/unknown_resolvent", job.State, job.ErrorCode)
	}
	if !strings.Contains(job.ErrorMessage, "resolvent-tidal") {
		t.Errorf("error message must name the type: %s", job.ErrorMessage)
	}
	if calls.Load() != 0 {
		t.Errorf("resource called %d times before validation, want 0", calls.Load())
	}
}

func TestTargetPermanentErrorNoRetry(t *testing.T) {
	cfg := baseConfig(t)
	target, _ := fakeDirectTarget(t, 400, `{"error":"bad model"}`)
	cfg.Targets["buem"] = directTargetCfg(target.URL)

	st := openStore(t)
	id := createJob(t, st, "buem", `{"time-series":[]}`, 3)
	startPool(t, cfg, st)

	job := waitForTerminal(t, st, id)
	if job.State != store.StateFailed || job.ErrorCode != errTargetError {
		t.Fatalf("state=%s code=%s, want failed/target_error", job.State, job.ErrorCode)
	}
	if job.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 (4xx must not retry)", job.Attempts)
	}
}

func TestMaxAttemptsExhausted(t *testing.T) {
	cfg := baseConfig(t)
	resource, _ := fakeResource(t, 1000) // always 500
	target, _ := fakeDirectTarget(t, 200, `{"ok":true}`)
	cfg.Resolvents["resolvent-pv1"] = resolventCfg(resource.URL)
	cfg.Targets["buem"] = directTargetCfg(target.URL)

	st := openStore(t)
	id := createJob(t, st, "buem", `{"time-series":[{"type":"resolvent-pv1"}]}`, 2)
	startPool(t, cfg, st)

	job := waitForTerminal(t, st, id)
	if job.State != store.StateFailed || job.ErrorCode != errMaxAttempts {
		t.Fatalf("state=%s code=%s, want failed/max_attempts_exceeded", job.State, job.ErrorCode)
	}
	if job.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", job.Attempts)
	}
}

func TestPollDeadlineExceeded(t *testing.T) {
	cfg := baseConfig(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /simulate", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"job_id":"m-slow"}`)
	})
	mux.HandleFunc("GET /jobs/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"running"}`)
	})
	targetSrv := httptest.NewServer(mux)
	t.Cleanup(targetSrv.Close)

	cfg.Targets["meme"] = config.Target{
		URL: targetSrv.URL + "/simulate", Method: "POST", Timeout: dur(2 * time.Second),
		TimeseriesPath: "time-series", APIKeyInject: config.InjectNone,
		Response: config.Response{Mode: config.ModePoll, Poll: &config.Poll{
			IDJSONPath: "job_id", URLTemplate: targetSrv.URL + "/jobs/{id}",
			ResultURLTemplate: targetSrv.URL + "/jobs/{id}",
			StatusJSONPath:    "status", DoneValues: []string{"done"}, FailedValues: []string{"failed"},
			Interval: dur(10 * time.Millisecond), Timeout: dur(60 * time.Millisecond),
		}},
	}

	st := openStore(t)
	id := createJob(t, st, "meme", `{"time-series":[]}`, 3)
	startPool(t, cfg, st)

	job := waitForTerminal(t, st, id)
	if job.State != store.StateFailed || job.ErrorCode != errTargetTimeout {
		t.Fatalf("state=%s code=%s, want failed/target_timeout", job.State, job.ErrorCode)
	}
}

func TestTargetJobFailedStatus(t *testing.T) {
	cfg := baseConfig(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /simulate", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"job_id":"m-bad"}`)
	})
	mux.HandleFunc("GET /jobs/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"failed","reason":"solver blew up"}`)
	})
	targetSrv := httptest.NewServer(mux)
	t.Cleanup(targetSrv.Close)

	cfg.Targets["meme"] = config.Target{
		URL: targetSrv.URL + "/simulate", Method: "POST", Timeout: dur(2 * time.Second),
		TimeseriesPath: "time-series", APIKeyInject: config.InjectNone,
		Response: config.Response{Mode: config.ModePoll, Poll: &config.Poll{
			IDJSONPath: "job_id", URLTemplate: targetSrv.URL + "/jobs/{id}",
			ResultURLTemplate: targetSrv.URL + "/jobs/{id}",
			StatusJSONPath:    "status", DoneValues: []string{"done"}, FailedValues: []string{"failed"},
			Interval: dur(10 * time.Millisecond), Timeout: dur(5 * time.Second),
		}},
	}

	st := openStore(t)
	id := createJob(t, st, "meme", `{"time-series":[]}`, 3)
	startPool(t, cfg, st)

	job := waitForTerminal(t, st, id)
	if job.State != store.StateFailed || job.ErrorCode != errTargetJobFailed {
		t.Fatalf("state=%s code=%s, want failed/target_job_failed", job.State, job.ErrorCode)
	}
	if !strings.Contains(job.ErrorMessage, "m-bad") {
		t.Errorf("error must name the target job: %s", job.ErrorMessage)
	}
}

func TestNonJSONResultStoredAsFile(t *testing.T) {
	cfg := baseConfig(t)
	target, _ := fakeDirectTarget(t, 200, "PK\x03\x04-binary-zip-bytes")
	cfg.Targets["buem"] = directTargetCfg(target.URL)

	st := openStore(t)
	id := createJob(t, st, "buem", `{"time-series":[]}`, 3)
	startPool(t, cfg, st)

	job := waitForTerminal(t, st, id)
	if job.State != store.StateCompleted {
		t.Fatalf("state = %s: %s", job.State, job.ErrorMessage)
	}
	if job.ResultPath == "" {
		t.Fatal("binary result must be stored as a file")
	}
	data, err := os.ReadFile(job.ResultPath)
	if err != nil || !strings.HasPrefix(string(data), "PK") {
		t.Errorf("result file: %v, %q", err, data)
	}
}

func TestRecoveryCompletesInterruptedJob(t *testing.T) {
	cfg := baseConfig(t)
	target, _ := fakeDirectTarget(t, 200, `{"ok":true}`)
	cfg.Targets["buem"] = directTargetCfg(target.URL)

	st := openStore(t)
	id := createJob(t, st, "buem", `{"time-series":[]}`, 3)
	// Simulate a crash mid-processing: claim the job so it sits in
	// "resolving", then boot the pool, whose startup recovery requeues it.
	if job, err := st.ClaimNext(context.Background(), func(string) time.Duration { return time.Minute }); err != nil || job == nil {
		t.Fatalf("pre-claim: %v, %v", job, err)
	}
	startPool(t, cfg, st)

	job := waitForTerminal(t, st, id)
	if job.State != store.StateCompleted {
		t.Fatalf("state = %s: %s", job.State, job.ErrorMessage)
	}
}

func TestShutdownParksInFlightJob(t *testing.T) {
	cfg := baseConfig(t)
	release := make(chan struct{})
	slowResource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, seriesResponse)
	}))
	t.Cleanup(func() {
		close(release)
		slowResource.Close()
	})
	target, _ := fakeDirectTarget(t, 200, `{"ok":true}`)
	cfg.Resolvents["resolvent-pv1"] = resolventCfg(slowResource.URL)
	cfg.Targets["buem"] = directTargetCfg(target.URL)

	st := openStore(t)
	id := createJob(t, st, "buem", `{"time-series":[{"type":"resolvent-pv1"}]}`, 5)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool := New(cfg, st, upstream.New(cfg.Server.MaxBodyBytes, nil), logger, make(chan struct{}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		pool.Run(ctx)
	}()

	// Wait until the job is actually being processed, then shut down.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, _ := st.GetJob(context.Background(), id)
		if job.State == store.StateResolving {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	job, err := st.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != store.StateReceived {
		t.Fatalf("state after shutdown = %s, want received (parked for retry)", job.State)
	}
}

// A resource API answering 200 "null" must fail the job with the documented
// invalid_resource_response — not poison the series cache and panic
// Substitute into an "internal" failure.
func TestNullResourceBodyFailsCleanly(t *testing.T) {
	cfg := baseConfig(t)
	nullResource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `null`)
	}))
	t.Cleanup(nullResource.Close)
	target, _ := fakeDirectTarget(t, 200, `{"ok":true}`)
	cfg.Resolvents["resolvent-pv1"] = resolventCfg(nullResource.URL)
	cfg.Targets["buem"] = directTargetCfg(target.URL)

	st := openStore(t)
	id := createJob(t, st, "buem", `{"time-series":[{"type":"resolvent-pv1"}]}`, 3)
	startPool(t, cfg, st)

	job := waitForTerminal(t, st, id)
	if job.State != store.StateFailed || job.ErrorCode != errInvalidResource {
		t.Fatalf("state=%s code=%s, want failed/invalid_resource_response", job.State, job.ErrorCode)
	}
}

// A non-object resource body maps to invalid_resource_response, keeping
// resource_error reserved for HTTP-level failures as documented.
func TestArrayResourceBodyUsesInvalidResourceCode(t *testing.T) {
	cfg := baseConfig(t)
	arrResource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[1,2,3]`)
	}))
	t.Cleanup(arrResource.Close)
	target, _ := fakeDirectTarget(t, 200, `{"ok":true}`)
	cfg.Resolvents["resolvent-pv1"] = resolventCfg(arrResource.URL)
	cfg.Targets["buem"] = directTargetCfg(target.URL)

	st := openStore(t)
	id := createJob(t, st, "buem", `{"time-series":[{"type":"resolvent-pv1"}]}`, 3)
	startPool(t, cfg, st)

	job := waitForTerminal(t, st, id)
	if job.State != store.StateFailed || job.ErrorCode != errInvalidResource {
		t.Fatalf("state=%s code=%s, want failed/invalid_resource_response", job.State, job.ErrorCode)
	}
	if job.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 (malformed body is permanent)", job.Attempts)
	}
}

// A target job that finished must complete even when its "done" status is
// only observed after the poll deadline: the deadline exists for unfinished
// jobs. The old pre-poll deadline check failed such jobs as target_timeout
// without ever asking the target.
func TestFinishedJobPastDeadlineStillCompletes(t *testing.T) {
	cfg := baseConfig(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /simulate", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"job_id":"m-edge"}`)
	})
	mux.HandleFunc("GET /jobs/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"done","objective":7}`)
	})
	targetSrv := httptest.NewServer(mux)
	t.Cleanup(targetSrv.Close)

	cfg.Targets["meme"] = config.Target{
		URL: targetSrv.URL + "/simulate", Method: "POST", Timeout: dur(2 * time.Second),
		TimeseriesPath: "time-series", APIKeyInject: config.InjectNone,
		Response: config.Response{Mode: config.ModePoll, Poll: &config.Poll{
			IDJSONPath: "job_id", URLTemplate: targetSrv.URL + "/jobs/{id}",
			ResultURLTemplate: targetSrv.URL + "/jobs/{id}",
			StatusJSONPath:    "status", DoneValues: []string{"done"}, FailedValues: []string{"failed"},
			Interval: dur(10 * time.Millisecond),
			// The deadline is over before the first poll tick can run.
			Timeout: dur(time.Millisecond),
		}},
	}

	st := openStore(t)
	id := createJob(t, st, "meme", `{"time-series":[]}`, 3)
	startPool(t, cfg, st)

	job := waitForTerminal(t, st, id)
	if job.State != store.StateCompleted {
		t.Fatalf("state=%s code=%s: %s — a finished target job must not become target_timeout",
			job.State, job.ErrorCode, job.ErrorMessage)
	}
}

func TestSweepPrunesCacheAndOldJobs(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Storage.Retention = dur(0) // everything terminal is immediately stale
	st := openStore(t)
	ctx := context.Background()

	if err := st.PutSeries(ctx, "h-old", "resolvent-pv1", []byte(`{}`), -time.Second); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(cfg.Storage.ResultsDir, "gone.zip")
	if err := os.WriteFile(resultPath, []byte("zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := createJob(t, st, "buem", `{}`, 3)
	if err := st.MarkCompleted(ctx, id, 200, nil, resultPath, "application/zip", "done"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond) // let completed_at fall behind the cutoff

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool := New(cfg, st, upstream.New(cfg.Server.MaxBodyBytes, nil), logger, make(chan struct{}))
	pool.sweep(ctx)

	if _, ok, _ := st.GetSeries(ctx, "h-old"); ok {
		t.Error("expired series survived the sweep")
	}
	if _, err := st.GetJob(ctx, id); err == nil {
		t.Error("stale terminal job survived the sweep")
	}
	if _, err := os.Stat(resultPath); !os.IsNotExist(err) {
		t.Error("result file survived the sweep")
	}
}

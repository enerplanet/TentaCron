package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/store"
	"github.com/enerplanet/tentacron/internal/upstream"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// runPool starts a pool with the given logger and returns its cancel func
// (which also waits for the pool to drain).
func runPool(t *testing.T, cfg *config.Config, st *store.Store, logger *slog.Logger) (nudge chan struct{}, stop func()) {
	t.Helper()
	nudge = make(chan struct{}, 1)
	pool := New(cfg, st, upstream.New(cfg.Server.MaxBodyBytes, nil), logger, nudge)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		pool.Run(ctx)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	t.Cleanup(stop)
	return nudge, stop
}

func pollTarget(base string, interval, timeout time.Duration) config.Target {
	return config.Target{
		URL: base + "/simulate", Method: "POST", Timeout: dur(2 * time.Second),
		TimeseriesPath: "time-series", APIKeyInject: config.InjectNone,
		Response: config.Response{Mode: config.ModePoll, Poll: &config.Poll{
			IDJSONPath: "job_id", URLTemplate: base + "/jobs/{id}",
			ResultURLTemplate: base + "/jobs/{id}",
			StatusJSONPath:    "status", DoneValues: []string{"done"}, FailedValues: []string{"failed"},
			Interval: dur(interval), Timeout: dur(timeout),
		}},
	}
}

func parkAwaiting(t *testing.T, st *store.Store, id string) {
	t.Helper()
	ctx := context.Background()
	if c, err := st.ClaimNext(ctx, func(string) time.Duration { return time.Minute }); err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	if err := st.SetResolved(ctx, id, []byte(`{}`), "resolved"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkAwaitingTarget(ctx, id, "m-1", 202, nil, time.Now().Add(-time.Second), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func TestProxyTargetWithBodyFieldInjection(t *testing.T) {
	cfg := baseConfig(t)
	target, lastBody := fakeDirectTarget(t, 200, `{"ok":true}`)
	cfg.Targets["proxy"] = config.Target{
		URL: target.URL, Method: "POST", Timeout: dur(2 * time.Second), Proxy: true,
		APIKey: "proxy-secret", APIKeyInject: config.InjectBodyField, APIKeyField: "api_key",
		Response: config.Response{Mode: config.ModeDirect},
	}
	st := openStore(t)
	id := createJob(t, st, "proxy", `{"zeta":1,"time-series":[{"type":"resolvent-pv1"}]}`, 3)
	startPool(t, cfg, st)
	if job := waitForTerminal(t, st, id); job.State != store.StateCompleted {
		t.Fatalf("state = %s: %s", job.State, job.ErrorMessage)
	}
	var fwd map[string]any
	if err := json.Unmarshal(lastBody(), &fwd); err != nil {
		t.Fatal(err)
	}
	slot := fwd["time-series"].([]any)[0].(map[string]any)
	if fwd["api_key"] != "proxy-secret" || fwd["zeta"] != float64(1) || slot["type"] != "resolvent-pv1" {
		t.Errorf("forwarded = %v", fwd)
	}
}

func TestProxyTargetMissingURLFieldFailsPermanently(t *testing.T) {
	cfg := baseConfig(t)
	var calls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(target.Close)
	cfg.Targets["ignis"] = config.Target{
		URL: target.URL + "/calc/{code}", Method: "POST", Timeout: dur(2 * time.Second), Proxy: true,
		APIKeyInject: config.InjectNone, Response: config.Response{Mode: config.ModeDirect},
	}
	st := openStore(t)
	id := createJob(t, st, "ignis", `{"A_ref":1}`, 3)
	startPool(t, cfg, st)
	job := waitForTerminal(t, st, id)
	if job.State != store.StateFailed || job.ErrorCode != errTargetError || job.Attempts != 1 {
		t.Fatalf("state=%s code=%s attempts=%d, want failed/target_error on the first attempt", job.State, job.ErrorCode, job.Attempts)
	}
	if !strings.Contains(job.ErrorMessage, "placeholder") || calls.Load() != 0 {
		t.Errorf("message=%q calls=%d", job.ErrorMessage, calls.Load())
	}
}

func TestTargetBackedResolventAuthoringErrors(t *testing.T) {
	newStack := func(t *testing.T, resolvent config.Resolvent) (*store.Store, *atomic.Int64, *config.Config) {
		cfg := baseConfig(t)
		var calls atomic.Int64
		nested := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			_, _ = io.WriteString(w, `{"series":{}}`)
		}))
		t.Cleanup(nested.Close)
		outer, _ := fakeDirectTarget(t, 200, `{"ok":true}`)
		cfg.Targets["nested"] = directTargetCfg(nested.URL)
		cfg.Targets["outer"] = directTargetCfg(outer.URL)
		cfg.Resolvents["resolvent-buem"] = resolvent
		return openStore(t), &calls, cfg
	}
	t.Run("payload_field must hold an object", func(t *testing.T) {
		st, calls, cfg := newStack(t, config.Resolvent{Target: "nested", PayloadField: "payload", CacheTTL: dur(time.Hour)})
		id := createJob(t, st, "outer", `{"time-series":[{"type":"resolvent-buem","payload":"oops"}]}`, 3)
		startPool(t, cfg, st)
		job := waitForTerminal(t, st, id)
		if job.State != store.StateFailed || job.ErrorCode != errResourceError || job.Attempts != 1 {
			t.Fatalf("state=%s code=%s attempts=%d", job.State, job.ErrorCode, job.Attempts)
		}
		if !strings.Contains(job.ErrorMessage, "must hold the payload object") || calls.Load() != 0 {
			t.Errorf("message=%q calls=%d", job.ErrorMessage, calls.Load())
		}
	})
	t.Run("backing target missing at runtime", func(t *testing.T) {
		st, calls, cfg := newStack(t, config.Resolvent{Target: "ghost", CacheTTL: dur(time.Hour)})
		id := createJob(t, st, "outer", `{"time-series":[{"type":"resolvent-buem","payload":{}}]}`, 3)
		startPool(t, cfg, st)
		job := waitForTerminal(t, st, id)
		if job.State != store.StateFailed || job.ErrorCode != errResourceError {
			t.Fatalf("state=%s code=%s", job.State, job.ErrorCode)
		}
		if !strings.Contains(job.ErrorMessage, "no longer configured") || calls.Load() != 0 {
			t.Errorf("message=%q calls=%d", job.ErrorMessage, calls.Load())
		}
	})
}

func TestTargetBackedResolventNestedTargetErrors(t *testing.T) {
	run := func(t *testing.T, status int, maxAttempts int) *store.Job {
		cfg := baseConfig(t)
		nested, _ := fakeDirectTarget(t, status, `{"error":"nested"}`)
		outer, _ := fakeDirectTarget(t, 200, `{"ok":true}`)
		cfg.Targets["nested"] = directTargetCfg(nested.URL)
		cfg.Targets["outer"] = directTargetCfg(outer.URL)
		cfg.Resolvents["resolvent-buem"] = config.Resolvent{Target: "nested", PayloadField: "payload", CacheTTL: dur(time.Hour)}
		st := openStore(t)
		id := createJob(t, st, "outer", `{"time-series":[{"type":"resolvent-buem","payload":{"id":"b"}}]}`, maxAttempts)
		startPool(t, cfg, st)
		return waitForTerminal(t, st, id)
	}
	t.Run("4xx is permanent", func(t *testing.T) {
		job := run(t, 400, 3)
		if job.State != store.StateFailed || job.ErrorCode != errResourceError || job.Attempts != 1 {
			t.Errorf("state=%s code=%s attempts=%d", job.State, job.ErrorCode, job.Attempts)
		}
	})
	t.Run("5xx retries until exhausted", func(t *testing.T) {
		job := run(t, 503, 2)
		if job.State != store.StateFailed || job.ErrorCode != errMaxAttempts || job.Attempts != 2 {
			t.Errorf("state=%s code=%s attempts=%d", job.State, job.ErrorCode, job.Attempts)
		}
	})
}

func TestTargetBackedResolventInheritsTargetAuthAndTemplating(t *testing.T) {
	cfg := baseConfig(t)
	var mu sync.Mutex
	var gotPath string
	var gotBody map[string]any
	nested := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"series":{"unit":"kW","heating":[1]}}`)
	}))
	t.Cleanup(nested.Close)
	outer, lastBody := fakeDirectTarget(t, 200, `{"ok":true}`)
	cfg.Targets["nested"] = config.Target{
		URL: nested.URL + "/b/{id}", Method: "POST", Timeout: dur(2 * time.Second),
		APIKey: "nested-key", APIKeyInject: config.InjectBodyField, APIKeyField: "api_key",
		Response: config.Response{Mode: config.ModeDirect},
	}
	cfg.Targets["outer"] = directTargetCfg(outer.URL)
	cfg.Resolvents["resolvent-buem"] = config.Resolvent{Target: "nested", PayloadField: "payload", ResponsePath: "series", CacheTTL: dur(time.Hour)}

	st := openStore(t)
	id := createJob(t, st, "outer", `{"time-series":[{"type":"resolvent-buem","payload":{"id":"b-7","x":1}}]}`, 3)
	startPool(t, cfg, st)
	if job := waitForTerminal(t, st, id); job.State != store.StateCompleted {
		t.Fatalf("state = %s: %s", job.State, job.ErrorMessage)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/b/b-7" || gotBody["api_key"] != "nested-key" || gotBody["x"] != float64(1) {
		t.Errorf("nested call: path=%s body=%v", gotPath, gotBody)
	}
	if _, present := gotBody["id"]; present {
		t.Errorf("templated field must be stripped from the nested body: %v", gotBody)
	}
	var fwd map[string]any
	if err := json.Unmarshal(lastBody(), &fwd); err != nil {
		t.Fatal(err)
	}
	slot := fwd["time-series"].([]any)[0].(map[string]any)
	if slot["unit"] != "kW" || slot["resolvent"] == nil {
		t.Errorf("outer slot = %v", slot)
	}
}

func TestResponsePathMissingFailsAsInvalidResource(t *testing.T) {
	cfg := baseConfig(t)
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"a":{}}`)
	}))
	t.Cleanup(resource.Close)
	target, _ := fakeDirectTarget(t, 200, `{"ok":true}`)
	r := resolventCfg(resource.URL)
	r.ResponsePath = "a.b"
	cfg.Resolvents["resolvent-pv1"] = r
	cfg.Targets["demo"] = directTargetCfg(target.URL)
	st := openStore(t)
	id := createJob(t, st, "demo", `{"time-series":[{"type":"resolvent-pv1"}]}`, 3)
	startPool(t, cfg, st)
	job := waitForTerminal(t, st, id)
	if job.State != store.StateFailed || job.ErrorCode != errInvalidResource || job.Attempts != 1 {
		t.Fatalf("state=%s code=%s attempts=%d", job.State, job.ErrorCode, job.Attempts)
	}
	if !strings.Contains(job.ErrorMessage, `"a.b"`) {
		t.Errorf("message must name the path: %s", job.ErrorMessage)
	}
}

func TestSeriesTypeWarningIsLoggedNotFatal(t *testing.T) {
	cfg := baseConfig(t)
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"unit":"kW","values":[1]}`)
	}))
	t.Cleanup(resource.Close)
	target, _ := fakeDirectTarget(t, 200, `{"ok":true}`)
	cfg.Resolvents["resolvent-pv1"] = resolventCfg(resource.URL)
	cfg.Targets["demo"] = directTargetCfg(target.URL)
	st := openStore(t)
	id := createJob(t, st, "demo", `{"time-series":[{"type":"resolvent-pv1"}]}`, 3)
	var logs syncBuffer
	_, stop := runPool(t, cfg, st, slog.New(slog.NewTextHandler(&logs, nil)))
	if job := waitForTerminal(t, st, id); job.State != store.StateCompleted {
		t.Fatalf("state = %s: %s", job.State, job.ErrorMessage)
	}
	stop()
	if !strings.Contains(logs.String(), "substitution warning") {
		t.Errorf("expected a substitution warning in the logs:\n%s", logs.String())
	}
}

func TestJobTimeoutRequeuesThenExhausts(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Worker.JobTimeout = dur(40 * time.Millisecond)
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Drain the body: only then does the server watch the connection
		// and cancel the request context when the client gives up.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		hang.Close()
	})
	target, _ := fakeDirectTarget(t, 200, `{"ok":true}`)
	cfg.Resolvents["resolvent-pv1"] = resolventCfg(hang.URL)
	cfg.Targets["demo"] = directTargetCfg(target.URL)
	st := openStore(t)
	id := createJob(t, st, "demo", `{"time-series":[{"type":"resolvent-pv1"}]}`, 2)
	startPool(t, cfg, st)
	job := waitForTerminal(t, st, id)
	if job.State != store.StateFailed || job.ErrorCode != errMaxAttempts || job.Attempts != 2 {
		t.Fatalf("state=%s code=%s attempts=%d", job.State, job.ErrorCode, job.Attempts)
	}
	if !strings.Contains(job.ErrorMessage, "deadline exceeded") {
		t.Errorf("message must show the timeout cause: %s", job.ErrorMessage)
	}
	events, _ := st.ListEvents(context.Background(), id)
	var requeued bool
	for _, e := range events {
		if strings.Contains(e.Detail, "retrying in") {
			requeued = true
		}
	}
	if !requeued {
		t.Error("a job timeout must requeue with backoff, not fail on the spot")
	}
}

// The audit trail's "retrying in" values must follow base*2^(n-1), capped at
// backoff_max, with ±20% jitter.
func TestBackoffScheduleRespectsBounds(t *testing.T) {
	cfg := baseConfig(t) // base 10ms, max 40ms
	resource, _ := fakeResource(t, 1<<30)
	target, _ := fakeDirectTarget(t, 200, `{"ok":true}`)
	cfg.Resolvents["resolvent-pv1"] = resolventCfg(resource.URL)
	cfg.Targets["demo"] = directTargetCfg(target.URL)
	st := openStore(t)
	id := createJob(t, st, "demo", `{"time-series":[{"type":"resolvent-pv1"}]}`, 4)
	startPool(t, cfg, st)
	if job := waitForTerminal(t, st, id); job.State != store.StateFailed || job.Attempts != 4 {
		t.Fatalf("state=%s attempts=%d", job.State, job.Attempts)
	}
	re := regexp.MustCompile(`(?s)attempt (\d)/4 failed .* retrying in (\S+)$`)
	bounds := map[string][2]time.Duration{
		"1": {8 * time.Millisecond, 12 * time.Millisecond},
		"2": {16 * time.Millisecond, 24 * time.Millisecond},
		"3": {32 * time.Millisecond, 48 * time.Millisecond},
	}
	events, _ := st.ListEvents(context.Background(), id)
	seen := 0
	for _, e := range events {
		m := re.FindStringSubmatch(e.Detail)
		if m == nil {
			continue
		}
		seen++
		d, err := time.ParseDuration(m[2])
		if err != nil {
			t.Fatalf("unparseable backoff in %q: %v", e.Detail, err)
		}
		b := bounds[m[1]]
		if d < b[0]-time.Millisecond || d > b[1]+time.Millisecond {
			t.Errorf("attempt %s backoff %v outside [%v, %v]", m[1], d, b[0], b[1])
		}
	}
	if seen != 3 {
		t.Errorf("found %d requeue events, want 3", seen)
	}
}

// Shifting the base by a large attempt count overflows; the cap must still
// bound the schedule instead of producing a negative or enormous backoff.
func TestRetryOrFailCapsOverflowedBackoff(t *testing.T) {
	cfg := baseConfig(t)
	st := openStore(t)
	id := createJob(t, st, "demo", `{}`, 200)
	ctx := context.Background()
	job, err := st.ClaimNext(ctx, func(string) time.Duration { return time.Minute })
	if err != nil || job == nil {
		t.Fatal("claim failed")
	}
	job.Attempts = 70
	p := New(cfg, st, upstream.New(1<<20, nil), discardLogger(), nil)
	before := time.Now()
	p.retryOrFail(ctx, job, errResourceError, &upstream.Error{Op: "resource x", Transient: true})
	got, _ := st.GetJob(ctx, id)
	if got.State != store.StateReceived || got.NextAttemptAt == nil {
		t.Fatalf("job not requeued: state=%s next=%v", got.State, got.NextAttemptAt)
	}
	maxBackoff := time.Duration(float64(cfg.Worker.BackoffMax.Std()) * 1.2)
	if delay := got.NextAttemptAt.Sub(before); delay < 0 || delay > maxBackoff+50*time.Millisecond {
		t.Errorf("overflowed backoff scheduled %v ahead, want within the %v cap", delay, cfg.Worker.BackoffMax.Std())
	}
}

func TestLargeJSONResultStoredAsJSONFile(t *testing.T) {
	cfg := baseConfig(t)
	body := `{"data":"` + strings.Repeat("x", 300<<10) + `"}`
	target, _ := fakeDirectTarget(t, 200, body)
	cfg.Targets["demo"] = directTargetCfg(target.URL)
	st := openStore(t)
	id := createJob(t, st, "demo", `{}`, 3)
	startPool(t, cfg, st)
	job := waitForTerminal(t, st, id)
	if job.State != store.StateCompleted {
		t.Fatalf("state = %s: %s", job.State, job.ErrorMessage)
	}
	if !strings.HasSuffix(job.ResultPath, ".json") || job.ResultContentType != "application/json" || len(job.TargetResponse) != 0 {
		t.Errorf("large JSON must land in a .json file: path=%s ct=%s inline=%d", job.ResultPath, job.ResultContentType, len(job.TargetResponse))
	}
	if data, err := os.ReadFile(job.ResultPath); err != nil || string(data) != body {
		t.Errorf("result file content mismatch (err %v)", err)
	}
}

func TestResultExt(t *testing.T) {
	cases := map[string]string{
		"application/zip": ".zip", "application/x-zip-compressed": ".zip",
		"application/json": ".json", "application/json; charset=utf-8": ".json",
		"text/plain": ".bin", "": ".bin",
	}
	for ct, want := range cases {
		if got := resultExt(ct); got != want {
			t.Errorf("resultExt(%q) = %q, want %q", ct, got, want)
		}
	}
}

func TestResultFileWriteFailureFailsInternal(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Storage.ResultsDir = filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(cfg.Storage.ResultsDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	target, _ := fakeDirectTarget(t, 200, "PK\x03\x04binary")
	cfg.Targets["demo"] = directTargetCfg(target.URL)
	st := openStore(t)
	id := createJob(t, st, "demo", `{}`, 3)
	startPool(t, cfg, st)
	job := waitForTerminal(t, st, id)
	if job.State != store.StateFailed || job.ErrorCode != errInternal || !strings.Contains(job.ErrorMessage, "results dir") {
		t.Fatalf("state=%s code=%s message=%s", job.State, job.ErrorCode, job.ErrorMessage)
	}
}

// A late failure from an overlapping worker never overwrites a completion a
// client may already have seen.
func TestLateFailureNeverOverwritesCompletion(t *testing.T) {
	cfg := baseConfig(t)
	st := openStore(t)
	id := createJob(t, st, "demo", `{}`, 3)
	ctx := context.Background()
	job, _ := st.GetJob(ctx, id)
	p := New(cfg, st, upstream.New(1<<20, nil), discardLogger(), nil)
	p.complete(ctx, job, 200, "", []byte(`{"first":true}`), "first")
	p.failJob(ctx, job, errTargetError, "late failure")
	got, _ := st.GetJob(ctx, id)
	if got.State != store.StateCompleted || got.ErrorCode != "" || string(got.TargetResponse) != `{"first":true}` {
		t.Errorf("late failure overwrote the completion: %+v", got)
	}
	if events, _ := st.ListEvents(ctx, id); len(events) != 2 {
		t.Errorf("audit trail must not record the dropped outcome, got %d events", len(events))
	}
}

func TestSweepRescuesStuckJob(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Worker.JobTimeout = dur(time.Millisecond)
	st := openStore(t)
	id := createJob(t, st, "demo", `{}`, 3)
	ctx := context.Background()
	if c, _ := st.ClaimNext(ctx, func(string) time.Duration { return time.Minute }); c == nil {
		t.Fatal("claim failed")
	}
	time.Sleep(5 * time.Millisecond)
	p := New(cfg, st, upstream.New(1<<20, nil), discardLogger(), nil)
	p.sweep(ctx)
	got, _ := st.GetJob(ctx, id)
	if got.State != store.StateReceived {
		t.Fatalf("stuck job not rescued: %s", got.State)
	}
	events, _ := st.ListEvents(ctx, id)
	if last := events[len(events)-1]; last.Detail != "rescued stuck job" {
		t.Errorf("last event = %q", last.Detail)
	}
}

func TestSweepToleratesMissingAndUndeletableResultFiles(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Storage.Retention = dur(0)
	st := openStore(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "occupied")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "child"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := createJob(t, st, "demo", `{}`, 3)
	occupied := createJob(t, st, "demo", `{}`, 3)
	if err := st.MarkCompleted(ctx, missing, 200, nil, filepath.Join(t.TempDir(), "gone.zip"), "application/zip", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkCompleted(ctx, occupied, 200, nil, dir, "application/zip", ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	p := New(cfg, st, upstream.New(1<<20, nil), discardLogger(), nil)
	p.sweep(ctx)
	for _, id := range []string{missing, occupied} {
		if _, err := st.GetJob(ctx, id); err == nil {
			t.Errorf("job %s survived retention", id)
		}
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("an undeletable path must be left alone: %v", err)
	}
}

func TestPollTargetNoLongerConfigured(t *testing.T) {
	direct, _ := fakeDirectTarget(t, 200, `{"ok":true}`)
	for name, targets := range map[string]map[string]config.Target{
		"target removed":            {},
		"target switched to direct": {"meme": directTargetCfg(direct.URL)},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := baseConfig(t)
			cfg.Targets = targets
			st := openStore(t)
			id := createJob(t, st, "meme", `{}`, 3)
			parkAwaiting(t, st, id)
			startPool(t, cfg, st)
			job := waitForTerminal(t, st, id)
			if job.State != store.StateFailed || job.ErrorCode != errUnknownTarget ||
				!strings.Contains(job.ErrorMessage, "no longer configured for polling") {
				t.Errorf("state=%s code=%s message=%s", job.State, job.ErrorCode, job.ErrorMessage)
			}
		})
	}
}

func TestStatusEndpointUnreachablePastDeadline(t *testing.T) {
	cfg := baseConfig(t)
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /simulate", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"job_id":"m-1"}`)
	})
	accept := httptest.NewServer(mux)
	t.Cleanup(accept.Close)
	tcfg := pollTarget(accept.URL, 10*time.Millisecond, 40*time.Millisecond)
	tcfg.Response.Poll.URLTemplate = deadURL + "/jobs/{id}"
	cfg.Targets["meme"] = tcfg
	st := openStore(t)
	id := createJob(t, st, "meme", `{}`, 3)
	startPool(t, cfg, st)
	job := waitForTerminal(t, st, id)
	if job.State != store.StateFailed || job.ErrorCode != errTargetTimeout ||
		!strings.Contains(job.ErrorMessage, "status endpoint unreachable") {
		t.Errorf("state=%s code=%s message=%s", job.State, job.ErrorCode, job.ErrorMessage)
	}
}

func TestResultFetchFailurePastDeadlineIsTargetError(t *testing.T) {
	cfg := baseConfig(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /simulate", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"job_id":"m-1"}`)
	})
	mux.HandleFunc("GET /jobs/{id}/status", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"done"}`)
	})
	mux.HandleFunc("GET /jobs/{id}", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"storage down"}`, http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	tcfg := pollTarget(srv.URL, 10*time.Millisecond, 50*time.Millisecond)
	tcfg.Response.Poll.URLTemplate = srv.URL + "/jobs/{id}/status"
	cfg.Targets["meme"] = tcfg
	st := openStore(t)
	id := createJob(t, st, "meme", `{}`, 3)
	startPool(t, cfg, st)
	job := waitForTerminal(t, st, id)
	if job.State != store.StateFailed || job.ErrorCode != errTargetError || !strings.Contains(job.ErrorMessage, "result meme") {
		t.Errorf("state=%s code=%s message=%s (a finished target job with an unfetchable result is a target_error, not a timeout)",
			job.State, job.ErrorCode, job.ErrorMessage)
	}
}

func TestPollModeResumesAfterRestartWithoutResubmitting(t *testing.T) {
	cfg := baseConfig(t)
	var accepts atomic.Int64
	var finished atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /simulate", func(w http.ResponseWriter, _ *http.Request) {
		accepts.Add(1)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"job_id":"m-restart"}`)
	})
	mux.HandleFunc("GET /jobs/{id}", func(w http.ResponseWriter, _ *http.Request) {
		if finished.Load() {
			_, _ = io.WriteString(w, `{"status":"done","v":1}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"running"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cfg.Targets["meme"] = pollTarget(srv.URL, 10*time.Millisecond, 5*time.Second)
	st := openStore(t)
	id := createJob(t, st, "meme", `{}`, 3)

	_, stop := runPool(t, cfg, st, discardLogger())
	deadline := time.Now().Add(3 * time.Second)
	for {
		job, _ := st.GetJob(context.Background(), id)
		if job.State == store.StateAwaitingTarget {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never reached awaiting_target (state %s)", job.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	if job, _ := st.GetJob(context.Background(), id); job.State != store.StateAwaitingTarget {
		t.Fatalf("shutdown must leave a polling job in awaiting_target, got %s", job.State)
	}

	finished.Store(true)
	startPool(t, cfg, st)
	job := waitForTerminal(t, st, id)
	if job.State != store.StateCompleted || job.TargetJobID != "m-restart" {
		t.Fatalf("state=%s target_job=%s: %s", job.State, job.TargetJobID, job.ErrorMessage)
	}
	if accepts.Load() != 1 {
		t.Errorf("target job submitted %d times across the restart, want 1", accepts.Load())
	}
}

func TestNudgeWakesIdleWorker(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Worker.PollInterval = dur(30 * time.Second)
	target, _ := fakeDirectTarget(t, 200, `{"ok":true}`)
	cfg.Targets["demo"] = directTargetCfg(target.URL)
	st := openStore(t)
	nudge := startPool(t, cfg, st)
	time.Sleep(20 * time.Millisecond) // let the initial drain find an empty queue
	id := createJob(t, st, "demo", `{}`, 3)
	select {
	case nudge <- struct{}{}:
	default:
	}
	if job := waitForTerminal(t, st, id); job.State != store.StateCompleted {
		t.Fatalf("state = %s", job.State)
	}
}

func TestWorkersProcessJobsInParallel(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Worker.Count = 2
	var inflight atomic.Int64
	both := make(chan struct{})
	var once sync.Once
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if inflight.Add(1) >= 2 {
			once.Do(func() { close(both) })
		}
		select {
		case <-both:
		case <-time.After(2 * time.Second):
		}
		inflight.Add(-1)
		_, _ = io.WriteString(w, seriesResponse)
	}))
	t.Cleanup(resource.Close)
	target, _ := fakeDirectTarget(t, 200, `{"ok":true}`)
	cfg.Resolvents["resolvent-pv1"] = resolventCfg(resource.URL)
	cfg.Targets["demo"] = directTargetCfg(target.URL)
	st := openStore(t)
	a := createJob(t, st, "demo", `{"time-series":[{"type":"resolvent-pv1","site":"a"}]}`, 3)
	b := createJob(t, st, "demo", `{"time-series":[{"type":"resolvent-pv1","site":"b"}]}`, 3)
	startPool(t, cfg, st)
	waitForTerminal(t, st, a)
	waitForTerminal(t, st, b)
	select {
	case <-both:
	default:
		t.Error("two workers never had a job in flight at the same time")
	}
}

func TestResolventConcurrencyIsBounded(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Worker.ResolventConcurrency = 2
	var inflight, peak, calls atomic.Int64
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		cur := inflight.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		inflight.Add(-1)
		_, _ = io.WriteString(w, seriesResponse)
	}))
	t.Cleanup(resource.Close)
	target, _ := fakeDirectTarget(t, 200, `{"ok":true}`)
	cfg.Resolvents["resolvent-pv1"] = resolventCfg(resource.URL)
	cfg.Targets["demo"] = directTargetCfg(target.URL)
	var sb strings.Builder
	sb.WriteString(`{"time-series":[`)
	for i := 0; i < 6; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"type":"resolvent-pv1","site":%d}`, i)
	}
	sb.WriteString(`]}`)
	st := openStore(t)
	id := createJob(t, st, "demo", sb.String(), 3)
	startPool(t, cfg, st)
	if job := waitForTerminal(t, st, id); job.State != store.StateCompleted {
		t.Fatalf("state = %s: %s", job.State, job.ErrorMessage)
	}
	if peak.Load() > 2 || calls.Load() != 6 {
		t.Errorf("peak in-flight %d (bound 2), calls %d (want 6)", peak.Load(), calls.Load())
	}
}

func TestManyResolventsAreBoundedAndAllSubstituted(t *testing.T) {
	const n = 3000
	cfg := baseConfig(t)
	cfg.Worker.ResolventConcurrency = 8
	resource, calls := fakeResource(t, 0)
	target, lastBody := fakeDirectTarget(t, 200, `{"ok":true}`)
	cfg.Resolvents["resolvent-pv1"] = resolventCfg(resource.URL)
	cfg.Targets["demo"] = directTargetCfg(target.URL)
	var sb strings.Builder
	sb.WriteString(`{"time-series":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"type":"resolvent-pv1","site":%d}`, i)
	}
	sb.WriteString(`]}`)
	st := openStore(t)
	id := createJob(t, st, "demo", sb.String(), 3)
	baseline := runtime.NumGoroutine()
	startPool(t, cfg, st)
	peak := 0
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if g := runtime.NumGoroutine(); g > peak {
			peak = g
		}
		job, _ := st.GetJob(context.Background(), id)
		if job.State == store.StateCompleted || job.State == store.StateFailed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	job := waitForTerminal(t, st, id)
	if job.State != store.StateCompleted {
		t.Fatalf("state = %s: %s", job.State, job.ErrorMessage)
	}
	if calls.Load() != n {
		t.Errorf("resource calls = %d, want %d", calls.Load(), n)
	}
	if got := strings.Count(string(lastBody()), `"resolvent"`); got != n {
		t.Errorf("%d substituted slots, want %d", got, n)
	}
	if peak-baseline > 200 {
		t.Errorf("goroutines peaked at %d over baseline for %d resolvents; fan-out is not bounded", peak-baseline, n)
	}
}

func TestFirstFailureStopsFeedingRemainingResolvents(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Worker.ResolventConcurrency = 1
	var calls atomic.Int64
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, `{"error":"bad params"}`, http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, seriesResponse)
	}))
	t.Cleanup(resource.Close)
	target, _ := fakeDirectTarget(t, 200, `{"ok":true}`)
	cfg.Resolvents["resolvent-pv1"] = resolventCfg(resource.URL)
	cfg.Targets["demo"] = directTargetCfg(target.URL)
	st := openStore(t)
	id := createJob(t, st, "demo", `{"time-series":[
		{"type":"resolvent-pv1","site":1},{"type":"resolvent-pv1","site":2},{"type":"resolvent-pv1","site":3},
		{"type":"resolvent-pv1","site":4},{"type":"resolvent-pv1","site":5}]}`, 3)
	startPool(t, cfg, st)
	job := waitForTerminal(t, st, id)
	if job.State != store.StateFailed || job.ErrorCode != errResourceError || job.Attempts != 1 {
		t.Fatalf("state=%s code=%s attempts=%d", job.State, job.ErrorCode, job.Attempts)
	}
	if !strings.Contains(job.ErrorMessage, "HTTP 400") || strings.Contains(job.ErrorMessage, "skipped") {
		t.Errorf("the genuine failure must be reported, not a skip: %s", job.ErrorMessage)
	}
	if calls.Load() > 5 {
		t.Errorf("resource called %d times for 5 resolvents", calls.Load())
	}
}

func TestStoredNonObjectPayloadFailsInvalidPayload(t *testing.T) {
	cfg := baseConfig(t)
	target, _ := fakeDirectTarget(t, 200, `{"ok":true}`)
	cfg.Targets["demo"] = directTargetCfg(target.URL)
	st := openStore(t)
	id := createJob(t, st, "demo", `[1,2]`, 3)
	startPool(t, cfg, st)
	job := waitForTerminal(t, st, id)
	if job.State != store.StateFailed || job.ErrorCode != errInvalidPayload {
		t.Fatalf("state=%s code=%s", job.State, job.ErrorCode)
	}
}

func TestAcceptWith200AndNumericJobID(t *testing.T) {
	cfg := baseConfig(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /simulate", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"job_id":77}`)
	})
	mux.HandleFunc("GET /jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "77" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"status":"done"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cfg.Targets["meme"] = pollTarget(srv.URL, 10*time.Millisecond, 5*time.Second)
	st := openStore(t)
	id := createJob(t, st, "meme", `{}`, 3)
	startPool(t, cfg, st)
	job := waitForTerminal(t, st, id)
	if job.State != store.StateCompleted || job.TargetJobID != "77" {
		t.Fatalf("state=%s target_job=%q: %s", job.State, job.TargetJobID, job.ErrorMessage)
	}
}

func TestUnknownTargetAtProcessingTime(t *testing.T) {
	cfg := baseConfig(t)
	st := openStore(t)
	id := createJob(t, st, "gone", `{}`, 3)
	startPool(t, cfg, st)
	job := waitForTerminal(t, st, id)
	if job.State != store.StateFailed || job.ErrorCode != errUnknownTarget {
		t.Fatalf("state=%s code=%s", job.State, job.ErrorCode)
	}
}

// A retention backlog larger than one batch is drained in one sweep, batch
// by batch, with every result file removed before its row.
func TestSweepPrunesBacklogInBatches(t *testing.T) {
	prev := pruneBatch
	pruneBatch = 3
	t.Cleanup(func() { pruneBatch = prev })
	cfg := baseConfig(t)
	cfg.Storage.Retention = dur(0)
	st := openStore(t)
	ctx := context.Background()
	var ids, files []string
	for i := 0; i < 7; i++ {
		id := createJob(t, st, "demo", `{}`, 3)
		path := filepath.Join(t.TempDir(), id+".zip")
		if err := os.WriteFile(path, []byte("PK"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkCompleted(ctx, id, 200, nil, path, "application/zip", ""); err != nil {
			t.Fatal(err)
		}
		ids, files = append(ids, id), append(files, path)
	}
	time.Sleep(5 * time.Millisecond) // completed_at strictly before the cutoff
	var logs syncBuffer
	p := New(cfg, st, upstream.New(1<<20, nil), slog.New(slog.NewTextHandler(&logs, nil)), nil)
	p.sweep(ctx)
	for _, id := range ids {
		if _, err := st.GetJob(ctx, id); err == nil {
			t.Errorf("job %s survived a multi-batch prune", id)
		}
	}
	for _, f := range files {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("result file %s survived the prune", f)
		}
	}
	if !strings.Contains(logs.String(), "jobs=7") || !strings.Contains(logs.String(), "result_files=7") {
		t.Errorf("sweep summary must report totals across batches:\n%s", logs.String())
	}
}

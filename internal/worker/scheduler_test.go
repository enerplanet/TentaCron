package worker

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/store"
	"github.com/enerplanet/tentacron/internal/upstream"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// startPoolWithClock runs a pool on an injected clock; several may share one
// store to model a restart racing the old process.
func startPoolWithClock(t *testing.T, cfg *config.Config, st *store.Store, clk Clock) {
	t.Helper()
	pool := New(cfg, st, upstream.New(cfg.Upstream.MaxResponseBytes, nil), slog.New(slog.NewTextHandler(io.Discard, nil)), make(chan struct{}, 1)).WithClock(clk)
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
}

func runsOf(t *testing.T, st *store.Store, id string) []*store.Job {
	t.Helper()
	jobs, err := st.ListJobs(context.Background(), store.ListFilter{IdempotencyPrefix: store.RunKeyPrefix(id), Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A due schedule becomes exactly one job per due time — even with two
// schedulers on the same store (a restart racing the old process) — carrying
// the schedule's client, priority and cache refresh; the schedule advances
// to the next due time computed in its zone.
func TestSchedulerMaterialisesEachDueTimeOnce(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Worker.SchedulerInterval = dur(5 * time.Millisecond)
	outer, _ := fakeDirectTarget(t, 200, `{"ok":true}`)
	cfg.Targets["outer"] = directTargetCfg(outer.URL)
	st := openStore(t)
	clk := &fakeClock{t: time.Date(2026, 9, 5, 4, 29, 0, 0, time.UTC)}
	due := time.Date(2026, 9, 5, 4, 30, 0, 0, time.UTC) // 06:30 Berlin in September
	sc := &store.Schedule{ID: "s1", Client: "acme", Target: "outer", Payload: []byte(`{"time-series":[]}`), Cron: "30 6 * * *", Timezone: "Europe/Berlin", Priority: 3, NextRunAt: &due}
	if err := st.CreateSchedule(context.Background(), sc); err != nil {
		t.Fatal(err)
	}
	startPoolWithClock(t, cfg, st, clk)
	startPoolWithClock(t, cfg, st, clk)
	time.Sleep(30 * time.Millisecond)
	if runs := runsOf(t, st, "s1"); len(runs) != 0 {
		t.Fatalf("a run before the due time: %v", runs)
	}
	clk.Set(due.Add(90 * time.Second)) // the tick after the due time
	waitFor(t, "the first run", func() bool { return len(runsOf(t, st, "s1")) >= 1 })
	time.Sleep(50 * time.Millisecond) // give a duplicate every chance
	runs := runsOf(t, st, "s1")
	if len(runs) != 1 {
		t.Fatalf("%d runs for one due time", len(runs))
	}
	run := runs[0]
	if run.IdempotencyKey != store.RunKey("s1", due) || run.Client != "acme" || run.Priority != 3 || run.Options.Cache != store.CacheRefresh || run.Target != "outer" {
		t.Errorf("run = %+v", run)
	}
	got, _ := st.GetSchedule(context.Background(), "s1")
	wantNext := due.Add(24 * time.Hour)
	if got.NextRunAt == nil || !got.NextRunAt.Equal(wantNext) || got.LastJobID != run.ID || got.LastRunAt == nil || !got.LastRunAt.Equal(due) {
		t.Errorf("schedule after run = next %v last %v job %s", got.NextRunAt, got.LastRunAt, got.LastJobID)
	}
	// Missed due times collapse: three days later, exactly one more run,
	// and the next due time is computed from now.
	clk.Set(due.Add(3*24*time.Hour + time.Minute))
	waitFor(t, "the second run", func() bool { return len(runsOf(t, st, "s1")) >= 2 })
	time.Sleep(50 * time.Millisecond)
	if runs := runsOf(t, st, "s1"); len(runs) != 2 {
		t.Errorf("%d runs after downtime, want 2", len(runs))
	}
	got, _ = st.GetSchedule(context.Background(), "s1")
	if !got.NextRunAt.Equal(due.Add(4 * 24 * time.Hour)) {
		t.Errorf("next after catch-up = %v", got.NextRunAt)
	}
	waitFor(t, "runs to complete", func() bool {
		for _, r := range runsOf(t, st, "s1") {
			if r.State != store.StateCompleted {
				return false
			}
		}
		return true
	})
}

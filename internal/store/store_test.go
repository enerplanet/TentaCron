package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newJob(t *testing.T, target string) *Job {
	t.Helper()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	return &Job{ID: id, Target: target, MaxAttempts: 5, Payload: []byte(`{"x":1}`)}
}

func mustCreate(t *testing.T, s *Store, j *Job) {
	t.Helper()
	created, _, err := s.CreateJob(context.Background(), j)
	if err != nil || !created {
		t.Fatalf("CreateJob: created=%v err=%v", created, err)
	}
}

func noPoll(string) time.Duration { return time.Minute }

func TestMigrationsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	for i := 0; i < 2; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open #%d: %v", i+1, err)
		}
		_ = s.Close()
	}
}

func TestCreateAndGetJob(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	j := newJob(t, "meme")
	mustCreate(t, s, j)

	got, err := s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.State != StateReceived || got.Target != "meme" || string(got.Payload) != `{"x":1}` {
		t.Errorf("job mismatch: %+v", got)
	}
	events, err := s.ListEvents(ctx, j.ID)
	if err != nil || len(events) != 1 || events[0].ToState != StateReceived {
		t.Errorf("want one accept event, got %v (err %v)", events, err)
	}

	if _, err := s.GetJob(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestIdempotencyReplay(t *testing.T) {
	s := openTest(t)
	j1 := newJob(t, "meme")
	j1.IdempotencyKey = "idem-1"
	mustCreate(t, s, j1)

	j2 := newJob(t, "meme")
	j2.IdempotencyKey = "idem-1"
	created, existing, err := s.CreateJob(context.Background(), j2)
	if err != nil {
		t.Fatalf("CreateJob replay: %v", err)
	}
	if created || existing.ID != j1.ID {
		t.Errorf("replay: created=%v existing=%s, want false/%s", created, existing.ID, j1.ID)
	}
}

func TestClaimNextAndTransitions(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	j := newJob(t, "meme")
	mustCreate(t, s, j)

	claimed, err := s.ClaimNext(ctx, noPoll)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNext: %v, %v", claimed, err)
	}
	if claimed.State != StateResolving || claimed.Attempts != 1 {
		t.Fatalf("claimed state=%s attempts=%d, want resolving/1", claimed.State, claimed.Attempts)
	}
	if again, _ := s.ClaimNext(ctx, noPoll); again != nil {
		t.Fatal("job claimed twice")
	}

	if err := s.SetResolved(ctx, j.ID, []byte(`{"resolved":true}`), "resolved 1 resolvent"); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}
	if err := s.MarkAwaitingTarget(ctx, j.ID, "meme-42", 202, []byte(`{"job_id":"meme-42"}`),
		time.Now().Add(-time.Second), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("MarkAwaitingTarget: %v", err)
	}

	poll, err := s.ClaimNext(ctx, noPoll)
	if err != nil || poll == nil {
		t.Fatalf("poll claim: %v, %v", poll, err)
	}
	if poll.State != StateAwaitingTarget || poll.TargetJobID != "meme-42" {
		t.Fatalf("poll job state=%s targetJob=%s", poll.State, poll.TargetJobID)
	}
	// The poll tick was pushed a minute out, so nothing is due now.
	if again, _ := s.ClaimNext(ctx, noPoll); again != nil {
		t.Fatal("poll tick claimed twice")
	}

	if err := s.MarkCompleted(ctx, j.ID, 200, []byte(`{"ok":true}`), "", "", "target job finished"); err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}
	final, _ := s.GetJob(ctx, j.ID)
	if final.State != StateCompleted || final.CompletedAt == nil || *final.TargetStatus != 200 {
		t.Errorf("final job: %+v", final)
	}
	events, _ := s.ListEvents(ctx, j.ID)
	if len(events) != 5 { // accepted, claimed, resolved, awaiting, completed
		for _, e := range events {
			t.Logf("event: %s -> %s (%s)", e.FromState, e.ToState, e.Detail)
		}
		t.Errorf("want 5 events, got %d", len(events))
	}
}

func TestInvalidTransitionRejected(t *testing.T) {
	s := openTest(t)
	j := newJob(t, "meme")
	mustCreate(t, s, j)
	// Job is "received", not "resolving" — SetResolved must refuse.
	if err := s.SetResolved(context.Background(), j.ID, nil, ""); err == nil {
		t.Fatal("want transition error, got nil")
	}
}

func TestConcurrentClaimOneWinner(t *testing.T) {
	s := openTest(t)
	j := newJob(t, "meme")
	mustCreate(t, s, j)

	var mu sync.Mutex
	var winners int
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.ClaimNext(context.Background(), noPoll)
			if err != nil {
				t.Errorf("ClaimNext: %v", err)
			}
			if got != nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
}

func TestRequeueBackoffScheduling(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	j := newJob(t, "meme")
	mustCreate(t, s, j)

	if c, _ := s.ClaimNext(ctx, noPoll); c == nil {
		t.Fatal("initial claim failed")
	}
	if err := s.Requeue(ctx, j.ID, time.Now().Add(time.Hour), "resource timeout"); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	if c, _ := s.ClaimNext(ctx, noPoll); c != nil {
		t.Fatal("job claimable before backoff elapsed")
	}
	if err := s.Requeue(ctx, j.ID, time.Now().Add(-time.Second), "backoff over"); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	c, _ := s.ClaimNext(ctx, noPoll)
	if c == nil {
		t.Fatal("job not claimable after backoff elapsed")
	}
	if c.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", c.Attempts)
	}
}

func TestMarkFailed(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	j := newJob(t, "meme")
	mustCreate(t, s, j)
	if err := s.MarkFailed(ctx, j.ID, "unknown_resolvent", `no resolvent config for type "resolvent-tidal"`); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	got, _ := s.GetJob(ctx, j.ID)
	if got.State != StateFailed || got.ErrorCode != "unknown_resolvent" || got.CompletedAt == nil {
		t.Errorf("failed job: %+v", got)
	}
}

func TestRecoverInFlight(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	j1 := newJob(t, "meme") // will be resolving
	j2 := newJob(t, "buem") // stays received
	j3 := newJob(t, "meme") // will be awaiting_target
	for _, j := range []*Job{j1, j2, j3} {
		mustCreate(t, s, j)
	}
	if c, _ := s.ClaimNext(ctx, noPoll); c == nil || c.ID != j1.ID {
		t.Fatalf("expected to claim j1 first, got %v", c)
	}
	// Move j3 through to awaiting_target.
	if c, _ := s.ClaimNext(ctx, noPoll); c == nil || c.ID != j2.ID {
		t.Fatal("expected to claim j2")
	}
	if c, _ := s.ClaimNext(ctx, noPoll); c == nil || c.ID != j3.ID {
		t.Fatal("expected to claim j3")
	}
	if err := s.SetResolved(ctx, j3.ID, nil, ""); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := s.MarkAwaitingTarget(ctx, j3.ID, "m-1", 202, nil, future, future); err != nil {
		t.Fatal(err)
	}
	// Park j2 back so only j1 (resolving) and j2 (resolving) count as in-flight.
	if err := s.Requeue(ctx, j2.ID, time.Now().Add(time.Hour), "parked"); err != nil {
		t.Fatal(err)
	}

	n, err := s.RecoverInFlight(ctx)
	if err != nil {
		t.Fatalf("RecoverInFlight: %v", err)
	}
	if n != 1 { // only j1 was resolving
		t.Errorf("recovered %d, want 1", n)
	}
	got1, _ := s.GetJob(ctx, j1.ID)
	if got1.State != StateReceived || got1.NextAttemptAt != nil {
		t.Errorf("j1 after recovery: state=%s next=%v", got1.State, got1.NextAttemptAt)
	}
	got3, _ := s.GetJob(ctx, j3.ID)
	if got3.State != StateAwaitingTarget {
		t.Errorf("j3 must keep awaiting_target, got %s", got3.State)
	}
}

func TestPruneTerminal(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	j1 := newJob(t, "meme")
	j2 := newJob(t, "meme")
	mustCreate(t, s, j1)
	mustCreate(t, s, j2)
	if err := s.MarkCompleted(ctx, j1.ID, 200, nil, "/data/results/old.zip", "application/zip", "done"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFailed(ctx, j2.ID, "target_error", "boom"); err != nil {
		t.Fatal(err)
	}

	paths, err := s.PruneTerminal(ctx, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("PruneTerminal: %v", err)
	}
	if len(paths) != 1 || paths[0] != "/data/results/old.zip" {
		t.Errorf("paths = %v", paths)
	}
	if _, err := s.GetJob(ctx, j1.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("j1 should be pruned, got %v", err)
	}
	// Events cascade with the job.
	events, _ := s.ListEvents(ctx, j1.ID)
	if len(events) != 0 {
		t.Errorf("events should cascade-delete, got %d", len(events))
	}
}

func TestSeriesCache(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if _, ok, err := s.GetSeries(ctx, "h1"); err != nil || ok {
		t.Fatalf("empty cache: ok=%v err=%v", ok, err)
	}
	if err := s.PutSeries(ctx, "h1", "resolvent-pv1", []byte(`{"type":"time-series"}`), time.Hour); err != nil {
		t.Fatalf("PutSeries: %v", err)
	}
	body, ok, err := s.GetSeries(ctx, "h1")
	if err != nil || !ok || string(body) != `{"type":"time-series"}` {
		t.Fatalf("GetSeries: ok=%v body=%s err=%v", ok, body, err)
	}

	// Upsert refreshes.
	if err := s.PutSeries(ctx, "h1", "resolvent-pv1", []byte(`{"v":2}`), time.Hour); err != nil {
		t.Fatalf("PutSeries upsert: %v", err)
	}
	body, _, _ = s.GetSeries(ctx, "h1")
	if string(body) != `{"v":2}` {
		t.Errorf("upsert body = %s", body)
	}

	// Expired entries are invisible and purgeable.
	if err := s.PutSeries(ctx, "h2", "resolvent-wind", []byte(`{}`), -time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetSeries(ctx, "h2"); ok {
		t.Error("expired entry served")
	}
	n, err := s.PurgeExpiredSeries(ctx)
	if err != nil || n != 1 {
		t.Errorf("purged %d (err %v), want 1", n, err)
	}
}

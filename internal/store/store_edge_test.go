package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewIDFormatAndUniqueness(t *testing.T) {
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id, err := NewID()
		if err != nil {
			t.Fatal(err)
		}
		if !hex32.MatchString(id) {
			t.Fatalf("id %q is not 32 lowercase hex chars", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = true
	}
}

func TestListJobsOrderingFilterAndLimit(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	var ids []string
	for i := 0; i < 5; i++ {
		j := newJob(t, "meme")
		mustCreate(t, s, j)
		ids = append(ids, j.ID)
		time.Sleep(2 * time.Millisecond) // distinct created_at milliseconds
	}
	if err := s.MarkFailed(ctx, ids[1], "target_error", "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFailed(ctx, ids[3], "target_error", "y"); err != nil {
		t.Fatal(err)
	}

	all, err := s.ListJobs(ctx, ListFilter{Limit: 10})
	if err != nil || len(all) != 5 {
		t.Fatalf("ListJobs all: %d jobs, err %v", len(all), err)
	}
	for i, j := range all {
		if j.ID != ids[4-i] {
			t.Fatalf("newest-first ordering broken at %d: %s", i, j.ID)
		}
	}
	failed, err := s.ListJobs(ctx, ListFilter{State: StateFailed, Limit: 10})
	if err != nil || len(failed) != 2 || failed[0].ID != ids[3] || failed[1].ID != ids[1] {
		t.Errorf("failed filter = %v (err %v)", failed, err)
	}
	limited, _ := s.ListJobs(ctx, ListFilter{Limit: 2})
	if len(limited) != 2 || limited[0].ID != ids[4] {
		t.Errorf("limit 2 = %d jobs, first %s", len(limited), limited[0].ID)
	}
	if none, err := s.ListJobs(ctx, ListFilter{State: StateCompleted, Limit: 10}); err != nil || len(none) != 0 {
		t.Errorf("no completed jobs: %v (err %v)", none, err)
	}
}

// Jobs created within the same millisecond still list newest first: the
// rowid tiebreaker keeps the order stable.
func TestListJobsSameMillisecondTiebreak(t *testing.T) {
	s := openTest(t)
	var ids []string
	for i := 0; i < 3; i++ {
		j := newJob(t, "meme")
		mustCreate(t, s, j)
		ids = append(ids, j.ID)
	}
	got, err := s.ListJobs(context.Background(), ListFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for i := range ids {
		if got[i].ID != ids[2-i] {
			t.Fatalf("position %d = %s, want %s", i, got[i].ID, ids[2-i])
		}
	}
}

func TestGetJobByIdempotencyNotFound(t *testing.T) {
	s := openTest(t)
	if _, err := s.GetJobByIdempotency(context.Background(), "c", "k"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

// Concurrent submissions with the same key from the same client must settle
// on exactly one job: the unique index wins the race and the losers replay.
func TestConcurrentCreateWithSameIdempotencyKey(t *testing.T) {
	s := openTest(t)
	const n = 8
	jobs := make([]*Job, n)
	for i := range jobs {
		jobs[i] = newJob(t, "meme")
		jobs[i].Client, jobs[i].IdempotencyKey = "frontend", "race-key"
	}
	results := make([]struct {
		created bool
		id      string
		err     error
	}, n)
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			created, stored, err := s.CreateJob(context.Background(), jobs[i])
			results[i].created, results[i].err = created, err
			if stored != nil {
				results[i].id = stored.ID
			}
		}(i)
	}
	wg.Wait()
	creates := 0
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("goroutine %d: %v", i, r.err)
		}
		if r.created {
			creates++
		}
		if r.id != results[0].id {
			t.Errorf("goroutine %d settled on %s, others on %s", i, r.id, results[0].id)
		}
	}
	if creates != 1 {
		t.Errorf("created %d jobs, want exactly 1", creates)
	}
}

func TestTransitionsOnUnknownJob(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	checks := map[string]error{
		"MarkFailed":         s.MarkFailed(ctx, "missing", "x", "y"),
		"MarkCompleted":      s.MarkCompleted(ctx, "missing", 200, nil, "", "", ""),
		"Requeue":            s.Requeue(ctx, "missing", time.Now(), ""),
		"SetResolved":        s.SetResolved(ctx, "missing", nil, ""),
		"MarkAwaitingTarget": s.MarkAwaitingTarget(ctx, "missing", "t", 202, nil, time.Now(), time.Now()),
	}
	for name, err := range checks {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s on unknown job: %v, want ErrNotFound", name, err)
		}
	}
}

func TestTransitionsOnTerminalJobAreRefused(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	j := newJob(t, "meme")
	mustCreate(t, s, j)
	if err := s.MarkFailed(ctx, j.ID, "target_error", "boom"); err != nil {
		t.Fatal(err)
	}
	checks := map[string]error{
		"Requeue":            s.Requeue(ctx, j.ID, time.Now(), "late"),
		"SetResolved":        s.SetResolved(ctx, j.ID, nil, ""),
		"MarkAwaitingTarget": s.MarkAwaitingTarget(ctx, j.ID, "t", 202, nil, time.Now(), time.Now()),
		"MarkCompleted":      s.MarkCompleted(ctx, j.ID, 200, nil, "", "", ""),
	}
	for name, err := range checks {
		if !errors.Is(err, ErrTerminalState) {
			t.Errorf("%s on failed job: %v, want ErrTerminalState", name, err)
		}
	}
	if got, _ := s.GetJob(ctx, j.ID); got.State != StateFailed || len(mustEvents(t, s, j.ID)) != 2 {
		t.Errorf("terminal job was touched: state=%s events=%d", got.State, len(mustEvents(t, s, j.ID)))
	}
}

func TestMarkAwaitingTargetRequiresForwarding(t *testing.T) {
	s := openTest(t)
	j := newJob(t, "meme")
	mustCreate(t, s, j)
	err := s.MarkAwaitingTarget(context.Background(), j.ID, "t", 202, nil, time.Now(), time.Now())
	if err == nil || errors.Is(err, ErrTerminalState) || errors.Is(err, ErrNotFound) {
		t.Fatalf("want a plain invalid-transition error, got %v", err)
	}
}

func TestPollTickScheduling(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	j := newJob(t, "meme")
	mustCreate(t, s, j)
	if c, _ := s.ClaimNext(ctx, noPoll); c == nil {
		t.Fatal("claim failed")
	}
	if err := s.SetResolved(ctx, j.ID, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAwaitingTarget(ctx, j.ID, "m-1", 202, nil, time.Now().Add(60*time.Millisecond), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.ClaimNext(ctx, noPoll); c != nil {
		t.Fatal("poll tick claimed before it was due")
	}
	time.Sleep(80 * time.Millisecond)
	before := time.Now()
	c, err := s.ClaimNext(ctx, ClaimPolicy{PollLease: func(string) time.Duration { return 5 * time.Minute }})
	if err != nil || c == nil || c.State != StateAwaitingTarget {
		t.Fatalf("due poll tick not claimed: %v %v", c, err)
	}
	if c.NextAttemptAt == nil {
		t.Fatal("claimed tick must reschedule next_attempt_at")
	}
	if d := c.NextAttemptAt.Sub(before); d < 5*time.Minute-time.Second || d > 5*time.Minute+time.Second {
		t.Errorf("next poll scheduled %v ahead, want the target's 5m interval", d)
	}
	if c.Attempts != 1 {
		t.Errorf("poll ticks must not burn attempts, got %d", c.Attempts)
	}
}

func TestConcurrentPollTickSingleWinner(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	j := newJob(t, "meme")
	mustCreate(t, s, j)
	if c, _ := s.ClaimNext(ctx, noPoll); c == nil {
		t.Fatal("claim failed")
	}
	if err := s.SetResolved(ctx, j.ID, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAwaitingTarget(ctx, j.ID, "m-1", 202, nil, time.Now().Add(-time.Second), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	winners := 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := s.ClaimNext(ctx, noPoll)
			if err != nil {
				t.Errorf("ClaimNext: %v", err)
			}
			if c != nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("poll tick winners = %d, want exactly 1", winners)
	}
}

func TestManyWorkersClaimEachJobExactlyOnce(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	const jobs = 40
	for i := 0; i < jobs; i++ {
		mustCreate(t, s, newJob(t, "meme"))
	}
	var mu sync.Mutex
	claims := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				c, err := s.ClaimNext(ctx, noPoll)
				if err != nil {
					t.Errorf("ClaimNext: %v", err)
					return
				}
				if c == nil {
					return
				}
				mu.Lock()
				claims[c.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claims) != jobs {
		t.Fatalf("claimed %d distinct jobs, want %d", len(claims), jobs)
	}
	for id, n := range claims {
		if n != 1 {
			t.Errorf("job %s claimed %d times", id, n)
		}
	}
}

// An exhausted candidate is failed and skipped; the claim continues with
// the next eligible job in the same pass.
func TestClaimNextSkipsExhaustedAndClaimsNext(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	exhausted := newJob(t, "meme")
	exhausted.MaxAttempts = 1
	mustCreate(t, s, exhausted)
	time.Sleep(2 * time.Millisecond)
	fresh := newJob(t, "meme")
	mustCreate(t, s, fresh)

	if c, _ := s.ClaimNext(ctx, noPoll); c == nil || c.ID != exhausted.ID {
		t.Fatalf("first claim = %v, want the older job", c)
	}
	if _, err := s.RecoverInFlight(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := s.ClaimNext(ctx, noPoll)
	if err != nil || c == nil || c.ID != fresh.ID {
		t.Fatalf("claim after exhaustion = %v (err %v), want the fresh job", c, err)
	}
	got, _ := s.GetJob(ctx, exhausted.ID)
	if got.State != StateFailed || got.ErrorCode != "max_attempts_exceeded" || got.CompletedAt == nil {
		t.Errorf("exhausted job = %s/%s completed_at=%v", got.State, got.ErrorCode, got.CompletedAt)
	}
}

func TestRescueStuckLeavesOtherStatesAlone(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	received := newJob(t, "meme")
	awaiting := newJob(t, "meme")
	done := newJob(t, "meme")
	for _, j := range []*Job{received, awaiting, done} {
		mustCreate(t, s, j)
	}
	// Move `awaiting` through to awaiting_target and `done` to completed.
	for _, want := range []string{received.ID, awaiting.ID, done.ID} {
		if c, _ := s.ClaimNext(ctx, noPoll); c == nil || c.ID != want {
			t.Fatalf("claim order broke at %s", want)
		}
	}
	if err := s.Requeue(ctx, received.ID, time.Now(), "back to received"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetResolved(ctx, awaiting.ID, nil, ""); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := s.MarkAwaitingTarget(ctx, awaiting.ID, "m", 202, nil, future, future); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkCompleted(ctx, done.ID, 200, nil, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RescueStuck(ctx, cutoffAt(time.Now().Add(time.Hour))); err != nil || n != 0 {
		t.Fatalf("rescued %d (err %v), want 0: only resolving/forwarding count as stuck", n, err)
	}
}

func TestRecoverInFlightIncludesForwarding(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	j := newJob(t, "meme")
	mustCreate(t, s, j)
	if c, _ := s.ClaimNext(ctx, noPoll); c == nil {
		t.Fatal("claim failed")
	}
	if err := s.SetResolved(ctx, j.ID, []byte(`{}`), "resolved"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RecoverInFlight(ctx); err != nil || n != 1 {
		t.Fatalf("recovered %d (err %v), want 1", n, err)
	}
	got, _ := s.GetJob(ctx, j.ID)
	if got.State != StateReceived || got.Attempts != 1 {
		t.Errorf("recovered job = %s attempts %d", got.State, got.Attempts)
	}
	events := mustEvents(t, s, j.ID)
	if last := events[len(events)-1]; last.FromState != StateForwarding || last.Detail != "recovered after restart" {
		t.Errorf("last event = %+v", last)
	}
}

func TestTerminalBeforeRespectsCutoffAndEmptyDelete(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	j := newJob(t, "meme")
	mustCreate(t, s, j)
	if err := s.MarkCompleted(ctx, j.ID, 200, nil, "", "", ""); err != nil {
		t.Fatal(err)
	}
	ids, paths, err := s.TerminalBefore(ctx, time.Now().Add(-time.Hour), 100)
	if err != nil || len(ids) != 0 || len(paths) != 0 {
		t.Errorf("a fresh terminal job must not be listed: ids=%v paths=%v err=%v", ids, paths, err)
	}
	if err := s.DeleteJobs(ctx, nil); err != nil {
		t.Errorf("DeleteJobs(nil) must be a no-op: %v", err)
	}
	if _, err := s.GetJob(ctx, j.ID); err != nil {
		t.Errorf("job vanished: %v", err)
	}
}

func TestPayloadStoredVerbatim(t *testing.T) {
	s := openTest(t)
	payload := []byte("{ \"a\" : 1 ,\n  \"big\": 12345678901234567890, \"s\": \"ü\\n\" }")
	j := newJob(t, "meme")
	j.Payload = payload
	mustCreate(t, s, j)
	got, err := s.GetJob(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Payload) != string(payload) {
		t.Errorf("payload bytes changed:\n got %s\nwant %s", got.Payload, payload)
	}
}

func TestTimestampsAreUTCAndMonotonic(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	j := newJob(t, "meme")
	mustCreate(t, s, j)
	first, _ := s.GetJob(ctx, j.ID)
	if first.CreatedAt.Location() != time.UTC || first.UpdatedAt.Location() != time.UTC {
		t.Errorf("timestamps must be UTC: %v / %v", first.CreatedAt.Location(), first.UpdatedAt.Location())
	}
	if first.UpdatedAt.Before(first.CreatedAt) || first.CompletedAt != nil || first.NextAttemptAt != nil ||
		first.PollDeadline != nil || first.TargetStatus != nil || first.IdempotencyKey != "" {
		t.Errorf("fresh job has unexpected optional fields: %+v", first)
	}
	time.Sleep(2 * time.Millisecond)
	if c, _ := s.ClaimNext(ctx, noPoll); c == nil {
		t.Fatal("claim failed")
	}
	second, _ := s.GetJob(ctx, j.ID)
	if !second.UpdatedAt.After(first.UpdatedAt) || !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("updated_at must advance and created_at stay: %v -> %v", first.UpdatedAt, second.UpdatedAt)
	}
}

func TestOpenCreatesNestedDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "c", "tentacron.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = s.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("database file not created: %v", err)
	}
}

func TestOpenRejectsGarbageFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garbage.db")
	if err := os.WriteFile(path, []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		_ = s.Close()
		t.Fatal("Open must fail on a non-database file")
	}
}

func TestOpenFailsWhenParentIsAFile(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(filepath.Join(parent, "test.db")); err == nil {
		_ = s.Close()
		t.Fatal("Open must fail when the parent path is a file")
	}
}

func TestPingAfterClose(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping on open store: %v", err)
	}
	_ = s.Close()
	if err := s.Ping(context.Background()); err == nil {
		t.Error("Ping after Close must fail (readyz depends on it)")
	}
}

func TestSeriesCacheZeroTTLIsNotServed(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.PutSeries(ctx, "h0", "resolvent-x", []byte(`{}`), 0); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetSeries(ctx, "h0"); ok {
		t.Error("zero-TTL entry served")
	}
	if _, ok, _ := s.GetSeries(ctx, ""); ok {
		t.Error("empty hash must never hit")
	}
	if n, err := s.PurgeExpiredSeries(ctx); err != nil || n != 1 {
		t.Errorf("purged %d (err %v), want 1", n, err)
	}
}

func TestCancelledContextReturnsError(t *testing.T) {
	s := openTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.GetJob(ctx, "any"); err == nil {
		t.Error("cancelled context must surface an error")
	}
	if _, err := s.ClaimNext(ctx, noPoll); err == nil {
		t.Error("cancelled context must surface an error from ClaimNext")
	}
}

func TestListEventsForUnknownJobIsEmpty(t *testing.T) {
	s := openTest(t)
	events, err := s.ListEvents(context.Background(), "missing")
	if err != nil || len(events) != 0 {
		t.Errorf("events = %v, err %v", events, err)
	}
}

// Shutdown parks in-flight jobs with Requeue regardless of their state; an
// awaiting_target job parked this way becomes a plain received job again.
func TestRequeueFromAwaitingTargetParksJob(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	j := newJob(t, "meme")
	mustCreate(t, s, j)
	if c, _ := s.ClaimNext(ctx, noPoll); c == nil {
		t.Fatal("claim failed")
	}
	if err := s.SetResolved(ctx, j.ID, nil, ""); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := s.MarkAwaitingTarget(ctx, j.ID, "m", 202, nil, future, future); err != nil {
		t.Fatal(err)
	}
	if err := s.Requeue(ctx, j.ID, time.Now().Add(-time.Second), "parked"); err != nil {
		t.Fatal(err)
	}
	c, err := s.ClaimNext(ctx, noPoll)
	if err != nil || c == nil || c.State != StateResolving || c.Attempts != 2 {
		t.Fatalf("parked job must be claimable as received again: %v %v", c, err)
	}
}

func BenchmarkClaimNext(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		id, _ := NewID()
		if _, _, err := s.CreateJob(ctx, &Job{ID: id, Target: "t", MaxAttempts: 1, Payload: []byte(`{}`)}); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if c, err := s.ClaimNext(ctx, noPoll); err != nil || c == nil {
			b.Fatalf("claim %d: %v %v", i, c, err)
		}
	}
}

// insertTerminalRows bulk-inserts completed jobs straight into the table
// (CreateJob would take one transaction per row) with completed_at spread
// one millisecond apart from base, oldest first.
func insertTerminalRows(t *testing.T, s *Store, n int, base time.Time) []string {
	t.Helper()
	ids := make([]string, 0, n)
	const perStmt = 400
	for start := 0; start < n; start += perStmt {
		end := min(start+perStmt, n)
		var sb strings.Builder
		sb.WriteString(`INSERT INTO jobs (id, target, state, max_attempts, payload, created_at, updated_at, completed_at) VALUES `)
		args := make([]any, 0, (end-start)*8)
		for i := start; i < end; i++ {
			if i > start {
				sb.WriteString(",")
			}
			sb.WriteString("(?,?,?,?,?,?,?,?)")
			id := fmt.Sprintf("%032x", i)
			at := ts(base.Add(time.Duration(i) * time.Millisecond))
			args = append(args, id, "demo", StateCompleted, 1, []byte(`{}`), at, at, at)
			ids = append(ids, id)
		}
		if _, err := s.db.ExecContext(context.Background(), sb.String(), args...); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

// One id is one bound parameter and SQLite caps a statement at 32766 of
// them; a retention backlog past that size used to fail every sweep, so
// the database grew without bound. The delete must chunk.
func TestDeleteJobsBeyondSQLiteParameterLimit(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	const n = 40000
	insertTerminalRows(t, s, n, time.Now().Add(-48*time.Hour))
	ids, _, err := s.TerminalBefore(ctx, time.Now().Add(-time.Hour), n+1)
	if err != nil || len(ids) != n {
		t.Fatalf("TerminalBefore listed %d rows (err %v), want %d", len(ids), err, n)
	}
	if err := s.DeleteJobs(ctx, ids); err != nil {
		t.Fatalf("DeleteJobs(%d ids): %v", n, err)
	}
	var left int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs`).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d rows left after the prune (err %v)", left, err)
	}
}

// TerminalBefore returns at most limit rows, oldest completion first, so a
// bounded sweep always makes progress on the oldest backlog.
func TestTerminalBeforeHonoursLimitOldestFirst(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	base := time.Now().Add(-2 * time.Hour)
	ids := insertTerminalRows(t, s, 5, base)
	got, _, err := s.TerminalBefore(ctx, time.Now().Add(-time.Hour), 3)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, ids[:3]) {
		t.Errorf("limited listing = %v, want the three oldest %v", got, ids[:3])
	}
	if err := s.DeleteJobs(ctx, got); err != nil {
		t.Fatal(err)
	}
	rest, _, err := s.TerminalBefore(ctx, time.Now().Add(-time.Hour), 3)
	if err != nil || !reflect.DeepEqual(rest, ids[3:]) {
		t.Errorf("second pass = %v (err %v), want the remaining %v", rest, err, ids[3:])
	}
}

// A second completion or failure of a terminal job is refused and leaves
// the first outcome untouched: an overlapping poll tick must never replace
// a result body or file a client may already have fetched.
func TestRepeatedTerminalTransitionsKeepFirstOutcome(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	done := newJob(t, "meme")
	mustCreate(t, s, done)
	if err := s.MarkCompleted(ctx, done.ID, 200, []byte(`{"first":true}`), "", "", "first"); err != nil {
		t.Fatal(err)
	}
	err := s.MarkCompleted(ctx, done.ID, 200, []byte(`{"second":true}`), "/results/late.zip", "application/zip", "second")
	if !errors.Is(err, ErrTerminalState) {
		t.Fatalf("duplicate completion: %v, want ErrTerminalState", err)
	}
	got, _ := s.GetJob(ctx, done.ID)
	if string(got.TargetResponse) != `{"first":true}` || got.ResultPath != "" || len(mustEvents(t, s, done.ID)) != 2 {
		t.Errorf("first completion was overwritten: response=%s path=%q events=%d", got.TargetResponse, got.ResultPath, len(mustEvents(t, s, done.ID)))
	}

	failed := newJob(t, "meme")
	mustCreate(t, s, failed)
	if err := s.MarkFailed(ctx, failed.ID, "target_error", "first"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFailed(ctx, failed.ID, "internal", "later"); !errors.Is(err, ErrTerminalState) {
		t.Fatalf("duplicate failure: %v, want ErrTerminalState", err)
	}
	if got, _ := s.GetJob(ctx, failed.ID); got.ErrorCode != "target_error" || got.ErrorMessage != "first" {
		t.Errorf("first failure was overwritten: %s/%s", got.ErrorCode, got.ErrorMessage)
	}
}

// A claim that committed must always be handed to the caller, even when the
// context is cancelled in the instant between the commit and the read-back
// (a shutdown signal): otherwise the job sits in resolving with no worker,
// unclaimable until restart recovery. The cancellation races the claim many
// times; every job that ended up resolving must have been returned.
func TestClaimNextNeverOrphansAClaimedJob(t *testing.T) {
	s := openTest(t)
	orphaned, returned := 0, 0
	for i := 0; i < 150; i++ {
		j := newJob(t, "meme")
		mustCreate(t, s, j)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(time.Duration(i%5) * 50 * time.Microsecond)
			cancel()
		}()
		claimed, err := s.ClaimNext(ctx, noPoll)
		cancel()
		got, getErr := s.GetJob(context.Background(), j.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		switch {
		case got.State == StateResolving && (claimed == nil || err != nil):
			orphaned++
		case claimed != nil:
			returned++
			if claimed.State != StateResolving || claimed.ID != j.ID {
				t.Fatalf("returned job %s in state %s, want the claimed %s in resolving", claimed.ID, claimed.State, j.ID)
			}
		}
		// Reset for the next iteration so the queue holds one candidate.
		if got.State == StateResolving {
			if err := s.Requeue(context.Background(), j.ID, time.Now(), "reset"); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.MarkFailed(context.Background(), j.ID, "x", "done with it"); err != nil && !errors.Is(err, ErrTerminalState) {
			t.Fatal(err)
		}
	}
	if orphaned > 0 {
		t.Fatalf("%d claims committed but were not handed to the caller (%d returned)", orphaned, returned)
	}
}

func TestCountByState(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if counts, err := s.CountByState(ctx); err != nil || len(counts) != 0 {
		t.Fatalf("empty store: %v (err %v)", counts, err)
	}
	a, b, c := newJob(t, "meme"), newJob(t, "meme"), newJob(t, "meme")
	for _, j := range []*Job{a, b, c} {
		mustCreate(t, s, j)
	}
	if err := s.MarkFailed(ctx, c.ID, "target_error", "x"); err != nil {
		t.Fatal(err)
	}
	if claimed, _ := s.ClaimNext(ctx, noPoll); claimed == nil || claimed.ID != a.ID {
		t.Fatalf("claim = %v", claimed)
	}
	counts, err := s.CountByState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{StateReceived: 1, StateResolving: 1, StateFailed: 1}
	if !reflect.DeepEqual(counts, want) {
		t.Errorf("counts = %v, want %v", counts, want)
	}
}

// The client filter backs read scoping: a client lists only the jobs it
// submitted, combined with the state filter.
func TestListJobsFiltersByClient(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, spec := range []struct {
		client string
		fail   bool
	}{{"a", false}, {"b", false}, {"a", true}, {"b", true}, {"a", false}} {
		j := newJob(t, "meme")
		j.Client = spec.client
		mustCreate(t, s, j)
		if spec.fail {
			if err := s.MarkFailed(ctx, j.ID, "target_error", "x"); err != nil {
				t.Fatal(err)
			}
		}
	}
	count := func(f ListFilter) int {
		jobs, err := s.ListJobs(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range jobs {
			if f.Client != "" && j.Client != f.Client {
				t.Errorf("job %s of client %s leaked into client %s's list", j.ID, j.Client, f.Client)
			}
		}
		return len(jobs)
	}
	if n := count(ListFilter{Client: "a", Limit: 10}); n != 3 {
		t.Errorf("client a: %d jobs, want 3", n)
	}
	if n := count(ListFilter{Client: "b", State: StateFailed, Limit: 10}); n != 1 {
		t.Errorf("client b failed: %d jobs, want 1", n)
	}
	if n := count(ListFilter{Limit: 10}); n != 5 {
		t.Errorf("unfiltered: %d jobs, want 5", n)
	}
	if n := count(ListFilter{Client: "nobody", Limit: 10}); n != 0 {
		t.Errorf("unknown client: %d jobs, want 0", n)
	}
}

// Target and time-window filters combine with the others; the cursor walks
// the newest-first listing without skipping or repeating, even across jobs
// created in the same millisecond.
func TestListJobsTargetWindowAndCursor(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	var ids []string
	for i, target := range []string{"meme", "demo", "meme", "demo", "meme"} {
		j := newJob(t, target)
		mustCreate(t, s, j)
		ids = append(ids, j.ID)
		if i == 1 {
			time.Sleep(3 * time.Millisecond) // a visible gap for the time window
		}
	}
	list := func(f ListFilter) []*Job {
		jobs, err := s.ListJobs(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return jobs
	}
	if got := list(ListFilter{Target: "demo", Limit: 10}); len(got) != 2 || got[0].ID != ids[3] || got[1].ID != ids[1] {
		t.Errorf("target filter = %v", got)
	}
	second, _ := s.GetJob(ctx, ids[2])
	if got := list(ListFilter{Since: &second.CreatedAt, Limit: 10}); len(got) != 3 {
		t.Errorf("since (inclusive) returned %d, want the three newest", len(got))
	}
	if got := list(ListFilter{Until: &second.CreatedAt, Limit: 10}); len(got) != 2 {
		t.Errorf("until (exclusive) returned %d, want the two oldest", len(got))
	}
	// Walk everything one item per page; same-millisecond neighbours must
	// still come out exactly once each, newest first.
	var walked []string
	var cursor *Cursor
	for range 10 {
		page := list(ListFilter{Before: cursor, Limit: 1})
		if len(page) == 0 {
			break
		}
		walked = append(walked, page[0].ID)
		cursor = &Cursor{CreatedAt: page[0].CreatedAt, Seq: page[0].Seq}
	}
	want := []string{ids[4], ids[3], ids[2], ids[1], ids[0]}
	if !reflect.DeepEqual(walked, want) {
		t.Errorf("cursor walk = %v, want %v", walked, want)
	}
	if s0, _ := s.GetJob(ctx, ids[0]); s0.Seq <= 0 {
		t.Errorf("Seq must expose the insertion order, got %d", s0.Seq)
	}
}

func newClientJob(t *testing.T, client string, priority int) *Job {
	t.Helper()
	j := newJob(t, "demo")
	j.Client, j.Priority = client, priority
	return j
}

func claimOrder(t *testing.T, s *Store, policy ClaimPolicy, n int) []string {
	t.Helper()
	var order []string
	for i := 0; i < n; i++ {
		c, err := s.ClaimNext(context.Background(), policy)
		if err != nil {
			t.Fatal(err)
		}
		if c == nil {
			break
		}
		order = append(order, c.ID)
	}
	return order
}

// At equal priority claims interleave clients — each client's first due job
// before any client's second — so one client's batch never starves another
// client's interactive requests.
func TestClaimOrderIsRoundRobinAcrossClients(t *testing.T) {
	s := openTest(t)
	var batch []*Job
	for i := 0; i < 4; i++ {
		j := newClientJob(t, "batch", 0)
		mustCreate(t, s, j)
		batch = append(batch, j)
		time.Sleep(time.Millisecond)
	}
	ui := newClientJob(t, "ui", 0)
	mustCreate(t, s, ui)
	time.Sleep(time.Millisecond)
	late := newClientJob(t, "batch", 0)
	mustCreate(t, s, late)

	got := claimOrder(t, s, noPoll, 6)
	want := []string{batch[0].ID, ui.ID, batch[1].ID, batch[2].ID, batch[3].ID, late.ID}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("claim order = %v\nwant %v", got, want)
	}
}

// Priority outranks fairness and age: a higher priority claims first across
// clients, a negative one waits behind everything at the default.
func TestClaimOrderHonoursPriority(t *testing.T) {
	s := openTest(t)
	low := newClientJob(t, "a", -3)
	normal := newClientJob(t, "a", 0)
	urgent := newClientJob(t, "b", 5)
	other := newClientJob(t, "b", 0)
	for _, j := range []*Job{low, normal, urgent, other} {
		mustCreate(t, s, j)
		time.Sleep(time.Millisecond)
	}
	got := claimOrder(t, s, noPoll, 4)
	want := []string{urgent.ID, normal.ID, other.ID, low.ID}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("claim order = %v\nwant %v", got, want)
	}
	if c, _ := s.GetJob(context.Background(), urgent.ID); c.Priority != 5 {
		t.Errorf("priority not stored: %d", c.Priority)
	}
}

// A client at its in-flight ceiling is skipped in favour of other clients'
// work, and the ceiling holds under concurrent claims because it is checked
// inside the claim's write transaction.
func TestClaimRespectsPerClientCeiling(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	policy := ClaimPolicy{
		PollLease:     func(string) time.Duration { return time.Minute },
		MaxConcurrent: func(client string) int { return map[string]int{"batch": 2}[client] },
	}
	var batch []*Job
	for i := 0; i < 10; i++ {
		j := newClientJob(t, "batch", 0)
		mustCreate(t, s, j)
		batch = append(batch, j)
	}
	ui := newClientJob(t, "ui", 0)
	mustCreate(t, s, ui)

	var mu sync.Mutex
	var claimed []string
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := s.ClaimNext(ctx, policy)
			if err != nil {
				t.Errorf("ClaimNext: %v", err)
				return
			}
			if c != nil {
				mu.Lock()
				claimed = append(claimed, c.Client)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	counts := map[string]int{}
	for _, c := range claimed {
		counts[c]++
	}
	if counts["batch"] != 2 || counts["ui"] != 1 {
		t.Fatalf("claimed %v, want exactly 2 batch (the ceiling) and 1 ui", counts)
	}
	if c, _ := s.ClaimNext(ctx, policy); c != nil {
		t.Fatalf("nothing else may be claimable while batch sits at its ceiling, got %s", c.ID)
	}
	// Finishing one batch job frees one slot.
	if err := s.MarkFailed(ctx, batch[0].ID, "target_error", "x"); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.ClaimNext(ctx, policy); c == nil || c.Client != "batch" {
		t.Fatalf("a freed slot must admit the next batch job, got %v", c)
	}
}

// A database created by the first schema version gains the priority column
// (default 0) on open, and its existing rows stay claimable.
func TestMigrationAddsPriorityToExistingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	initSQL, err := migrationFS.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`,
		string(initSQL),
		`INSERT INTO schema_migrations (version, applied_at) VALUES (1, '2026-01-01T00:00:00.000Z')`,
		`INSERT INTO jobs (id, client, target, state, max_attempts, payload, created_at, updated_at)
		 VALUES ('old-job', 'frontend', 'demo', 'received', 3, '{}', '2026-01-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z')`,
		`INSERT INTO jobs (id, client, target, state, max_attempts, payload, created_at, updated_at, completed_at)
		 VALUES ('done-job', 'frontend', 'demo', 'completed', 3, '{}', '2026-01-01T00:00:00.000Z', '2026-01-01T00:00:01.000Z', '2026-01-01T00:00:01.000Z')`,
		`INSERT INTO job_events (job_id, from_state, to_state, detail, created_at) VALUES ('old-job', NULL, 'received', 'job accepted', '2026-01-01T00:00:00.000Z')`,
		`INSERT INTO job_events (job_id, from_state, to_state, detail, created_at) VALUES ('done-job', 'forwarding', 'completed', 'done', '2026-01-01T00:00:01.000Z')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt[:40], err)
		}
	}
	_ = db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a v1 database: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	job, err := s.GetJob(context.Background(), "old-job")
	if err != nil || job.Priority != 0 || job.Client != "frontend" || !job.Options.IsZero() || job.Seq != 1 {
		t.Fatalf("existing row after migration: %+v (err %v)", job, err)
	}
	// The table rebuild behind the cancelled state must keep every event
	// (foreign keys were off while the old table was dropped) and the
	// insertion order.
	for id, wantDetail := range map[string]string{"old-job": "job accepted", "done-job": "done"} {
		events := mustEvents(t, s, id)
		if len(events) != 1 || events[0].Detail != wantDetail {
			t.Errorf("events of %s after the rebuild = %+v", id, events)
		}
	}
	if done, _ := s.GetJob(context.Background(), "done-job"); done.Seq != 2 || done.State != StateCompleted {
		t.Errorf("done-job after rebuild = %+v", done)
	}
	if c, err := s.ClaimNext(context.Background(), noPoll); err != nil || c == nil || c.ID != "old-job" {
		t.Fatalf("existing row must stay claimable: %v %v", c, err)
	}
	if err := s.MarkCancelled(context.Background(), "old-job", "x"); !errors.Is(err, ErrNotCancellable) {
		t.Errorf("the rebuilt table must accept the new state machine: %v", err)
	}
}

// Job options round-trip through their JSON column; a job created without
// any reads back with defaults, as do rows from before the column existed.
func TestJobOptionsRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	plain := newJob(t, "meme")
	mustCreate(t, s, plain)
	tuned := newJob(t, "meme")
	tuned.Options = JobOptions{Cache: CacheRefresh}
	mustCreate(t, s, tuned)
	got, _ := s.GetJob(ctx, plain.ID)
	if !got.Options.IsZero() {
		t.Errorf("plain job options = %+v, want defaults", got.Options)
	}
	got, _ = s.GetJob(ctx, tuned.ID)
	if got.Options.Cache != CacheRefresh {
		t.Errorf("tuned job options = %+v", got.Options)
	}
	if _, err := decodeOptions("not json"); err == nil {
		t.Error("corrupt options must be reported, not ignored")
	}
}

// Cancellation ends a queued or target-waiting job; a job a worker holds
// right now is refused, a finished one is final. Cancelled jobs count as
// terminal for retention.
func TestMarkCancelled(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	queued := newJob(t, "meme")
	mustCreate(t, s, queued)
	if err := s.MarkCancelled(ctx, queued.ID, "cancelled by client a"); err != nil {
		t.Fatalf("cancel a queued job: %v", err)
	}
	got, _ := s.GetJob(ctx, queued.ID)
	if got.State != StateCancelled || got.CompletedAt == nil || got.ErrorCode != "" || got.NextAttemptAt != nil {
		t.Errorf("cancelled job = %+v", got)
	}
	if c, _ := s.ClaimNext(ctx, noPoll); c != nil {
		t.Errorf("a cancelled job must never be claimed, got %s", c.ID)
	}
	if err := s.MarkCancelled(ctx, queued.ID, "again"); !errors.Is(err, ErrTerminalState) {
		t.Errorf("second cancel: %v, want ErrTerminalState", err)
	}
	if err := s.MarkCompleted(ctx, queued.ID, 200, []byte(`{}`), "", "", "late"); !errors.Is(err, ErrTerminalState) {
		t.Errorf("late completion of a cancelled job: %v, want ErrTerminalState", err)
	}

	held := newJob(t, "meme")
	mustCreate(t, s, held)
	if c, _ := s.ClaimNext(ctx, noPoll); c == nil || c.ID != held.ID {
		t.Fatal("claim failed")
	}
	if err := s.MarkCancelled(ctx, held.ID, "x"); !errors.Is(err, ErrNotCancellable) {
		t.Errorf("cancel while resolving: %v, want ErrNotCancellable", err)
	}
	if err := s.SetResolved(ctx, held.ID, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkCancelled(ctx, held.ID, "x"); !errors.Is(err, ErrNotCancellable) {
		t.Errorf("cancel while forwarding: %v, want ErrNotCancellable", err)
	}
	if err := s.MarkAwaitingTarget(ctx, held.ID, "m-1", 202, nil, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkCancelled(ctx, held.ID, "cancelled while awaiting"); err != nil {
		t.Fatalf("cancel while awaiting the target: %v", err)
	}
	events := mustEvents(t, s, held.ID)
	if last := events[len(events)-1]; last.FromState != StateAwaitingTarget || last.ToState != StateCancelled || last.Detail != "cancelled while awaiting" {
		t.Errorf("last event = %+v", last)
	}
	time.Sleep(3 * time.Millisecond)
	ids, _, err := s.TerminalBefore(ctx, time.Now(), 10)
	if err != nil || len(ids) != 2 {
		t.Errorf("retention must include cancelled jobs: %v (err %v)", ids, err)
	}
	if counts, _ := s.CountByState(ctx); counts[StateCancelled] != 2 {
		t.Errorf("counts = %v", counts)
	}
	if !IsTerminal(StateCancelled) || IsTerminal(StateAwaitingTarget) {
		t.Error("IsTerminal is wrong")
	}
}

// The rescue asks for the cutoff per target: a job on a target with a long
// attempt deadline stays in flight while a job on the default target of
// the same age is rescued.
func TestRescueStuckUsesEachTargetsCutoff(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	slow, quick := newJob(t, "slow"), newJob(t, "quick")
	mustCreate(t, s, slow)
	mustCreate(t, s, quick)
	for range 2 {
		if c, _ := s.ClaimNext(ctx, noPoll); c == nil {
			t.Fatal("claim failed")
		}
	}
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	n, err := s.RescueStuck(ctx, func(target string) time.Time {
		if target == "slow" {
			return past // its deadline has not passed
		}
		return future
	})
	if err != nil || n != 1 {
		t.Fatalf("rescued %d (err %v), want 1", n, err)
	}
	if got, _ := s.GetJob(ctx, slow.ID); got.State != StateResolving {
		t.Errorf("slow job = %s, want still resolving", got.State)
	}
	if got, _ := s.GetJob(ctx, quick.ID); got.State != StateReceived {
		t.Errorf("quick job = %s, want rescued to received", got.State)
	}
}

// A client at its in-flight ceiling must not block other clients even when
// its jobs outrank theirs: its queued jobs would otherwise fill the
// candidate batch, every candidate would be skipped, and the worker would
// report no work while another client's job is due.
func TestClaimSkipsAClientAtItsCeilingForOthers(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ceiling := ClaimPolicy{MaxConcurrent: func(client string) int {
		if client == "batch" {
			return 1
		}
		return 0
	}}
	inFlight := newJob(t, "demo")
	inFlight.Client, inFlight.Priority = "batch", 10
	mustCreate(t, s, inFlight)
	if c, _ := s.ClaimNext(ctx, ceiling); c == nil || c.Client != "batch" {
		t.Fatal("the first batch job must be claimed")
	}
	for range 9 {
		j := newJob(t, "demo")
		j.Client, j.Priority = "batch", 10
		mustCreate(t, s, j)
	}
	other := newJob(t, "demo")
	other.Client = "frontend"
	mustCreate(t, s, other)
	got, err := s.ClaimNext(ctx, ceiling)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != other.ID {
		t.Fatalf("claimed %v, want the frontend job: the capped client filled the candidate batch", got)
	}
}

// BenchmarkClaimNextDeepQueue measures one claim against a deep queue, with and
// without a configured ceiling: the ceiling-aware path adds one in-flight
// count per claim and must stay noise next to the candidate ranking.
func BenchmarkClaimNextDeepQueue(b *testing.B) {
	for _, tc := range []struct {
		name   string
		policy ClaimPolicy
	}{
		{"no-ceiling", ClaimPolicy{}},
		{"ceiling", ClaimPolicy{MaxConcurrent: func(string) int { return 100 }}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			s := openBench(b)
			ctx := context.Background()
			for i := range 10000 {
				id, _ := NewID()
				j := &Job{ID: id, Client: "c" + strconv.Itoa(i%10), Target: "demo", MaxAttempts: 1 << 30, Payload: []byte(`{}`), Priority: i % 3}
				if created, _, err := s.CreateJob(ctx, j); err != nil || !created {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			for range b.N {
				j, err := s.ClaimNext(ctx, tc.policy)
				if err != nil || j == nil {
					b.Fatalf("claim: %v %v", j, err)
				}
				if err := s.Requeue(ctx, j.ID, time.Now(), "bench"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func openBench(b *testing.B) *Store {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	return s
}

// A claimed poll tick is leased: nobody claims the next tick before the
// lease ends, unless the worker reschedules explicitly when its tick is
// over; a reschedule of a job that already ended reports false.
func TestPollTickLeaseAndReschedule(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	j := newJob(t, "meme")
	mustCreate(t, s, j)
	if c, _ := s.ClaimNext(ctx, noPoll); c == nil {
		t.Fatal("claim failed")
	}
	if err := s.SetResolved(ctx, j.ID, []byte(`{}`), "resolved"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.MarkAwaitingTarget(ctx, j.ID, "m-1", 202, []byte(`{}`), now.Add(-time.Second), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	lease := ClaimPolicy{PollLease: func(string) time.Duration { return time.Hour }}
	tick, err := s.ClaimNext(ctx, lease)
	if err != nil || tick == nil || tick.State != StateAwaitingTarget {
		t.Fatalf("first tick not claimed: %v %v", tick, err)
	}
	if again, _ := s.ClaimNext(ctx, lease); again != nil {
		t.Fatalf("the next tick was claimed inside the lease: %+v", again)
	}
	if ok, err := s.ReschedulePoll(ctx, j.ID, time.Now().Add(-time.Millisecond)); err != nil || !ok {
		t.Fatalf("reschedule: ok=%v err=%v", ok, err)
	}
	if next, _ := s.ClaimNext(ctx, lease); next == nil {
		t.Fatal("the rescheduled tick must be claimable")
	}
	if err := s.MarkCancelled(ctx, j.ID, "cancelled"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ReschedulePoll(ctx, j.ID, time.Now()); err != nil || ok {
		t.Fatalf("a finished job must not be rescheduled: ok=%v err=%v", ok, err)
	}
}

// After a restart every waiting job is due within its target's spread from
// now, at a random point in it, whatever lease the previous process took.
func TestResetPollSchedulesSpreadsWithinOneInterval(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	var ids []string
	for range 5 {
		j := newJob(t, "meme")
		mustCreate(t, s, j)
		if c, _ := s.ClaimNext(ctx, noPoll); c == nil {
			t.Fatal("claim failed")
		}
		if err := s.SetResolved(ctx, j.ID, []byte(`{}`), "resolved"); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkAwaitingTarget(ctx, j.ID, "m", 202, []byte(`{}`), time.Now().Add(time.Hour), time.Now().Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, j.ID)
	}
	before := time.Now()
	n, err := s.ResetPollSchedules(ctx, func(string) time.Duration { return 500 * time.Millisecond })
	if err != nil || n != 5 {
		t.Fatalf("reset %d (err %v), want 5", n, err)
	}
	for _, id := range ids {
		got, _ := s.GetJob(ctx, id)
		if got.NextAttemptAt == nil || got.NextAttemptAt.Before(before.Add(-time.Second)) || got.NextAttemptAt.After(before.Add(600*time.Millisecond)) {
			t.Errorf("job %s next poll %v, want within half a second of the restart", id, got.NextAttemptAt)
		}
	}
}

// The retention scan must run over its index: a later change to the query
// or the schema that loses the index fails here, not in production.
func TestRetentionScanUsesTheTerminalIndex(t *testing.T) {
	s := openTest(t)
	rows, err := s.db.QueryContext(context.Background(), `EXPLAIN QUERY PLAN SELECT id, result_path FROM jobs
		WHERE state IN (?, ?, ?) AND completed_at < ?
		ORDER BY completed_at LIMIT ?`, StateCompleted, StateFailed, StateCancelled, ts(time.Now()), 10)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "jobs_terminal_ix") {
		t.Fatalf("retention scan does not use jobs_terminal_ix:\n%s", strings.Join(plan, "\n"))
	}
}

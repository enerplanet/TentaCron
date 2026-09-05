package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestScheduleLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	due := time.Date(2026, 9, 6, 4, 30, 0, 0, time.UTC)
	sc := &Schedule{ID: "s1", Client: "c1", Target: "demo", Payload: []byte(`{"a":1}`), Cron: "30 6 * * *", Timezone: "Europe/Berlin",
		Priority: 2, Options: JobOptions{Cache: CacheBypass}, NextRunAt: &due}
	if err := s.CreateSchedule(ctx, sc); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSchedule(ctx, &Schedule{ID: "s2", Client: "c2", Target: "demo", Payload: []byte(`{}`), Cron: "@daily", Timezone: "UTC", NextRunAt: &due}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSchedule(ctx, "s1")
	if err != nil || got.Client != "c1" || got.Cron != "30 6 * * *" || got.Timezone != "Europe/Berlin" || got.Priority != 2 ||
		got.Options.Cache != CacheBypass || got.NextRunAt == nil || !got.NextRunAt.Equal(due) || got.LastRunAt != nil || got.LastJobID != "" || string(got.Payload) != `{"a":1}` {
		t.Fatalf("schedule = %+v (%v)", got, err)
	}
	if all, _ := s.ListSchedules(ctx, "", 10); len(all) != 2 || all[0].ID != "s1" {
		t.Errorf("list all = %v", all)
	}
	if mine, _ := s.ListSchedules(ctx, "c2", 10); len(mine) != 1 || mine[0].ID != "s2" {
		t.Errorf("list c2 = %v", mine)
	}
	// Due: nothing before the due time, both at it.
	if d, _ := s.DueSchedules(ctx, due.Add(-time.Second), 10); len(d) != 0 {
		t.Errorf("due early = %v", d)
	}
	if d, _ := s.DueSchedules(ctx, due, 10); len(d) != 2 {
		t.Errorf("due = %d", len(d))
	}
	// Advance is a compare-and-set on the due time.
	next := due.Add(24 * time.Hour)
	ok, err := s.AdvanceSchedule(ctx, "s1", due, "job-1", next)
	if err != nil || !ok {
		t.Fatalf("advance = %v %v", ok, err)
	}
	if ok, _ := s.AdvanceSchedule(ctx, "s1", due, "job-dup", next.Add(time.Hour)); ok {
		t.Error("a second advance for the same due time must lose")
	}
	got, _ = s.GetSchedule(ctx, "s1")
	if !got.NextRunAt.Equal(next) || got.LastRunAt == nil || !got.LastRunAt.Equal(due) || got.LastJobID != "job-1" {
		t.Errorf("after advance = %+v", got)
	}
	if d, _ := s.DueSchedules(ctx, due, 10); len(d) != 1 || d[0].ID != "s2" {
		t.Errorf("due after advance = %v", d)
	}
	if err := s.DeleteSchedule(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSchedule(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get deleted = %v", err)
	}
	if err := s.DeleteSchedule(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete twice = %v", err)
	}
}

// The runs of a schedule are found by their key prefix; LIKE wildcards in
// the prefix are literal.
func TestListJobsByIdempotencyPrefix(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	due := time.Date(2026, 9, 6, 4, 30, 0, 0, time.UTC)
	for i, key := range []string{RunKey("sched_a", due), RunKey("sched_a", due.Add(time.Hour)), RunKey("schedXa", due), "other"} {
		id := "j" + string(rune('0'+i))
		if _, _, err := s.CreateJob(ctx, &Job{ID: id, Client: "c", IdempotencyKey: key, Target: "t", MaxAttempts: 1, Payload: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.ListJobs(ctx, ListFilter{IdempotencyPrefix: RunKeyPrefix("sched_a"), Limit: 10})
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs = %v (%v); the underscore must not match schedXa", runs, err)
	}
	if RunKey("s", due) != "schedule:s:2026-09-06T04:30:00Z" {
		t.Errorf("run key = %s", RunKey("s", due))
	}
}

// A terminal transition of a job with a callback enqueues exactly one
// delivery in the same transaction; attempts are recorded with a
// compare-and-set; deleting the job removes the delivery.
func TestTerminalTransitionEnqueuesCallback(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, id := range []string{"cb", "plain"} {
		job := &Job{ID: id, Client: "c", Target: "t", MaxAttempts: 1, Payload: []byte(`{}`)}
		if id == "cb" {
			job.CallbackURL = "https://hooks.example.com/x"
		}
		if _, _, err := s.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.GetDelivery(ctx, "cb"); !errors.Is(err, ErrNotFound) {
		t.Errorf("no delivery before the job is terminal: %v", err)
	}
	if err := s.MarkCancelled(ctx, "cb", "by test"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkCompleted(ctx, "plain", 200, nil, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDelivery(ctx, "plain"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a job without callback_url gets no delivery: %v", err)
	}
	d, err := s.GetDelivery(ctx, "cb")
	if err != nil || d.Event != "request.cancelled" || d.URL != "https://hooks.example.com/x" || d.State() != DeliveryPending || d.NextAttemptAt == nil {
		t.Fatalf("delivery = %+v (%v)", d, err)
	}
	if due, _ := s.DueDeliveries(ctx, time.Now(), 10); len(due) != 1 || due[0].JobID != "cb" {
		t.Errorf("due = %v", due)
	}
	status := 500
	later := time.Now().Add(time.Hour)
	if err := s.RecordAttempt(ctx, "cb", 0, &status, "HTTP 500", false, &later); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAttempt(ctx, "cb", 0, &status, "stale", false, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("a stale attempt count must not overwrite: %v", err)
	}
	if due, _ := s.DueDeliveries(ctx, time.Now(), 10); len(due) != 0 {
		t.Errorf("not due until the backoff passed: %v", due)
	}
	ok := 200
	if err := s.RecordAttempt(ctx, "cb", 1, &ok, "", true, nil); err != nil {
		t.Fatal(err)
	}
	d, _ = s.GetDelivery(ctx, "cb")
	if d.State() != DeliveryDelivered || d.Attempts != 2 || *d.LastStatus != 200 || d.LastError != "" {
		t.Errorf("delivered = %+v", d)
	}
	if err := s.DeleteJobs(ctx, []string{"cb"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDelivery(ctx, "cb"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delivery must be deleted with its job: %v", err)
	}
	// Giving up leaves a failed delivery.
	if _, _, err := s.CreateJob(ctx, &Job{ID: "gone", Client: "c", Target: "t", MaxAttempts: 1, Payload: []byte(`{}`), CallbackURL: "https://hooks.example.com/y"}); err != nil {
		t.Fatal(err)
	}
	_ = s.MarkFailed(ctx, "gone", "target_error", "x")
	st := 410
	_ = s.RecordAttempt(ctx, "gone", 0, &st, "HTTP 410", false, nil)
	if d, _ := s.GetDelivery(ctx, "gone"); d.State() != DeliveryFailed || d.Event != "request.failed" {
		t.Errorf("given up = %+v", d)
	}
}

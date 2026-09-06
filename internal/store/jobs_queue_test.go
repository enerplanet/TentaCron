package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func queuedJob(t *testing.T, client, key string) *Job {
	t.Helper()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	return &Job{ID: id, Client: client, Target: "demo", MaxAttempts: 1, Payload: []byte(`{}`), IdempotencyKey: key}
}

// A key's cap counts its non-terminal jobs: the one past it is refused,
// a finished job frees its place, a replay is never refused, other clients
// and an uncapped key are unaffected.
func TestCreateJobRespectsTheQueueCap(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	first := queuedJob(t, "a", "k-1")
	if _, _, err := s.CreateJob(ctx, first, 2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateJob(ctx, queuedJob(t, "a", ""), 2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateJob(ctx, queuedJob(t, "a", ""), 2); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("third job: err = %v, want ErrQueueFull", err)
	}
	replay := queuedJob(t, "a", "k-1")
	replay.ID = first.ID
	if created, stored, err := s.CreateJob(ctx, replay, 2); err != nil || created || stored.ID != first.ID {
		t.Fatalf("replay at the cap: created=%v stored=%v err=%v, want the stored job", created, stored, err)
	}
	if _, _, err := s.CreateJob(ctx, queuedJob(t, "b", ""), 2); err != nil {
		t.Fatalf("another client's first job: %v", err)
	}
	if _, _, err := s.CreateJob(ctx, queuedJob(t, "a", ""), 0); err != nil {
		t.Fatalf("cap 0 must not refuse: %v", err)
	}
	if err := s.MarkCompleted(ctx, first.ID, 200, nil, "", "", "done"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateJob(ctx, queuedJob(t, "a", ""), 3); err != nil {
		t.Fatalf("a finished job must free its place: %v", err)
	}
}

// The count runs in the insert's own transaction, so concurrent
// submissions land exactly cap jobs, never one more.
func TestCreateJobQueueCapIsAtomicUnderConcurrentSubmits(t *testing.T) {
	s := openTest(t)
	const cap, submits = 5, 24
	var (
		wg              sync.WaitGroup
		mu              sync.Mutex
		created, capped int
	)
	for range submits {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.CreateJob(context.Background(), queuedJob(t, "a", ""), cap)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case errors.Is(err, ErrQueueFull):
				capped++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if created != cap || capped != submits-cap {
		t.Fatalf("created %d, refused %d; want %d and %d", created, capped, cap, submits-cap)
	}
}

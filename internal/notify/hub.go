// Package notify wakes long-polling API requests when a job reaches a
// terminal state. It is in-process only, which is exactly right for a
// single-instance service: every transition happens in this process. A nil
// *Hub is a valid no-op, so packages using it need no wiring in tests.
package notify

import "sync"

// Hub fans out per-job terminal notifications to waiting requests.
type Hub struct {
	mu      sync.Mutex
	waiters map[string][]chan struct{}
}

// New builds an empty hub.
func New() *Hub { return &Hub{waiters: map[string][]chan struct{}{}} }

// Wait returns a channel that is closed on the next Notify for id. Register
// before reading the job's state, so a transition between the read and the
// wait is never missed; the caller re-reads the job after the wake-up. On a
// nil hub the returned channel is nil and never fires, which leaves a caller
// with its own fallback polling.
func (h *Hub) Wait(id string) <-chan struct{} {
	if h == nil {
		return nil
	}
	ch := make(chan struct{})
	h.mu.Lock()
	h.waiters[id] = append(h.waiters[id], ch)
	h.mu.Unlock()
	return ch
}

// Notify wakes every waiter for id and forgets them.
func (h *Hub) Notify(id string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	chans := h.waiters[id]
	delete(h.waiters, id)
	h.mu.Unlock()
	for _, ch := range chans {
		close(ch)
	}
}

// Forget drops one waiter that stopped waiting (its request timed out or
// the client went away), so abandoned channels do not accumulate.
func (h *Hub) Forget(id string, ch <-chan struct{}) {
	if h == nil || ch == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	remaining := h.waiters[id][:0]
	for _, c := range h.waiters[id] {
		if c != ch {
			remaining = append(remaining, c)
		}
	}
	if len(remaining) == 0 {
		delete(h.waiters, id)
	} else {
		h.waiters[id] = remaining
	}
}

// Pending reports how many waiters are registered, for tests.
func (h *Hub) Pending() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, chans := range h.waiters {
		n += len(chans)
	}
	return n
}

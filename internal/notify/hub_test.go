package notify

import (
	"testing"
	"time"
)

func TestNotifyWakesEveryWaiterOnce(t *testing.T) {
	h := New()
	a, b := h.Wait("job-1"), h.Wait("job-1")
	other := h.Wait("job-2")
	if h.Pending() != 3 {
		t.Fatalf("pending = %d", h.Pending())
	}
	h.Notify("job-1")
	for name, ch := range map[string]<-chan struct{}{"a": a, "b": b} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Errorf("waiter %s not woken", name)
		}
	}
	select {
	case <-other:
		t.Error("a waiter on another job must not be woken")
	default:
	}
	if h.Pending() != 1 {
		t.Errorf("pending after notify = %d, want the other job's waiter only", h.Pending())
	}
	h.Notify("job-1") // nobody waiting: no panic, no effect
}

func TestForgetDropsOnlyThatWaiter(t *testing.T) {
	h := New()
	keep, drop := h.Wait("j"), h.Wait("j")
	h.Forget("j", drop)
	if h.Pending() != 1 {
		t.Fatalf("pending = %d", h.Pending())
	}
	h.Notify("j")
	select {
	case <-keep:
	case <-time.After(time.Second):
		t.Error("kept waiter not woken")
	}
	select {
	case <-drop:
		t.Error("forgotten waiter must not be closed")
	default:
	}
}

func TestNilHubIsANoOp(t *testing.T) {
	var h *Hub
	if ch := h.Wait("x"); ch != nil {
		t.Error("nil hub must hand out a nil channel")
	}
	h.Notify("x")
	h.Forget("x", nil)
	if h.Pending() != 0 {
		t.Error("nil hub has no waiters")
	}
}

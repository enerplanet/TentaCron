package api

import (
	"net/http"
	"sync/atomic"
	"testing"
)

type longPollCounter struct{ started, ended atomic.Int32 }

func (c *longPollCounter) LongPollStarted() { c.started.Add(1) }
func (c *longPollCounter) LongPollEnded()   { c.ended.Add(1) }

// A status read with ?wait= is reported for exactly the time it waits: one
// start and one end per long-poll, none for a plain read.
func TestLongPollsAreReportedWhileTheyWait(t *testing.T) {
	e := newEnv(t)
	var counter longPollCounter
	e.server.WithMetrics(&counter)
	created := decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests", validBody, nil))
	auth := map[string]string{"X-API-Key": "valid-key"}
	if rec := e.do(t, "GET", "/v1/requests/"+created.ID, "", auth); rec.Code != http.StatusOK {
		t.Fatalf("plain read: %d", rec.Code)
	}
	if counter.started.Load() != 0 {
		t.Fatalf("a plain read must not count as a long-poll (started %d)", counter.started.Load())
	}
	if rec := e.do(t, "GET", "/v1/requests/"+created.ID+"?wait=50ms", "", auth); rec.Code != http.StatusOK {
		t.Fatalf("long-poll: %d %s", rec.Code, rec.Body.String())
	}
	if s, n := counter.started.Load(), counter.ended.Load(); s != 1 || n != 1 {
		t.Fatalf("started %d, ended %d; want 1 and 1", s, n)
	}
}

package api

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/enerplanet/tentacron/internal/config"
)

// A key at its max_queued cap is answered 429 queue_full with a Retry-After
// of one worker poll interval; a batch has each item refused individually.
func TestCreateRefusesAKeyAtItsQueueCap(t *testing.T) {
	e := newEnvWith(t, func(cfg *config.Config) { cfg.Auth.APIKeys[0].MaxQueued = 1 }, nil)
	if rec := e.do(t, "POST", "/v1/requests", validBody, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("first request: %d %s", rec.Code, rec.Body.String())
	}
	rec := e.do(t, "POST", "/v1/requests", validBody, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %s)", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != CodeQueueFull {
		t.Errorf("error code = %q, want %q", got, CodeQueueFull)
	}
	if secs, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || secs < 1 {
		t.Errorf("Retry-After = %q, want a positive number of seconds", rec.Header().Get("Retry-After"))
	}

	batch := `{"requests":[{"target":"meme","payload":{"time-series":[]}},{"target":"meme","payload":{"time-series":[]}}]}`
	rec = e.do(t, "POST", "/v1/requests/batch", batch, map[string]string{"X-API-Key": "valid-key"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("batch status = %d, want 400 as no item was accepted (body %s)", rec.Code, rec.Body.String())
	}
	items := decodeBody[map[string][]batchResult](t, rec)["items"]
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	for i, it := range items {
		if it.Error == nil || it.Error.Code != CodeQueueFull {
			t.Errorf("item %d = %+v, want queue_full", i, it)
		}
	}
}

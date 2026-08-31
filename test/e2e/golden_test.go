package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/store"
)

// TestResolutionAndCaching covers the resolvent engine's end-to-end edges:
// deduplication, cache reuse across requests, and resource-API misbehavior.
func TestResolutionAndCaching(t *testing.T) {
	runScenarios(t, resolutionScenarios)
}

// TestTargetProtocol covers the forward/poll protocol edges against the
// target API: failures, deadlines, malformed accept responses, redaction.
func TestTargetProtocol(t *testing.T) {
	runScenarios(t, targetProtocolScenarios)
}

// TestAPIContract covers the HTTP surface: validation errors, idempotency
// semantics, authenticated reads, and the list endpoint.
func TestAPIContract(t *testing.T) {
	runScenarios(t, apiContractScenarios)
}

// TestGoldenCorpusMatchesScenarios keeps testdata/golden/ and the scenario
// corpus in lockstep: a removed or renamed scenario must not leave a zombie
// golden behind, and scenario names must be unique.
func TestGoldenCorpusMatchesScenarios(t *testing.T) {
	names := map[string]bool{}
	for _, sc := range allScenarios() {
		if names[sc.name] {
			t.Fatalf("duplicate scenario name %q", sc.name)
		}
		names[sc.name] = true
	}
	files, err := filepath.Glob(filepath.Join("testdata", "golden", "*.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".golden.json")
		if !names[name] {
			if *update {
				_ = os.Remove(f)
				continue
			}
			t.Errorf("stale golden %s: no scenario %q (remove it or run `make golden-update`)", f, name)
		}
	}
}

var resolutionScenarios = []scenario{
	{
		// Two byte-identical resolvents plus one distinct: exactly two
		// resource calls, all three slots substituted.
		name: "duplicate-resolvents-single-fetch",
		run: func(t *testing.T, h *harness) {
			payload := `{"time-series":[
				{"type":"resolvent-pv1","lat":48.83,"capacity_kw":12.5},
				{"type":"resolvent-pv1","lat":48.83,"capacity_kw":12.5},
				{"type":"resolvent-wind","hub_height_m":120}
			]}`
			id := h.post("submit payload with duplicate resolvents", requestBody("buem", payload), nil)
			h.await("final state", id)
			h.forwarded("both duplicate slots substituted from one fetch", "direct")
			h.counts()
		},
	},
	{
		// A second request with an identical resolvent is served from the
		// series cache — the resource API is hit exactly once overall.
		name: "series-cache-reuse",
		run: func(t *testing.T, h *harness) {
			payload := `{"time-series":[{"type":"resolvent-pv1","lat":48.83}]}`
			first := h.post("first request", requestBody("buem", payload), nil)
			h.await("first final state", first)
			h.resourceReceived("resolvent object and key the resource API received")
			second := h.post("second request, identical resolvent", requestBody("buem", payload), nil)
			h.await("second final state", second)
			h.events("second job's audit trail (resolved from cache)", second)
			h.counts()
		},
	},
	{
		name: "unknown-resolvent",
		run: func(t *testing.T, h *harness) {
			payload := `{"time-series":[{"type":"resolvent-pv1"},{"type":"resolvent-tidal"}]}`
			id := h.post("submit payload with unconfigured resolvent type", requestBody("buem", payload), nil)
			h.await("final state (failed before any resource call)", id)
			h.events("audit trail", id)
			h.counts()
		},
	},
	{
		// 500, 500, then success: transient errors retry with backoff.
		name: "resource-flaky-retries",
		fakes: fakes{resource: func(call int64) reply {
			if call <= 2 {
				return reply{500, `{"error":"flaky"}`, ""}
			}
			return reply{200, defaultSeries, ""}
		}},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("buem", `{"time-series":[{"type":"resolvent-pv1"}]}`), nil)
			h.await("final state (completed on third attempt)", id)
			h.events("audit trail with two backoff requeues", id)
			h.counts()
		},
	},
	{
		name: "resource-persistent-500",
		fakes: fakes{resource: func(int64) reply {
			return reply{500, `{"error":"down"}`, ""}
		}},
		mod: func(cfg *config.Config) { cfg.Worker.MaxAttempts = 2 },
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("buem", `{"time-series":[{"type":"resolvent-pv1"}]}`), nil)
			h.await("final state (attempts exhausted)", id)
			h.events("audit trail", id)
		},
	},
	{
		// A resource 4xx is permanent: exactly one wire call, no retry.
		name: "resource-400-permanent",
		fakes: fakes{resource: func(int64) reply {
			return reply{400, `{"error":"bad params"}`, ""}
		}},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("buem", `{"time-series":[{"type":"resolvent-pv1"}]}`), nil)
			h.await("final state (failed permanently, no retry)", id)
			h.events("audit trail", id)
			h.counts()
		},
	},
	{
		// "null" is valid JSON but not a series; must fail cleanly with
		// invalid_resource_response and must not poison the cache.
		name:  "resource-null-body",
		fakes: fakes{resource: func(int64) reply { return reply{200, `null`, ""} }},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("buem", `{"time-series":[{"type":"resolvent-pv1"}]}`), nil)
			h.await("final state", id)
			h.events("audit trail", id)
		},
	},
	{
		name:  "resource-array-body",
		fakes: fakes{resource: func(int64) reply { return reply{200, `[1,2,3]`, ""} }},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("buem", `{"time-series":[{"type":"resolvent-pv1"}]}`), nil)
			h.await("final state", id)
			h.events("audit trail", id)
		},
	},
}

var targetProtocolScenarios = []scenario{
	{
		// A transient target 5xx requeues the job; the retry re-resolves
		// from the series cache and succeeds.
		name: "target-flaky-retry",
		fakes: fakes{direct: func(call int64) reply {
			if call == 1 {
				return reply{500, `{"error":"busy"}`, ""}
			}
			return reply{200, `{"ok":true}`, ""}
		}},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("buem", `{"time-series":[{"type":"resolvent-pv1"}]}`), nil)
			h.await("final state (completed on second attempt)", id)
			h.events("audit trail: target 500 requeued, retry resolves from cache", id)
			h.counts()
		},
	},
	{
		// The target echoes credentials back in its error body; the
		// stored/served error message must show them redacted.
		name: "target-400-secret-echo",
		fakes: fakes{direct: func(int64) reply {
			return reply{400, `{"error":"rejected key ` + buemSecret + ` for scenario"}`, ""}
		}},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("buem", `{"time-series":[]}`), nil)
			h.await("final state (error message redacted)", id)
		},
	},
	{
		name: "target-job-failed",
		fakes: fakes{poll: func(int64) reply {
			return reply{200, `{"status":"failed","reason":"solver exploded"}`, ""}
		}},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("meme", `{"model":{"timeseries":{}}}`), nil)
			h.await("final state", id)
			h.events("audit trail", id)
		},
	},
	{
		// A transient 500 on one status poll leaves no scar: the next tick
		// completes the job as if nothing happened.
		name: "poll-status-flaky-then-done",
		fakes: fakes{poll: func(call int64) reply {
			switch call {
			case 1:
				return reply{500, `{"error":"blip"}`, ""}
			case 2:
				return reply{200, `{"status":"done"}`, ""}
			default:
				return reply{200, `{"status":"done","objective":1234.5}`, ""}
			}
		}},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("meme", `{"model":{"timeseries":{}}}`), nil)
			h.await("final state (completed despite a transient status flake)", id)
			h.events("audit trail (no scar from the flake)", id)
		},
	},
	{
		// A transient 500 on the result fetch parks the job for the next
		// tick, which fetches successfully.
		name: "result-fetch-flaky-then-success",
		fakes: fakes{poll: func(call int64) reply {
			switch call {
			case 1, 3:
				return reply{200, `{"status":"done"}`, ""}
			case 2:
				return reply{500, `{"error":"blip"}`, ""}
			default:
				return reply{200, `{"status":"done","objective":1234.5}`, ""}
			}
		}},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("meme", `{"model":{"timeseries":{}}}`), nil)
			h.await("final state (completed despite a transient result flake)", id)
			h.events("audit trail (no scar from the flake)", id)
		},
	},
	{
		name:  "poll-deadline-exceeded",
		fakes: fakes{poll: func(int64) reply { return reply{200, `{"status":"running"}`, ""} }},
		mod: func(cfg *config.Config) {
			meme := cfg.Targets["meme"]
			meme.Response.Poll.Timeout = config.Duration(50 * time.Millisecond)
			cfg.Targets["meme"] = meme
		},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("meme", `{"model":{"timeseries":{}}}`), nil)
			h.await("final state (target never finished)", id)
		},
	},
	{
		// The deadline exists for unfinished jobs: a job whose "done" is
		// only observed after the deadline still completes.
		name: "finished-past-deadline",
		mod: func(cfg *config.Config) {
			meme := cfg.Targets["meme"]
			meme.Response.Poll.Timeout = config.Duration(time.Millisecond)
			cfg.Targets["meme"] = meme
		},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("meme", `{"model":{"timeseries":{}}}`), nil)
			h.await("final state (completed despite the elapsed deadline)", id)
		},
	},
	{
		// The documented awaiting_target state, observed mid-flight.
		name:  "awaiting-target-mid-flight",
		fakes: fakes{poll: func(int64) reply { return reply{200, `{"status":"running"}`, ""} }},
		mod: func(cfg *config.Config) {
			meme := cfg.Targets["meme"]
			meme.Response.Poll.Timeout = config.Duration(60 * time.Second)
			cfg.Targets["meme"] = meme
		},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("meme", `{"model":{"timeseries":{}}}`), nil)
			h.awaitState("status while polling", id, store.StateAwaitingTarget)
			auth := map[string]string{"X-API-Key": clientKey}
			h.get("result not available while polling", "/v1/requests/"+id+"/result", auth)
			h.get("list filtered by awaiting_target", "/v1/requests?state=awaiting_target", auth)
		},
	},
	{
		name:  "accept-missing-job-id",
		fakes: fakes{accept: func(int64) reply { return reply{202, `{}`, ""} }},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("meme", `{"model":{"timeseries":{}}}`), nil)
			h.await("final state", id)
			h.countsWithPolls() // the rejected accept must produce zero polls
		},
	},
	{
		// The target-supplied job id feeds URL templates and must be
		// rejected when it carries URL metacharacters.
		name: "unsafe-job-id",
		fakes: fakes{accept: func(int64) reply {
			return reply{202, `{"job_id":"x/../../admin?full=1"}`, ""}
		}},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("meme", `{"model":{"timeseries":{}}}`), nil)
			h.await("final state", id)
			h.countsWithPolls() // the unsafe id must never reach the wire
		},
	},
	{
		// A permanently broken status endpoint fails the job fast.
		name:  "poll-status-404",
		fakes: fakes{poll: func(int64) reply { return reply{404, `{"error":"no such job"}`, ""} }},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("meme", `{"model":{"timeseries":{}}}`), nil)
			h.await("final state (status endpoint permanently broken)", id)
			h.events("audit trail", id)
		},
	},
	{
		name: "result-fetch-404",
		fakes: fakes{poll: func(call int64) reply {
			if call == 1 {
				return reply{200, `{"status":"done"}`, ""}
			}
			return reply{404, `{"error":"result purged"}`, ""}
		}},
		// A wide poll interval so a second tick can never overlap the
		// first one's in-flight fetch and race the recorded error text.
		mod: func(cfg *config.Config) {
			meme := cfg.Targets["meme"]
			meme.Response.Poll.Interval = config.Duration(500 * time.Millisecond)
			cfg.Targets["meme"] = meme
		},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("meme", `{"model":{"timeseries":{}}}`), nil)
			h.await("final state (result gone at the target)", id)
		},
	},
	{
		// A binary (zip) result is stored as a file and streamed via
		// /result with its content type.
		name: "binary-result-file",
		fakes: fakes{poll: func(call int64) reply {
			if call == 1 {
				return reply{200, `{"status":"done"}`, ""}
			}
			return reply{200, "PK\x03\x04golden-bundle-bytes", "application/zip"}
		}},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("meme", `{"model":{"timeseries":{}}}`), nil)
			h.await("final state (result referenced via href)", id)
			h.get("download the result", "/v1/requests/"+id+"/result",
				map[string]string{"X-API-Key": clientKey})
		},
	},
	{
		// Direct mode trusts the bytes over the header: a binary body lands
		// as a file, and the upstream Content-Type is discarded in favor of
		// application/octet-stream — a pinned quirk.
		name: "direct-binary-result",
		fakes: fakes{direct: func(int64) reply {
			return reply{200, "PK\x03\x04direct-bundle-bytes", "application/zip"}
		}},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("buem", `{"time-series":[]}`), nil)
			h.await("final state (file result; upstream content type discarded)", id)
			h.get("download", "/v1/requests/"+id+"/result",
				map[string]string{"X-API-Key": clientKey})
		},
	},
	{
		// Numbers flow back too: the target's response embeds integers
		// above 2^53 which must reach the client byte-exact, inline and
		// via the /result endpoint.
		name: "big-numbers-in-result",
		fakes: fakes{direct: func(int64) reply {
			return reply{200, `{"objective":9007199254740993,"meter_id":1234567890123456789}`, ""}
		}},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("buem", `{"time-series":[]}`), nil)
			h.await("final state (digits exact in embedded result)", id)
			h.get("download inline JSON result", "/v1/requests/"+id+"/result",
				map[string]string{"X-API-Key": clientKey})
		},
	},
}

var apiContractScenarios = []scenario{
	{
		name: "validation-errors",
		run: func(t *testing.T, h *harness) {
			h.postRaw("no content type", requestBody("buem", `{}`), "", nil)
			h.postRaw("wrong content type", requestBody("buem", `{}`), "text/plain", nil)
			h.postRaw("invalid json", `{"api_key":`, "application/json", nil)
			h.postRaw("trailing garbage", requestBody("buem", `{}`)+`garbage`, "application/json", nil)
			h.postRaw("missing api_key", `{"target":"buem","payload":{}}`, "application/json", nil)
			h.postRaw("missing target", `{"api_key":"`+clientKey+`","payload":{}}`, "application/json", nil)
			h.postRaw("missing payload", `{"api_key":"`+clientKey+`","target":"buem"}`, "application/json", nil)
			h.postRaw("payload not an object", `{"api_key":"`+clientKey+`","target":"buem","payload":[1]}`, "application/json", nil)
			h.postRaw("wrong api key", `{"api_key":"nope","target":"buem","payload":{}}`, "application/json", nil)
			h.postRaw("unknown target", requestBody("hydra", `{}`), "application/json", nil)
			h.postRaw("oversized body", requestBody("buem", `{"blob":"`+strings.Repeat("x", 5000)+`"}`), "application/json", nil)
			h.post("content type with charset parameter is accepted",
				requestBody("buem", `{"time-series":[]}`),
				map[string]string{"Content-Type": "application/json; charset=utf-8"})
		},
	},
	{
		name: "idempotency",
		run: func(t *testing.T, h *harness) {
			payload := `{"time-series":[]}`
			hdr := map[string]string{"Idempotency-Key": "golden-key-1"}
			id := h.post("first submit with Idempotency-Key", requestBody("buem", payload), hdr)
			// Let the job finish before replaying: the replay echoes the
			// stored job's *current* state, which would otherwise race
			// between received/resolving/completed.
			h.await("first job finishes", id)
			h.post("identical resubmit returns the same job (in its current state)",
				requestBody("buem", payload), hdr)
			h.postRaw("same key, different payload → conflict",
				requestBody("buem", `{"time-series":[1]}`), "application/json", hdr)
			h.post("same key from another client → independent job",
				strings.Replace(requestBody("buem", payload), clientKey, secondClientKey, 1), hdr)
		},
	},
	{
		// Deliberately pins that reads are NOT owner-scoped: any configured
		// API key can read any job. A future scoping decision must show up
		// here as a reviewable golden diff.
		name: "cross-client-read",
		run: func(t *testing.T, h *harness) {
			id := h.post("client A submits", requestBody("buem", `{"time-series":[]}`), nil)
			h.await("job completes", id)
			auth2 := map[string]string{"X-API-Key": secondClientKey}
			h.get("client B reads A's job status", "/v1/requests/"+id, auth2)
			h.get("client B streams A's result", "/v1/requests/"+id+"/result", auth2)
			h.get("client B lists jobs", "/v1/requests?limit=5", auth2)
		},
	},
	{
		name: "read-endpoint-errors",
		run: func(t *testing.T, h *harness) {
			auth := map[string]string{"X-API-Key": clientKey}
			h.get("status without api key", "/v1/requests/0123456789abcdef0123456789abcdef", nil)
			h.get("status for unknown id", "/v1/requests/0123456789abcdef0123456789abcdef", auth)
			h.get("unknown route", "/v2/other", auth)
			h.get("list with unknown state filter", "/v1/requests?state=bogus", auth)
			h.get("list with invalid limit", "/v1/requests?limit=-1", auth)
		},
	},
	{
		name: "list-failed-jobs",
		run: func(t *testing.T, h *harness) {
			id := h.post("submit a job that will fail",
				requestBody("buem", `{"time-series":[{"type":"resolvent-tidal"}]}`), nil)
			h.await("final state", id)
			// A distinct created_at millisecond, so newest-first ordering
			// never falls back to the insertion tiebreaker.
			time.Sleep(2 * time.Millisecond)
			second := h.post("submit a second job that will fail",
				requestBody("buem", `{"time-series":[{"type":"resolvent-tidal"}]}`), nil)
			h.await("second final state", second)
			h.get("result is not available for a failed job", "/v1/requests/"+id+"/result",
				map[string]string{"X-API-Key": clientKey})
			h.get("list failed jobs, newest first", "/v1/requests?state=failed&limit=5",
				map[string]string{"X-API-Key": clientKey})
		},
	},
}

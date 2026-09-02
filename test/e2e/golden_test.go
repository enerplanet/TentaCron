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
			id := h.post("submit payload with duplicate resolvents", requestBody("demo", payload), nil)
			h.await("final state", id)
			h.forwarded("both duplicate slots substituted from one fetch", "demo")
			h.counts()
		},
	},
	{
		// A second request with an identical resolvent is served from the
		// series cache — the resource API is hit exactly once overall.
		name: "series-cache-reuse",
		run: func(t *testing.T, h *harness) {
			payload := `{"time-series":[{"type":"resolvent-pv1","lat":48.83}]}`
			first := h.post("first request", requestBody("demo", payload), nil)
			h.await("first final state", first)
			h.resourceReceived("resolvent object and key the resource API received")
			second := h.post("second request, identical resolvent", requestBody("demo", payload), nil)
			h.await("second final state", second)
			h.events("second job's audit trail (resolved from cache)", second)
			h.counts()
		},
	},
	{
		name: "unknown-resolvent",
		run: func(t *testing.T, h *harness) {
			payload := `{"time-series":[{"type":"resolvent-pv1"},{"type":"resolvent-tidal"}]}`
			id := h.post("submit payload with unconfigured resolvent type", requestBody("demo", payload), nil)
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
			id := h.post("submit", requestBody("demo", `{"time-series":[{"type":"resolvent-pv1"}]}`), nil)
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
			id := h.post("submit", requestBody("demo", `{"time-series":[{"type":"resolvent-pv1"}]}`), nil)
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
			id := h.post("submit", requestBody("demo", `{"time-series":[{"type":"resolvent-pv1"}]}`), nil)
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
			id := h.post("submit", requestBody("demo", `{"time-series":[{"type":"resolvent-pv1"}]}`), nil)
			h.await("final state", id)
			h.events("audit trail", id)
		},
	},
	{
		name:  "resource-array-body",
		fakes: fakes{resource: func(int64) reply { return reply{200, `[1,2,3]`, ""} }},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("demo", `{"time-series":[{"type":"resolvent-pv1"}]}`), nil)
			h.await("final state", id)
			h.events("audit trail", id)
		},
	},
	{
		// Both resolvents fail concurrently; the frozen error message names
		// the FIRST one in document order — attribution is deterministic,
		// never the scheduling race of whichever failure landed first.
		name: "multi-resolvent-failure-attribution",
		fakes: fakes{resource: func(int64) reply {
			return reply{400, `{"error":"bad params"}`, ""}
		}},
		run: func(t *testing.T, h *harness) {
			payload := `{"time-series":[
				{"type":"resolvent-pv1","site":"a"},
				{"type":"resolvent-wind","site":"b"}
			]}`
			id := h.post("submit payload whose resolvents both fail", requestBody("demo", payload), nil)
			h.await("final state (error names the first resolvent)", id)
		},
	},
	{
		// GET resolvents against the verified city2tabula and ignis
		// contracts: object fields map onto query parameters and the {code}
		// path template, and response_path "0" selects the single matched
		// building out of city2tabula's list response. The frozen request
		// lines are the proof of the URL mapping.
		name: "get-resolvents-city2tabula-ignis",
		run: func(t *testing.T, h *harness) {
			payload := `{"time-series":[
				{"type":"resolvent-city2tabula","country":"germany","osm_ids":["123456","789012"]},
				{"type":"resolvent-ignis","code":"DE.N.SFH.04.Gen.ReEx.001.001"}
			]}`
			id := h.post("submit payload with city2tabula and ignis resolvents", requestBody("demo", payload), nil)
			h.await("final state", id)
			h.resourceRequests("exact GET requests the resource APIs received")
			h.forwarded("building attributes and TABULA data substituted", "demo")
			h.counts()
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
			id := h.post("submit", requestBody("demo", `{"time-series":[{"type":"resolvent-pv1"}]}`), nil)
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
			return reply{400, `{"error":"rejected key ` + demoSecret + ` for scenario"}`, ""}
		}},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("demo", `{"time-series":[]}`), nil)
			h.await("final state (error message redacted)", id)
		},
	},
	{
		name: "target-job-failed",
		fakes: fakes{status: func(int64) reply {
			return reply{200, `{"id":"m-golden-1","state":"failed","reason":"solver exploded"}`, ""}
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
		fakes: fakes{status: func(call int64) reply {
			if call == 1 {
				return reply{500, `{"error":"blip"}`, ""}
			}
			return reply{200, `{"id":"m-golden-1","state":"succeeded"}`, ""}
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
		fakes: fakes{result: func(call int64) reply {
			if call == 1 {
				return reply{500, `{"error":"blip"}`, ""}
			}
			return reply{200, `{"id":"m-golden-1","state":"succeeded","objective":1234.5}`, ""}
		}},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("meme", `{"model":{"timeseries":{}}}`), nil)
			h.await("final state (completed despite a transient result flake)", id)
			h.events("audit trail (no scar from the flake)", id)
		},
	},
	{
		name:  "poll-deadline-exceeded",
		fakes: fakes{status: func(int64) reply { return reply{200, `{"id":"m-golden-1","state":"running"}`, ""} }},
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
		fakes: fakes{status: func(int64) reply { return reply{200, `{"id":"m-golden-1","state":"running"}`, ""} }},
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
			return reply{202, `{"id":"x/../../admin?full=1","state":"queued"}`, ""}
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
		fakes: fakes{status: func(int64) reply { return reply{404, `{"error":"no such job"}`, ""} }},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("meme", `{"model":{"timeseries":{}}}`), nil)
			h.await("final state (status endpoint permanently broken)", id)
			h.events("audit trail", id)
		},
	},
	{
		name: "result-fetch-404",
		fakes: fakes{result: func(int64) reply {
			return reply{404, `{"error":"result purged"}`, ""}
		}},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("meme", `{"model":{"timeseries":{}}}`), nil)
			h.await("final state (result gone at the target)", id)
		},
	},
	{
		// A binary (zip) result is stored as a file and streamed via
		// /result with its content type.
		name: "binary-result-file",
		fakes: fakes{result: func(int64) reply {
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
			id := h.post("submit", requestBody("demo", `{"time-series":[]}`), nil)
			h.await("final state (file result; upstream content type discarded)", id)
			h.get("download", "/v1/requests/"+id+"/result",
				map[string]string{"X-API-Key": clientKey})
		},
	},
	{
		// buem-gateway reports per-building failures INSIDE a 200 response
		// (verified against its handler: writeJSON with the default status;
		// a missing envelope fails that building's entry, not the batch).
		// For tentacron that is a COMPLETED job whose stored result carries
		// the error entries — partial building failures are result data,
		// never a job failure. This golden keeps that semantic from being
		// "fixed" into a target_error later.
		name: "buem-partial-building-errors",
		fakes: fakes{
			gateway: func(int64) reply {
				return reply{200, `[` +
					`{"id":"b-1","buem":{"thermal_load_profile":{"summary":{"heating":{"total":{"value":12345.6,"unit":"kWh"}}}}}},` +
					`{"id":"b-2","error":"envelope missing or empty"}]`, ""}
			},
		},
		run: func(t *testing.T, h *harness) {
			// The weather resolvent uses the flat point-query fields of the
			// GET contract; a nested location object would fail the job
			// before the gateway is ever reached (see get-resolvent-nested-object).
			payload := `{
				"start_date": "2018-01-01T00:00:00Z", "end_date": "2018-01-02T00:00:00Z",
				"resolution": 60, "model_id": "m-partial",
				"weather": {"type": "resolvent-weather", "provider": "era5-land", "lat": 48.83, "lon": 12.95, "year": 2018, "use_case": "solar"},
				"buildings": [
					{"id": "b-1", "building": {"envelope": {"elements": [{"id": "W1", "type": "wall"}]}}},
					{"id": "b-2", "building": {}}
				]
			}`
			id := h.post("submit batch where one building lacks its envelope", requestBody("buem", payload), nil)
			h.await("final state (completed; the per-building error is result data)", id)
			h.forwarded("payload buem-gateway received", "buem")
			h.get("result carries one buem block and one error entry", "/v1/requests/"+id+"/result",
				map[string]string{"X-API-Key": clientKey})
			h.events("audit trail", id)
			h.counts()
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
			id := h.post("submit", requestBody("demo", `{"time-series":[]}`), nil)
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
			h.postRaw("no content type", requestBody("demo", `{}`), "", nil)
			h.postRaw("wrong content type", requestBody("demo", `{}`), "text/plain", nil)
			h.postRaw("invalid json", `{"api_key":`, "application/json", nil)
			h.postRaw("trailing garbage", requestBody("demo", `{}`)+`garbage`, "application/json", nil)
			h.postRaw("missing api_key", `{"target":"buem","payload":{}}`, "application/json", nil)
			h.postRaw("missing target", `{"api_key":"`+clientKey+`","payload":{}}`, "application/json", nil)
			h.postRaw("missing payload", `{"api_key":"`+clientKey+`","target":"buem"}`, "application/json", nil)
			h.postRaw("payload not an object", `{"api_key":"`+clientKey+`","target":"buem","payload":[1]}`, "application/json", nil)
			h.postRaw("wrong api key", `{"api_key":"nope","target":"buem","payload":{}}`, "application/json", nil)
			h.postRaw("unknown target", requestBody("hydra", `{}`), "application/json", nil)
			h.postRaw("oversized body", requestBody("demo", `{"blob":"`+strings.Repeat("x", 5000)+`"}`), "application/json", nil)
			h.post("content type with charset parameter is accepted",
				requestBody("demo", `{"time-series":[]}`),
				map[string]string{"Content-Type": "application/json; charset=utf-8"})
		},
	},
	{
		name: "idempotency",
		run: func(t *testing.T, h *harness) {
			payload := `{"time-series":[]}`
			hdr := map[string]string{"Idempotency-Key": "golden-key-1"}
			id := h.post("first submit with Idempotency-Key", requestBody("demo", payload), hdr)
			// Let the job finish before replaying: the replay echoes the
			// stored job's *current* state, which would otherwise race
			// between received/resolving/completed.
			h.await("first job finishes", id)
			h.post("identical resubmit returns the same job (in its current state)",
				requestBody("demo", payload), hdr)
			h.postRaw("same key, different payload → conflict",
				requestBody("demo", `{"time-series":[1]}`), "application/json", hdr)
			h.post("same key from another client → independent job",
				strings.Replace(requestBody("demo", payload), clientKey, secondClientKey, 1), hdr)
		},
	},
	{
		// Deliberately pins that reads are NOT owner-scoped: any configured
		// API key can read any job. A future scoping decision must show up
		// here as a reviewable golden diff.
		name: "cross-client-read",
		run: func(t *testing.T, h *harness) {
			id := h.post("client A submits", requestBody("demo", `{"time-series":[]}`), nil)
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
				requestBody("demo", `{"time-series":[{"type":"resolvent-tidal"}]}`), nil)
			h.await("final state", id)
			// A distinct created_at millisecond, so newest-first ordering
			// never falls back to the insertion tiebreaker.
			time.Sleep(2 * time.Millisecond)
			second := h.post("submit a second job that will fail",
				requestBody("demo", `{"time-series":[{"type":"resolvent-tidal"}]}`), nil)
			h.await("second final state", second)
			h.get("result is not available for a failed job", "/v1/requests/"+id+"/result",
				map[string]string{"X-API-Key": clientKey})
			h.get("list failed jobs, newest first", "/v1/requests?state=failed&limit=5",
				map[string]string{"X-API-Key": clientKey})
		},
	},
}

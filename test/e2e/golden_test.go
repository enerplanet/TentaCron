package e2e

import (
	"net/http"
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
	runScenarios(t, containerIsResolventScenarios)
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

// TestScheduling covers claim ordering across clients and priorities and
// per-client in-flight ceilings.
func TestScheduling(t *testing.T) {
	runScenarios(t, schedulingScenarios)
}

// TestCancellation covers DELETE /v1/requests/{id} in every state.
func TestCancellation(t *testing.T) {
	runScenarios(t, cancellationScenarios)
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
		// Resolvent chaining, the pipeline the service exists for: the
		// building lookup (city2tabula) feeds the typology lookup (ignis),
		// and both plus the weather series feed a BuEM simulation whose
		// nested payload is a target-backed resolvent. Three levels, one
		// request. The frozen resource requests prove the filled values
		// went upstream; the marker keeps the original references.
		name: "chain-city2tabula-ignis-buem",
		run: func(t *testing.T, h *harness) {
			body := exampleRequest(t, "buildings-chain.json")
			h.validate("dry run lists the dependencies", body, nil)
			id := h.post("submit the chained request", body, nil)
			h.await("final state", id)
			h.resourceRequests("resource requests: ignis received the code city2tabula answered")
			h.forwarded("nested BuEM payload with storeys, U-values and weather filled in", "buem-building")
			h.forwarded("payload the demo target received (markers keep the $from references)", "demo")
			h.events("audit trail", id)
			h.counts()
		},
	},
	{
		// References that cannot be followed are payload problems: the dry
		// run names them, and a submitted job fails before any call.
		name: "chain-cycle",
		run: func(t *testing.T, h *harness) {
			cycle := requestBody("demo", `{"time-series":{
				"a":{"type":"resolvent-pv1","p":{"$from":"b","path":"v"}},
				"b":{"type":"resolvent-pv1","p":{"$from":"c","path":"v"}},
				"c":{"type":"resolvent-pv1","p":{"$from":"a","path":"v"}}}}`)
			h.validate("dry run: a cycle", cycle, nil)
			h.validate("dry run: an unknown name", requestBody("demo", `{"time-series":{"a":{"type":"resolvent-pv1","p":{"$from":"nobody"}}}}`), nil)
			h.validate("dry run: a self reference", requestBody("demo", `{"time-series":{"a":{"type":"resolvent-pv1","p":{"$from":"a"}}}}`), nil)
			h.validate("dry run: an ambiguous name", requestBody("demo", `{"time-series":[{"type":"resolvent-pv1","name":"n"},{"type":"resolvent-wind","name":"n"},{"type":"resolvent-pv1","p":{"$from":"n"}}]}`), nil)
			h.validate("dry run: a path missing from the series is only found while resolving", requestBody("demo", `{"time-series":{"a":{"type":"resolvent-pv1","p":1},"b":{"type":"resolvent-pv1","p":{"$from":"a","path":"nope"}}}}`), nil)
			id := h.post("submit the cycle", cycle, nil)
			h.await("it fails before any call", id)
			id = h.post("submit the missing path", requestBody("demo", `{"time-series":{"a":{"type":"resolvent-pv1","p":1},"b":{"type":"resolvent-pv1","p":{"$from":"a","path":"nope"}}}}`), nil)
			h.await("it fails after fetching only the dependency", id)
			h.counts()
		},
	},
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
			h.get("audit trail through the api", "/v1/requests/"+id+"/events", map[string]string{"X-API-Key": clientKey})
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
		// options.cache: refresh re-fetches a cached series and rewrites the
		// entry; bypass fetches fresh and leaves the cache alone; the default
		// serves the cache. The resource call count tells the story.
		name: "cache-modes",
		run: func(t *testing.T, h *harness) {
			payload := `{"time-series":[{"type":"resolvent-pv1","lat":48.83}]}`
			with := func(mode string) string {
				return `{"api_key":"` + clientKey + `","target":"demo","options":{"cache":"` + mode + `"},"payload":` + payload + `}`
			}
			first := h.post("first request fills the cache", requestBody("demo", payload), nil)
			h.await("first completes (one fetch)", first)
			refreshed := h.post("refresh: fetch fresh despite the cache", with("refresh"), nil)
			h.await("refresh completes (second fetch)", refreshed)
			h.events("refresh's audit trail (0 from cache)", refreshed)
			bypassed := h.post("bypass: fetch fresh, touch nothing", with("bypass"), nil)
			h.await("bypass completes (third fetch)", bypassed)
			again := h.post("default mode serves the cache again", requestBody("demo", payload), nil)
			h.await("served from cache (no fetch)", again)
			h.events("audit trail of the cached run", again)
			h.get("the option is echoed on the job", "/v1/requests/"+refreshed, map[string]string{"X-API-Key": clientKey})
			h.counts()
		},
	},
	{
		// The name label is not part of the cache key: two resolvents that
		// differ only in name share one fetch and both slots are filled.
		name: "cache-ignores-name",
		run: func(t *testing.T, h *harness) {
			payload := `{"time-series":[
				{"name":"north_roof","type":"resolvent-pv1","lat":48.83,"capacity_kw":5},
				{"name":"south_roof","type":"resolvent-pv1","lat":48.83,"capacity_kw":5}
			]}`
			id := h.post("submit two resolvents differing only in name", requestBody("demo", payload), nil)
			h.await("final state", id)
			h.forwarded("both slots filled from one fetch, names preserved in the markers", "demo")
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

var containerIsResolventScenarios = []scenario{
	{
		// timeseries_path may point straight at one resolvent object (a
		// contract with a single weather slot, say); the object itself is
		// resolved and its slot replaced in place.
		name: "container-is-resolvent",
		mod: func(cfg *config.Config) {
			demo := cfg.Targets["demo"]
			demo.TimeseriesPath = "weather"
			cfg.Targets["demo"] = demo
		},
		run: func(t *testing.T, h *harness) {
			payload := `{"site":"deggendorf","weather":{"type":"resolvent-pv1","lat":48.83},"other":{"type":"resolvent-wind"}}`
			id := h.post("submit payload whose container is the resolvent", requestBody("demo", payload), nil)
			h.await("final state", id)
			h.forwarded("the addressed object is substituted, the sibling outside the path is not", "demo")
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
		// A target with retry_on_timeout: false (an expensive synchronous
		// simulation) is never re-submitted when the forward hits its
		// deadline: exactly one call, one attempt, target_timeout.
		name:  "target-timeout-not-retried",
		fakes: fakes{directDelay: func(int64) time.Duration { return 300 * time.Millisecond }},
		mod: func(cfg *config.Config) {
			demo := cfg.Targets["demo"]
			demo.Timeout = config.Duration(50 * time.Millisecond)
			demo.RetryOnTimeout = boolPtr(false)
			cfg.Targets["demo"] = demo
		},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit to a slow target that must not be re-submitted", requestBody("demo", `{"time-series":[]}`), nil)
			h.await("final state (target_timeout after a single attempt)", id)
			h.events("audit trail (no requeue)", id)
			h.countsWithPolls()
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

var cancellationScenarios = []scenario{
	{
		// A queued request is cancelled at once and never reaches the
		// target; a finished one cannot be cancelled any more.
		name: "cancel-received",
		fakes: fakes{directDelay: func(call int64) time.Duration {
			if call == 1 {
				return time.Second // the blocker keeps the single worker busy
			}
			return 0
		}},
		mod: func(cfg *config.Config) { cfg.Worker.Count = 1 },
		run: func(t *testing.T, h *harness) {
			auth := map[string]string{"X-API-Key": clientKey}
			blocker := h.post("blocker occupies the worker", requestBody("demo", `{"who":"b0","time-series":[]}`), nil)
			h.awaitState("blocker is forwarding", blocker, store.StateForwarding)
			queued := h.post("a second request queues behind it", requestBody("demo", `{"who":"q1","time-series":[]}`), nil)
			h.del("cancel the queued request", "/v1/requests/"+queued, auth)
			h.del("cancelling it again is refused", "/v1/requests/"+queued, auth)
			h.get("cancelled requests are listable", "/v1/requests?state=cancelled", auth)
			h.events("audit trail of the cancelled request", queued)
			h.await("the blocker completes normally", blocker)
			h.del("a finished request cannot be cancelled", "/v1/requests/"+blocker, auth)
			h.get("result of the cancelled request is not available", "/v1/requests/"+queued+"/result", auth)
			h.targetCallOrder("only the blocker reached the target", "demo")
		},
	},
	{
		// A request awaiting its target is cancelled and the target is asked
		// to stop its job through cancel_url_template; polling stops.
		name:  "cancel-awaiting-target",
		fakes: fakes{status: func(int64) reply { return reply{200, `{"id":"m-golden-1","state":"running"}`, ""} }},
		mod: func(cfg *config.Config) {
			meme := cfg.Targets["meme"]
			meme.Response.Poll.Timeout = config.Duration(60 * time.Second)
			cfg.Targets["meme"] = meme
		},
		run: func(t *testing.T, h *harness) {
			auth := map[string]string{"X-API-Key": clientKey}
			id := h.post("submit to the polling target", requestBody("meme", `{"model":{"timeseries":{}}}`), nil)
			h.awaitState("the target accepted the job", id, store.StateAwaitingTarget)
			h.del("cancel while awaiting the target", "/v1/requests/"+id, auth)
			h.awaitCancelCalls(1)
			h.events("audit trail", id)
			h.counts()
		},
	},
	{
		// A request a worker is processing right now is not cancellable:
		// the client is told to retry, and the job finishes as usual.
		name:  "cancel-conflict-in-flight",
		fakes: fakes{directDelay: func(call int64) time.Duration { return 500 * time.Millisecond }},
		run: func(t *testing.T, h *harness) {
			auth := map[string]string{"X-API-Key": clientKey}
			id := h.post("submit", requestBody("demo", `{"time-series":[]}`), nil)
			h.awaitState("the worker is forwarding it", id, store.StateForwarding)
			h.del("cancel during processing is refused", "/v1/requests/"+id, auth)
			h.await("the request completes anyway", id)
		},
	},
}

var schedulingScenarios = []scenario{
	{
		// One worker, a batch client that queues six jobs, an interactive
		// client with one, and an admin job with priority 5 — all queued
		// while a blocker from the batch client occupies the worker. Claims
		// go by priority first, then round-robin across clients (each
		// client's oldest job, the least recently served client first),
		// then age: the interactive job runs right after the priority job,
		// ahead of the six queued batch jobs, because the batch client was
		// the last one served.
		name: "fair-claim-order",
		fakes: fakes{directDelay: func(call int64) time.Duration {
			if call == 1 {
				return time.Second // the blocker: long enough to queue everything else behind it
			}
			return 0
		}},
		mod: func(cfg *config.Config) { cfg.Worker.Count = 1 },
		run: func(t *testing.T, h *harness) {
			who := func(marker string) string { return `{"who":"` + marker + `","time-series":[]}` }
			blocker := h.post("batch client submits the blocker", requestBody("demo", who("b0")), nil)
			h.awaitState("blocker occupies the only worker", blocker, store.StateForwarding)
			var ids []string
			for _, m := range []string{"b1", "b2", "b3", "b4", "b5", "b6"} {
				ids = append(ids, h.post("batch client submits "+m, requestBody("demo", who(m)), nil))
			}
			ids = append(ids, h.post("interactive client submits s1",
				strings.Replace(requestBody("demo", who("s1")), clientKey, secondClientKey, 1), nil))
			ids = append(ids, h.post("admin submits o1 with priority 5",
				`{"api_key":"`+adminKey+`","target":"demo","priority":5,"payload":`+who("o1")+`}`, nil))
			for _, id := range append([]string{blocker}, ids...) {
				h.awaitState("job "+normalizePath(id, h.ids)+" completes", id, store.StateCompleted, store.StateFailed)
			}
			h.targetCallOrder("order in which the target received the jobs", "demo")
			h.get("priority is reported on the job", "/v1/requests/"+ids[len(ids)-1], map[string]string{"X-API-Key": adminKey})
		},
	},
	{
		// A per-key in-flight ceiling: the batch client may run one job at a
		// time, so with two workers its queue drains one by one while the
		// other client's job takes the free worker at once.
		name: "per-client-concurrency-ceiling",
		fakes: fakes{directDelay: func(call int64) time.Duration {
			if call <= 3 {
				return 400 * time.Millisecond
			}
			return 0
		}},
		mod: func(cfg *config.Config) {
			cfg.Auth.APIKeys[0].MaxConcurrent = 1
		},
		run: func(t *testing.T, h *harness) {
			who := func(marker string) string { return `{"who":"` + marker + `","time-series":[]}` }
			first := h.post("batch client submits b1", requestBody("demo", who("b1")), nil)
			h.awaitState("b1 occupies the batch client's single slot", first, store.StateForwarding)
			second := h.post("batch client submits b2 (must wait for b1)", requestBody("demo", who("b2")), nil)
			other := h.post("interactive client submits s1 (free worker)",
				strings.Replace(requestBody("demo", who("s1")), clientKey, secondClientKey, 1), nil)
			for _, id := range []string{first, second, other} {
				h.awaitState("job "+normalizePath(id, h.ids)+" completes", id, store.StateCompleted, store.StateFailed)
			}
			h.targetCallOrder("s1 reached the target before b2", "demo")
		},
	},
}

var apiContractScenarios = []scenario{
	{
		// Discovery reveals routing knobs — names, modes, container paths,
		// backend kinds, cache TTLs — and never a URL or credential.
		name: "discovery",
		run: func(t *testing.T, h *harness) {
			auth := map[string]string{"X-API-Key": clientKey}
			h.get("configured targets", "/v1/targets", auth)
			h.get("configured resolvent types", "/v1/resolvents", auth)
			h.get("discovery requires a key", "/v1/resolvents", nil)
		},
	},
	{
		// The dry run tells a frontend what a submission would do before it
		// submits: the resolvents (with paths and cache state) and the
		// problems that would fail the job — persisting nothing and calling
		// no upstream.
		name: "dry-run",
		run: func(t *testing.T, h *harness) {
			payload := `{"time-series":[{"name":"pv","type":"resolvent-pv1","lat":48.83},{"type":"resolvent-wind"}]}`
			h.validate("dry run before anything ran: nothing cached", requestBody("demo", payload), nil)
			id := h.post("the real submission", requestBody("demo", payload), nil)
			h.await("it completes", id)
			h.validate("dry run again: both series now cached", requestBody("demo", payload), nil)
			h.validate("unknown resolvent type is a problem, not an error", requestBody("demo", `{"time-series":[{"type":"resolvent-tidal"}]}`), nil)
			h.validate("proxy target whose url field is missing", requestBody("ignis-calculate", `{"A_ref":1}`), nil)
			h.validate("unknown target is refused like a submission", requestBody("hydra", `{}`), nil)
			h.get("dry runs created no requests", "/v1/requests?limit=5", map[string]string{"X-API-Key": clientKey})
			h.counts()
		},
	},
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
			h.post("api key in the X-API-Key header, no api_key field (preferred form)",
				`{"target":"demo","payload":{"time-series":[]}}`,
				map[string]string{"X-API-Key": clientKey})
			h.post("header key wins over a wrong body key",
				`{"api_key":"stale","target":"demo","payload":{"time-series":[]}}`,
				map[string]string{"X-API-Key": clientKey})
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
		// Reads are scoped to the submitting client: another client's key
		// gets the same 404 as for an unknown id (no id probing across
		// clients) and never sees the job in a list.
		name: "cross-client-read",
		run: func(t *testing.T, h *harness) {
			id := h.post("client A submits", requestBody("demo", `{"time-series":[]}`), nil)
			h.await("job completes", id)
			auth2 := map[string]string{"X-API-Key": secondClientKey}
			h.get("client B reads A's job status (404, as for an unknown id)", "/v1/requests/"+id, auth2)
			h.get("client B streams A's result (404)", "/v1/requests/"+id+"/result", auth2)
			h.get("client B lists jobs (A's job absent)", "/v1/requests?limit=5", auth2)
			h.get("client A lists its own job", "/v1/requests?limit=5", map[string]string{"X-API-Key": clientKey})
		},
	},
	{
		// An admin key reads every client's requests: status, result and the
		// unfiltered list.
		name: "admin-reads-all",
		run: func(t *testing.T, h *harness) {
			a := h.post("client A submits", requestBody("demo", `{"time-series":[]}`), nil)
			h.await("A's job completes", a)
			b := h.post("client B submits", strings.Replace(requestBody("demo", `{"time-series":[]}`), clientKey, secondClientKey, 1), nil)
			h.awaitState("B's job completes", b, store.StateCompleted, store.StateFailed)
			admin := map[string]string{"X-API-Key": adminKey}
			h.get("admin reads A's status", "/v1/requests/"+a, admin)
			h.get("admin streams B's result", "/v1/requests/"+b+"/result", admin)
			h.get("admin lists both clients' jobs", "/v1/requests?limit=5", admin)
		},
	},
	{
		// A batch stores each item on its own and answers per item: accepted
		// requests with their ids, rejected ones with the single endpoint's
		// error codes, an idempotent replay with the earlier id.
		name: "batch-mixed",
		run: func(t *testing.T, h *harness) {
			auth := map[string]string{"X-API-Key": clientKey}
			earlier := h.post("an earlier single submission with an idempotency key",
				requestBody("demo", `{"time-series":[]}`), map[string]string{"Idempotency-Key": "batch-golden-1"})
			h.await("it completes", earlier)
			accepted := h.postBatch("batch of five: two fine, unknown target, bad payload, a replay of the earlier one", `{"requests":[
				{"target":"demo","payload":{"who":"one","time-series":[]}},
				{"target":"demo","payload":{"who":"two","time-series":[]},"priority":2},
				{"target":"hydra","payload":{}},
				{"target":"demo","payload":[1]},
				{"target":"demo","payload":{"time-series":[]},"idempotency_key":"batch-golden-1"}
			]}`, auth)
			if len(accepted) != 3 {
				t.Fatalf("accepted ids = %v, want two new jobs and the replay", accepted)
			}
			h.await("the first new job completes", accepted[0])
			h.await("the second new job completes", accepted[1])
			h.postBatch("a batch where nothing is acceptable answers 400 with the items", `{"requests":[{"target":"hydra","payload":{}}]}`, auth)
			h.postBatch("an empty batch is refused", `{"requests":[]}`, auth)
			h.get("the accepted items are ordinary requests", "/v1/requests?limit=10", auth)
		},
	},
	{
		// A completion callback: the terminal job document is POSTed to the
		// allow-listed https receiver, signed with the configured secret;
		// GET reports the delivery. A 5xx is retried, a 4xx is final.
		name: "callback-delivered",
		run: func(t *testing.T, h *harness) {
			auth := map[string]string{"X-API-Key": clientKey}
			hook := h.callbacks.URL + "/hook"
			// The deliverer fires the moment the job ends, so the terminal
			// state is observed through the delivery rather than a racy GET.
			id := h.post("submit with a callback_url", `{"target":"demo","payload":{"time-series":[]},"callback_url":"`+hook+`"}`, auth)
			h.awaitDeliveries("the receiver got the signed job document", 1)
			h.awaitCallbackState("GET reports the delivery", id, "delivered")
			h.post("an http callback is refused", `{"target":"demo","payload":{},"callback_url":"http://`+strings.TrimPrefix(hook, "https://")+`"}`, auth)
			h.post("a host outside the allow-list is refused", `{"target":"demo","payload":{},"callback_url":"https://evil.example/hook"}`, auth)
			h.postBatch("batch items carry callbacks too", `{"requests":[{"target":"demo","payload":{},"callback_url":"https://evil.example/hook"},{"target":"demo","payload":{"time-series":[]},"callback_url":"`+hook+`/batch"}]}`, auth)
			h.awaitDeliveries("the accepted batch item's callback arrives", 2)
			h.counts()
		},
	},
	{
		name: "callback-retried-then-given-up",
		fakes: fakes{callback: func(call int64) reply {
			switch call {
			case 1:
				return reply{503, `{"busy":true}`, ""}
			case 2:
				return reply{200, `{}`, ""}
			default:
				return reply{410, `{"gone":true}`, ""}
			}
		}},
		run: func(t *testing.T, h *harness) {
			auth := map[string]string{"X-API-Key": clientKey}
			hook := h.callbacks.URL + "/hook"
			id := h.post("submit with a callback_url", `{"target":"demo","payload":{"time-series":[]},"callback_url":"`+hook+`"}`, auth)
			h.awaitDeliveries("503 then 200: two attempts", 2)
			h.awaitCallbackState("delivered on the second attempt", id, "delivered")
			gone := h.post("a second request whose receiver answers 410", `{"target":"demo","payload":{"time-series":[]},"callback_url":"`+hook+`"}`, auth)
			h.awaitCallbackState("a 4xx is final: one attempt, state failed", gone, "failed")
			h.counts()
		},
	},
	{
		// A recurring run: the schedule is created with its first due time,
		// materialises into an ordinary request (cache refresh by default,
		// the schedule's priority) at every due time, records its last run,
		// and is scoped like requests. Deleting it leaves the runs.
		name: "schedule-crud",
		run: func(t *testing.T, h *harness) {
			auth := map[string]string{"X-API-Key": clientKey}
			other := map[string]string{"X-API-Key": secondClientKey}
			admin := map[string]string{"X-API-Key": adminKey}
			created := h.call("create a schedule that runs every two seconds", http.MethodPost, "/v1/schedules",
				`{"target":"demo","payload":{"time-series":[{"type":"resolvent-pv1","capacity_kw":1}]},"cron":"@every 2s","priority":1}`, auth)
			id, _ := created["id"].(string)
			if id == "" {
				t.Fatalf("schedule not created: %v", created)
			}
			h.call("read it back", http.MethodGet, "/v1/schedules/"+id, "", auth)
			h.call("another client cannot see it", http.MethodGet, "/v1/schedules/"+id, "", other)
			h.call("the admin lists every client's schedules", http.MethodGet, "/v1/schedules", "", admin)
			h.call("no runs yet", http.MethodGet, "/v1/schedules/"+id+"/runs", "", auth)
			h.awaitRuns("the first run has completed", id, 1)
			after := h.call("the schedule records its last run", http.MethodGet, "/v1/schedules/"+id, "", auth)
			run, _ := after["last_job_id"].(string)
			h.call("the run is an ordinary request: cache refresh, the schedule's priority", http.MethodGet, "/v1/requests/"+run, "", auth)
			h.call("delete it", http.MethodDelete, "/v1/schedules/"+id, "", auth)
			h.call("gone", http.MethodGet, "/v1/schedules/"+id, "", auth)
			h.call("the run outlives the schedule", http.MethodGet, "/v1/requests/"+run, "", auth)
			h.call("an invalid cron expression is refused", http.MethodPost, "/v1/schedules", `{"target":"demo","payload":{},"cron":"61 * * * *"}`, auth)
			h.call("an unknown time zone is refused", http.MethodPost, "/v1/schedules", `{"target":"demo","payload":{},"cron":"@daily","timezone":"Mars/Olympus"}`, auth)
			h.call("a payload whose runs would fail is refused", http.MethodPost, "/v1/schedules", `{"target":"demo","payload":{"time-series":[{"type":"resolvent-tidal"}]},"cron":"@daily"}`, auth)
			h.call("an unknown target is refused", http.MethodPost, "/v1/schedules", `{"target":"hydra","payload":{},"cron":"@daily"}`, auth)
		},
	},
	{
		// A delayed run: not_before keeps the request in received until the
		// time arrives, visible in the audit trail and echoed by GET for the
		// job's whole life; the queue then runs it like any other request.
		name: "delayed-request",
		run: func(t *testing.T, h *harness) {
			auth := map[string]string{"X-API-Key": clientKey}
			// Far enough ahead that the immediate read below reliably sees the
			// request still waiting, even on a loaded CI runner.
			soon := time.Now().Add(2 * time.Second).UTC().Format(time.RFC3339)
			id := h.post("submit with not_before shortly ahead", `{"target":"demo","payload":{"time-series":[]},"not_before":"`+soon+`"}`, auth)
			h.get("still received while it waits", "/v1/requests/"+id, auth)
			h.await("it runs once the time arrives", id)
			h.get("not_before stays on the finished request", "/v1/requests/"+id, auth)
			h.events("audit trail", id)
			h.post("a not_before more than 30 days ahead is refused", `{"target":"demo","payload":{},"not_before":"`+time.Now().Add(31*24*time.Hour).UTC().Format(time.RFC3339)+`"}`, auth)
			h.post("a malformed not_before is refused", `{"target":"demo","payload":{},"not_before":"tomorrow"}`, auth)
		},
	},
	{
		// ?wait= turns the status read into a long poll: one call returns
		// the terminal state as soon as the worker gets there, instead of a
		// client-side polling loop.
		name:  "long-poll",
		fakes: fakes{directDelay: func(int64) time.Duration { return 300 * time.Millisecond }},
		run: func(t *testing.T, h *harness) {
			auth := map[string]string{"X-API-Key": clientKey}
			id := h.post("submit to a slow target", requestBody("demo", `{"time-series":[]}`), nil)
			h.get("long-poll until it completes", "/v1/requests/"+id+"?wait=10s", auth)
			h.get("waiting on a finished request answers at once", "/v1/requests/"+id+"?wait=10s", auth)
			h.get("an invalid wait is refused", "/v1/requests/"+id+"?wait=soon", auth)
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
			auth := map[string]string{"X-API-Key": clientKey}
			h.get("result is not available for a failed job", "/v1/requests/"+id+"/result", auth)
			h.get("list failed jobs, newest first", "/v1/requests?state=failed&limit=5", auth)
			// Pagination: a full page announces the next one through an
			// opaque cursor; the last page carries no cursor.
			page := h.getJSON("first page of one (with next_cursor)", "/v1/requests?state=failed&limit=1", auth)
			next, _ := page["next_cursor"].(string)
			h.get("second page via the cursor (no further page)", "/v1/requests?state=failed&limit=1&cursor="+next, auth)
			h.get("filtered by target", "/v1/requests?target=demo&limit=5", auth)
			h.get("filtered by an unused target", "/v1/requests?target=meme&limit=5", auth)
		},
	},
}

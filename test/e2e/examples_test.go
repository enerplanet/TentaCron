package e2e

import "testing"

// TestExamples runs every payload in examples/ through the full stack — the
// drift gate that keeps the published examples true to the actual contract.
func TestExamples(t *testing.T) {
	runScenarios(t, exampleScenarios)
}

var exampleScenarios = []scenario{
	{
		name: "example-demo-direct",
		run: func(t *testing.T, h *harness) {
			id := h.post("submit examples/demo-direct.json", exampleRequest(t, "demo-direct.json"), nil)
			h.await("final state", id)
			h.forwarded("payload the demo target received (header-injected key)", "demo")
			h.events("audit trail", id)
			h.counts()
		},
	},
	{
		// The real buem-gateway contract: the weather resolvent at the
		// payload root is resolved via the weather point-query GET (fields
		// mapped to query parameters) and substituted by the exact
		// {index, variables} block the gateway requires — no tentacron
		// marker, buildings untouched.
		name: "example-buem-buildings",
		run: func(t *testing.T, h *harness) {
			id := h.post("submit examples/buem-buildings.json", exampleRequest(t, "buem-buildings.json"), nil)
			h.await("final state", id)
			h.resourceRequests("exact weather point query sent")
			h.forwarded("payload buem-gateway received (weather substituted, marker-free)", "buem")
			h.events("audit trail", id)
			h.counts()
		},
	},
	{
		name: "example-meme-poll",
		run: func(t *testing.T, h *harness) {
			id := h.post("submit examples/meme-poll.json", exampleRequest(t, "meme-poll.json"), nil)
			h.await("final state", id)
			h.forwarded("payload the meme target received (body-injected key)", "meme")
			h.events("audit trail", id)
			h.counts()
		},
	},
	{
		// GET resolvents against the verified city2tabula and ignis
		// contracts, in a name→object registry: country/osm_ids map onto
		// query parameters (arrays joined comma-separated), {code} onto
		// ignis's path, and city2tabula's list response is indexed via
		// response_path "0". The frozen request lines prove the mapping.
		name: "example-city2tabula-ignis",
		run: func(t *testing.T, h *harness) {
			id := h.post("submit examples/city2tabula-ignis.json", exampleRequest(t, "city2tabula-ignis.json"), nil)
			h.await("final state", id)
			h.resourceRequests("exact GET requests the resource APIs received")
			h.forwarded("building attributes and TABULA data substituted into the registry", "demo")
			h.events("audit trail", id)
			h.counts()
		},
	},
	{
		// Target composition, end to end: a MEME model whose heat-demand
		// series is produced by a BuEM simulation — resolvent-buem forwards
		// its "payload" field through the buem-building target (never
		// scanned for resolvents), extracts the load-profile timeseries via
		// response_path, and substitutes it into the MEME registry with the
		// marker attached (the outer target's policy). The pv_cf resolvent
		// resolves through a plain resource API in the same run.
		name: "example-meme-with-buem",
		run: func(t *testing.T, h *harness) {
			id := h.post("submit examples/meme-with-buem.json", exampleRequest(t, "meme-with-buem.json"), nil)
			h.await("final state", id)
			h.forwarded("nested payload the buem-building target received", "buem-building")
			h.forwarded("composed payload the meme target received", "meme")
			h.events("audit trail (two resolvents, one of them a buem run)", id)
			h.counts()
		},
	},
	{
		// A proxy target: tentacron contributes auth, persistence, audit
		// and retries but hands the payload through unresolved — the {code}
		// field addresses ignis's calculate endpoint via URL templating and
		// is stripped from the forwarded body (ignis's overrides schema has
		// no code field).
		name: "example-ignis-calculate",
		run: func(t *testing.T, h *harness) {
			id := h.post("submit examples/ignis-calculate.json", exampleRequest(t, "ignis-calculate.json"), nil)
			h.await("final state", id)
			h.forwarded("overrides ignis received (code templated into the path, stripped from the body)", "ignis-calculate")
			h.events("audit trail (handed through, nothing resolved)", id)
			h.counts()
		},
	},
	{
		// A real third-party backend without a shim: PVGIS's seriescalc
		// answers with its own document shape; query_map renames the
		// resolvent's fields onto PVGIS parameters (the exact request line
		// is frozen) and response_map reshapes the hourly rows into the
		// time-series object the target receives, power rescaled to kW.
		name: "example-pvgis-hourly",
		run: func(t *testing.T, h *harness) {
			id := h.post("submit examples/pvgis-hourly.json", exampleRequest(t, "pvgis-hourly.json"), nil)
			h.await("final state", id)
			h.resourceRequests("exact PVGIS request (fields renamed, name and type never sent)")
			h.forwarded("payload the demo target received (hourly rows mapped to a series)", "demo")
			h.counts()
		},
	},
	{
		// Resolvent chaining end to end: city2tabula → ignis → BuEM in one
		// request, the nested simulation payload assembled from the two
		// lookups and the weather series. The frozen requests prove which
		// values each backend received.
		name: "example-buildings-chain",
		run: func(t *testing.T, h *harness) {
			id := h.post("submit examples/buildings-chain.json", exampleRequest(t, "buildings-chain.json"), nil)
			h.await("final state", id)
			h.resourceRequests("resource requests in level order")
			h.forwarded("nested payload the buem-building target received", "buem-building")
			h.forwarded("payload the demo target received", "demo")
			h.counts()
		},
	},
	{
		name: "example-no-resolvents",
		run: func(t *testing.T, h *harness) {
			id := h.post("submit examples/no-resolvents.json", exampleRequest(t, "no-resolvents.json"), nil)
			h.await("final state", id)
			h.forwarded("payload forwarded unchanged", "demo")
			h.events("audit trail", id)
			h.counts()
		},
	},
	{
		name: "example-big-numbers",
		run: func(t *testing.T, h *harness) {
			id := h.post("submit examples/big-numbers.json", exampleRequest(t, "big-numbers.json"), nil)
			h.await("final state", id)
			h.forwarded("payload with integers above 2^53 — digits must be exact", "demo")
			h.counts()
		},
	},
}

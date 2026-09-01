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
		// payload root is substituted by the exact {index, variables} block
		// the gateway requires — no tentacron marker, buildings untouched.
		name: "example-buem-buildings",
		fakes: fakes{resource: func(int64) reply {
			return reply{200, `{"index":["2018-01-01T00:30:00Z","2018-01-01T01:30:00Z"],"variables":{"T":[1.0,1.2],"GHI":[0.0,12.5]}}`, ""}
		}},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit examples/buem-buildings.json", exampleRequest(t, "buem-buildings.json"), nil)
			h.await("final state", id)
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
		// Target composition, end to end: a MEME model whose heat-demand
		// series is produced by a BuEM simulation — resolvent-buem forwards
		// its "payload" field through the buem-building target (as-is,
		// never re-resolved), extracts the load-profile timeseries via
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

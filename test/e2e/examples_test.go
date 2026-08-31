package e2e

import "testing"

// TestExamples runs every payload in examples/ through the full stack — the
// drift gate that keeps the published examples true to the actual contract.
func TestExamples(t *testing.T) {
	runScenarios(t, exampleScenarios)
}

var exampleScenarios = []scenario{
	{
		name: "example-buem-direct",
		run: func(t *testing.T, h *harness) {
			id := h.post("submit examples/buem-direct.json", exampleRequest(t, "buem-direct.json"), nil)
			h.await("final state", id)
			h.forwarded("payload the buem target received (header-injected key)", "direct")
			h.events("audit trail", id)
			h.counts()
		},
	},
	{
		name: "example-meme-poll",
		run: func(t *testing.T, h *harness) {
			id := h.post("submit examples/meme-poll.json", exampleRequest(t, "meme-poll.json"), nil)
			h.await("final state", id)
			h.forwarded("payload the meme target received (body-injected key)", "accept")
			h.events("audit trail", id)
			h.counts()
		},
	},
	{
		name: "example-no-resolvents",
		run: func(t *testing.T, h *harness) {
			id := h.post("submit examples/no-resolvents.json", exampleRequest(t, "no-resolvents.json"), nil)
			h.await("final state", id)
			h.forwarded("payload forwarded unchanged", "direct")
			h.events("audit trail", id)
			h.counts()
		},
	},
	{
		name: "example-big-numbers",
		run: func(t *testing.T, h *harness) {
			id := h.post("submit examples/big-numbers.json", exampleRequest(t, "big-numbers.json"), nil)
			h.await("final state", id)
			h.forwarded("payload with integers above 2^53 — digits must be exact", "direct")
			h.counts()
		},
	},
}

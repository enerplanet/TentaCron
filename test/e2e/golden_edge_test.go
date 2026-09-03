package e2e

import (
	"strings"
	"testing"

	"github.com/enerplanet/tentacron/internal/config"
)

// TestEdgeContracts freezes the less common paths: proxy targets with
// body-field auth, authoring errors that must fail fast without touching
// the wire, and file-backed JSON results.
func TestEdgeContracts(t *testing.T) {
	runScenarios(t, edgeScenarios)
}

var edgeScenarios = []scenario{
	{
		// A proxy target is byte-exact only without rewrites: body-field key
		// injection re-encodes the payload, but every field — including the
		// untouched resolvent object — survives.
		name: "proxy-body-field-key-injection",
		mod: func(cfg *config.Config) {
			demo := cfg.Targets["demo"]
			cfg.Targets["proxy-demo"] = config.Target{
				URL: demo.URL, Method: "POST", Timeout: demo.Timeout, Proxy: true,
				APIKey: demoSecret, APIKeyInject: config.InjectBodyField, APIKeyField: "api_key",
				Response: config.Response{Mode: config.ModeDirect},
			}
		},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit through a proxy target with body-field key injection",
				requestBody("proxy-demo", `{"zeta":1,"time-series":[{"type":"resolvent-pv1","lat":48.83}]}`), nil)
			h.await("final state", id)
			h.forwarded("payload the target received (key injected, resolvent untouched)", "demo")
			h.events("audit trail", id)
			h.countsWithPolls()
		},
	},
	{
		// The {code} placeholder of ignis-calculate has no payload field to
		// fill it: a permanent authoring error, no request on the wire.
		name: "proxy-url-field-missing",
		run: func(t *testing.T, h *harness) {
			id := h.post("submit to ignis-calculate without the code field",
				requestBody("ignis-calculate", `{"A_ref":{"value":120,"unit":"m2"}}`), nil)
			h.await("final state (permanent authoring error)", id)
			h.events("audit trail", id)
			h.countsWithPolls()
		},
	},
	{
		// response_path pointing nowhere in a healthy response is an
		// invalid_resource_response, not a retryable resource_error.
		name: "response-path-missing",
		mod: func(cfg *config.Config) {
			r := cfg.Resolvents["resolvent-city2tabula"]
			r.ResponsePath = "0.tabula.missing"
			cfg.Resolvents["resolvent-city2tabula"] = r
		},
		run: func(t *testing.T, h *harness) {
			id := h.post("submit with a resolvent whose response_path does not match",
				requestBody("demo", `{"time-series":[{"type":"resolvent-city2tabula","country":"germany","osm_ids":["1"]}]}`), nil)
			h.await("final state", id)
			h.events("audit trail", id)
			h.countsWithPolls()
		},
	},
	{
		// A target-backed resolvent whose payload_field is not an object
		// fails before the backing target is called.
		name: "target-backed-resolvent-bad-payload-field",
		run: func(t *testing.T, h *harness) {
			id := h.post("submit a resolvent-buem whose payload is a string",
				requestBody("meme", `{"model":{"timeseries":{"heat":{"type":"resolvent-buem","payload":"not-an-object"}}}}`), nil)
			h.await("final state", id)
			h.events("audit trail", id)
			h.countsWithPolls()
		},
	},
	{
		// GET resolvents take flat fields: a nested object is a permanent
		// authoring error with guidance, and no request is sent.
		name: "get-resolvent-nested-object",
		run: func(t *testing.T, h *harness) {
			id := h.post("submit weather resolvent with a nested location object",
				requestBody("buem", `{"model_id":"m","weather":{"type":"resolvent-weather","location":{"lat":48.83,"lon":12.95}},"buildings":[]}`), nil)
			h.await("final state (permanent authoring error)", id)
			h.events("audit trail", id)
			h.countsWithPolls()
		},
	},
	{
		// A JSON result above the inline limit lands in a .json file and is
		// referenced via href with its content type.
		name: "large-json-result-file",
		fakes: fakes{direct: func(int64) reply {
			return reply{200, `{"data":"` + strings.Repeat("x", 300<<10) + `"}`, ""}
		}},
		mod: func(cfg *config.Config) { cfg.Server.MaxBodyBytes = 1 << 20 },
		run: func(t *testing.T, h *harness) {
			id := h.post("submit", requestBody("demo", `{"time-series":[]}`), nil)
			h.await("final state (result referenced via href, application/json)", id)
			h.events("audit trail", id)
		},
	},
}

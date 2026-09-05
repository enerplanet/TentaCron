package plan

import (
	"reflect"
	"testing"
	"time"

	"github.com/enerplanet/tentacron/internal/config"
)

func testConfig() *config.Config {
	attachOff := false
	return &config.Config{
		Targets: map[string]config.Target{
			"demo":  {URL: "https://demo.example.com/run", Method: "POST", TimeseriesPath: "time-series", Response: config.Response{Mode: config.ModeDirect}},
			"buem":  {URL: "https://buem.example.com/buildings", Method: "POST", TimeseriesPath: ".", AttachResolvent: &attachOff, Response: config.Response{Mode: config.ModeDirect}},
			"ignis": {URL: "https://ignis.example.com/calc/{code}", Method: "POST", Proxy: true, Response: config.Response{Mode: config.ModeDirect}},
			"meme":  {URL: "https://meme.example.com/simulate", Method: "POST", TimeseriesPath: "model.timeseries", Response: config.Response{Mode: config.ModePoll, Poll: &config.Poll{}}},
		},
		Resolvents: map[string]config.Resolvent{
			"resolvent-pv1":     {URL: "https://pv.example.com/gen", Method: "POST", CacheTTL: config.Duration(24 * time.Hour)},
			"resolvent-weather": {URL: "https://weather.example.com/point", Method: "GET", CacheTTL: config.Duration(time.Hour)},
			"resolvent-buem":    {Target: "buem", PayloadField: "payload", CacheTTL: config.Duration(2 * time.Hour)},
		},
	}
}

func TestInspectFindsResolventsAndPredictsProblems(t *testing.T) {
	cfg := testConfig()
	p := Inspect(cfg, "demo", []byte(`{"time-series":[{"type":"resolvent-pv1","name":"pv"},{"type":"resolvent-weather","lat":1}]}`))
	if !p.OK() || len(p.Found) != 2 || p.Found[0].Path != "/time-series/0" || p.Found[0].Name != "pv" {
		t.Errorf("plan = %+v problems %v", p.Found, p.Problems)
	}
	cases := []struct {
		target, payload, wantCode, wantMsg string
	}{
		{"nope", `{}`, CodeUnknownTarget, `target "nope" is not configured`},
		{"demo", `[1]`, CodeInvalidPayload, "payload is not a JSON object"},
		{"demo", `{"time-series":[{"type":"resolvent-tidal"},{"type":"resolvent-pv1"},{"type":"resolvent-solar"}]}`, CodeUnknownResolvent, `no resolvent config for type "resolvent-tidal"`},
		{"ignis", `{"A_ref":1}`, CodeTargetError, "placeholder"},
	}
	for _, tt := range cases {
		p := Inspect(cfg, tt.target, []byte(tt.payload))
		if p.OK() || p.Problems[0].Code != tt.wantCode || !contains(p.Problems[0].Message, tt.wantMsg) {
			t.Errorf("%s %s: problems = %v", tt.target, tt.payload, p.Problems)
		}
	}
	// Every unknown type is reported, in document order, so a frontend can
	// show all of them at once; the worker fails on the first.
	p = Inspect(cfg, "demo", []byte(`{"time-series":[{"type":"resolvent-tidal"},{"type":"resolvent-pv1"},{"type":"resolvent-solar"}]}`))
	if len(p.Problems) != 2 || p.Problems[1].Code != CodeUnknownResolvent || !contains(p.Problems[1].Message, "resolvent-solar") {
		t.Errorf("unknown types = %v", p.Problems)
	}
	// A proxy target with a satisfiable URL is fine and scans nothing.
	if p := Inspect(cfg, "ignis", []byte(`{"code":"X","time-series":[{"type":"resolvent-tidal"}]}`)); !p.OK() || len(p.Found) != 0 {
		t.Errorf("proxy plan = %+v", p)
	}
	// The root-scanning target finds the weather block at the top level.
	if p := Inspect(cfg, "buem", []byte(`{"weather":{"type":"resolvent-weather"},"buildings":[]}`)); !p.OK() || len(p.Found) != 1 || p.Found[0].Path != "/weather" {
		t.Errorf("root scan plan = %+v", p.Found)
	}
}

func TestDiscoveryRevealsRoutingKnobsOnly(t *testing.T) {
	cfg := testConfig()
	targets := Targets(cfg)
	names := make([]string, 0, len(targets))
	for _, tg := range targets {
		names = append(names, tg.Name)
	}
	if !reflect.DeepEqual(names, []string{"buem", "demo", "ignis", "meme"}) {
		t.Errorf("target order = %v", names)
	}
	if targets[0].TimeseriesPath != "." || *targets[0].AttachResolvent || targets[2].TimeseriesPath != "" || targets[2].AttachResolvent != nil || !targets[2].Proxy || targets[3].ResponseMode != "poll" {
		t.Errorf("targets = %+v", targets)
	}
	res := Resolvents(cfg)
	if len(res) != 3 || res[0].Type != "resolvent-buem" || res[0].Backend != "target" || res[0].Target != "buem" ||
		res[1].Backend != "post" || res[1].CacheTTL != "24h0m0s" || res[2].Backend != "get" {
		t.Errorf("resolvents = %+v", res)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

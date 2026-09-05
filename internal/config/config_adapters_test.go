package config

import (
	"strings"
	"testing"
)

func TestAdapterValidation(t *testing.T) {
	good := `
auth:
  api_keys: [{name: t, key: k}]
targets:
  demo:
    url: "https://demo.example.com/run"
resolvents:
  resolvent-pvgis:
    url: "https://pvgis.example.com/seriescalc?outputformat=json"
    method: GET
    query_map: {capacity_kw: peakpower, tilt: angle}
    response_map:
      type: time-series
      unit: W
      index: ".outputs.hourly[*].time"
      values: {path: ".outputs.hourly[*].P", scale: 1}
      source: ".inputs.meteo_data.radiation_db"
`
	cfg, err := Load(writeConfig(t, good))
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Resolvents["resolvent-pvgis"]
	if r.QueryMap["tilt"] != "angle" || r.ResponseMap["unit"] != "W" || r.ResponseMap["values"].(map[string]any)["scale"] != 1 {
		t.Errorf("adapters not loaded: %+v", r)
	}
	for _, tt := range []struct{ name, yaml, wantErr string }{
		{"query_map on POST", strings.Replace(good, "method: GET", "method: POST", 1), "only GET resolvents map fields"},
		{"duplicate parameter", strings.Replace(good, "{capacity_kw: peakpower, tilt: angle}", "{capacity_kw: peakpower, kwp: peakpower}", 1), `both map to parameter "peakpower"`},
		{"empty parameter", strings.Replace(good, "{capacity_kw: peakpower, tilt: angle}", "{capacity_kw: \"\"}", 1), "must not be empty"},
		{"bad path", strings.Replace(good, `".outputs.hourly[*].time"`, `".outputs..time"`, 1), `response_map "index": path ".outputs..time" has an empty segment`},
		{"bad scale", strings.Replace(good, "scale: 1", "scale: big", 1), "scale must be a number"},
		{"empty response_map", strings.Replace(good, "response_map:\n      type: time-series\n      unit: W\n      index: \".outputs.hourly[*].time\"\n      values: {path: \".outputs.hourly[*].P\", scale: 1}\n      source: \".inputs.meteo_data.radiation_db\"\n", "response_map: {}\n", 1), "response_map must not be empty"},
	} {
		_, err := Load(writeConfig(t, tt.yaml))
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: want %q, got %v", tt.name, tt.wantErr, err)
		}
	}
}

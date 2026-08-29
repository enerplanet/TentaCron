package resolver

import (
	"encoding/json"
	"reflect"
	"testing"
)

func parse(t *testing.T, payload string) map[string]any {
	t.Helper()
	root, err := Parse([]byte(payload))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return root
}

func find(t *testing.T, root map[string]any, path string) []*Found {
	t.Helper()
	found, err := Find(root, path)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	return found
}

func TestFindInArrayContainer(t *testing.T) {
	root := parse(t, `{
		"scenario": "s1",
		"time-series": [
			{"name": "pv", "type": "resolvent-pv1", "capacity_kw": 12.5},
			{"name": "load", "type": "time-series", "values": [1, 2]},
			{"name": "wind", "type": "resolvent-wind", "hub_height": 120}
		]
	}`)
	found := find(t, root, "time-series")
	if len(found) != 2 {
		t.Fatalf("found %d resolvents, want 2", len(found))
	}
	if found[0].Type != "resolvent-pv1" || found[1].Type != "resolvent-wind" {
		t.Errorf("types = %s, %s", found[0].Type, found[1].Type)
	}
	if got := DistinctTypes(found); !reflect.DeepEqual(got, []string{"resolvent-pv1", "resolvent-wind"}) {
		t.Errorf("DistinctTypes = %v", got)
	}
}

func TestFindInRegistryContainer(t *testing.T) {
	// MEME-style: model.timeseries is a name -> object registry.
	root := parse(t, `{
		"model": {
			"timeseries": {
				"pv_cf": {"type": "resolvent-pv1", "lat": 48.8},
				"demand": {"type": "time-series", "values": [3]}
			}
		}
	}`)
	found := find(t, root, "model.timeseries")
	if len(found) != 1 || found[0].Type != "resolvent-pv1" {
		t.Fatalf("found = %+v", found)
	}
}

func TestFindNestedInsideContainer(t *testing.T) {
	root := parse(t, `{
		"time-series": {
			"groups": [
				{"members": [{"type": "resolvent-wind", "id": 1}]}
			]
		}
	}`)
	found := find(t, root, "time-series")
	if len(found) != 1 || found[0].Type != "resolvent-wind" {
		t.Fatalf("found = %+v", found)
	}
}

func TestFindMissingContainerIsEmpty(t *testing.T) {
	root := parse(t, `{"scenario": "no series here"}`)
	if found := find(t, root, "time-series"); len(found) != 0 {
		t.Errorf("found = %+v, want none", found)
	}
	if found := find(t, root, "a.b.c"); len(found) != 0 {
		t.Errorf("deep missing path: found = %+v", found)
	}
}

func TestFindPathThroughNonObjectIsEmpty(t *testing.T) {
	root := parse(t, `{"model": "just a string"}`)
	if found := find(t, root, "model.timeseries"); len(found) != 0 {
		t.Errorf("found = %+v, want none", found)
	}
}

func TestFindSkipsNonObjectEntriesAndNonStringTypes(t *testing.T) {
	root := parse(t, `{
		"time-series": [
			42,
			"resolvent-pv1",
			{"type": 7},
			{"no_type": true},
			{"type": "resolvent-pv1"}
		]
	}`)
	found := find(t, root, "time-series")
	if len(found) != 1 {
		t.Fatalf("found %d, want 1", len(found))
	}
}

func TestFindDoesNotDescendIntoTimeSeriesOrResolvents(t *testing.T) {
	root := parse(t, `{
		"time-series": [
			{"type": "time-series", "meta": {"origin": {"type": "resolvent-pv1"}}},
			{"type": "resolvent-wind", "fallback": {"type": "resolvent-pv1"}}
		]
	}`)
	found := find(t, root, "time-series")
	if len(found) != 1 || found[0].Type != "resolvent-wind" {
		t.Fatalf("found = %+v, want only the outer resolvent-wind", found)
	}
}

func TestFindEmptyPathScansWholeDocument(t *testing.T) {
	root := parse(t, `{"deep": {"stack": [{"type": "resolvent-pv1"}]}}`)
	found := find(t, root, "")
	if len(found) != 1 {
		t.Fatalf("found %d, want 1", len(found))
	}
}

func TestSubstituteArraySlot(t *testing.T) {
	root := parse(t, `{
		"scenario": "rooftop",
		"time-series": [
			{"name": "pv", "type": "resolvent-pv1", "capacity_kw": 12.5},
			{"name": "load", "type": "time-series", "values": [1]}
		]
	}`)
	found := find(t, root, "time-series")
	warnings, err := found[0].Substitute([]byte(`{"type":"time-series","unit":"kW","values":[0.1,0.2]}`))
	if err != nil || len(warnings) != 0 {
		t.Fatalf("Substitute: warn=%v err=%v", warnings, err)
	}

	out, err := Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Scenario string `json:"scenario"`
		Series   []struct {
			Name      string         `json:"name"`
			Type      string         `json:"type"`
			Values    []float64      `json:"values"`
			Unit      string         `json:"unit"`
			Resolvent map[string]any `json:"resolvent"`
		} `json:"time-series"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Scenario != "rooftop" {
		t.Error("sibling keys must survive")
	}
	got := doc.Series[0]
	if got.Type != "time-series" || got.Unit != "kW" || len(got.Values) != 2 {
		t.Errorf("substituted series = %+v", got)
	}
	if got.Resolvent["type"] != "resolvent-pv1" || got.Resolvent["capacity_kw"] != 12.5 {
		t.Errorf("original resolvent not preserved: %+v", got.Resolvent)
	}
	if pass := doc.Series[1]; pass.Type != "time-series" || pass.Resolvent != nil {
		t.Errorf("pass-through series was touched: %+v", pass)
	}
}

func TestSubstituteRegistrySlot(t *testing.T) {
	root := parse(t, `{"model":{"timeseries":{"pv_cf":{"type":"resolvent-pv1"}}}}`)
	found := find(t, root, "model.timeseries")
	if _, err := found[0].Substitute([]byte(`{"type":"time-series","values":[9]}`)); err != nil {
		t.Fatal(err)
	}
	model := root["model"].(map[string]any)
	entry := model["timeseries"].(map[string]any)["pv_cf"].(map[string]any)
	if entry["type"] != "time-series" {
		t.Errorf("registry slot not replaced: %+v", entry)
	}
	if entry[ResolventKey].(map[string]any)["type"] != "resolvent-pv1" {
		t.Errorf("resolvent not attached: %+v", entry)
	}
}

func TestSubstituteWarnings(t *testing.T) {
	root := parse(t, `{"time-series":[{"type":"resolvent-pv1"}]}`)
	found := find(t, root, "time-series")
	warnings, err := found[0].Substitute([]byte(`{"type":"other","resolvent":"pre-existing"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 2 {
		t.Errorf("warnings = %v, want 2 (bad type + overwritten resolvent key)", warnings)
	}
}

func TestSubstituteRejectsNonObject(t *testing.T) {
	for _, body := range []string{`[1,2,3]`, `"text"`, `not json`} {
		root := parse(t, `{"time-series":[{"type":"resolvent-pv1"}]}`)
		found := find(t, root, "time-series")
		if _, err := found[0].Substitute([]byte(body)); err == nil {
			t.Errorf("Substitute(%q): want error, got nil", body)
		}
	}
}

func TestHashKeyOrderIndependent(t *testing.T) {
	a := parse(t, `{"time-series":[{"type":"resolvent-pv1","lat":48.8,"lon":12.9}]}`)
	b := parse(t, `{"time-series":[{"lon":12.9,"lat":48.8,"type":"resolvent-pv1"}]}`)
	fa, fb := find(t, a, "time-series")[0], find(t, b, "time-series")[0]
	if fa.Hash == "" || fa.Hash != fb.Hash {
		t.Errorf("hashes differ for identical params: %s vs %s", fa.Hash, fb.Hash)
	}
}

func TestHashSensitivity(t *testing.T) {
	base := find(t, parse(t, `{"time-series":[{"type":"resolvent-pv1","lat":48.8}]}`), "time-series")[0]
	diffValue := find(t, parse(t, `{"time-series":[{"type":"resolvent-pv1","lat":49.0}]}`), "time-series")[0]
	if base.Hash == diffValue.Hash {
		t.Error("different parameter values must hash differently")
	}
	// Same params under a different resolvent type must differ too: the type
	// participates in the hash beyond the object body.
	nested := parse(t, `{"time-series":[{"type":"resolvent-pv1","nested":{"a":[1,{"b":2}]}}]}`)
	nested2 := parse(t, `{"time-series":[{"nested":{"a":[1,{"b":2}]},"type":"resolvent-pv1"}]}`)
	h1 := find(t, nested, "time-series")[0].Hash
	h2 := find(t, nested2, "time-series")[0].Hash
	if h1 != h2 {
		t.Error("nested structures must hash stably")
	}
}

func TestDuplicateResolventsShareHash(t *testing.T) {
	root := parse(t, `{"time-series":[
		{"type":"resolvent-pv1","lat":48.8},
		{"type":"resolvent-pv1","lat":48.8}
	]}`)
	found := find(t, root, "time-series")
	if len(found) != 2 || found[0].Hash != found[1].Hash {
		t.Fatalf("want 2 founds sharing a hash, got %d", len(found))
	}
	// Both slots must be independently replaceable.
	if _, err := found[0].Substitute([]byte(`{"type":"time-series","values":[1]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := found[1].Substitute([]byte(`{"type":"time-series","values":[1]}`)); err != nil {
		t.Fatal(err)
	}
	arr := root["time-series"].([]any)
	s0, s1 := arr[0].(map[string]any), arr[1].(map[string]any)
	if s0["type"] != "time-series" || s1["type"] != "time-series" {
		t.Error("both duplicate slots must be replaced")
	}
	if &s0 == &s1 {
		t.Error("slots must not share the same map instance")
	}
}

func TestParseRejectsNonObjectPayload(t *testing.T) {
	if _, err := Parse([]byte(`[1,2]`)); err == nil {
		t.Error("array payload must be rejected")
	}
}

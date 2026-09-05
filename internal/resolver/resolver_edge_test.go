package resolver

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func foundTypes(found []*Found) []string {
	types := make([]string, 0, len(found))
	for _, f := range found {
		types = append(types, f.Type)
	}
	return types
}

func TestFindContainerScalarOrNullYieldsNothing(t *testing.T) {
	for _, payload := range []string{
		`{"time-series": 5}`, `{"time-series": null}`, `{"time-series": "x"}`,
		`{"time-series": true}`, `{"time-series": []}`, `{"time-series": {}}`,
	} {
		root := parse(t, payload)
		if found := find(t, root, "time-series"); len(found) != 0 {
			t.Errorf("%s: found %d, want none", payload, len(found))
		}
	}
}

func TestFindPathWithEmptySegmentIsNotAnError(t *testing.T) {
	root := parse(t, `{"a":{"b":[{"type":"resolvent-x"}]}}`)
	if found := find(t, root, "a..b"); len(found) != 0 {
		t.Errorf("empty path segment must simply miss, found %d", len(found))
	}
	if found := find(t, root, "a.b"); len(found) != 1 {
		t.Errorf("found %d, want 1", len(found))
	}
}

func TestResolventTypePrefixIsExactAndCaseSensitive(t *testing.T) {
	root := parse(t, `{"time-series":[
		{"type":"Resolvent-pv1"},
		{"type":"resolvent"},
		{"type":"xresolvent-pv1"},
		{"type":" resolvent-pv1"},
		{"type":"resolvent-"}
	]}`)
	found := find(t, root, "time-series")
	if got := foundTypes(found); !reflect.DeepEqual(got, []string{"resolvent-"}) {
		t.Errorf("found %v, want only the bare prefix match", got)
	}
}

// Sorted keys for objects, document order for arrays — and the same order
// on every run regardless of Go's randomized map iteration.
func TestFindOrderIsDeterministic(t *testing.T) {
	const payload = `{"time-series":{
		"zeta":{"type":"resolvent-c"},
		"alpha":{"type":"resolvent-a"},
		"mid":[{"type":"resolvent-b2"},{"type":"resolvent-b1"}]
	}}`
	want := []string{"resolvent-a", "resolvent-b2", "resolvent-b1", "resolvent-c"}
	for i := 0; i < 25; i++ {
		got := foundTypes(find(t, parse(t, payload), "time-series"))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: order %v, want %v", i, got, want)
		}
	}
}

// A timeseries_path may address a single resolvent object directly; it is
// then resolved in place, replacing the parent's slot.
func TestContainerItselfIsResolved(t *testing.T) {
	root := parse(t, `{"weather":{"type":"resolvent-weather","lat":48.8},"other":{"type":"resolvent-x"}}`)
	found := find(t, root, "weather")
	if len(found) != 1 || found[0].Type != "resolvent-weather" {
		t.Fatalf("found %v, want exactly the addressed resolvent", foundTypes(found))
	}
	if _, err := found[0].Substitute([]byte(`{"type":"time-series","values":[1]}`), true); err != nil {
		t.Fatal(err)
	}
	slot := root["weather"].(map[string]any)
	if slot["type"] != TimeSeriesType || slot[ResolventKey].(map[string]any)["lat"] != json.Number("48.8") {
		t.Errorf("container slot not replaced in place: %v", slot)
	}
	if other := root["other"].(map[string]any); other["type"] != "resolvent-x" {
		t.Errorf("sibling outside the path must stay untouched: %v", other)
	}
	nested := parse(t, `{"a":{"b":{"type":"resolvent-x","v":1}}}`)
	if found := find(t, nested, "a.b"); len(found) != 1 {
		t.Fatalf("nested path to a resolvent: found %d, want 1", len(found))
	} else if _, err := found[0].Substitute([]byte(`{"type":"time-series"}`), false); err != nil {
		t.Fatal(err)
	}
	if nested["a"].(map[string]any)["b"].(map[string]any)["type"] != TimeSeriesType {
		t.Errorf("nested slot not replaced: %v", nested)
	}
	// The root object itself has no slot to replace and is always scanned.
	rootRes := parse(t, `{"type":"resolvent-x","child":{"type":"resolvent-y"}}`)
	if got := foundTypes(find(t, rootRes, "")); !reflect.DeepEqual(got, []string{"resolvent-y"}) {
		t.Errorf("root scan found %v, want the child only", got)
	}
}

func TestSubstituteWithoutAttachKeepsResponseResolventKey(t *testing.T) {
	root := parse(t, `{"time-series":[{"type":"resolvent-pv1"}]}`)
	found := find(t, root, "time-series")
	warnings, err := found[0].Substitute([]byte(`{"type":"time-series","resolvent":"from-upstream"}`), false)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("warnings=%v err=%v", warnings, err)
	}
	slot := root["time-series"].([]any)[0].(map[string]any)
	if slot[ResolventKey] != "from-upstream" {
		t.Errorf("upstream's own resolvent key must survive when not attaching: %v", slot)
	}
}

func TestSubstituteWarnsOnMissingType(t *testing.T) {
	root := parse(t, `{"time-series":[{"type":"resolvent-pv1"}]}`)
	found := find(t, root, "time-series")
	warnings, err := found[0].Substitute([]byte(`{"values":[1]}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], `expected "time-series"`) {
		t.Errorf("warnings = %v", warnings)
	}
}

// encoding/json caps nesting at 10000 levels; a hostile payload must be
// rejected as invalid, never blow the stack.
func TestParseRejectsPathologicalNesting(t *testing.T) {
	const depth = 20000
	payload := `{"a":` + strings.Repeat("[", depth) + strings.Repeat("]", depth) + `}`
	if _, err := Parse([]byte(payload)); err == nil {
		t.Fatal("want an error for pathological nesting")
	}
}

func TestHashDependsOnType(t *testing.T) {
	a := find(t, parse(t, `{"time-series":[{"type":"resolvent-a","v":1}]}`), "time-series")[0]
	b := find(t, parse(t, `{"time-series":[{"type":"resolvent-b","v":1}]}`), "time-series")[0]
	if a.Hash == b.Hash {
		t.Error("identical parameters under different types must not share a cache key")
	}
	if len(a.Hash) != 64 {
		t.Errorf("hash must be hex SHA-256 (64 chars), got %d", len(a.Hash))
	}
}

// json.Number keeps the number's spelling, so 1, 1.0 and 1e0 are distinct
// cache keys — a pinned consequence of number fidelity.
func TestHashDistinguishesNumberSpelling(t *testing.T) {
	seen := map[string]string{}
	for _, n := range []string{"1", "1.0", "1e0", "10e-1"} {
		h := find(t, parse(t, `{"time-series":[{"type":"resolvent-x","v":`+n+`}]}`), "time-series")[0].Hash
		if prev, dup := seen[h]; dup {
			t.Errorf("%s and %s share a hash", prev, n)
		}
		seen[h] = n
	}
}

// Strings survive the decode → encode round trip semantically intact:
// HTML-sensitive characters, quotes, unicode, emoji and line separators.
func TestRoundTripPreservesStrings(t *testing.T) {
	payload := "{\"s\":\"<b>&amp;</b> \\\"quoted\\\" ünïcödé 🌞 \\u2028 tab\\t\"," +
		`"time-series":[{"type":"resolvent-x","name":"ü/ä"}]}`
	root := parse(t, payload)
	out, err := Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(out) {
		t.Fatalf("Marshal produced invalid JSON: %s", out)
	}
	var want, got any
	if err := json.Unmarshal([]byte(payload), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("round trip changed the document:\n got %s\nwant %s", out, payload)
	}
}

func BenchmarkFindAndSubstitute(b *testing.B) {
	var sb strings.Builder
	sb.WriteString(`{"time-series":[`)
	for i := 0; i < 5000; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"type":"resolvent-pv1","site":%d,"capacity_kw":12.5}`, i)
	}
	sb.WriteString(`]}`)
	payload := []byte(sb.String())
	series := []byte(`{"type":"time-series","values":[0.1,0.2,0.3]}`)
	b.ReportAllocs()
	for b.Loop() {
		root, err := Parse(payload)
		if err != nil {
			b.Fatal(err)
		}
		found, err := Find(root, "time-series")
		if err != nil {
			b.Fatal(err)
		}
		for _, f := range found {
			if _, err := f.Substitute(series, true); err != nil {
				b.Fatal(err)
			}
		}
		if _, err := Marshal(root); err != nil {
			b.Fatal(err)
		}
	}
}

// Every found resolvent carries its JSON pointer and a name: the object's
// own "name" field, else its registry key, else nothing for an anonymous
// array element. Pointer tokens are escaped per RFC 6901.
func TestFoundPathsAndNames(t *testing.T) {
	root := parse(t, `{"model":{"timeseries":{
		"pv_cf":{"type":"resolvent-pv1"},
		"a/b~c":{"type":"resolvent-x","name":"explicit"},
		"list":[{"type":"resolvent-y"},{"name":"named","type":"resolvent-y"},{"inner":[{"type":"resolvent-z"}]}]
	}}}`)
	found := find(t, root, "model.timeseries")
	got := map[string]string{}
	for _, f := range found {
		got[f.Path] = f.Name
	}
	want := map[string]string{
		"/model/timeseries/a~1b~0c":        "explicit",
		"/model/timeseries/list/0":         "",
		"/model/timeseries/list/1":         "named",
		"/model/timeseries/list/2/inner/0": "",
		"/model/timeseries/pv_cf":          "pv_cf",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paths/names = %v\nwant %v", got, want)
	}
	// Root scan and a container that is itself the resolvent.
	rootScan := find(t, parse(t, `{"weather":{"type":"resolvent-weather"},"x":[{"type":"resolvent-a"}]}`), "")
	if rootScan[0].Path != "/weather" || rootScan[0].Name != "weather" || rootScan[1].Path != "/x/0" {
		t.Errorf("root scan paths = %s %s", rootScan[0].Path, rootScan[1].Path)
	}
	self := find(t, parse(t, `{"weather":{"type":"resolvent-weather"}}`), "weather")
	if self[0].Path != "/weather" || self[0].Name != "weather" {
		t.Errorf("container-as-resolvent path/name = %s/%s", self[0].Path, self[0].Name)
	}
}

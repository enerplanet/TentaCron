package resolver

import (
	"encoding/json"
	"strings"
	"testing"
)

// FuzzParseFindSubstitute drives arbitrary documents and container paths
// through the whole engine: valid JSON must never make Find error or panic,
// every found resolvent must be substitutable, the result must re-encode as
// valid JSON, and no resolvent may remain in the container afterwards.
func FuzzParseFindSubstitute(f *testing.F) {
	seeds := []string{
		`{"time-series":[{"type":"resolvent-pv1","a":1}]}`,
		`{"model":{"timeseries":{"x":{"type":"resolvent-x"},"y":{"type":"time-series","v":[1]}}}}`,
		`{"a":[[[{"type":"resolvent-deep"}]]]}`,
		`{"time-series":{"type":"resolvent-self"}}`,
		`{"time-series":[{"type":"resolvent-a"},{"type":"resolvent-a"},7,"s",null,{"type":5}]}`,
		`{"type":"time-series","time-series":[{"type":"resolvent-hidden"}]}`,
		`{"n":12345678901234567890,"time-series":[{"type":"resolvent-n","v":1e400}]}`,
		`null`, `[]`, `{}`, `{"time-series":null}`,
	}
	for _, s := range seeds {
		f.Add(s, "time-series")
		f.Add(s, "")
		f.Add(s, "model.timeseries")
		f.Add(s, "a..b")
	}
	f.Fuzz(func(t *testing.T, payload, path string) {
		root, err := Parse([]byte(payload))
		if err != nil {
			return
		}
		found, err := Find(root, path)
		if err != nil {
			t.Fatalf("Find on valid JSON must not error: %v", err)
		}
		for i, fd := range found {
			if !strings.HasPrefix(fd.Type, TypePrefix) {
				t.Fatalf("found type %q without prefix", fd.Type)
			}
			if len(fd.Hash) != 64 {
				t.Fatalf("hash %q is not hex SHA-256", fd.Hash)
			}
			if _, err := fd.Substitute([]byte(`{"type":"time-series","values":[1]}`), i%2 == 0); err != nil {
				t.Fatalf("Substitute: %v", err)
			}
		}
		out, err := Marshal(root)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if !json.Valid(out) {
			t.Fatalf("Marshal produced invalid JSON: %s", out)
		}
		if again, _ := Find(root, path); len(again) != 0 {
			t.Fatalf("%d resolvents remain after substituting all %d", len(again), len(found))
		}
	})
}

// FuzzSubstituteBody: any bytes as a resource response either substitute
// cleanly (a JSON object) or error — never panic, never corrupt the slot.
func FuzzSubstituteBody(f *testing.F) {
	for _, s := range []string{`{}`, `null`, `[]`, `"s"`, `{"type":"time-series"}`, `{"resolvent":1}`, `{"a":`, ``} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, body string) {
		root, _ := Parse([]byte(`{"time-series":[{"type":"resolvent-x","p":1}]}`))
		found, _ := Find(root, "time-series")
		_, err := found[0].Substitute([]byte(body), true)
		slot := root["time-series"].([]any)[0].(map[string]any)
		if err != nil {
			if slot["type"] != "resolvent-x" {
				t.Fatalf("failed substitution must leave the slot untouched: %v", slot)
			}
			return
		}
		if _, ok := slot[ResolventKey].(map[string]any); !ok {
			t.Fatalf("substituted slot lacks the resolvent marker: %v", slot)
		}
	})
}

// FuzzApplyMap feeds arbitrary responses and path expressions through the
// response mapper: it must never panic, and whatever it produces must be a
// valid JSON object.
func FuzzApplyMap(f *testing.F) {
	for _, body := range []string{
		`{"outputs":{"hourly":[{"time":"a","P":1},{"time":"b","P":2.5}]},"inputs":{"x":null}}`,
		`[1,[2,[3]]]`, `{"a":{"b":{"c":[1,2,3]}}}`, `null`, `{}`, `{"n":1e400,"big":12345678901234567890}`,
	} {
		for _, expr := range []string{".", ".outputs.hourly[*].P", ".outputs.hourly.0.time", ".a.b.c[*]", ".0.1.0", ".x[*]", "literal", ".a..b"} {
			f.Add(body, expr)
		}
	}
	f.Fuzz(func(t *testing.T, body, expr string) {
		m := map[string]any{"v": expr, "s": map[string]any{"path": expr, "scale": 0.5}, "lit": 7}
		out, err := ApplyMap([]byte(body), m)
		if err != nil {
			return
		}
		var obj map[string]any
		if json.Unmarshal(out, &obj) != nil {
			t.Fatalf("ApplyMap produced invalid JSON: %s", out)
		}
	})
}

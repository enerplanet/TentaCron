package resolver

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// chain parses, finds and orders a registry payload, returning the levels
// as names so a test can state the expected order literally.
func chain(t *testing.T, payload string) ([][]*Found, error) {
	t.Helper()
	found := find(t, parse(t, payload), "time-series")
	return Chain(found)
}

func names(levels [][]*Found) [][]string {
	out := make([][]string, len(levels))
	for i, lvl := range levels {
		out[i] = []string{}
		for _, f := range lvl {
			out[i] = append(out[i], f.label())
		}
	}
	return out
}

func TestChainOrdersLevelsAndKeepsDocumentOrder(t *testing.T) {
	levels, err := chain(t, `{"time-series":{
		"d":{"type":"resolvent-x","a":{"$from":"b","path":"v"},"c":{"$from":"c"}},
		"c":{"type":"resolvent-x","a":{"$from":"a","path":".v"}},
		"b":{"type":"resolvent-x","a":{"$from":"a","path":"v"}},
		"a":{"type":"resolvent-x","p":1},
		"e":{"type":"resolvent-x","p":2}}}`)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"a", "e"}, {"b", "c"}, {"d"}}
	if got := names(levels); !reflect.DeepEqual(got, want) {
		t.Errorf("levels = %v, want %v (a diamond: d needs b and c, both need a)", got, want)
	}
	if deps := levels[2][0].DependsOn(); !reflect.DeepEqual(deps, []string{"b", "c"}) {
		t.Errorf("d depends on %v", deps)
	}
}

func TestChainWithoutReferencesIsOneLevel(t *testing.T) {
	levels, err := chain(t, `{"time-series":[{"type":"resolvent-x","a":1},{"type":"resolvent-y","$from":"not-a-ref-at-top-level"}]}`)
	if err != nil || len(levels) != 1 || len(levels[0]) != 2 {
		t.Errorf("levels = %v, err = %v", names(levels), err)
	}
}

func TestChainRejectsBadReferences(t *testing.T) {
	cases := map[string]string{
		`{"time-series":{"a":{"type":"resolvent-x","p":{"$from":"zz"}}}}`:                                                                                          `no resolvent named "zz"`,
		`{"time-series":{"a":{"type":"resolvent-x","p":{"$from":"a"}}}}`:                                                                                           `references itself`,
		`{"time-series":{"a":{"type":"resolvent-x","p":{"$from":"b"}},"b":{"type":"resolvent-x","p":{"$from":"a"}}}}`:                                              `cycle: a -> b -> a`,
		`{"time-series":{"a":{"type":"resolvent-x","p":{"$from":"b"}},"b":{"type":"resolvent-x","p":{"$from":"c"}},"c":{"type":"resolvent-x","p":{"$from":"b"}}}}`: `cycle: a -> b -> c -> b`,
		`{"time-series":{"a":{"type":"resolvent-x","p":{"$from":"n"}},"x":{"type":"resolvent-x","name":"n"},"y":{"type":"resolvent-x","name":"n"}}}`:               `2 resolvents are named "n"`,
		`{"time-series":{"a":{"type":"resolvent-x","p":{"$from":""}}}}`:                                                                                            `$from must be a non-empty string`,
		`{"time-series":{"a":{"type":"resolvent-x","p":{"$from":"b","path":7}},"b":{"type":"resolvent-x"}}}`:                                                       `path must be a string`,
		`{"time-series":{"a":{"type":"resolvent-x","p":{"$from":"b","path":"x..y"}},"b":{"type":"resolvent-x"}}}`:                                                  `empty segment`,
		`{"time-series":{"a":{"type":"resolvent-x","p":{"$from":"b","scale":2}},"b":{"type":"resolvent-x"}}}`:                                                      `unknown key "scale"`,
	}
	for payload, want := range cases {
		_, err := chain(t, payload)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", payload, err, want)
		}
	}
	// Error messages locate the reference with a JSON pointer.
	_, err := chain(t, `{"time-series":{"a":{"type":"resolvent-x","nested":{"list":[1,{"$from":"zz"}]}}}}`)
	if err == nil || !strings.Contains(err.Error(), "/time-series/a/nested/list/1") {
		t.Errorf("err = %v, want the reference's pointer", err)
	}
}

func TestChainCapsDepth(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"time-series":{"r0":{"type":"resolvent-x","p":1}`)
	for i := 1; i <= MaxChainDepth; i++ {
		b.WriteString(`,"r` + string(rune('0'+i)) + `":{"type":"resolvent-x","p":{"$from":"r` + string(rune('0'+i-1)) + `"}}`)
	}
	b.WriteString(`}}`)
	if _, err := chain(t, b.String()); err == nil || !strings.Contains(err.Error(), "deeper than 8 levels") {
		t.Errorf("a chain of %d levels must be refused: %v", MaxChainDepth+1, err)
	}
}

func TestFillReplacesReferencesAndRehashes(t *testing.T) {
	root := parse(t, `{"time-series":{
		"building":{"type":"resolvent-city2tabula","osm_id":1},
		"typology":{"type":"resolvent-ignis","name":"typo","code":{"$from":"building","path":"tabula_variant_code"},
			"payload":{"storeys":{"$from":"building","path":".number_of_storeys"},"whole":{"$from":"building"},"walls":{"$from":"building","path":"walls[*].u"}}}}}`)
	found := find(t, root, "time-series")
	levels, err := Chain(found)
	if err != nil {
		t.Fatal(err)
	}
	building, typology := levels[0][0], levels[1][0]
	if filled, err := building.Fill(nil); filled || err != nil {
		t.Errorf("a resolvent without references fills nothing: %v %v", filled, err)
	}
	before := typology.Hash
	series := []byte(`{"tabula_variant_code":"DE.N.SFH.04","number_of_storeys":2,"big":12345678901234567890,"walls":[{"u":1.5},{"u":0.25}]}`)
	filled, err := typology.Fill(func(dep *Found) []byte {
		if dep != building {
			t.Errorf("unexpected dependency %s", dep.label())
		}
		return series
	})
	if !filled || err != nil {
		t.Fatalf("fill: %v %v", filled, err)
	}
	got, _ := json.Marshal(typology.Input)
	want := `{"code":"DE.N.SFH.04","name":"typo","payload":{"storeys":2,"walls":[1.5,0.25],"whole":{"big":12345678901234567890,"number_of_storeys":2,"tabula_variant_code":"DE.N.SFH.04","walls":[{"u":1.5},{"u":0.25}]}},"type":"resolvent-ignis"}`
	if string(got) != want {
		t.Errorf("input = %s\nwant    %s", got, want)
	}
	// The payload still holds the reference (the marker's content), and
	// the cache key now reflects the filled input.
	orig, _ := json.Marshal(typology.Object)
	if !strings.Contains(string(orig), `"$from":"building"`) {
		t.Errorf("payload object must keep its references: %s", orig)
	}
	if err := typology.Rehash(nil); err != nil {
		t.Fatal(err)
	}
	if typology.Hash == before {
		t.Error("hash must change once references are filled")
	}
	other := &Found{Type: "resolvent-ignis", Input: map[string]any{"type": "resolvent-ignis", "code": "DE.N.SFH.04", "name": "typo", "payload": typology.Input["payload"]}}
	if err := other.Rehash(nil); err != nil || other.Hash != typology.Hash {
		t.Error("the filled hash must equal the hash of a literal object with the same parameters")
	}
}

func TestFillReportsMissingValues(t *testing.T) {
	root := parse(t, `{"time-series":{"a":{"type":"resolvent-x","p":1},"b":{"type":"resolvent-x","p":{"$from":"a","path":"missing.deep"}}}}`)
	found := find(t, root, "time-series")
	if _, err := Chain(found); err != nil {
		t.Fatal(err)
	}
	b := found[1]
	_, err := b.Fill(func(*Found) []byte { return []byte(`{"other":1}`) })
	if !errors.Is(err, ErrReference) || !strings.Contains(err.Error(), `series of "a"`) || !strings.Contains(err.Error(), `"missing": not found`) {
		t.Errorf("err = %v", err)
	}
	if _, err := b.Fill(func(*Found) []byte { return nil }); !errors.Is(err, ErrReference) || !strings.Contains(err.Error(), "not resolved") {
		t.Errorf("unresolved dependency: %v", err)
	}
	if _, err := b.Fill(func(*Found) []byte { return []byte(`nope`) }); !errors.Is(err, ErrReference) {
		t.Errorf("malformed series: %v", err)
	}
	if b.Input["p"].(map[string]any)["$from"] != "a" {
		t.Error("a failed fill must leave Input untouched")
	}
}

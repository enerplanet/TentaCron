package upstream

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/enerplanet/tentacron/internal/config"
)

func decodeFuzzPayload(s string) (map[string]any, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

// FuzzBuildResolventURL: any template and any resolvent object either map to
// a parseable URL or return an error — never panic, never leave a filled
// placeholder's field in the query.
func FuzzBuildResolventURL(f *testing.F) {
	f.Add("https://w.example.com/point?format=json", `{"type":"t","lat":48.8,"ids":["1","2"],"ok":true}`)
	f.Add("https://i.example.com/data/{code}", `{"type":"t","code":"DE.N.SFH.04"}`)
	f.Add("https://x.example.com/{a}/{b}?{c}=1", `{"a":"1","b":2,"c":"q"}`)
	f.Add("http://[::1]:8080/{x}", `{"x":"a b/c"}`)
	f.Add("not a url", `{"type":"t"}`)
	f.Add("https://x.example.com/{code}", `{"code":{"nested":1}}`)
	f.Add("https://x.example.com/{}?{c}=1", `{"a":"1","b":2,"c":"c"}`) // placeholder as a query *name*
	f.Fuzz(func(t *testing.T, tmpl, payload string) {
		obj, ok := decodeFuzzPayload(payload)
		if !ok {
			return
		}
		got, err := buildResolventURL(tmpl, obj)
		if err != nil {
			return
		}
		u, perr := url.Parse(got)
		if perr != nil {
			t.Fatalf("built URL %q does not parse: %v", got, perr)
		}
		q := u.Query()
		if _, present := q["type"]; present {
			t.Fatalf("the type marker must never be sent: %s", got)
		}
		// A consumed placeholder field must not be appended as a query
		// parameter by the mapper. It may still show up in the query when
		// the template's own query part references it — as a fixed
		// parameter (?code=1) or as a placeholder in name or value
		// position (?{code}=1, ?x={code}).
		tmplQuery := ""
		if i := strings.IndexByte(tmpl, '?'); i >= 0 {
			tmplQuery = tmpl[i+1:]
		}
		for _, m := range placeholderPattern.FindAllStringSubmatch(tmpl, -1) {
			field := m[1]
			if _, present := q[field]; !present {
				continue
			}
			if strings.Contains(tmplQuery, "{"+field+"}") || strings.Contains(tmplQuery, field+"=") {
				continue
			}
			t.Fatalf("consumed placeholder %q leaked into the query: %s", field, got)
		}
	})
}

// FuzzFillTargetURL: the fill either succeeds — every placeholder field was
// a string or number, is stripped from the document, and every other field
// survives — or errors and leaves the document untouched. (The output is
// not re-scanned for placeholder-looking text: a template like "{{a}}"
// legitimately yields "{0}".)
func FuzzFillTargetURL(f *testing.F) {
	f.Add("https://i.example.com/calc/{code}", `{"code":"DE.N.SFH.04","A_ref":{"value":120}}`)
	f.Add("https://x.example.com/{a}/{a}", `{"a":12345678901234567890}`)
	f.Add("https://x.example.com/{a}", `{"a":true}`)
	f.Add("https://x.example.com/plain", `{"a":1}`)
	f.Add("{{a}}", `{"a":0}`)
	f.Fuzz(func(t *testing.T, tmpl, payload string) {
		var doc map[string]json.RawMessage
		if err := json.Unmarshal([]byte(payload), &doc); err != nil || doc == nil {
			return
		}
		before := make(map[string]json.RawMessage, len(doc))
		for k, v := range doc {
			before[k] = v
		}
		placeholders := map[string]bool{}
		wantErr := false
		for _, m := range placeholderPattern.FindAllStringSubmatch(tmpl, -1) {
			placeholders[m[1]] = true
			if _, ok := scalarFromRaw(before[m[1]]); !ok {
				wantErr = true
			}
		}
		_, err := fillTargetURL(tmpl, doc)
		if (err != nil) != wantErr {
			t.Fatalf("err=%v, want error=%v for template %q payload %s", err, wantErr, tmpl, payload)
		}
		if err != nil {
			if len(doc) != len(before) {
				t.Fatal("document mutated although the fill failed")
			}
			return
		}
		for field := range placeholders {
			if _, still := doc[field]; still {
				t.Fatalf("consumed field %q not stripped", field)
			}
		}
		for k := range before {
			if placeholders[k] {
				continue
			}
			if _, ok := doc[k]; !ok {
				t.Fatalf("non-placeholder field %q removed", k)
			}
		}
	})
}

// FuzzJSONPath: arbitrary documents and paths never panic.
func FuzzJSONPath(f *testing.F) {
	f.Add(`{"a":{"b":[1,{"c":2}]}}`, "a.b.1.c")
	f.Add(`[[[1]]]`, "0.0.0")
	f.Add(`{"a":1}`, "a.b")
	f.Add(`null`, "")
	f.Add(`"s"`, "0")
	f.Fuzz(func(_ *testing.T, body, path string) {
		_, _ = jsonPath([]byte(body), path)
		_, _ = ExtractPath([]byte(body), path)
	})
}

// FuzzExtractJobID: whatever the target answers, an accepted id is always
// safe to substitute into a URL template.
func FuzzExtractJobID(f *testing.F) {
	f.Add(`{"id":"m-1"}`, "id")
	f.Add(`{"id":"x/../admin"}`, "id")
	f.Add(`{"id":9007199254740993}`, "id")
	f.Add(`{"job":{"id":""}}`, "job.id")
	f.Fuzz(func(t *testing.T, body, path string) {
		id, err := ExtractJobID([]byte(body), &config.Poll{IDJSONPath: path})
		if err != nil {
			return
		}
		if !jobIDPattern.MatchString(id) {
			t.Fatalf("accepted unsafe job id %q", id)
		}
		if url.PathEscape(id) != id {
			t.Fatalf("accepted id %q needs escaping", id)
		}
	})
}

// FuzzExcerpt: a configured secret never survives into an error excerpt,
// and the excerpt is always bounded.
func FuzzExcerpt(f *testing.F) {
	f.Add("rejected key SECRET-abcdefghijkl for scenario", "SECRET-abcdefghijkl")
	f.Add(strings.Repeat("x", 600)+"SECRET-abcdefghijkl", "SECRET-abcdefghijkl")
	f.Add("\xff\xfe SECRET-abcdefghijkl", "SECRET-abcdefghijkl")
	f.Fuzz(func(t *testing.T, body, secret string) {
		if len(secret) < 12 || strings.Contains(secret, "…") {
			return
		}
		got := New(1<<20, []string{secret}).excerpt([]byte(body))
		if strings.Contains(got, secret) {
			t.Fatalf("secret survived redaction: %q", got)
		}
		if len(got) > errBodyExcerpt+len("…") {
			t.Fatalf("excerpt too long: %d", len(got))
		}
	})
}

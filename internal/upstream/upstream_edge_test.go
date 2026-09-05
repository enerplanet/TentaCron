package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/enerplanet/tentacron/internal/config"
)

func TestErrorFormattingAndUnwrap(t *testing.T) {
	cases := []struct {
		err  *Error
		want string
	}{
		{&Error{Op: "target meme", Err: errors.New("boom")}, "target meme: boom"},
		{&Error{Op: "resource pv", Status: 400, Body: `{"error":"bad"}`}, `resource pv: HTTP 400: {"error":"bad"}`},
		{&Error{Op: "poll meme", Status: 404}, "poll meme: HTTP 404"},
	}
	for _, tt := range cases {
		if got := tt.err.Error(); got != tt.want {
			t.Errorf("Error() = %q, want %q", got, tt.want)
		}
	}
	wrapped := &Error{Op: "x", Transient: true, Err: context.DeadlineExceeded}
	if !errors.Is(wrapped, context.DeadlineExceeded) {
		t.Error("Unwrap must expose the transport error")
	}
	if !IsTransient(fmt.Errorf("attempt: %w", wrapped)) {
		t.Error("IsTransient must see through wrapping")
	}
	if IsTransient(nil) || IsTransient(errors.New("plain")) {
		t.Error("non-upstream errors are never transient")
	}
}

func TestCallTimeoutIsTransientDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	_, _, err := testClient(1<<20).do(context.Background(), "test", http.MethodGet, srv.URL, nil, nil, 30*time.Millisecond)
	if err == nil || !IsTransient(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("per-call timeout must be a transient deadline error, got %v", err)
	}
}

func TestCancelledContextIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := testClient(1<<20).do(ctx, "test", http.MethodGet, srv.URL, nil, nil, time.Second)
	if err == nil || !IsTransient(err) || !errors.Is(err, context.Canceled) {
		t.Errorf("cancellation must surface as a transient context error, got %v", err)
	}
}

func TestResponseSizeCapBoundary(t *testing.T) {
	const capBytes = 1024
	var size atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, size.Load()))
	}))
	defer srv.Close()
	c := testClient(capBytes)
	for _, n := range []int64{capBytes - 1, capBytes} {
		size.Store(n)
		_, body, err := c.do(context.Background(), "test", http.MethodGet, srv.URL, nil, nil, time.Second)
		if err != nil || int64(len(body)) != n {
			t.Errorf("%d bytes (within cap): err=%v len=%d", n, err, len(body))
		}
	}
	size.Store(capBytes + 1)
	if _, _, err := c.do(context.Background(), "test", http.MethodGet, srv.URL, nil, nil, time.Second); err == nil || IsTransient(err) {
		t.Errorf("one byte over the cap must fail permanently, got %v", err)
	}
}

func TestSendHeadersAndMethod(t *testing.T) {
	var gotMethod, gotCT, gotAccept, gotCustom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotCT, gotAccept, gotCustom = r.Method, r.Header.Get("Content-Type"), r.Header.Get("Accept"), r.Header.Get("X-Custom")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := testClient(1 << 20)
	if _, _, err := c.do(context.Background(), "t", http.MethodGet, srv.URL, nil, map[string]string{"X-Custom": "1"}, time.Second); err != nil {
		t.Fatal(err)
	}
	if gotCT != "" || gotAccept != "application/json" || gotCustom != "1" {
		t.Errorf("bodyless GET: content-type=%q accept=%q custom=%q", gotCT, gotAccept, gotCustom)
	}
	if _, _, err := c.do(context.Background(), "t", http.MethodPut, srv.URL, []byte(`{}`), nil, time.Second); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPut || gotCT != "application/json" {
		t.Errorf("PUT with body: method=%s content-type=%q", gotMethod, gotCT)
	}
}

func TestNon2xxWithEmptyBodyAndNoContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/404":
			w.WriteHeader(http.StatusNotFound)
		case "/204":
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	c := testClient(1 << 20)
	_, _, err := c.do(context.Background(), "test", http.MethodGet, srv.URL+"/404", nil, nil, time.Second)
	if err == nil || err.Error() != "test: HTTP 404" {
		t.Errorf("empty error body must format as status only, got %v", err)
	}
	status, body, err := c.do(context.Background(), "test", http.MethodGet, srv.URL+"/204", nil, nil, time.Second)
	if err != nil || status != http.StatusNoContent || len(body) != 0 {
		t.Errorf("204: status=%d len=%d err=%v", status, len(body), err)
	}
}

func TestMalformedURLIsPermanent(t *testing.T) {
	_, _, err := testClient(1<<20).do(context.Background(), "test", http.MethodGet, "://nope", nil, nil, time.Second)
	if err == nil || IsTransient(err) {
		t.Errorf("a request that cannot be built is a permanent error, got %v", err)
	}
}

func TestExcerptTruncatesAndRedacts(t *testing.T) {
	secrets := (&config.Config{Targets: map[string]config.Target{
		"a": {APIKey: "ab"}, "b": {APIKey: "abcdef"},
	}}).UpstreamSecrets()
	c := New(1<<20, secrets)

	if got := c.excerpt([]byte("xxabcdefyy")); got != "xx[redacted]yy" {
		t.Errorf("longest secret must be redacted whole, got %q", got)
	}
	if got := c.excerpt([]byte("ab-abcdef")); got != "[redacted]-[redacted]" {
		t.Errorf("every secret redacted, got %q", got)
	}
	long := c.excerpt([]byte(strings.Repeat("z", 600)))
	if !strings.HasSuffix(long, "…") || len(long) != errBodyExcerpt+len("…") {
		t.Errorf("excerpt must be cut at %d bytes plus an ellipsis, got len %d", errBodyExcerpt, len(long))
	}
	straddling := strings.Repeat("z", errBodyExcerpt-3) + "abcdef" + strings.Repeat("y", 100)
	if got := c.excerpt([]byte(straddling)); strings.Contains(got, "abcdef") {
		t.Errorf("a secret straddling the cut must be redacted before truncation: %q", got)
	}
	if got := c.excerpt([]byte("\xff\xfeok")); got != "ok" {
		t.Errorf("invalid UTF-8 must be stripped, got %q", got)
	}
	// 3-byte runes never align with the 512-byte cut: the excerpt must end
	// on a rune boundary and stay valid UTF-8.
	multibyte := c.excerpt([]byte(strings.Repeat("€", 300)))
	if !utf8.ValidString(multibyte) || !strings.HasSuffix(multibyte, "€…") || len(multibyte) > errBodyExcerpt+len("…") {
		t.Errorf("multibyte cut must fall on a rune boundary: valid=%v len=%d", utf8.ValidString(multibyte), len(multibyte))
	}
	if got := c.excerpt([]byte("{\"error\":\"x\"}\n")); got != `{"error":"x"}` {
		t.Errorf("trailing newline (http.Error) must be trimmed, got %q", got)
	}
}

func TestResolveResolventPOSTBodyFidelity(t *testing.T) {
	var gotMethod, gotCT, gotAuth string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotCT, gotAuth = r.Method, r.Header.Get("Content-Type"), r.Header.Get("X-Res-Key")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"type":"time-series"}`)
	}))
	defer srv.Close()
	rcfg := config.Resolvent{URL: srv.URL, Method: http.MethodPut, APIKey: "rk", APIKeyHeader: "X-Res-Key", Timeout: dur(time.Second)}
	payload := decodePayload(t, `{"type":"resolvent-x","meter":1234567890123456789,"nested":{"a":[1,2.50]}}`)
	if _, err := testClient(1<<20).ResolveResolvent(context.Background(), "resolvent-x", rcfg, payload); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPut || gotCT != "application/json" || gotAuth != "rk" {
		t.Errorf("method=%s content-type=%q auth=%q", gotMethod, gotCT, gotAuth)
	}
	for _, want := range []string{"1234567890123456789", "2.50", `"type":"resolvent-x"`} {
		if !strings.Contains(string(gotBody), want) {
			t.Errorf("body %s must contain %s verbatim", gotBody, want)
		}
	}
}

func TestResolveResolventGETRejectsNestedObjectWithoutCalling(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	rcfg := config.Resolvent{URL: srv.URL, Method: http.MethodGet, Timeout: dur(time.Second)}
	payload := decodePayload(t, `{"type":"resolvent-x","location":{"lat":1}}`)
	_, err := testClient(1<<20).ResolveResolvent(context.Background(), "resolvent-x", rcfg, payload)
	if err == nil || IsTransient(err) {
		t.Errorf("nested object must be a permanent authoring error, got %v", err)
	}
	if calls.Load() != 0 {
		t.Errorf("no request may be sent for an unmappable object, got %d", calls.Load())
	}
}

func TestBuildResolventURLEdgeCases(t *testing.T) {
	cases := []struct{ name, tmpl, payload, want, wantErr string }{
		{"duplicate placeholder", "https://x.example.com/{code}/v/{code}", `{"type":"t","code":"AB"}`, "https://x.example.com/AB/v/AB", ""},
		{"placeholder value escaped", "https://x.example.com/d/{code}", `{"type":"t","code":"a/b c?"}`, "https://x.example.com/d/a%2Fb%20c%3F", ""},
		{"field overrides fixed query", "https://x.example.com/q?format=json", `{"type":"t","format":"csv"}`, "https://x.example.com/q?format=csv", ""},
		{"bool false", "https://x.example.com/q", `{"type":"t","flag":false}`, "https://x.example.com/q?flag=false", ""},
		{"empty array", "https://x.example.com/q", `{"type":"t","ids":[]}`, "https://x.example.com/q?ids=", ""},
		{"mixed scalar array", "https://x.example.com/q", `{"type":"t","ids":["a",1,true]}`, "https://x.example.com/q?ids=a%2C1%2Ctrue", ""},
		{"placeholder in query", "https://x.example.com/q?c={code}", `{"type":"t","code":"a b"}`, "https://x.example.com/q?c=a+b", ""},
		{"only type sends nothing", "https://x.example.com/q", `{"type":"t"}`, "https://x.example.com/q", ""},
		{"null value", "https://x.example.com/q", `{"type":"t","v":null}`, "", "not a scalar"},
		{"array with object element", "https://x.example.com/q", `{"type":"t","ids":[{"a":1}]}`, "", "element 0"},
		{"placeholder object value", "https://x.example.com/{code}", `{"type":"t","code":{"a":1}}`, "", "placeholder"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildResolventURL(tt.tmpl, decodePayload(t, tt.payload), nil)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v (url %q)", tt.wantErr, err, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("url = %s\nwant  %s", got, tt.want)
			}
		})
	}
	// A brace group that is not a placeholder ([A-Za-z0-9_]+) stays literal
	// and the field of that name goes to the query like any other.
	got, err := buildResolventURL("https://x.example.com/{co-de}", decodePayload(t, `{"type":"t","co-de":"x"}`), nil)
	if err != nil || !strings.Contains(got, "%7Bco-de%7D") || !strings.Contains(got, "co-de=x") {
		t.Errorf("non-placeholder braces: url=%q err=%v", got, err)
	}
}

func rawDoc(t *testing.T, s string) map[string]json.RawMessage {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestFillTargetURLEdgeCases(t *testing.T) {
	for _, payload := range []string{`{"code":true}`, `{"code":{"a":1}}`, `{"code":null}`, `{"code":[1]}`, `{"outer":{"code":"A"}}`} {
		if _, err := fillTargetURL("https://x.example.com/{code}", rawDoc(t, payload)); err == nil || !strings.Contains(err.Error(), "placeholder") {
			t.Errorf("%s: want placeholder error, got %v", payload, err)
		}
	}
	doc := rawDoc(t, `{"code":1e3,"x":1}`)
	got, err := fillTargetURL("https://x.example.com/{code}", doc)
	if err != nil || got != "https://x.example.com/1e3" {
		t.Errorf("numeric placeholder: url=%q err=%v", got, err)
	}
	if _, consumed := doc["code"]; consumed || len(doc) != 1 {
		t.Errorf("consumed field must be deleted and the rest kept: %v", doc)
	}
	got, err = fillTargetURL("https://x.example.com/{code}/{code}", rawDoc(t, `{"code":"a/b"}`))
	if err != nil || got != "https://x.example.com/a%2Fb/a%2Fb" {
		t.Errorf("duplicate placeholder with escaping: url=%q err=%v", got, err)
	}
}

func TestForwardBodyFieldOverridesClientSuppliedKey(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	tcfg := config.Target{URL: srv.URL, Method: "POST", Timeout: dur(time.Second),
		APIKey: "target-secret", APIKeyInject: config.InjectBodyField, APIKeyField: "api_key"}
	if _, err := testClient(1<<20).ForwardToTarget(context.Background(), "meme", tcfg, []byte(`{"api_key":"client-attempt","m":1}`)); err != nil {
		t.Fatal(err)
	}
	if received["api_key"] != "target-secret" {
		t.Errorf("client-supplied key must be overwritten by the target's, got %v", received["api_key"])
	}
}

func TestForwardRewritesRejectNonObjectPayload(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	bodyField := config.Target{URL: srv.URL, Method: "POST", Timeout: dur(time.Second),
		APIKey: "k", APIKeyInject: config.InjectBodyField, APIKeyField: "api_key"}
	templated := config.Target{URL: srv.URL + "/{code}", Method: "POST", Timeout: dur(time.Second), APIKeyInject: config.InjectNone}
	for name, tcfg := range map[string]config.Target{"body_field": bodyField, "templated": templated} {
		_, err := testClient(1<<20).ForwardToTarget(context.Background(), name, tcfg, []byte(`[1]`))
		if err == nil || IsTransient(err) {
			t.Errorf("%s: array payload must be a permanent error, got %v", name, err)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("no request may be sent when the rewrite fails, got %d", calls.Load())
	}
}

func TestForwardTemplatingAndBodyFieldTogether(t *testing.T) {
	var gotPath string
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&received)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	tcfg := config.Target{URL: srv.URL + "/calc/{code}", Method: "POST", Timeout: dur(time.Second),
		APIKey: "k", APIKeyInject: config.InjectBodyField, APIKeyField: "api_key"}
	if _, err := testClient(1<<20).ForwardToTarget(context.Background(), "t", tcfg, []byte(`{"code":"C1","x":1}`)); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/calc/C1" || received["api_key"] != "k" || received["x"] != float64(1) {
		t.Errorf("path=%s body=%v", gotPath, received)
	}
	if _, present := received["code"]; present {
		t.Errorf("consumed placeholder field must be stripped: %v", received)
	}
}

func TestPollTargetStatusValueTypes(t *testing.T) {
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body.Load().(string))
	}))
	defer srv.Close()
	tcfg := config.Target{Timeout: dur(time.Second), APIKeyInject: config.InjectNone,
		Response: config.Response{Mode: config.ModePoll, Poll: &config.Poll{
			URLTemplate: srv.URL + "/{id}", StatusJSONPath: "status",
			DoneValues: []string{"done", "2"}, FailedValues: []string{"failed", "true"},
		}}}
	cases := []struct {
		body         string
		done, failed bool
		wantErr      bool
	}{
		{`{"status":2}`, true, false, false},
		{`{"status":true}`, false, true, false},
		{`{"status":{"x":1}}`, false, false, false},
		{`{"status":null}`, false, false, false},
		{`{"nostatus":1}`, false, false, true},
		{`not json`, false, false, true},
	}
	for _, tt := range cases {
		body.Store(tt.body)
		st, err := testClient(1<<20).PollTarget(context.Background(), "meme", tcfg, "j1")
		if tt.wantErr {
			if err == nil || IsTransient(err) {
				t.Errorf("%s: want permanent error, got %v", tt.body, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tt.body, err)
			continue
		}
		if st.Done != tt.done || st.Failed != tt.failed {
			t.Errorf("%s: done=%v failed=%v", tt.body, st.Done, st.Failed)
		}
	}
}

func TestPollStatusHTTPClassification(t *testing.T) {
	var status atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"x"}`, int(status.Load()))
	}))
	defer srv.Close()
	tcfg := pollTargetCfg(srv.URL)
	status.Store(500)
	if _, err := testClient(1<<20).PollTarget(context.Background(), "meme", tcfg, "j1"); err == nil || !IsTransient(err) {
		t.Errorf("500 on status must be transient, got %v", err)
	}
	status.Store(404)
	if _, err := testClient(1<<20).PollTarget(context.Background(), "meme", tcfg, "j1"); err == nil || IsTransient(err) {
		t.Errorf("404 on status must be permanent, got %v", err)
	}
}

// Header injection reaches the status polls and the result fetch;
// body-field injection cannot (they are bodiless GETs) — a pinned
// consequence operators must know when a status endpoint needs auth.
func TestPollAndResultAuthByInjectionMode(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("X-Api-Key"))
		_, _ = io.WriteString(w, `{"status":"done"}`)
	}))
	defer srv.Close()
	c := testClient(1 << 20)
	for _, tt := range []struct {
		name   string
		inject string
		want   string
	}{{"header", config.InjectHeader, "k"}, {"body_field", config.InjectBodyField, ""}} {
		seen = nil
		tcfg := pollTargetCfg(srv.URL)
		tcfg.APIKey, tcfg.APIKeyInject, tcfg.APIKeyHeader, tcfg.APIKeyField = "k", tt.inject, "X-Api-Key", "api_key"
		if _, err := c.PollTarget(context.Background(), "meme", tcfg, "j1"); err != nil {
			t.Fatal(err)
		}
		stream, err := c.FetchResult(context.Background(), "meme", tcfg, "j1")
		if err != nil {
			t.Fatal(err)
		}
		_ = stream.Close()
		if len(seen) != 2 || seen[0] != tt.want || seen[1] != tt.want {
			t.Errorf("%s injection: headers on poll/result = %q, want %q on both", tt.name, seen, tt.want)
		}
	}
}

func TestFetchResultClassificationAndContentType(t *testing.T) {
	var status atomic.Int64
	var ct atomic.Value
	ct.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if c := ct.Load().(string); c != "" {
			w.Header().Set("Content-Type", c)
		} else {
			w.Header()["Content-Type"] = nil // suppress Go's sniffing
		}
		w.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(w, "bytes")
	}))
	defer srv.Close()
	c := testClient(1 << 20)
	tcfg := pollTargetCfg(srv.URL)
	for _, tt := range []struct {
		status    int
		transient bool
	}{{404, false}, {503, true}, {429, true}, {400, false}} {
		status.Store(int64(tt.status))
		stream, err := c.FetchResult(context.Background(), "meme", tcfg, "j1")
		if err == nil || stream != nil || IsTransient(err) != tt.transient {
			t.Errorf("status %d: err=%v transient=%v, want %v", tt.status, err, IsTransient(err), tt.transient)
		}
	}
	status.Store(200)
	ct.Store("application/zip")
	stream, err := c.FetchResult(context.Background(), "meme", tcfg, "j1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(stream.Body)
	_ = stream.Close()
	if stream.ContentType != "application/zip" || string(body) != "bytes" {
		t.Errorf("ct=%q body=%q", stream.ContentType, body)
	}
	ct.Store("")
	stream, err = c.FetchResult(context.Background(), "meme", tcfg, "j1")
	if err != nil || stream.ContentType != "" {
		t.Errorf("missing content type must be reported empty, got %q (err %v)", stream.ContentType, err)
	}
	if stream != nil {
		_ = stream.Close()
	}
}

func TestExtractJobIDBoundaries(t *testing.T) {
	poll := &config.Poll{IDJSONPath: "id"}
	ok256 := strings.Repeat("a", 256)
	if got, err := ExtractJobID([]byte(`{"id":"`+ok256+`"}`), poll); err != nil || got != ok256 {
		t.Errorf("256-char id must be accepted: %v", err)
	}
	if _, err := ExtractJobID([]byte(`{"id":"`+ok256+`a"}`), poll); err == nil {
		t.Error("257-char id must be rejected")
	}
	if got, err := ExtractJobID([]byte(`{"id":1.5}`), poll); err != nil || got != "1.5" {
		t.Errorf("fractional numeric id: %q %v", got, err)
	}
	for _, body := range []string{`{"id":true}`, `{"id":null}`, `{"id":{"x":1}}`} {
		if _, err := ExtractJobID([]byte(body), poll); err == nil {
			t.Errorf("%s: want unsupported-type error", body)
		}
	}
	nested := &config.Poll{IDJSONPath: "jobs.0.id"}
	if got, err := ExtractJobID([]byte(`{"jobs":[{"id":"j-9"}]}`), nested); err != nil || got != "j-9" {
		t.Errorf("array index in path: %q %v", got, err)
	}
}

func TestJSONPathEdges(t *testing.T) {
	body := []byte(`{"a":[[1,2],[3]],"s":"x"}`)
	if v, err := jsonPath(body, "a.0.1"); err != nil || v != json.Number("2") {
		t.Errorf("a.0.1 = %v (%v)", v, err)
	}
	for _, path := range []string{"a.-1", "a.5", "a.0.1.x", "s.deeper", "", "0", "a.first"} {
		if _, err := jsonPath(body, path); err == nil {
			t.Errorf("path %q must error", path)
		}
	}
}

// The cancel call is a DELETE on the template with the target's auth; a
// non-2xx answer is reported so the caller can log it.
func TestCancelTargetSendsDeleteWithAuth(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var status atomic.Int64
	status.Store(204)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.EscapedPath(), r.Header.Get("X-Api-Key")
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	tcfg := pollTargetCfg(srv.URL)
	tcfg.Response.Poll.CancelURLTemplate = srv.URL + "/jobs/{id}/cancel"
	tcfg.APIKey, tcfg.APIKeyInject, tcfg.APIKeyHeader = "k", config.InjectHeader, "X-Api-Key"
	if err := testClient(1<<20).CancelTarget(context.Background(), "meme", tcfg, "j 1"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/jobs/j%201/cancel" || gotAuth != "k" {
		t.Errorf("cancel call: %s %s auth=%q", gotMethod, gotPath, gotAuth)
	}
	status.Store(404)
	if err := testClient(1<<20).CancelTarget(context.Background(), "meme", tcfg, "j1"); err == nil {
		t.Error("a non-2xx answer must be reported")
	}
}

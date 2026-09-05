package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enerplanet/tentacron/internal/config"
)

func testClient(maxBody int64) *Client {
	return New(maxBody, nil)
}

func dur(d time.Duration) config.Duration { return config.Duration(d) }

func TestErrorClassification(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		wantTransient bool
	}{
		{"server error", 500, true},
		{"bad gateway", 502, true},
		{"rate limited", 429, true},
		{"bad request", 400, false},
		{"not found", 404, false},
		{"unauthorized", 401, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, `{"error":"nope"}`, tt.status)
			}))
			defer srv.Close()
			c := testClient(1 << 20)
			_, _, err := c.do(context.Background(), "test", "POST", srv.URL, []byte(`{}`), nil, time.Second)
			if err == nil {
				t.Fatal("want error")
			}
			if IsTransient(err) != tt.wantTransient {
				t.Errorf("IsTransient = %v, want %v (err: %v)", IsTransient(err), tt.wantTransient, err)
			}
		})
	}
}

func TestConnectionErrorIsTransient(t *testing.T) {
	c := testClient(1 << 20)
	// Port from a server we already closed: connection refused.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	_, _, err := c.do(context.Background(), "test", "POST", url, []byte(`{}`), nil, time.Second)
	if err == nil || !IsTransient(err) {
		t.Errorf("connection error must be transient, got %v", err)
	}
}

func TestOversizedResponseRejectedPermanently(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 2048))
	}))
	defer srv.Close()
	c := testClient(1024)
	_, _, err := c.do(context.Background(), "test", "GET", srv.URL, nil, nil, time.Second)
	if err == nil || IsTransient(err) {
		t.Errorf("oversized response must fail permanently, got %v", err)
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error should mention the limit: %v", err)
	}
}

func TestForwardBodyFieldInjectionLeavesInputAlone(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	tcfg := config.Target{
		URL: srv.URL, Method: "POST", Timeout: dur(time.Second),
		APIKey: "target-secret", APIKeyInject: config.InjectBodyField, APIKeyField: "api_key",
	}
	payload := []byte(`{"scenario":"s1"}`)
	if _, err := testClient(1<<20).ForwardToTarget(context.Background(), "meme", tcfg, payload); err != nil {
		t.Fatal(err)
	}
	if received["api_key"] != "target-secret" || received["scenario"] != "s1" {
		t.Errorf("outbound body = %v", received)
	}
	if string(payload) != `{"scenario":"s1"}` {
		t.Errorf("caller's payload mutated: %s", payload)
	}
}

func TestForwardHeaderInjection(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	tcfg := config.Target{
		URL: srv.URL, Method: "POST", Timeout: dur(time.Second),
		APIKey: "hdr-secret", APIKeyInject: config.InjectHeader, APIKeyHeader: "X-API-Key",
	}
	if _, err := testClient(1<<20).ForwardToTarget(context.Background(), "buem", tcfg, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if gotKey != "hdr-secret" {
		t.Errorf("header key = %q", gotKey)
	}
}

func TestExtractJobID(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		path    string
		want    string
		wantErr bool
	}{
		{"string id", `{"job_id":"m-1"}`, "job_id", "m-1", false},
		{"nested id", `{"data":{"id":"x9"}}`, "data.id", "x9", false},
		{"numeric id", `{"job_id":77}`, "job_id", "77", false},
		{"missing", `{"other":1}`, "job_id", "", true},
		{"empty string", `{"job_id":""}`, "job_id", "", true},
		{"wrong type", `{"job_id":[1]}`, "job_id", "", true},
		{"not json", `oops`, "job_id", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtractJobID([]byte(tt.body), &config.Poll{IDJSONPath: tt.path})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("id = %q, want %q", got, tt.want)
			}
		})
	}
}

// Go's default redirect policy forwards custom headers (our injected API
// keys) to cross-origin redirect targets; the client must not follow
// redirects at all.
func TestRedirectsAreNotFollowed(t *testing.T) {
	var leaked atomic.Int64
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srvB.Close()
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srvB.URL, http.StatusFound)
	}))
	defer srvA.Close()

	c := testClient(1 << 20)
	_, _, err := c.do(context.Background(), "test", http.MethodGet, srvA.URL, nil,
		map[string]string{"X-API-Key": "credential"}, time.Second)
	if err == nil {
		t.Fatal("3xx must surface as an error")
	}
	var ue *Error
	if !errors.As(err, &ue) || ue.Status != http.StatusFound || ue.Transient {
		t.Errorf("err = %v, want permanent *Error with status 302", err)
	}
	if leaked.Load() != 0 {
		t.Errorf("redirect target was contacted %d times; credentials leaked", leaked.Load())
	}
}

// Numeric job ids above 2^53 must survive verbatim — float64 round-tripping
// would poll a different (rounded) job id on every tick.
func TestExtractJobIDPreservesLargeIntegers(t *testing.T) {
	got, err := ExtractJobID([]byte(`{"job_id":1234567890123456789}`), &config.Poll{IDJSONPath: "job_id"})
	if err != nil || got != "1234567890123456789" {
		t.Fatalf("id = %q (err %v), want the exact digits", got, err)
	}
}

// The job id comes from the target's response and is substituted into URL
// templates; URL metacharacters must be rejected.
func TestExtractJobIDRejectsUnsafeIDs(t *testing.T) {
	for _, body := range []string{
		`{"job_id":"x/../../admin?full=1"}`,
		`{"job_id":"ab#cd"}`,
		`{"job_id":"a b"}`,
		"{\"job_id\":\"j\x01b\"}",
	} {
		if _, err := ExtractJobID([]byte(body), &config.Poll{IDJSONPath: "job_id"}); err == nil {
			t.Errorf("ExtractJobID(%s): want error, got nil", body)
		}
	}
}

// body_field injection must not round payload values through float64.
func TestForwardBodyFieldPreservesNumbers(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	tcfg := config.Target{
		URL: srv.URL, Method: "POST", Timeout: dur(time.Second),
		APIKey: "k", APIKeyInject: config.InjectBodyField, APIKeyField: "api_key",
	}
	payload := []byte(`{"meter_id":1234567890123456789,"scenario":"s1"}`)
	if _, err := testClient(1<<20).ForwardToTarget(context.Background(), "meme", tcfg, payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(received), "1234567890123456789") {
		t.Errorf("large integer corrupted in outbound body: %s", received)
	}
	if !strings.Contains(string(received), `"api_key":"k"`) {
		t.Errorf("api key not injected: %s", received)
	}
}

// Upstream error bodies often echo the request back; configured credentials
// must be redacted before the excerpt is stored, logged, or served.
func TestErrorExcerptRedactsSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"invalid request: api_key tk-super-secret rejected"}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	c := New(1<<20, []string{"tk-super-secret"})
	_, _, err := c.do(context.Background(), "target meme", "POST", srv.URL, []byte(`{}`), nil, time.Second)
	if err == nil {
		t.Fatal("want error")
	}
	msg := err.Error()
	if strings.Contains(msg, "tk-super-secret") {
		t.Errorf("secret leaked into error: %s", msg)
	}
	if !strings.Contains(msg, "[redacted]") {
		t.Errorf("redaction marker missing: %s", msg)
	}
}

// A connection dropped mid-body surfaces as a read error on the stream, for
// the worker to classify as transient on the next poll tick.
func TestFetchResultStreamSurfacesMidBodyFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "short")
		w.(http.Flusher).Flush()    // headers and the partial body reach the client first
		panic(http.ErrAbortHandler) // then the connection drops mid-body
	}))
	defer srv.Close()

	tcfg := pollTargetCfg(srv.URL)
	stream, err := testClient(1<<20).FetchResult(context.Background(), "meme", tcfg, "j1")
	if err != nil {
		t.Fatalf("headers arrived, the stream must open: %v", err)
	}
	defer func() { _ = stream.Close() }()
	if _, err := io.ReadAll(stream.Body); err == nil {
		t.Error("a dropped connection must surface as a read error on the stream")
	}
}

// Result downloads are not bounded by the client's JSON response cap: the
// worker streams them to disk under storage.max_result_bytes instead.
func TestFetchResultStreamIgnoresTheResponseCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(make([]byte, 256))
	}))
	defer srv.Close()

	tcfg := pollTargetCfg(srv.URL)
	stream, err := testClient(64).FetchResult(context.Background(), "meme", tcfg, "j1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	body, err := io.ReadAll(stream.Body)
	if err != nil || len(body) != 256 || stream.ContentType != "application/zip" || stream.Status != http.StatusOK {
		t.Errorf("stream: len=%d ct=%q status=%d err=%v", len(body), stream.ContentType, stream.Status, err)
	}
}

func pollTargetCfg(base string) config.Target {
	return config.Target{
		Timeout: dur(2 * time.Second), APIKeyInject: config.InjectNone,
		Response: config.Response{Mode: config.ModePoll, Poll: &config.Poll{
			IDJSONPath:  "job_id",
			URLTemplate: base + "/jobs/{id}", ResultURLTemplate: base + "/jobs/{id}",
			StatusJSONPath: "status", DoneValues: []string{"done"}, FailedValues: []string{"failed"},
		}},
	}
}

// Target URL templating: {field} placeholders are filled from top-level
// payload fields, which are then stripped from the forwarded body — and a
// target without placeholders or body injection forwards bytes verbatim.
func TestForwardToTargetURLTemplating(t *testing.T) {
	var gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"q_h_nd":112.4}`)
	}))
	defer srv.Close()

	tcfg := config.Target{
		URL: srv.URL + "/api/v1/calculate/{code}", Method: "POST",
		Timeout: dur(time.Second), APIKeyInject: config.InjectNone,
	}
	payload := []byte(`{"code":"DE.N.SFH.01.Gen.ReEx.001.001","A_ref":{"value":120,"unit":"m2"},"meter":1234567890123456789}`)
	if _, err := testClient(1<<20).ForwardToTarget(context.Background(), "ignis-calculate", tcfg, payload); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v1/calculate/DE.N.SFH.01.Gen.ReEx.001.001" {
		t.Errorf("path = %s", gotPath)
	}
	if strings.Contains(string(gotBody), `"code"`) {
		t.Errorf("consumed field not stripped from body: %s", gotBody)
	}
	if !strings.Contains(string(gotBody), "1234567890123456789") {
		t.Errorf("nested bytes must pass through verbatim: %s", gotBody)
	}

	// Missing placeholder field is a permanent authoring error.
	if _, err := testClient(1<<20).ForwardToTarget(context.Background(), "ignis-calculate", tcfg,
		[]byte(`{"A_ref":1}`)); err == nil || IsTransient(err) {
		t.Errorf("missing placeholder field: err = %v, want permanent error", err)
	}
}

func TestForwardWithoutRewritesIsByteExact(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	tcfg := config.Target{URL: srv.URL, Method: "POST", Timeout: dur(time.Second), APIKeyInject: config.InjectNone}
	// Deliberately unusual key order and spacing: only byte-exact
	// forwarding preserves it.
	payload := []byte(`{"zeta": 1,  "alpha": {"b":2,"a":1}}`)
	if _, err := testClient(1<<20).ForwardToTarget(context.Background(), "proxy", tcfg, payload); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBody, payload) {
		t.Errorf("body rewritten: %s", gotBody)
	}
}

func TestExtractPath(t *testing.T) {
	body := []byte(`{"id":"b-1","buem":{"thermal_load_profile":{"timeseries":{"unit":"kW","meter":1234567890123456789,"heating":[19.0,19.1]}}}}`)

	got, err := ExtractPath(body, "buem.thermal_load_profile.timeseries")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"unit":"kW"`, `1234567890123456789`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("extracted %s, want it to contain %s", got, want)
		}
	}

	if got, err := ExtractPath(body, ""); err != nil || !bytes.Equal(got, body) {
		t.Errorf("empty path must return the body unchanged (err %v)", err)
	}
	if _, err := ExtractPath(body, "buem.missing.path"); err == nil {
		t.Error("missing path must error")
	}
	if _, err := ExtractPath(body, "id.deeper"); err == nil {
		t.Error("path through a non-object must error")
	}
}

func decodePayload(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber() // matches how resolver.Parse decodes real payloads
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBuildResolventURL(t *testing.T) {
	t.Run("query mapping with fixed params, arrays and numbers", func(t *testing.T) {
		payload := decodePayload(t, `{"type":"resolvent-weather","provider":"era5-land",
			"lat":48.831,"lon":12.957,"year":2018,"variables":["T","GHI"]}`)
		got, err := buildResolventURL("https://w.example.com/v1/weather/point?format=json", payload, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := "https://w.example.com/v1/weather/point?format=json&lat=48.831&lon=12.957&provider=era5-land&variables=T%2CGHI&year=2018"
		if got != want {
			t.Errorf("url = %s\nwant  %s", got, want)
		}
	})
	t.Run("path templating consumes the field", func(t *testing.T) {
		payload := decodePayload(t, `{"type":"resolvent-ignis","code":"DE.N.SFH.01.Gen.ReEx.001.001"}`)
		got, err := buildResolventURL("https://i.example.com/api/v1/data/{code}", payload, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got != "https://i.example.com/api/v1/data/DE.N.SFH.01.Gen.ReEx.001.001" {
			t.Errorf("url = %s", got)
		}
	})
	t.Run("number fidelity above 2^53", func(t *testing.T) {
		payload := decodePayload(t, `{"type":"resolvent-x","meter":1234567890123456789,"active":true}`)
		got, err := buildResolventURL("https://x.example.com/q", payload, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "meter=1234567890123456789") || !strings.Contains(got, "active=true") {
			t.Errorf("url = %s", got)
		}
	})
	t.Run("missing placeholder field errors", func(t *testing.T) {
		payload := decodePayload(t, `{"type":"resolvent-ignis","country":"DE"}`)
		if _, err := buildResolventURL("https://i.example.com/api/v1/data/{code}", payload, nil); err == nil {
			t.Error("want error for unresolved placeholder")
		}
	})
	t.Run("nested object errors with guidance", func(t *testing.T) {
		payload := decodePayload(t, `{"type":"resolvent-x","location":{"lat":1}}`)
		_, err := buildResolventURL("https://x.example.com/q", payload, nil)
		if err == nil || !strings.Contains(err.Error(), "POST resolvent") {
			t.Errorf("err = %v, want flat-fields guidance", err)
		}
	})
}

func TestResolveResolventGETSendsNoBody(t *testing.T) {
	var gotMethod, gotQuery, gotAuth string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotQuery, gotAuth = r.Method, r.URL.RawQuery, r.Header.Get("X-API-Key")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"index":[],"variables":{}}`)
	}))
	defer srv.Close()

	rcfg := config.Resolvent{URL: srv.URL + "/point?format=json", Method: http.MethodGet,
		APIKey: "wkey", APIKeyHeader: "X-API-Key", Timeout: dur(time.Second)}
	payload := decodePayload(t, `{"type":"resolvent-weather","lat":48.83,"year":2018}`)
	if _, err := testClient(1<<20).ResolveResolvent(context.Background(), "resolvent-weather", rcfg, payload); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodGet || len(gotBody) != 0 {
		t.Errorf("method=%s bodyLen=%d, want bodyless GET", gotMethod, len(gotBody))
	}
	if gotQuery != "format=json&lat=48.83&year=2018" {
		t.Errorf("query = %q", gotQuery)
	}
	if gotAuth != "wkey" {
		t.Errorf("auth = %q", gotAuth)
	}
}

func TestExtractPathArrayIndex(t *testing.T) {
	body := []byte(`[{"object_id":"A","area":30},{"object_id":"B","area":41}]`)
	got, err := ExtractPath(body, "1")
	if err != nil || !strings.Contains(string(got), `"object_id":"B"`) {
		t.Errorf("got %s (err %v)", got, err)
	}
	if got, err := ExtractPath(body, "0.object_id"); err != nil || string(got) != `"A"` {
		t.Errorf("got %s (err %v)", got, err)
	}
	if _, err := ExtractPath(body, "2"); err == nil {
		t.Error("out-of-range index must error")
	}
	if _, err := ExtractPath(body, "first"); err == nil {
		t.Error("non-numeric segment on an array must error")
	}
}

func TestPollTargetStatusMatching(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantDone   bool
		wantFailed bool
	}{
		{"running", `{"status":"running"}`, false, false},
		{"done", `{"status":"done"}`, true, false},
		{"completed alias", `{"status":"completed"}`, true, false},
		{"failed", `{"status":"failed"}`, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			tcfg := config.Target{
				Timeout: dur(time.Second), APIKeyInject: config.InjectNone,
				Response: config.Response{Mode: config.ModePoll, Poll: &config.Poll{
					URLTemplate: srv.URL + "/{id}", StatusJSONPath: "status",
					DoneValues: []string{"done", "completed"}, FailedValues: []string{"failed"},
				}},
			}
			st, err := testClient(1<<20).PollTarget(context.Background(), "meme", tcfg, "j1")
			if err != nil {
				t.Fatal(err)
			}
			if st.Done != tt.wantDone || st.Failed != tt.wantFailed {
				t.Errorf("done=%v failed=%v, want %v/%v", st.Done, st.Failed, tt.wantDone, tt.wantFailed)
			}
		})
	}
}

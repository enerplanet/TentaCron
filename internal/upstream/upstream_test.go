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

// A failure while downloading the result body is retryable on the next poll
// tick; only the size cap is final.
func TestFetchResultReadFailureIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "short")
		panic(http.ErrAbortHandler) // drop the connection mid-body
	}))
	defer srv.Close()

	tcfg := pollTargetCfg(srv.URL)
	_, _, err := testClient(1<<20).FetchResult(context.Background(), "meme", tcfg, "j1")
	if err == nil || !IsTransient(err) {
		t.Errorf("mid-body read failure must be transient, got %v", err)
	}
}

func TestFetchResultOversizedIsPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 256))
	}))
	defer srv.Close()

	tcfg := pollTargetCfg(srv.URL)
	_, _, err := testClient(64).FetchResult(context.Background(), "meme", tcfg, "j1")
	if err == nil || IsTransient(err) {
		t.Errorf("oversized result must fail permanently, got %v", err)
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

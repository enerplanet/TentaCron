package upstream

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/enerplanet/tentacron/internal/config"
)

func testClient(maxBody int64) *Client {
	return New(maxBody, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

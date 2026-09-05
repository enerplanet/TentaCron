package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/notify"
	"github.com/enerplanet/tentacron/internal/plan"
	"github.com/enerplanet/tentacron/internal/store"
	"github.com/enerplanet/tentacron/internal/upstream"
)

var authHdr = map[string]string{"X-API-Key": "valid-key"}

// newEnvWith builds an env with a mutated config and an optional logger.
func newEnvWith(t *testing.T, mutate func(*config.Config), logger *slog.Logger) *testEnv {
	t.Helper()
	cfg := &config.Config{
		Server:  config.Server{MaxBodyBytes: 1 << 20},
		Auth:    config.Auth{APIKeys: []config.APIKey{{Name: "test", Key: "valid-key"}}},
		Worker:  config.Worker{MaxAttempts: 5},
		Targets: map[string]config.Target{"meme": {URL: "https://meme.example.com/simulate"}},
	}
	if mutate != nil {
		mutate(cfg)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	nudge := make(chan struct{}, 1)
	return &testEnv{server: New(config.Static(cfg), st, logger, nudge), store: st, nudge: nudge}
}

func createJobDirect(t *testing.T, e *testEnv) string {
	t.Helper()
	id, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.store.CreateJob(context.Background(), &store.Job{
		ID: id, Client: "test", Target: "meme", MaxAttempts: 1, Payload: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestContentTypeVariants(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		ct   string
		want int
	}{
		{"application/json; charset=utf-8", http.StatusAccepted},
		{"APPLICATION/JSON", http.StatusAccepted},
		{"text/json", http.StatusUnsupportedMediaType},
		{"application/vnd.api+json", http.StatusUnsupportedMediaType},
		{"*/*", http.StatusUnsupportedMediaType},
	}
	for _, tt := range cases {
		rec := e.do(t, "POST", "/v1/requests", validBody, map[string]string{"Content-Type": tt.ct})
		if rec.Code != tt.want {
			t.Errorf("Content-Type %q: status %d, want %d", tt.ct, rec.Code, tt.want)
		}
	}
}

func TestPayloadShapes(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name, payload string
		wantCode      int
		wantErr       string
	}{
		{"null", `null`, http.StatusBadRequest, CodeMissingField},
		{"string", `"str"`, http.StatusBadRequest, CodeInvalidJSON},
		{"number", `5`, http.StatusBadRequest, CodeInvalidJSON},
		{"bool", `true`, http.StatusBadRequest, CodeInvalidJSON},
		{"empty object", `{}`, http.StatusAccepted, ""},
		{"object after whitespace", `  {"a":1}`, http.StatusAccepted, ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			rec := e.do(t, "POST", "/v1/requests", `{"api_key":"valid-key","target":"meme","payload":`+tt.payload+`}`, nil)
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantErr != "" {
				if got := errCode(t, rec); got != tt.wantErr {
					t.Errorf("error code = %q, want %q", got, tt.wantErr)
				}
			}
		})
	}
}

func TestUnknownTopLevelFieldsAreIgnored(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, "POST", "/v1/requests", `{"api_key":"valid-key","target":"meme","payload":{},"note":"high","tags":["x"]}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202 (unknown fields are ignored)", rec.Code)
	}
}

func TestWrongFieldTypesAreInvalidJSON(t *testing.T) {
	e := newEnv(t)
	for _, body := range []string{
		`{"api_key":5,"target":"meme","payload":{}}`,
		`{"api_key":"valid-key","target":["meme"],"payload":{}}`,
	} {
		rec := e.do(t, "POST", "/v1/requests", body, nil)
		if rec.Code != http.StatusBadRequest || errCode(t, rec) != CodeInvalidJSON {
			t.Errorf("%s: status %d code %s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestEmptyAndWhitespaceBody(t *testing.T) {
	e := newEnv(t)
	for _, body := range []string{"", "   \n"} {
		rec := e.do(t, "POST", "/v1/requests", body, nil)
		if rec.Code != http.StatusBadRequest || errCode(t, rec) != CodeInvalidJSON {
			t.Errorf("body %q: status %d %s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestBodySizeBoundary(t *testing.T) {
	limit := int64(len(validBody))
	exact := newEnvWith(t, func(c *config.Config) { c.Server.MaxBodyBytes = limit }, nil)
	if rec := exact.do(t, "POST", "/v1/requests", validBody, nil); rec.Code != http.StatusAccepted {
		t.Errorf("body exactly at the limit: status %d, want 202", rec.Code)
	}
	under := newEnvWith(t, func(c *config.Config) { c.Server.MaxBodyBytes = limit - 1 }, nil)
	rec := under.do(t, "POST", "/v1/requests", validBody, nil)
	if rec.Code != http.StatusRequestEntityTooLarge || errCode(t, rec) != CodePayloadTooLarge {
		t.Errorf("body one byte over the limit: status %d %s", rec.Code, rec.Body.String())
	}
}

func TestListParameterValidation(t *testing.T) {
	e := newEnv(t)
	for _, q := range []string{"limit=0", "limit=-5", "limit=abc", "limit=1.5", "limit=%20"} {
		rec := e.do(t, "GET", "/v1/requests?"+q, "", authHdr)
		if rec.Code != http.StatusBadRequest || errCode(t, rec) != CodeInvalidParameter {
			t.Errorf("%s: status %d %s", q, rec.Code, rec.Body.String())
		}
	}
	for _, s := range []string{store.StateReceived, store.StateResolving, store.StateForwarding,
		store.StateAwaitingTarget, store.StateCompleted, store.StateFailed} {
		if rec := e.do(t, "GET", "/v1/requests?state="+s, "", authHdr); rec.Code != http.StatusOK {
			t.Errorf("state=%s: status %d", s, rec.Code)
		}
	}
	if rec := e.do(t, "GET", "/v1/requests", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("list without key: status %d, want 401", rec.Code)
	}
}

func TestListLimitCapAndDefault(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 205; i++ {
		createJobDirect(t, e)
	}
	count := func(q string) int {
		t.Helper()
		rec := e.do(t, "GET", "/v1/requests"+q, "", authHdr)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", q, rec.Code)
		}
		return len(decodeBody[listResponse](t, rec).Items)
	}
	if n := count("?limit=1000"); n != 200 {
		t.Errorf("limit=1000 returned %d, want the 200 cap", n)
	}
	if n := count(""); n != 50 {
		t.Errorf("default limit returned %d, want 50", n)
	}
	if n := count("?limit="); n != 50 {
		t.Errorf("empty limit returned %d, want the default 50", n)
	}
}

func TestListOrderNewestFirstAndEmptyBody(t *testing.T) {
	e := newEnv(t)
	if rec := e.do(t, "GET", "/v1/requests", "", authHdr); rec.Body.String() != "{\"items\":[]}\n" {
		t.Errorf("empty list body = %q, want an empty array, never null", rec.Body.String())
	}
	first := decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests", validBody, nil))
	time.Sleep(2 * time.Millisecond)
	second := decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests", validBody, nil))
	items := decodeBody[listResponse](t, e.do(t, "GET", "/v1/requests", "", authHdr)).Items
	if len(items) != 2 || items[0].ID != second.ID || items[1].ID != first.ID {
		t.Errorf("list order = %v, want newest first", items)
	}
}

// POST takes the key from the X-API-Key header when present — the preferred
// form — and falls back to the deprecated body field; a header wins over a
// body field.
func TestPostAcceptsHeaderKeyAndFallsBackToBody(t *testing.T) {
	e := newEnv(t)
	if rec := e.do(t, "POST", "/v1/requests", `{"target":"meme","payload":{}}`, authHdr); rec.Code != http.StatusAccepted {
		t.Errorf("header only: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, "POST", "/v1/requests", `{"api_key":"nope","target":"meme","payload":{}}`, authHdr); rec.Code != http.StatusAccepted {
		t.Errorf("valid header must win over a wrong body key: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, "POST", "/v1/requests", validBody, nil); rec.Code != http.StatusAccepted {
		t.Errorf("body key alone (deprecated) must still work: %d", rec.Code)
	}
	rec := e.do(t, "POST", "/v1/requests", `{"target":"meme","payload":{}}`, nil)
	if rec.Code != http.StatusBadRequest || errCode(t, rec) != CodeMissingField || !strings.Contains(rec.Body.String(), "X-API-Key") {
		t.Errorf("no key at all: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, "POST", "/v1/requests", `{"target":"meme","payload":{}}`, map[string]string{"X-API-Key": "nope"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong header key: %d, want 401", rec.Code)
	}
}

// Reads are scoped to the submitting client: another client's id answers
// 404 exactly like an unknown one and its jobs never appear in a list; an
// admin key sees everything.
func TestReadsAreScopedToTheClientUnlessAdmin(t *testing.T) {
	e := newEnvWith(t, func(c *config.Config) {
		c.Auth.APIKeys = []config.APIKey{
			{Name: "a", Key: "key-a", Role: config.RoleClient},
			{Name: "b", Key: "key-b", Role: config.RoleClient},
			{Name: "ops", Key: "key-ops", Role: config.RoleAdmin},
		}
	}, nil)
	created := decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests", `{"target":"meme","payload":{}}`, map[string]string{"X-API-Key": "key-a"}))
	if err := e.store.MarkCompleted(context.Background(), created.ID, 200, []byte(`{"ok":true}`), "", "", "done"); err != nil {
		t.Fatal(err)
	}
	hdr := func(k string) map[string]string { return map[string]string{"X-API-Key": k} }
	for _, path := range []string{"/v1/requests/" + created.ID, "/v1/requests/" + created.ID + "/result"} {
		if rec := e.do(t, "GET", path, "", hdr("key-a")); rec.Code != http.StatusOK {
			t.Errorf("owner %s: %d", path, rec.Code)
		}
		if rec := e.do(t, "GET", path, "", hdr("key-b")); rec.Code != http.StatusNotFound || errCode(t, rec) != CodeNotFound {
			t.Errorf("other client %s: %d %s, want the same 404 as an unknown id", path, rec.Code, rec.Body.String())
		}
		if rec := e.do(t, "GET", path, "", hdr("key-ops")); rec.Code != http.StatusOK {
			t.Errorf("admin %s: %d", path, rec.Code)
		}
	}
	items := func(k string) int {
		return len(decodeBody[listResponse](t, e.do(t, "GET", "/v1/requests", "", hdr(k))).Items)
	}
	if items("key-a") != 1 || items("key-b") != 0 || items("key-ops") != 1 {
		t.Errorf("list sizes a=%d b=%d ops=%d, want 1/0/1", items("key-a"), items("key-b"), items("key-ops"))
	}
}

func TestHeaderLengthCaps(t *testing.T) {
	e := newEnv(t)
	long := strings.Repeat("k", maxIdempotencyKeyLen+1)
	rec := e.do(t, "POST", "/v1/requests", validBody, map[string]string{"Idempotency-Key": long})
	if rec.Code != http.StatusBadRequest || errCode(t, rec) != CodeInvalidParameter {
		t.Errorf("oversized Idempotency-Key: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, "POST", "/v1/requests", validBody, map[string]string{"Idempotency-Key": strings.Repeat("k", maxIdempotencyKeyLen)}); rec.Code != http.StatusAccepted {
		t.Errorf("Idempotency-Key at the cap: %d", rec.Code)
	}
	rec = e.do(t, "GET", "/healthz", "", map[string]string{"X-Request-ID": strings.Repeat("r", maxRequestIDLen+1)})
	if got := rec.Header().Get("X-Request-ID"); !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(got) {
		t.Errorf("oversized request id must be replaced by a generated one, got %q", got)
	}
}

func TestRequestLogCarriesTheClientName(t *testing.T) {
	var buf bytes.Buffer
	e := newEnvWith(t, nil, slog.New(slog.NewJSONHandler(&buf, nil)))
	e.do(t, "POST", "/v1/requests", `{"target":"meme","payload":{}}`, authHdr)
	e.do(t, "GET", "/v1/requests", "", authHdr)
	e.do(t, "GET", "/healthz", "", nil)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var requestLines []string
	for _, l := range lines {
		if strings.Contains(l, `"msg":"request"`) {
			requestLines = append(requestLines, l)
		}
	}
	if len(requestLines) != 3 {
		t.Fatalf("want 3 request log lines, got %d:\n%s", len(requestLines), buf.String())
	}
	if !strings.Contains(requestLines[0], `"client":"test"`) || !strings.Contains(requestLines[1], `"client":"test"`) {
		t.Errorf("authenticated requests must log the client name:\n%s", buf.String())
	}
	if !strings.Contains(requestLines[2], `"client":""`) {
		t.Errorf("unauthenticated request must log an empty client:\n%s", requestLines[2])
	}
}

func TestKeyComparisonIsExact(t *testing.T) {
	e := newEnv(t)
	for _, k := range []string{" valid-key", "valid-key ", "VALID-KEY", "valid-ke", ""} {
		rec := e.do(t, "GET", "/v1/requests", "", map[string]string{"X-API-Key": k})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("key %q: status %d, want 401", k, rec.Code)
		}
	}
}

func TestClientNameIsRecorded(t *testing.T) {
	e := newEnvWith(t, func(c *config.Config) {
		c.Auth.APIKeys = append(c.Auth.APIKeys, config.APIKey{Name: "second", Key: "second-key"})
	}, nil)
	for key, want := range map[string]string{"valid-key": "test", "second-key": "second"} {
		created := decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests",
			`{"api_key":"`+key+`","target":"meme","payload":{}}`, nil))
		job, err := e.store.GetJob(context.Background(), created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if job.Client != want {
			t.Errorf("key %s recorded client %q, want %q", key, job.Client, want)
		}
	}
}

func TestRequestIDHeader(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, "GET", "/healthz", "", map[string]string{"X-Request-ID": "req-abc"})
	if got := rec.Header().Get("X-Request-ID"); got != "req-abc" {
		t.Errorf("supplied request id not echoed: %q", got)
	}
	rec = e.do(t, "GET", "/v2/nope", "", nil)
	if got := rec.Header().Get("X-Request-ID"); !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(got) {
		t.Errorf("generated request id = %q, want 16 hex chars (also on error responses)", got)
	}
}

func TestRecoveryMiddlewareTurnsPanicsIntoJSON500(t *testing.T) {
	e := newEnv(t)
	h := e.server.withRecovery(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusInternalServerError || errCode(t, rec) != CodeInternal {
		t.Errorf("panic response: %d %s", rec.Code, rec.Body.String())
	}
}

func TestReadyzReportsClosedDatabase(t *testing.T) {
	e := newEnv(t)
	_ = e.store.Close()
	rec := e.do(t, "GET", "/readyz", "", nil)
	if rec.Code != http.StatusServiceUnavailable || errCode(t, rec) != CodeInternal {
		t.Errorf("readyz with closed db: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, "GET", "/healthz", "", nil); rec.Code != http.StatusOK {
		t.Errorf("healthz must stay alive: %d", rec.Code)
	}
}

// Every store failure surfaces as the opaque 500 envelope, never a panic or
// a leaked driver message.
func TestStoreFailuresAreInternalErrors(t *testing.T) {
	e := newEnv(t)
	id := createJobDirect(t, e)
	_ = e.store.Close()
	for _, tt := range []struct{ method, path, body string }{
		{"POST", "/v1/requests", validBody},
		{"GET", "/v1/requests/" + id, ""},
		{"GET", "/v1/requests", ""},
		{"GET", "/v1/requests/" + id + "/result", ""},
	} {
		rec := e.do(t, tt.method, tt.path, tt.body, authHdr)
		if rec.Code != http.StatusInternalServerError || errCode(t, rec) != CodeInternal {
			t.Errorf("%s %s with closed db: %d %s", tt.method, tt.path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "sql") {
			t.Errorf("%s %s leaked a driver message: %s", tt.method, tt.path, rec.Body.String())
		}
	}
}

func TestResultEndpointEdgeCases(t *testing.T) {
	ctx := context.Background()
	t.Run("completed without a body", func(t *testing.T) {
		e := newEnv(t)
		id := createJobDirect(t, e)
		if err := e.store.MarkCompleted(ctx, id, 200, nil, "", "", "done"); err != nil {
			t.Fatal(err)
		}
		job := decodeBody[jobResponse](t, e.do(t, "GET", "/v1/requests/"+id, "", authHdr))
		if job.Result == nil || job.Result.TargetStatus != 200 || job.Result.Href != "" || len(job.Result.TargetResponse) != 0 {
			t.Errorf("result = %+v", job.Result)
		}
		rec := e.do(t, "GET", "/v1/requests/"+id+"/result", "", authHdr)
		if rec.Code != http.StatusNotFound || errCode(t, rec) != CodeNotFound {
			t.Errorf("result endpoint: %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("file without recorded content type", func(t *testing.T) {
		e := newEnv(t)
		id := createJobDirect(t, e)
		path := filepath.Join(t.TempDir(), "r.bin")
		if err := os.WriteFile(path, []byte("raw"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := e.store.MarkCompleted(ctx, id, 200, nil, path, "", "done"); err != nil {
			t.Fatal(err)
		}
		rec := e.do(t, "GET", "/v1/requests/"+id+"/result", "", authHdr)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/octet-stream" {
			t.Errorf("status %d content-type %q", rec.Code, rec.Header().Get("Content-Type"))
		}
	})
	t.Run("unreadable file is a 500", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores file permissions")
		}
		e := newEnv(t)
		id := createJobDirect(t, e)
		path := filepath.Join(t.TempDir(), "locked.zip")
		if err := os.WriteFile(path, []byte("zip"), 0o000); err != nil {
			t.Fatal(err)
		}
		if err := e.store.MarkCompleted(ctx, id, 200, nil, path, "application/zip", "done"); err != nil {
			t.Fatal(err)
		}
		rec := e.do(t, "GET", "/v1/requests/"+id+"/result", "", authHdr)
		if rec.Code != http.StatusInternalServerError || errCode(t, rec) != CodeInternal {
			t.Errorf("unreadable result: %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("auth and unknown id", func(t *testing.T) {
		e := newEnv(t)
		id := createJobDirect(t, e)
		if rec := e.do(t, "GET", "/v1/requests/"+id+"/result", "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("no key: %d", rec.Code)
		}
		if rec := e.do(t, "GET", "/v1/requests/0123456789abcdef0123456789abcdef/result", "", authHdr); rec.Code != http.StatusNotFound {
			t.Errorf("unknown id: %d", rec.Code)
		}
	})
}

// An inline result that is not JSON (only reachable through the store, the
// worker files such bodies) must not break the envelope.
func TestNonJSONInlineResultIsQuoted(t *testing.T) {
	e := newEnv(t)
	id := createJobDirect(t, e)
	if err := e.store.MarkCompleted(context.Background(), id, 200, []byte("not json"), "", "", "done"); err != nil {
		t.Fatal(err)
	}
	rec := e.do(t, "GET", "/v1/requests/"+id, "", authHdr)
	if !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("envelope is not valid JSON: %s", rec.Body.String())
	}
	if job := decodeBody[jobResponse](t, rec); string(job.Result.TargetResponse) != `"not json"` {
		t.Errorf("target_response = %s, want the body as a JSON string", job.Result.TargetResponse)
	}
	if rec := e.do(t, "GET", "/v1/requests/"+id+"/result", "", authHdr); rec.Code != http.StatusNotFound {
		t.Errorf("result endpoint for non-JSON inline body: %d, want 404", rec.Code)
	}
}

func TestJobResponseFieldPresence(t *testing.T) {
	e := newEnv(t)
	created := decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests", validBody, nil))
	doc := decodeBody[map[string]json.RawMessage](t, e.do(t, "GET", "/v1/requests/"+created.ID, "", authHdr))
	for _, k := range []string{"id", "target", "state", "attempts", "created_at", "updated_at", "result", "error"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("field %q missing from the job envelope", k)
		}
	}
	for _, k := range []string{"target_job_id", "completed_at"} {
		if _, ok := doc[k]; ok {
			t.Errorf("field %q must be omitted while unset", k)
		}
	}
	if string(doc["result"]) != "null" || string(doc["error"]) != "null" {
		t.Errorf("result/error must be explicit null while pending: %s / %s", doc["result"], doc["error"])
	}
	if !strings.HasSuffix(string(doc["created_at"]), `Z"`) {
		t.Errorf("timestamps must be UTC RFC3339: %s", doc["created_at"])
	}
}

// A wrong method on a known path is a 405 with an Allow header, an unknown
// path a 404 — both in the JSON envelope; the mux's plain text never
// surfaces.
func TestWrongMethodIs405WithAllowInJSONEnvelope(t *testing.T) {
	e := newEnv(t)
	for _, tt := range []struct{ method, path, allow string }{
		{"POST", "/v1/requests/abc", "GET"}, {"PUT", "/v1/requests", "GET"}, {"DELETE", "/healthz", "GET"},
	} {
		rec := e.do(t, tt.method, tt.path, "", nil)
		if rec.Code != http.StatusMethodNotAllowed || errCode(t, rec) != CodeMethodNotAllowed {
			t.Errorf("%s %s: %d %s", tt.method, tt.path, rec.Code, rec.Body.String())
		}
		if allow := rec.Header().Get("Allow"); !strings.Contains(allow, tt.allow) {
			t.Errorf("%s %s: Allow %q must list %s", tt.method, tt.path, allow, tt.allow)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s %s: content-type %q", tt.method, tt.path, ct)
		}
	}
	for _, tt := range []struct{ method, path string }{{"GET", "/v2/other"}, {"DELETE", "/nope"}, {"POST", "/v1"}} {
		rec := e.do(t, tt.method, tt.path, "", nil)
		if rec.Code != http.StatusNotFound || errCode(t, rec) != CodeNotFound || rec.Header().Get("Allow") != "" {
			t.Errorf("%s %s: %d %s allow=%q", tt.method, tt.path, rec.Code, rec.Body.String(), rec.Header().Get("Allow"))
		}
	}
}

func TestConcurrentCreatesAreIsolated(t *testing.T) {
	e := newEnv(t)
	const n = 50
	codes := make([]int, n)
	bodies := make([][]byte, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/requests", bytes.NewBufferString(validBody))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			e.server.Handler().ServeHTTP(rec, req)
			codes[i], bodies[i] = rec.Code, rec.Body.Bytes()
		}(i)
	}
	wg.Wait()
	ids := map[string]bool{}
	for i := range codes {
		if codes[i] != http.StatusAccepted {
			t.Fatalf("request %d: status %d %s", i, codes[i], bodies[i])
		}
		var resp createResponse
		if err := json.Unmarshal(bodies[i], &resp); err != nil {
			t.Fatal(err)
		}
		ids[resp.ID] = true
	}
	if len(ids) != n {
		t.Errorf("%d distinct ids for %d requests", len(ids), n)
	}
	jobs, err := e.store.ListJobs(context.Background(), store.ListFilter{Limit: 100})
	if err != nil || len(jobs) != n {
		t.Errorf("stored %d jobs (err %v), want %d", len(jobs), err, n)
	}
}

func TestRequestLogLineCarriesRequestID(t *testing.T) {
	var buf bytes.Buffer
	e := newEnvWith(t, nil, slog.New(slog.NewJSONHandler(&buf, nil)))
	e.do(t, "GET", "/healthz", "", map[string]string{"X-Request-ID": "req-log-1"})
	line := buf.String()
	for _, want := range []string{`"request_id":"req-log-1"`, `"path":"/healthz"`, `"status":200`, `"method":"GET"`} {
		if !strings.Contains(line, want) {
			t.Errorf("log line lacks %s: %s", want, line)
		}
	}
}

func TestPayloadStoredByteExact(t *testing.T) {
	e := newEnv(t)
	payload := `{ "a" : 1 , "b":[1, 2], "n": 12345678901234567890}`
	created := decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests",
		`{"api_key":"valid-key","target":"meme","payload":`+payload+`}`, nil))
	job, err := e.store.GetJob(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(job.Payload) != payload {
		t.Errorf("stored payload = %s, want the request bytes verbatim", job.Payload)
	}
}

// The attempt ceiling stored on a job comes from the target when it sets
// max_attempts, otherwise from the worker default.
func TestAcceptUsesTargetMaxAttempts(t *testing.T) {
	e := newEnvWith(t, func(c *config.Config) {
		c.Targets["meme"] = config.Target{URL: "https://meme.example.com/simulate", MaxAttempts: 2}
		c.Targets["other"] = config.Target{URL: "https://other.example.com/run"}
	}, nil)
	for target, want := range map[string]int{"meme": 2, "other": 5} {
		created := decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests",
			`{"api_key":"valid-key","target":"`+target+`","payload":{}}`, nil))
		job, err := e.store.GetJob(context.Background(), created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if job.MaxAttempts != want {
			t.Errorf("target %s stored max_attempts %d, want %d", target, job.MaxAttempts, want)
		}
	}
}

func TestVersionAndHealthzReportTheBuild(t *testing.T) {
	e := newEnv(t)
	e.server.Build = BuildInfo{Version: "v1.2.3", Go: "go1.26.8", Revision: "abc123"}
	rec := e.do(t, "GET", "/version", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("/version: %d %s", rec.Code, rec.Body.String())
	}
	doc := decodeBody[map[string]string](t, rec)
	if doc["version"] != "v1.2.3" || doc["go"] != "go1.26.8" || doc["revision"] != "abc123" {
		t.Errorf("/version = %v", doc)
	}
	if _, present := doc["built"]; present {
		t.Errorf("empty build fields must be omitted: %v", doc)
	}
	health := decodeBody[map[string]string](t, e.do(t, "GET", "/healthz", "", nil))
	if health["status"] != "ok" || health["version"] != "v1.2.3" {
		t.Errorf("/healthz = %v", health)
	}
}

// The audit trail is readable through the API, oldest first, under the same
// scoping as the job.
func TestEventsEndpoint(t *testing.T) {
	e := newEnvWith(t, func(c *config.Config) {
		c.Auth.APIKeys = append(c.Auth.APIKeys, config.APIKey{Name: "other", Key: "other-key", Role: config.RoleClient})
	}, nil)
	created := decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests", validBody, nil))
	ctx := context.Background()
	if _, err := e.store.ClaimNext(ctx, store.ClaimPolicy{PollInterval: func(string) time.Duration { return time.Minute }}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.MarkFailed(ctx, created.ID, "target_error", "boom"); err != nil {
		t.Fatal(err)
	}
	rec := e.do(t, "GET", "/v1/requests/"+created.ID+"/events", "", authHdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("events: %d %s", rec.Code, rec.Body.String())
	}
	items := decodeBody[map[string][]eventResponse](t, rec)["items"]
	if len(items) != 3 || items[0].FromState != "" || items[0].ToState != "received" ||
		items[1].ToState != "resolving" || items[2].ToState != "failed" || !strings.Contains(items[2].Detail, "boom") {
		t.Errorf("events = %+v", items)
	}
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`).MatchString(items[0].CreatedAt) {
		t.Errorf("created_at must be UTC with millisecond precision, got %q", items[0].CreatedAt)
	}
	if rec := e.do(t, "GET", "/v1/requests/"+created.ID+"/events", "", map[string]string{"X-API-Key": "other-key"}); rec.Code != http.StatusNotFound {
		t.Errorf("another client's events: %d, want 404", rec.Code)
	}
	if rec := e.do(t, "GET", "/v1/requests/"+created.ID+"/events", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated: %d, want 401", rec.Code)
	}
	if rec := e.do(t, "GET", "/v1/requests/0123456789abcdef0123456789abcdef/events", "", authHdr); rec.Code != http.StatusNotFound {
		t.Errorf("unknown id: %d, want 404", rec.Code)
	}
}

// File results are served with a length, a download filename, range and
// HEAD support so a client can resume or size a large bundle; inline JSON
// results carry their length too.
func TestResultDownloadHeadersRangesAndHead(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := createJobDirect(t, e)
	path := filepath.Join(t.TempDir(), id+".zip")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.store.MarkCompleted(ctx, id, 200, nil, path, "application/zip", "done"); err != nil {
		t.Fatal(err)
	}
	rec := e.do(t, "GET", "/v1/requests/"+id+"/result", "", authHdr)
	if rec.Code != http.StatusOK || rec.Body.String() != "0123456789" {
		t.Fatalf("full download: %d %q", rec.Code, rec.Body.String())
	}
	h := rec.Header()
	if h.Get("Content-Length") != "10" || h.Get("Accept-Ranges") != "bytes" || h.Get("Content-Type") != "application/zip" ||
		h.Get("Content-Disposition") != `attachment; filename=`+id+`.zip` {
		t.Errorf("headers = %v", h)
	}
	rec = e.do(t, "GET", "/v1/requests/"+id+"/result", "", map[string]string{"X-API-Key": "valid-key", "Range": "bytes=2-5"})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "2345" || rec.Header().Get("Content-Range") != "bytes 2-5/10" {
		t.Errorf("range: %d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Range"))
	}
	rec = e.do(t, "HEAD", "/v1/requests/"+id+"/result", "", authHdr)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Length") != "10" || rec.Body.Len() != 0 {
		t.Errorf("HEAD: %d len=%s body=%d", rec.Code, rec.Header().Get("Content-Length"), rec.Body.Len())
	}

	inline := createJobDirect(t, e)
	if err := e.store.MarkCompleted(ctx, inline, 200, []byte(`{"ok":true}`), "", "", "done"); err != nil {
		t.Fatal(err)
	}
	rec = e.do(t, "GET", "/v1/requests/"+inline+"/result", "", authHdr)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Length") != "11" || rec.Body.String() != `{"ok":true}` {
		t.Errorf("inline: %d len=%s %q", rec.Code, rec.Header().Get("Content-Length"), rec.Body.String())
	}
}

// Filters narrow the list and pages chain through an opaque cursor that
// never skips or repeats an item, even for same-millisecond neighbours.
func TestListFiltersAndCursorPagination(t *testing.T) {
	e := newEnvWith(t, func(c *config.Config) {
		c.Auth.APIKeys = append(c.Auth.APIKeys, config.APIKey{Name: "ops", Key: "ops-key", Role: config.RoleAdmin})
		c.Targets["other"] = config.Target{URL: "https://other.example.com/run"}
	}, nil)
	var ids []string
	for i := 0; i < 7; i++ {
		target := "meme"
		if i%3 == 0 {
			target = "other"
		}
		created := decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests", `{"target":"`+target+`","payload":{}}`, authHdr))
		ids = append(ids, created.ID)
	}
	list := func(q string, hdr map[string]string) listResponse {
		rec := e.do(t, "GET", "/v1/requests"+q, "", hdr)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
		return decodeBody[listResponse](t, rec)
	}
	if got := list("?target=other", authHdr); len(got.Items) != 3 || got.NextCursor != "" {
		t.Errorf("target filter: %d items, cursor %q", len(got.Items), got.NextCursor)
	}
	var walked []string
	page := list("?limit=3", authHdr)
	for {
		for _, it := range page.Items {
			walked = append(walked, it.ID)
		}
		if page.NextCursor == "" {
			break
		}
		page = list("?limit=3&cursor="+page.NextCursor, authHdr)
	}
	want := make([]string, 0, len(ids))
	for i := len(ids) - 1; i >= 0; i-- {
		want = append(want, ids[i])
	}
	if !reflect.DeepEqual(walked, want) {
		t.Errorf("paged walk = %v\nwant %v", walked, want)
	}
	if full := list("?limit=7", authHdr); full.NextCursor != "" {
		t.Errorf("an exactly full page must not announce a next page: %q", full.NextCursor)
	}
	since := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if got := list("?since="+since, authHdr); len(got.Items) != 0 {
		t.Errorf("since in the future must list nothing, got %d", len(got.Items))
	}
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if got := list("?until="+until, authHdr); len(got.Items) != 7 {
		t.Errorf("until in the future must list all, got %d", len(got.Items))
	}
	if got := list("?client=test", map[string]string{"X-API-Key": "ops-key"}); len(got.Items) != 7 {
		t.Errorf("admin client filter: %d items, want 7", len(got.Items))
	}
	for _, q := range []string{"?since=yesterday", "?until=2026-13-01T00:00:00Z", "?cursor=@@@", "?cursor=bm90LWEtY3Vyc29y", "?cursor=MTc4ODYxOTEzNTY4MS5hYmM", "?client=someone-else"} {
		rec := e.do(t, "GET", "/v1/requests"+q, "", authHdr)
		if rec.Code != http.StatusBadRequest || errCode(t, rec) != CodeInvalidParameter {
			t.Errorf("%s: %d %s, want 400 invalid_parameter", q, rec.Code, rec.Body.String())
		}
	}
	if got := list("?client=test", authHdr); len(got.Items) != 7 {
		t.Errorf("a client may name itself in the client filter, got %d", len(got.Items))
	}
}

// priority is bounded to -10..10 and capped per key; it is echoed on the job
// when set and omitted at the default.
func TestPriorityValidationAndKeyCap(t *testing.T) {
	zero := 0
	e := newEnvWith(t, func(c *config.Config) {
		c.Auth.APIKeys = append(c.Auth.APIKeys, config.APIKey{Name: "capped", Key: "capped-key", Role: config.RoleClient, MaxPriority: &zero})
	}, nil)
	post := func(body, key string) *httptest.ResponseRecorder {
		return e.do(t, "POST", "/v1/requests", body, map[string]string{"X-API-Key": key})
	}
	for _, p := range []string{"11", "-11", "1.5"} {
		rec := post(`{"target":"meme","payload":{},"priority":`+p+`}`, "valid-key")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("priority %s: %d %s, want 400", p, rec.Code, rec.Body.String())
		}
	}
	created := decodeBody[createResponse](t, post(`{"target":"meme","payload":{},"priority":7}`, "valid-key"))
	doc := decodeBody[map[string]json.RawMessage](t, e.do(t, "GET", "/v1/requests/"+created.ID, "", authHdr))
	if string(doc["priority"]) != "7" {
		t.Errorf("priority not echoed: %s", doc["priority"])
	}
	plain := decodeBody[createResponse](t, post(`{"target":"meme","payload":{}}`, "valid-key"))
	doc = decodeBody[map[string]json.RawMessage](t, e.do(t, "GET", "/v1/requests/"+plain.ID, "", authHdr))
	if _, present := doc["priority"]; present {
		t.Errorf("default priority must be omitted: %s", doc["priority"])
	}
	rec := post(`{"target":"meme","payload":{},"priority":1}`, "capped-key")
	if rec.Code != http.StatusBadRequest || errCode(t, rec) != CodeInvalidParameter || !strings.Contains(rec.Body.String(), "maximum of 0") {
		t.Errorf("capped key above its maximum: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(`{"target":"meme","payload":{},"priority":-5}`, "capped-key"); rec.Code != http.StatusAccepted {
		t.Errorf("capped key below its maximum: %d %s", rec.Code, rec.Body.String())
	}
}

// Configured origins get CORS headers and preflight answers; any other
// origin gets nothing, and with no origins configured nothing changes.
func TestCORSForConfiguredOriginsOnly(t *testing.T) {
	e := newEnvWith(t, func(c *config.Config) {
		c.Server.CORS.AllowedOrigins = []string{"https://app.example.org"}
	}, nil)
	rec := e.do(t, "GET", "/v1/requests", "", map[string]string{"X-API-Key": "valid-key", "Origin": "https://app.example.org"})
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example.org" || rec.Header().Get("Vary") != "Origin" {
		t.Errorf("allowed origin: %d %v", rec.Code, rec.Header())
	}
	rec = e.do(t, "OPTIONS", "/v1/requests", "", map[string]string{"Origin": "https://app.example.org", "Access-Control-Request-Method": "POST"})
	if rec.Code != http.StatusNoContent || !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "X-API-Key") ||
		!strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "POST") || rec.Header().Get("Access-Control-Max-Age") != "600" {
		t.Errorf("preflight: %d %v", rec.Code, rec.Header())
	}
	rec = e.do(t, "GET", "/v1/requests", "", map[string]string{"X-API-Key": "valid-key", "Origin": "https://evil.example.org"})
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("foreign origin must get no CORS headers: %d %v", rec.Code, rec.Header())
	}
	rec = e.do(t, "OPTIONS", "/v1/requests", "", map[string]string{"Origin": "https://evil.example.org", "Access-Control-Request-Method": "POST"})
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("foreign preflight falls through to the mux: %d %v", rec.Code, rec.Header())
	}
	plain := newEnv(t)
	rec = plain.do(t, "GET", "/healthz", "", map[string]string{"Origin": "https://app.example.org"})
	if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Vary") != "" {
		t.Errorf("without configured origins no CORS header may appear: %v", rec.Header())
	}
}

func TestDiscoveryEndpoints(t *testing.T) {
	e := newEnvWith(t, func(c *config.Config) {
		c.Targets["proxy"] = config.Target{URL: "https://p.example.com/{code}", Proxy: true, Response: config.Response{Mode: config.ModeDirect}}
		c.Resolvents = map[string]config.Resolvent{
			"resolvent-pv1":  {URL: "https://pv.example.com/gen", Method: "POST", APIKey: "secret-key", CacheTTL: config.Duration(time.Hour)},
			"resolvent-buem": {Target: "meme", CacheTTL: config.Duration(2 * time.Hour)},
		}
	}, nil)
	for _, path := range []string{"/v1/targets", "/v1/resolvents"} {
		if rec := e.do(t, "GET", path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without key: %d, want 401", path, rec.Code)
		}
	}
	rec := e.do(t, "GET", "/v1/targets", "", authHdr)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "example.com") {
		t.Fatalf("/v1/targets: %d %s", rec.Code, rec.Body.String())
	}
	targets := decodeBody[map[string][]plan.TargetInfo](t, rec)["items"]
	if len(targets) != 2 || targets[0].Name != "meme" || targets[1].Name != "proxy" || !targets[1].Proxy || targets[1].AttachResolvent != nil {
		t.Errorf("targets = %+v", targets)
	}
	rec = e.do(t, "GET", "/v1/resolvents", "", authHdr)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "secret-key") || strings.Contains(rec.Body.String(), "example.com") {
		t.Fatalf("/v1/resolvents leaks configuration: %d %s", rec.Code, rec.Body.String())
	}
	res := decodeBody[map[string][]plan.ResolventInfo](t, rec)["items"]
	if len(res) != 2 || res[0].Type != "resolvent-buem" || res[0].Backend != "target" || res[0].Target != "meme" || res[1].Backend != "post" || res[1].CacheTTL != "1h0m0s" {
		t.Errorf("resolvents = %+v", res)
	}
}

// The dry run inspects like the worker, consults only the cache, persists
// nothing and shares create's request validation.
func TestValidateDryRun(t *testing.T) {
	e := newEnvWith(t, func(c *config.Config) {
		c.Targets["demo"] = config.Target{URL: "https://demo.example.com/run", TimeseriesPath: "time-series", Response: config.Response{Mode: config.ModeDirect}}
		c.Targets["proxy"] = config.Target{URL: "https://p.example.com/{code}", Proxy: true, Response: config.Response{Mode: config.ModeDirect}}
		c.Resolvents = map[string]config.Resolvent{"resolvent-pv1": {URL: "https://pv.example.com/gen", Method: "POST", CacheTTL: config.Duration(time.Hour)}}
	}, nil)
	validate := func(body string) (int, validateResponse) {
		rec := e.do(t, "POST", "/v1/requests/validate", body, authHdr)
		if rec.Code != http.StatusOK {
			return rec.Code, validateResponse{}
		}
		return rec.Code, decodeBody[validateResponse](t, rec)
	}
	payload := `{"time-series":[{"name":"pv","type":"resolvent-pv1","lat":48.83},{"type":"time-series","values":[1]}]}`
	code, resp := validate(`{"target":"demo","payload":` + payload + `}`)
	if code != 200 || !resp.OK || len(resp.Problems) != 0 || len(resp.Resolvents) != 1 ||
		resp.Resolvents[0].Path != "/time-series/0" || resp.Resolvents[0].Name != "pv" || resp.Resolvents[0].Cached {
		t.Fatalf("dry run = %d %+v", code, resp)
	}
	// Prime the cache under the key the worker uses (the plan's, which
	// leaves the name label out); the dry run then reports the series as
	// cached.
	key := plan.Inspect(e.server.cfg(), "demo", []byte(payload)).Found[0].Hash
	if err := e.store.PutSeries(context.Background(), key, "resolvent-pv1", []byte(`{}`), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, resp := validate(`{"target":"demo","payload":` + payload + `}`); !resp.Resolvents[0].Cached {
		t.Errorf("cached flag not set after priming: %+v", resp)
	}
	if _, resp := validate(`{"target":"demo","payload":{"time-series":[{"type":"resolvent-tidal"}]}}`); resp.OK || len(resp.Problems) != 1 || resp.Problems[0].Code != plan.CodeUnknownResolvent {
		t.Errorf("unknown resolvent = %+v", resp)
	}
	if _, resp := validate(`{"target":"proxy","payload":{"A_ref":1}}`); resp.OK || resp.Problems[0].Code != plan.CodeTargetError || len(resp.Resolvents) != 0 {
		t.Errorf("proxy placeholder = %+v", resp)
	}
	if code, _ := validate(`{"target":"hydra","payload":{}}`); code != http.StatusUnprocessableEntity {
		t.Errorf("unknown target: %d, want 422 like create", code)
	}
	if code, _ := validate(`{"target":"demo","payload":[1]}`); code != http.StatusBadRequest {
		t.Errorf("non-object payload: %d, want 400 like create", code)
	}
	if rec := e.do(t, "POST", "/v1/requests/validate", `{"target":"demo","payload":{}}`, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("no key: %d", rec.Code)
	}
	if jobs, _ := e.store.ListJobs(context.Background(), store.ListFilter{Limit: 10}); len(jobs) != 0 {
		t.Errorf("dry runs must persist nothing, found %d jobs", len(jobs))
	}
}

// options.cache is validated and echoed on the job when it is not the
// default.
func TestCacheOptionValidationAndEcho(t *testing.T) {
	e := newEnv(t)
	for _, body := range []string{`{"target":"meme","payload":{},"options":{"cache":"maybe"}}`, `{"target":"meme","payload":{},"options":{"cache":5}}`} {
		if rec := e.do(t, "POST", "/v1/requests", body, authHdr); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", body, rec.Code)
		}
	}
	for mode, want := range map[string]string{"refresh": `{"cache":"refresh"}`, "bypass": `{"cache":"bypass"}`, "use": "", "": ""} {
		body := `{"target":"meme","payload":{},"options":{"cache":"` + mode + `"}}`
		created := decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests", body, authHdr))
		doc := decodeBody[map[string]json.RawMessage](t, e.do(t, "GET", "/v1/requests/"+created.ID, "", authHdr))
		if string(doc["options"]) != want {
			t.Errorf("mode %q echoed as %s, want %q", mode, doc["options"], want)
		}
	}
}

// DELETE cancels queued and target-waiting requests, refuses ones in
// flight or finished, is scoped like every read, and tells a target with a
// cancel URL to stop its job.
func TestCancelRequest(t *testing.T) {
	var cancels atomic.Int64
	var gotPath atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			cancels.Add(1)
			gotPath.Store(r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)
	e := newEnvWith(t, func(c *config.Config) {
		c.Auth.APIKeys = append(c.Auth.APIKeys, config.APIKey{Name: "other", Key: "other-key", Role: config.RoleClient})
		c.Targets["meme"] = config.Target{URL: target.URL + "/simulate", Timeout: config.Duration(2 * time.Second), APIKeyInject: config.InjectNone,
			Response: config.Response{Mode: config.ModePoll, Poll: &config.Poll{
				IDJSONPath: "id", URLTemplate: target.URL + "/jobs/{id}", CancelURLTemplate: target.URL + "/jobs/{id}/cancel",
				StatusJSONPath: "state", DoneValues: []string{"done"}}}}
	}, nil)
	e.server.WithUpstream(upstream.New(1<<20, nil))
	ctx := context.Background()
	create := func() string {
		return decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests", validBody, nil)).ID
	}
	queued := create()
	if rec := e.do(t, "DELETE", "/v1/requests/"+queued, "", map[string]string{"X-API-Key": "other-key"}); rec.Code != http.StatusNotFound {
		t.Errorf("another client's cancel: %d, want 404", rec.Code)
	}
	rec := e.do(t, "DELETE", "/v1/requests/"+queued, "", authHdr)
	if rec.Code != http.StatusOK || decodeBody[jobResponse](t, rec).State != store.StateCancelled {
		t.Fatalf("cancel queued: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, "DELETE", "/v1/requests/"+queued, "", authHdr); rec.Code != http.StatusConflict || errCode(t, rec) != CodeNotCancellable {
		t.Errorf("cancel a cancelled request: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, "GET", "/v1/requests?state=cancelled", "", authHdr); rec.Code != http.StatusOK || len(decodeBody[listResponse](t, rec).Items) != 1 {
		t.Errorf("list cancelled: %d %s", rec.Code, rec.Body.String())
	}

	held := create()
	if _, err := e.store.ClaimNext(ctx, store.ClaimPolicy{}); err != nil {
		t.Fatal(err)
	}
	if rec := e.do(t, "DELETE", "/v1/requests/"+held, "", authHdr); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "being processed") {
		t.Errorf("cancel while resolving: %d %s", rec.Code, rec.Body.String())
	}
	if err := e.store.SetResolved(ctx, held, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.store.MarkAwaitingTarget(ctx, held, "m-9", 202, nil, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if rec := e.do(t, "DELETE", "/v1/requests/"+held, "", authHdr); rec.Code != http.StatusOK {
		t.Fatalf("cancel while awaiting: %d %s", rec.Code, rec.Body.String())
	}
	// The cancel call is best effort and asynchronous: wait for the handler
	// to have recorded the path (its last write), not merely counted the call.
	deadline := time.Now().Add(2 * time.Second)
	for gotPath.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if cancels.Load() != 1 || gotPath.Load() != "/jobs/m-9/cancel" {
		t.Errorf("target cancel: %d calls, path %v", cancels.Load(), gotPath.Load())
	}
	done := create()
	if err := e.store.MarkFailed(ctx, done, "target_error", "x"); err != nil {
		t.Fatal(err)
	}
	if rec := e.do(t, "DELETE", "/v1/requests/"+done, "", authHdr); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already finished") {
		t.Errorf("cancel a failed request: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, "DELETE", "/v1/requests/"+done, "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated cancel: %d", rec.Code)
	}
}

// ?wait= long-polls: the read returns as soon as the job ends (woken by the
// notifier), or with the current state when the wait elapses; invalid waits
// are refused and terminal jobs answer at once.
func TestLongPollWait(t *testing.T) {
	hub := notify.New()
	e := newEnvWith(t, func(c *config.Config) { c.Server.WriteTimeout = config.Duration(30 * time.Second) }, nil)
	e.server.WithNotifier(hub)
	created := decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests", validBody, nil))
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = e.store.MarkFailed(context.Background(), created.ID, "target_error", "done waiting")
		hub.Notify(created.ID)
	}()
	start := time.Now()
	rec := e.do(t, "GET", "/v1/requests/"+created.ID+"?wait=10s", "", authHdr)
	if rec.Code != http.StatusOK || decodeBody[jobResponse](t, rec).State != store.StateFailed {
		t.Fatalf("long poll: %d %s", rec.Code, rec.Body.String())
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the wake-up took %v; the notifier should end the wait within milliseconds", took)
	}
	if hub.Pending() != 0 {
		t.Errorf("waiters must be forgotten after the wake-up, %d left", hub.Pending())
	}
	pending := decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests", validBody, nil))
	start = time.Now()
	rec = e.do(t, "GET", "/v1/requests/"+pending.ID+"?wait=200ms", "", authHdr)
	if rec.Code != http.StatusOK || decodeBody[jobResponse](t, rec).State != store.StateReceived || time.Since(start) < 150*time.Millisecond {
		t.Errorf("elapsed wait must answer with the current state after the wait: %d %s in %v", rec.Code, rec.Body.String(), time.Since(start))
	}
	if hub.Pending() != 0 {
		t.Errorf("a timed-out waiter must be forgotten, %d left", hub.Pending())
	}
	for _, q := range []string{"?wait=soon", "?wait=-1s"} {
		if rec := e.do(t, "GET", "/v1/requests/"+pending.ID+q, "", authHdr); rec.Code != http.StatusBadRequest || errCode(t, rec) != CodeInvalidParameter {
			t.Errorf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
	}
	start = time.Now()
	if rec := e.do(t, "GET", "/v1/requests/"+created.ID+"?wait=10s", "", authHdr); rec.Code != http.StatusOK || time.Since(start) > time.Second {
		t.Errorf("a terminal job must answer at once: %d in %v", rec.Code, time.Since(start))
	}
	if got := e.server.maxWait(); got != 25*time.Second {
		t.Errorf("maxWait = %v, want write_timeout - 5s", got)
	}
	if got := (&Server{cfgp: config.Static(&config.Config{})}).maxWait(); got != time.Minute {
		t.Errorf("maxWait without a write timeout = %v, want 1m", got)
	}
}

// A batch stores each item independently and reports one result per item
// in order, with the codes the single endpoint would answer.
func TestBatchSubmission(t *testing.T) {
	zero := 0
	e := newEnvWith(t, func(c *config.Config) {
		c.Auth.APIKeys = append(c.Auth.APIKeys, config.APIKey{Name: "capped", Key: "capped-key", Role: config.RoleClient, MaxPriority: &zero})
	}, nil)
	first := decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests", validBody, map[string]string{"X-API-Key": "valid-key", "Idempotency-Key": "k-1"}))
	body := `{"requests":[
		{"target":"meme","payload":{"a":1}},
		{"target":"hydra","payload":{}},
		{"target":"meme","payload":[1]},
		{"target":"meme","payload":{},"priority":99},
		{"target":"meme","payload":{},"idempotency_key":"k-1"},
		{"target":"meme","payload":` + validPayloadOf(validBody) + `,"idempotency_key":"k-1"},
		{"target":"meme","payload":{},"options":{"cache":"refresh"},"priority":3}
	]}`
	rec := e.do(t, "POST", "/v1/requests/batch", body, authHdr)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("batch: %d %s", rec.Code, rec.Body.String())
	}
	items := decodeBody[map[string][]batchResult](t, rec)["items"]
	if len(items) != 7 {
		t.Fatalf("%d results, want 7", len(items))
	}
	wantCodes := []string{"", CodeUnknownTarget, CodeInvalidJSON, CodeInvalidParameter, CodeIdempotencyConflict, "", ""}
	for i, want := range wantCodes {
		got := ""
		if items[i].Error != nil {
			got = items[i].Error.Code
		}
		if got != want {
			t.Errorf("item %d: code %q, want %q (%+v)", i, got, want, items[i])
		}
	}
	if items[0].State != store.StateReceived || items[0].Links["self"] != "/v1/requests/"+items[0].ID {
		t.Errorf("accepted item = %+v", items[0])
	}
	if items[5].ID != first.ID {
		t.Errorf("an idempotent replay must return the earlier request: %s vs %s", items[5].ID, first.ID)
	}
	job, _ := e.store.GetJob(context.Background(), items[6].ID)
	if job.Priority != 3 || job.Options.Cache != store.CacheRefresh {
		t.Errorf("options and priority must be stored per item: %+v", job)
	}
	jobs, _ := e.store.ListJobs(context.Background(), store.ListFilter{Limit: 100})
	if len(jobs) != 3 {
		t.Errorf("stored jobs = %d, want the earlier one plus two new", len(jobs))
	}

	if rec := e.do(t, "POST", "/v1/requests/batch", `{"requests":[{"target":"hydra","payload":{}}]}`, authHdr); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"items"`) {
		t.Errorf("all-rejected batch: %d %s, want 400 with the items", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, "POST", "/v1/requests/batch", `{"requests":[]}`, authHdr); rec.Code != http.StatusBadRequest || errCode(t, rec) != CodeInvalidParameter {
		t.Errorf("empty batch: %d %s", rec.Code, rec.Body.String())
	}
	many := strings.Repeat(`{"target":"meme","payload":{}},`, 101)
	if rec := e.do(t, "POST", "/v1/requests/batch", `{"requests":[`+many[:len(many)-1]+`]}`, authHdr); rec.Code != http.StatusBadRequest || errCode(t, rec) != CodeInvalidParameter {
		t.Errorf("oversized batch: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, "POST", "/v1/requests/batch", `{"requests":[{"target":"meme","payload":{}}]}`, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: %d", rec.Code)
	}
	if rec := e.do(t, "POST", "/v1/requests/batch", `{"api_key":"valid-key","requests":[{"target":"meme","payload":{}}]}`, nil); rec.Code != http.StatusAccepted {
		t.Errorf("body key fallback: %d", rec.Code)
	}
	if rec := e.do(t, "POST", "/v1/requests/batch", `{"requests":[{"target":"meme","payload":{},"priority":1}]}`, map[string]string{"X-API-Key": "capped-key"}); rec.Code != http.StatusBadRequest {
		t.Errorf("priority cap applies per item: %d %s", rec.Code, rec.Body.String())
	}
}

// validPayloadOf extracts the payload of the shared validBody so a batch
// item can replay the single request's idempotency key exactly.
func validPayloadOf(body string) string {
	var doc map[string]json.RawMessage
	_ = json.Unmarshal([]byte(body), &doc)
	return string(doc["payload"])
}

// not_before is validated at accept, seeds the queue time so the job is not
// claimed early, and is echoed by GET for the job's whole life.
func TestNotBeforeDelaysTheRun(t *testing.T) {
	e := newEnv(t)
	future := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	body := `{"target":"meme","payload":{},"not_before":"` + future.Format(time.RFC3339) + `"}`
	rec := e.do(t, "POST", "/v1/requests", body, authHdr)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	id := decodeBody[createResponse](t, rec).ID
	job, err := e.store.GetJob(context.Background(), id)
	if err != nil || job.NotBefore == nil || !job.NotBefore.Equal(future) || job.NextAttemptAt == nil || !job.NextAttemptAt.Equal(future) {
		t.Fatalf("stored job = %+v (%v)", job, err)
	}
	if claimed, _ := e.store.ClaimNext(context.Background(), store.ClaimPolicy{}); claimed != nil {
		t.Errorf("a delayed job must not be claimed before its time: %+v", claimed)
	}
	got := decodeBody[jobResponse](t, e.do(t, "GET", "/v1/requests/"+id, "", authHdr))
	if got.NotBefore == nil || *got.NotBefore != future.Format(time.RFC3339) || got.State != store.StateReceived {
		t.Errorf("GET = %+v", got)
	}
	for body, want := range map[string]string{
		`{"target":"meme","payload":{},"not_before":"tomorrow"}`:                                                           "RFC 3339",
		`{"target":"meme","payload":{},"not_before":"` + time.Now().Add(31*24*time.Hour).UTC().Format(time.RFC3339) + `"}`: "30 days",
	} {
		rec := e.do(t, "POST", "/v1/requests", body, authHdr)
		if rec.Code != http.StatusBadRequest || errCode(t, rec) != CodeInvalidParameter || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	// A time in the past is accepted and simply runs at once.
	past := `{"target":"meme","payload":{"p":1},"not_before":"2020-01-01T00:00:00Z"}`
	if rec := e.do(t, "POST", "/v1/requests", past, authHdr); rec.Code != http.StatusAccepted {
		t.Errorf("past not_before: %d %s", rec.Code, rec.Body.String())
	}
	if claimed, _ := e.store.ClaimNext(context.Background(), store.ClaimPolicy{}); claimed == nil || claimed.NotBefore == nil {
		t.Errorf("the past-dated job must be claimable now: %+v", claimed)
	}
	// Batch items take not_before too.
	rec = e.do(t, "POST", "/v1/requests/batch", `{"requests":[{"target":"meme","payload":{},"not_before":"`+future.Format(time.RFC3339)+`"},{"target":"meme","payload":{},"not_before":"nope"}]}`, authHdr)
	items := decodeBody[map[string][]batchResult](t, rec)["items"]
	if rec.Code != http.StatusAccepted || items[0].Error != nil || items[1].Error == nil || items[1].Error.Code != CodeInvalidParameter {
		t.Errorf("batch: %d %s", rec.Code, rec.Body.String())
	}
}

// callback_url is accepted only for https URLs on allow-listed hosts; GET
// reports the delivery as pending until the deliverer has run.
func TestCallbackURLAllowListAndStatus(t *testing.T) {
	e := newEnvWith(t, func(c *config.Config) {
		c.Callbacks = config.Callbacks{AllowedHosts: []string{"hooks.example.com"}, SigningSecret: "s", MaxAttempts: 3}
	}, nil)
	for body, want := range map[string]string{
		`{"target":"meme","payload":{},"callback_url":"http://hooks.example.com/x"}`:  "https",
		`{"target":"meme","payload":{},"callback_url":"https://other.example.com/x"}`: "not allow-listed",
		`{"target":"meme","payload":{},"callback_url":"nonsense"}`:                    "https",
	} {
		rec := e.do(t, "POST", "/v1/requests", body, authHdr)
		if rec.Code != http.StatusUnprocessableEntity || errCode(t, rec) != CodeCallbackNotAllowed || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	rec := e.do(t, "POST", "/v1/requests", `{"target":"meme","payload":{},"callback_url":"https://hooks.example.com/x"}`, authHdr)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("accept: %d %s", rec.Code, rec.Body.String())
	}
	id := decodeBody[createResponse](t, rec).ID
	got := decodeBody[jobResponse](t, e.do(t, "GET", "/v1/requests/"+id, "", authHdr))
	if got.Callback == nil || got.Callback.State != store.DeliveryPending || got.Callback.URL != "https://hooks.example.com/x" || got.Callback.Attempts != 0 {
		t.Errorf("callback before terminal = %+v", got.Callback)
	}
	if err := e.store.MarkFailed(context.Background(), id, "target_error", "x"); err != nil {
		t.Fatal(err)
	}
	status := 503
	next := time.Now().Add(time.Hour)
	if err := e.store.RecordAttempt(context.Background(), id, 0, &status, "HTTP 503", false, &next); err != nil {
		t.Fatal(err)
	}
	got = decodeBody[jobResponse](t, e.do(t, "GET", "/v1/requests/"+id, "", authHdr))
	if got.Callback.State != store.DeliveryPending || got.Callback.Attempts != 1 || got.Callback.LastStatus == nil || *got.Callback.LastStatus != 503 || got.Callback.LastError != "HTTP 503" {
		t.Errorf("callback after a failed attempt = %+v", got.Callback)
	}
	plain := decodeBody[jobResponse](t, e.do(t, "GET", "/v1/requests/"+decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests", validBody, authHdr)).ID, "", authHdr))
	if plain.Callback != nil {
		t.Error("a request without callback_url must not report a callback")
	}
	// Callbacks disabled: every callback_url is refused.
	off := newEnv(t)
	if rec := off.do(t, "POST", "/v1/requests", `{"target":"meme","payload":{},"callback_url":"https://hooks.example.com/x"}`, authHdr); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "not enabled") {
		t.Errorf("disabled: %d %s", rec.Code, rec.Body.String())
	}
}

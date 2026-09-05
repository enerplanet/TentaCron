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
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/store"
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
	return &testEnv{server: New(cfg, st, logger, nudge), store: st, nudge: nudge}
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
	rec := e.do(t, "POST", "/v1/requests", `{"api_key":"valid-key","target":"meme","payload":{},"priority":"high"}`, nil)
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
		return len(decodeBody[map[string][]jobResponse](t, rec)["items"])
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
	items := decodeBody[map[string][]jobResponse](t, e.do(t, "GET", "/v1/requests", "", authHdr))["items"]
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
		return len(decodeBody[map[string][]jobResponse](t, e.do(t, "GET", "/v1/requests", "", hdr(k)))["items"])
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
	if _, err := e.store.ClaimNext(ctx, func(string) time.Duration { return time.Minute }); err != nil {
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

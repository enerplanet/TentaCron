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
	"strings"
	"testing"
	"time"

	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/store"
)

type testEnv struct {
	server *Server
	store  *store.Store
	nudge  chan struct{}
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	cfg := &config.Config{
		Server: config.Server{MaxBodyBytes: 1 << 20},
		Auth:   config.Auth{APIKeys: []config.APIKey{{Name: "test", Key: "valid-key"}}},
		Worker: config.Worker{MaxAttempts: 5},
		Targets: map[string]config.Target{
			"meme": {URL: "https://meme.example.com/simulate"},
			"buem": {URL: "https://buem.example.com/run"},
		},
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	nudge := make(chan struct{}, 1)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &testEnv{server: New(config.Static(cfg), st, logger, nudge), store: st, nudge: nudge}
}

func (e *testEnv) do(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return v
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return decodeBody[errorBody](t, rec).Error.Code
}

const validBody = `{"api_key":"valid-key","target":"meme","payload":{"time-series":[]}}`

func TestCreateHappyPath(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, "POST", "/v1/requests", validBody, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeBody[createResponse](t, rec)
	if resp.State != store.StateReceived || resp.ID == "" {
		t.Errorf("resp = %+v", resp)
	}
	if resp.Links["self"] != "/v1/requests/"+resp.ID {
		t.Errorf("self link = %q", resp.Links["self"])
	}
	select {
	case <-e.nudge:
	default:
		t.Error("worker nudge not sent")
	}
	job, err := e.store.GetJob(context.Background(), resp.ID)
	if err != nil {
		t.Fatalf("job not persisted: %v", err)
	}
	if string(job.Payload) != `{"time-series":[]}` {
		t.Errorf("stored payload = %s", job.Payload)
	}
	if strings.Contains(string(job.Payload), "valid-key") {
		t.Error("client api key leaked into stored payload")
	}
}

func TestCreateValidation(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name     string
		body     string
		headers  map[string]string
		wantCode int
		wantErr  string
	}{
		{"bad content type", validBody, map[string]string{"Content-Type": "text/plain"},
			http.StatusUnsupportedMediaType, CodeUnsupportedMediaType},
		{"invalid json", `{"api_key":`, nil, http.StatusBadRequest, CodeInvalidJSON},
		{"missing api_key", `{"target":"meme","payload":{}}`, nil, http.StatusBadRequest, CodeMissingField},
		{"missing target", `{"api_key":"valid-key","payload":{}}`, nil, http.StatusBadRequest, CodeMissingField},
		{"missing payload", `{"api_key":"valid-key","target":"meme"}`, nil, http.StatusBadRequest, CodeMissingField},
		{"payload not object", `{"api_key":"valid-key","target":"meme","payload":[1]}`, nil,
			http.StatusBadRequest, CodeInvalidJSON},
		{"wrong api key", `{"api_key":"nope","target":"meme","payload":{}}`, nil,
			http.StatusUnauthorized, CodeUnauthorized},
		{"unknown target", `{"api_key":"valid-key","target":"nope","payload":{}}`, nil,
			http.StatusUnprocessableEntity, CodeUnknownTarget},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := e.do(t, "POST", "/v1/requests", tt.body, tt.headers)
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if got := errCode(t, rec); got != tt.wantErr {
				t.Errorf("error code = %q, want %q", got, tt.wantErr)
			}
		})
	}
}

func TestCreateOversizedBody(t *testing.T) {
	e := newEnv(t)
	big := `{"api_key":"valid-key","target":"meme","payload":{"blob":"` +
		strings.Repeat("x", 2<<20) + `"}}`
	rec := e.do(t, "POST", "/v1/requests", big, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if got := errCode(t, rec); got != CodePayloadTooLarge {
		t.Errorf("error code = %q", got)
	}
}

func TestCreateIdempotencyReplay(t *testing.T) {
	e := newEnv(t)
	h := map[string]string{"Idempotency-Key": "abc-123"}
	first := decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests", validBody, h))
	rec := e.do(t, "POST", "/v1/requests", validBody, h)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d", rec.Code)
	}
	second := decodeBody[createResponse](t, rec)
	if first.ID != second.ID {
		t.Errorf("replay returned different id: %s vs %s", first.ID, second.ID)
	}
}

func TestGetJob(t *testing.T) {
	e := newEnv(t)
	created := decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests", validBody, nil))
	auth := map[string]string{"X-API-Key": "valid-key"}

	if rec := e.do(t, "GET", "/v1/requests/"+created.ID, "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no auth header: status = %d, want 401", rec.Code)
	}
	if rec := e.do(t, "GET", "/v1/requests/nope", "", auth); rec.Code != http.StatusNotFound {
		t.Errorf("unknown id: status = %d, want 404", rec.Code)
	}

	rec := e.do(t, "GET", "/v1/requests/"+created.ID, "", auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	job := decodeBody[jobResponse](t, rec)
	if job.ID != created.ID || job.State != store.StateReceived || job.Target != "meme" {
		t.Errorf("job = %+v", job)
	}
}

func TestGetCompletedJobEmbedsResult(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	created := decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests", validBody, nil))
	if err := e.store.MarkCompleted(ctx, created.ID, 200, []byte(`{"ok":true}`), "", "", "done"); err != nil {
		t.Fatal(err)
	}

	auth := map[string]string{"X-API-Key": "valid-key"}
	job := decodeBody[jobResponse](t, e.do(t, "GET", "/v1/requests/"+created.ID, "", auth))
	if job.Result == nil || job.Result.TargetStatus != 200 || string(job.Result.TargetResponse) != `{"ok":true}` {
		t.Errorf("result = %+v", job.Result)
	}

	rec := e.do(t, "GET", "/v1/requests/"+created.ID+"/result", "", auth)
	if rec.Code != http.StatusOK || rec.Body.String() != `{"ok":true}` {
		t.Errorf("result endpoint: %d %s", rec.Code, rec.Body.String())
	}
}

func TestResultFromFile(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	created := decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests", validBody, nil))

	path := filepath.Join(t.TempDir(), "bundle.zip")
	if err := os.WriteFile(path, []byte("PKzipbytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.store.MarkCompleted(ctx, created.ID, 200, nil, path, "application/zip", "done"); err != nil {
		t.Fatal(err)
	}

	auth := map[string]string{"X-API-Key": "valid-key"}
	job := decodeBody[jobResponse](t, e.do(t, "GET", "/v1/requests/"+created.ID, "", auth))
	if job.Result == nil || job.Result.Href == "" {
		t.Fatalf("want result href, got %+v", job.Result)
	}
	rec := e.do(t, "GET", job.Result.Href, "", auth)
	if rec.Code != http.StatusOK || rec.Body.String() != "PKzipbytes" {
		t.Errorf("file result: %d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("content type = %q", ct)
	}
}

func TestResultNotAvailableWhilePending(t *testing.T) {
	e := newEnv(t)
	created := decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests", validBody, nil))
	rec := e.do(t, "GET", "/v1/requests/"+created.ID+"/result", "", map[string]string{"X-API-Key": "valid-key"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestFailedJobExposesError(t *testing.T) {
	e := newEnv(t)
	created := decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests", validBody, nil))
	if err := e.store.MarkFailed(context.Background(), created.ID, "unknown_resolvent", "boom"); err != nil {
		t.Fatal(err)
	}
	job := decodeBody[jobResponse](t, e.do(t, "GET", "/v1/requests/"+created.ID, "", map[string]string{"X-API-Key": "valid-key"}))
	if job.Error == nil || job.Error.Code != "unknown_resolvent" {
		t.Errorf("error = %+v", job.Error)
	}
	if job.CompletedAt == nil {
		t.Error("completed_at missing on failed job")
	}
	if _, err := time.Parse(time.RFC3339, *job.CompletedAt); err != nil {
		t.Errorf("completed_at not RFC3339: %v", err)
	}
}

func TestList(t *testing.T) {
	e := newEnv(t)
	auth := map[string]string{"X-API-Key": "valid-key"}
	for i := 0; i < 3; i++ {
		e.do(t, "POST", "/v1/requests", validBody, nil)
	}
	rec := e.do(t, "GET", "/v1/requests?state=received&limit=2", "", auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	list := decodeBody[listResponse](t, rec)
	if len(list.Items) != 2 {
		t.Errorf("items = %d, want 2", len(list.Items))
	}
	if rec := e.do(t, "GET", "/v1/requests?state=bogus", "", auth); rec.Code != http.StatusBadRequest {
		t.Errorf("bogus state: status = %d, want 400", rec.Code)
	}
}

// The docs declare Content-Type: application/json required; a missing header
// must 415 exactly like a wrong one.
func TestCreateRequiresContentType(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/requests", bytes.NewBufferString(validBody))
	// deliberately no Content-Type header
	rec := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", rec.Code)
	}
	if got := errCode(t, rec); got != CodeUnsupportedMediaType {
		t.Errorf("error code = %q", got)
	}
}

// json.Decoder stops after the first value; trailing data means the body was
// not a single JSON object and must be rejected, not silently accepted.
func TestCreateRejectsTrailingData(t *testing.T) {
	e := newEnv(t)
	for _, body := range []string{validBody + "garbage", validBody + validBody} {
		rec := e.do(t, "POST", "/v1/requests", body, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %.40s…)", rec.Code, body)
		}
		if got := errCode(t, rec); got != CodeInvalidJSON {
			t.Errorf("error code = %q", got)
		}
	}
}

// Reusing an Idempotency-Key with a different request must 409, never
// silently drop the new request in favor of the stored one.
func TestCreateIdempotencyConflict(t *testing.T) {
	e := newEnv(t)
	h := map[string]string{"Idempotency-Key": "k-1"}
	if rec := e.do(t, "POST", "/v1/requests", validBody, h); rec.Code != http.StatusAccepted {
		t.Fatalf("first request: %d", rec.Code)
	}
	other := `{"api_key":"valid-key","target":"meme","payload":{"time-series":[1]}}`
	rec := e.do(t, "POST", "/v1/requests", other, h)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != CodeIdempotencyConflict {
		t.Errorf("error code = %q", got)
	}
}

// A result file pruned between GetJob and os.Open is a 404, not a 500.
func TestResultFileMissingReturns404(t *testing.T) {
	e := newEnv(t)
	created := decodeBody[createResponse](t, e.do(t, "POST", "/v1/requests", validBody, nil))
	missing := filepath.Join(t.TempDir(), "already-pruned.zip")
	if err := e.store.MarkCompleted(context.Background(), created.ID, 200, nil, missing, "application/zip", "done"); err != nil {
		t.Fatal(err)
	}
	rec := e.do(t, "GET", "/v1/requests/"+created.ID+"/result", "", map[string]string{"X-API-Key": "valid-key"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := errCode(t, rec); got != CodeNotFound {
		t.Errorf("error code = %q", got)
	}
}

func TestHealthAndReady(t *testing.T) {
	e := newEnv(t)
	if rec := e.do(t, "GET", "/healthz", "", nil); rec.Code != http.StatusOK {
		t.Errorf("healthz = %d", rec.Code)
	}
	if rec := e.do(t, "GET", "/readyz", "", nil); rec.Code != http.StatusOK {
		t.Errorf("readyz = %d", rec.Code)
	}
}

func TestUnknownRouteReturnsJSON404(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, "GET", "/v2/other", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := errCode(t, rec); got != CodeNotFound {
		t.Errorf("error code = %q", got)
	}
}

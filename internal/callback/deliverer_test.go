package callback

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/notify"
	"github.com/enerplanet/tentacron/internal/store"
)

type received struct {
	body    []byte
	headers http.Header
}

// receiver is a TLS callback endpoint whose replies are scripted per call.
func receiver(t *testing.T, replies ...int) (*httptest.Server, func() []received) {
	t.Helper()
	var mu sync.Mutex
	var got []received
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, received{body, r.Header.Clone()})
		n := len(got)
		mu.Unlock()
		status := replies[min(n, len(replies))-1]
		if status == 302 {
			w.Header().Set("Location", "https://elsewhere.invalid/")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, strings.Repeat("x", 4096)) // more than the deliverer reads
	}))
	t.Cleanup(srv.Close)
	return srv, func() []received {
		mu.Lock()
		defer mu.Unlock()
		return append([]received(nil), got...)
	}
}

func setup(t *testing.T, srv *httptest.Server) (*Deliverer, *store.Store, *config.Config) {
	t.Helper()
	cfg := &config.Config{
		Worker:    config.Worker{PollInterval: config.Duration(10 * time.Millisecond), BackoffBase: config.Duration(5 * time.Millisecond), BackoffMax: config.Duration(10 * time.Millisecond)},
		Callbacks: config.Callbacks{AllowedHosts: []string{strings.TrimPrefix(srv.URL, "https://")}, SigningSecret: "s3cret", MaxAttempts: 3, Timeout: config.Duration(2 * time.Second)},
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "cb.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	d := New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil))).WithHTTPClient(srv.Client())
	return d, st, cfg
}

// terminalJob creates a job with a callback and completes it, which enqueues
// the delivery.
func terminalJob(t *testing.T, st *store.Store, url, id string, fail bool) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := st.CreateJob(ctx, &store.Job{ID: id, Client: "c", Target: "demo", MaxAttempts: 1, Payload: []byte(`{}`), CallbackURL: url}); err != nil {
		t.Fatal(err)
	}
	var err error
	if fail {
		err = st.MarkFailed(ctx, id, "target_error", "boom")
	} else {
		err = st.MarkCompleted(ctx, id, 200, []byte(`{"ok":true}`), "", "", "done")
	}
	if err != nil {
		t.Fatal(err)
	}
}

func waitState(t *testing.T, st *store.Store, id, want string) *store.Delivery {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if d, err := st.GetDelivery(context.Background(), id); err == nil && d.State() == want {
			return d
		}
		time.Sleep(5 * time.Millisecond)
	}
	d, _ := st.GetDelivery(context.Background(), id)
	t.Fatalf("delivery %s never reached %s: %+v", id, want, d)
	return nil
}

func TestDeliversSignedJobDocumentAndRetries(t *testing.T) {
	srv, got := receiver(t, 503, 200)
	d, st, cfg := setup(t, srv)
	hub := notify.New()
	d.WithNotifier(hub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	terminalJob(t, st, srv.URL+"/hook", "j1", false)
	hub.Notify("j1")
	del := waitState(t, st, "j1", store.DeliveryDelivered)
	if del.Attempts != 2 || del.LastStatus == nil || *del.LastStatus != 200 || del.LastError != "" || del.Event != "request.completed" {
		t.Errorf("delivery = %+v", del)
	}
	rec := got()
	if len(rec) != 2 {
		t.Fatalf("%d calls, want a retry after the 503", len(rec))
	}
	first, second := rec[0], rec[1]
	if first.headers.Get(HeaderAttempt) != "1" || second.headers.Get(HeaderAttempt) != "2" || second.headers.Get(HeaderEvent) != "request.completed" || second.headers.Get(HeaderRequestID) != "j1" {
		t.Errorf("headers = %v / %v", first.headers, second.headers)
	}
	if !Verify(cfg.Callbacks.SigningSecret, second.body, second.headers.Get(HeaderSignature)) || Verify("wrong", second.body, second.headers.Get(HeaderSignature)) {
		t.Error("signature must verify with the configured secret only")
	}
	var doc map[string]any
	if err := json.Unmarshal(second.body, &doc); err != nil || doc["id"] != "j1" || doc["state"] != "completed" || doc["result"].(map[string]any)["target_status"] != float64(200) {
		t.Errorf("body = %s", second.body)
	}
	if _, has := doc["callback"]; has {
		t.Error("the delivered document must not describe its own delivery")
	}
}

func TestGivesUpOnClientErrorsRedirectsAndBudget(t *testing.T) {
	srv, got := receiver(t, 410)
	d, st, _ := setup(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	terminalJob(t, st, srv.URL+"/gone", "j410", true)
	del := waitState(t, st, "j410", store.DeliveryFailed)
	if del.Attempts != 1 || *del.LastStatus != 410 || del.Event != "request.failed" || !strings.Contains(del.LastError, "HTTP 410") {
		t.Errorf("410 delivery = %+v", del)
	}
	if n := len(got()); n != 1 {
		t.Errorf("a 4xx must not be retried: %d calls", n)
	}
	// A redirect is not followed and counts as a permanent refusal.
	srv2, got2 := receiver(t, 302)
	d2, st2, _ := setup(t, srv2)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go d2.Run(ctx2)
	terminalJob(t, st2, srv2.URL+"/moved", "j302", false)
	if del := waitState(t, st2, "j302", store.DeliveryFailed); *del.LastStatus != 302 || len(got2()) != 1 {
		t.Errorf("redirect delivery = %+v calls %d", del, len(got2()))
	}
	// Persistent 5xx exhausts max_attempts (3).
	srv3, got3 := receiver(t, 500)
	d3, st3, _ := setup(t, srv3)
	ctx3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()
	go d3.Run(ctx3)
	terminalJob(t, st3, srv3.URL+"/down", "j500", false)
	if del := waitState(t, st3, "j500", store.DeliveryFailed); del.Attempts != 3 || len(got3()) != 3 {
		t.Errorf("budget: %+v calls %d", del, len(got3()))
	}
}

func TestCheckAllowList(t *testing.T) {
	cfg := config.Callbacks{AllowedHosts: []string{"hooks.example.com", "Internal.Example:8443"}}
	ok := []string{"https://hooks.example.com/x", "https://HOOKS.example.com/", "https://internal.example:8443/y?z=1"}
	for _, u := range ok {
		if err := Check(cfg, u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	bad := map[string]string{
		"http://hooks.example.com/x":           "https",
		"https://hooks.example.com:444/x":      "not allow-listed",
		"https://evil.example.com/x":           "not allow-listed",
		"https://user:pw@hooks.example.com/x":  "credentials",
		"https:///x":                           "no host",
		"://bad":                               "valid URL",
		"https://hooks.example.com.evil.net/x": "not allow-listed",
		"https://hooks.example.com@evil.net/x": "credentials",
	}
	for u, want := range bad {
		if err := Check(cfg, u); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", u, err, want)
		}
	}
	if err := Check(config.Callbacks{}, "https://hooks.example.com/x"); !errors.Is(err, ErrDisabled) {
		t.Errorf("disabled: %v", err)
	}
}

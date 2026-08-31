// End-to-end tests exercising the full stack: HTTP API -> store -> worker
// pool -> fake resource/target services -> status endpoint.
package worker_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enerplanet/tentacron/internal/api"
	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/store"
	"github.com/enerplanet/tentacron/internal/upstream"
	"github.com/enerplanet/tentacron/internal/worker"
)

const clientKey = "tk-e2e-key"

type e2eStack struct {
	api     *httptest.Server
	lastFwd func() []byte
}

// startStack boots the whole service in-process against fake upstream APIs:
// a MEME-style async target (accept -> poll -> result) and a resource API.
func startStack(t *testing.T) *e2eStack {
	t.Helper()

	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"time-series","unit":"kW","values":[0.4,0.9]}`)
	}))
	t.Cleanup(resource.Close)

	var mu sync.Mutex
	var lastForwarded []byte
	var polls int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /simulate", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastForwarded = body
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"job_id":"meme-7"}`)
	})
	mux.HandleFunc("GET /jobs/{id}", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		polls++
		running := polls < 2
		mu.Unlock()
		if running {
			_, _ = io.WriteString(w, `{"status":"running"}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"done","objective":1234.5}`)
	})
	target := httptest.NewServer(mux)
	t.Cleanup(target.Close)

	cfg := &config.Config{
		Server:  config.Server{MaxBodyBytes: 1 << 20},
		Auth:    config.Auth{APIKeys: []config.APIKey{{Name: "e2e", Key: clientKey}}},
		Storage: config.Storage{ResultsDir: t.TempDir(), Retention: config.Duration(time.Hour)},
		Worker: config.Worker{
			Count: 2, ResolventConcurrency: 4,
			PollInterval: config.Duration(15 * time.Millisecond), MaxAttempts: 3,
			BackoffBase: config.Duration(10 * time.Millisecond), BackoffMax: config.Duration(40 * time.Millisecond),
			JobTimeout: config.Duration(5 * time.Second),
		},
		Cache: config.Cache{DefaultTTL: config.Duration(time.Hour), CleanupInterval: config.Duration(time.Hour)},
		Targets: map[string]config.Target{"meme": {
			URL: target.URL + "/simulate", Method: "POST", Timeout: config.Duration(2 * time.Second),
			TimeseriesPath: "model.timeseries",
			APIKey:         "meme-secret", APIKeyInject: config.InjectBodyField, APIKeyField: "api_key",
			Response: config.Response{Mode: config.ModePoll, Poll: &config.Poll{
				IDJSONPath: "job_id", URLTemplate: target.URL + "/jobs/{id}",
				ResultURLTemplate: target.URL + "/jobs/{id}",
				StatusJSONPath:    "status", DoneValues: []string{"done"}, FailedValues: []string{"failed"},
				Interval: config.Duration(10 * time.Millisecond), Timeout: config.Duration(5 * time.Second),
			}},
		}},
		Resolvents: map[string]config.Resolvent{"resolvent-pv1": {
			URL: resource.URL, Method: "POST", APIKeyHeader: "X-API-Key",
			Timeout: config.Duration(2 * time.Second), CacheTTL: config.Duration(time.Hour),
		}},
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	nudge := make(chan struct{}, 1)
	pool := worker.New(cfg, st, upstream.New(cfg.Server.MaxBodyBytes, nil), logger, nudge)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		pool.Run(ctx)
	}()
	apiSrv := httptest.NewServer(api.New(cfg, st, logger, nudge).Handler())
	t.Cleanup(func() {
		apiSrv.Close()
		cancel()
		<-done
		_ = st.Close()
	})

	return &e2eStack{
		api: apiSrv,
		lastFwd: func() []byte {
			mu.Lock()
			defer mu.Unlock()
			return lastForwarded
		},
	}
}

func postJSON(t *testing.T, url, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func getStatus(t *testing.T, base, id string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/v1/requests/"+id, nil)
	req.Header.Set("X-API-Key", clientKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func waitState(t *testing.T, base, id string, want ...string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		doc := getStatus(t, base, id)
		state, _ := doc["state"].(string)
		for _, w := range want {
			if state == w {
				return doc
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s never reached %v; last: %v", id, want, getStatus(t, base, id))
	return nil
}

func TestE2EHappyPathThroughAPI(t *testing.T) {
	stack := startStack(t)

	status, body := postJSON(t, stack.api.URL+"/v1/requests", `{
		"api_key": "`+clientKey+`",
		"target": "meme",
		"payload": {
			"model": {
				"timeseries": {
					"pv_cf": {"type": "resolvent-pv1", "lat": 48.83, "lon": 12.95},
					"demand": {"type": "time-series", "values": [5, 6]}
				}
			}
		}
	}`)
	if status != http.StatusAccepted {
		t.Fatalf("POST status = %d: %s", status, body)
	}
	var accepted struct{ ID string `json:"id"` }
	if err := json.Unmarshal(body, &accepted); err != nil || accepted.ID == "" {
		t.Fatalf("accept body: %s", body)
	}

	final := waitState(t, stack.api.URL, accepted.ID, "completed", "failed")
	if final["state"] != "completed" {
		t.Fatalf("job failed: %v", final["error"])
	}
	if final["target_job_id"] != "meme-7" {
		t.Errorf("target_job_id = %v", final["target_job_id"])
	}
	result := final["result"].(map[string]any)
	inner := result["target_response"].(map[string]any)
	if inner["objective"] != 1234.5 {
		t.Errorf("target_response = %v", inner)
	}

	// What MEME received: registry slot replaced, resolvent preserved,
	// pass-through untouched, target key injected at the top level.
	var fwd map[string]any
	if err := json.Unmarshal(stack.lastFwd(), &fwd); err != nil {
		t.Fatalf("forwarded body: %v", err)
	}
	if fwd["api_key"] != "meme-secret" {
		t.Errorf("target api key not injected: %v", fwd["api_key"])
	}
	registry := fwd["model"].(map[string]any)["timeseries"].(map[string]any)
	pv := registry["pv_cf"].(map[string]any)
	if pv["type"] != "time-series" || pv["resolvent"].(map[string]any)["lat"] != 48.83 {
		t.Errorf("pv_cf slot = %v", pv)
	}
	demand := registry["demand"].(map[string]any)
	if demand["type"] != "time-series" || demand["resolvent"] != nil {
		t.Errorf("demand slot was touched: %v", demand)
	}
}

func TestE2EUnknownResolventSurfacesError(t *testing.T) {
	stack := startStack(t)
	status, body := postJSON(t, stack.api.URL+"/v1/requests", `{
		"api_key": "`+clientKey+`",
		"target": "meme",
		"payload": {"model": {"timeseries": {"x": {"type": "resolvent-tidal"}}}}
	}`)
	if status != http.StatusAccepted {
		t.Fatalf("POST status = %d: %s", status, body)
	}
	var accepted struct{ ID string `json:"id"` }
	_ = json.Unmarshal(body, &accepted)

	final := waitState(t, stack.api.URL, accepted.ID, "failed")
	errInfo := final["error"].(map[string]any)
	if errInfo["code"] != "unknown_resolvent" || !strings.Contains(errInfo["message"].(string), "resolvent-tidal") {
		t.Errorf("error = %v", errInfo)
	}
}

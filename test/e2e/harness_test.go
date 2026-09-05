// Package e2e is tentacron's golden end-to-end suite. Every scenario drives
// the complete in-process service — HTTP API, SQLite store, worker pool,
// outbound client — against scripted upstream fakes and freezes the entire
// observable behavior (API responses, the payload and auth header the target
// received, the audit trail, upstream call counts) into testdata/golden/. A
// behavior change anywhere in the pipeline shows up as a readable golden
// diff; intended changes are accepted with `go test ./test/e2e -update`
// (make golden-update).
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enerplanet/tentacron/internal/api"
	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/store"
	"github.com/enerplanet/tentacron/internal/upstream"
	"github.com/enerplanet/tentacron/internal/worker"
)

var update = flag.Bool("update", false, "rewrite golden files instead of comparing")

// Fixed credentials for the golden stack. They are test constants, not
// secrets — appearing in a golden file is fine and proves injection works.
const (
	clientKey       = "golden-client-key"
	secondClientKey = "golden-second-client-key"
	adminKey        = "golden-admin-key"
	demoSecret      = "demo-golden-secret"
	buemSecret      = "buem-golden-secret"
	ignisSecret     = "ignis-golden-secret"
	memeSecret      = "meme-golden-secret"
	resourceSecret  = "resource-golden-secret"
)

const defaultSeries = `{"type":"time-series","unit":"kW","values":[0.1,0.7,1.3]}`

// reply is one scripted upstream response.
type reply struct {
	status int
	body   string
	ct     string // optional Content-Type override
}

// fakes scripts the upstream servers per scenario; the call argument counts
// per endpoint, starting at 1, so behavior like "fail twice, then succeed"
// stays deterministic. Nil fields use the happy-path default. The meme
// endpoints speak meme's verified vocabulary (id/state, queued|running|
// succeeded|failed, status and result on separate routes) so goldens never
// teach a wrong contract.
type fakes struct {
	resource    func(call int64) reply              // POST <resource>/ (POST-style resolvents)
	resourceGET func(path string, call int64) reply // GET <resource>/... (weather/city2tabula/ignis-style)
	direct      func(call int64) reply              // POST <target>/run  (the "demo" direct target)
	accept      func(call int64) reply              // POST <target>/simulate (the "meme" poll target)
	status      func(call int64) reply              // GET  <target>/jobs/{id}/status
	result      func(call int64) reply              // GET  <target>/jobs/{id} (result fetch)
	gateway     func(call int64) reply              // POST <target>/api/v1/buem/buildings (the real buem contract)
	building    func(call int64) reply              // POST <target>/api/v1/buem/building (single building; backs resolvent-buem)
	calculate   func(call int64) reply              // POST <target>/api/v1/calculate/{code} (ignis; the proxy-target exemplar)
	directDelay func(call int64) time.Duration      // latency before the demo target answers a given call (deadline and scheduling scenarios)
}

const weatherBody = `{"index":["2018-01-01T00:30:00Z","2018-01-01T01:30:00Z"],"variables":{"T":[1.0,1.2],"GHI":[0.0,12.5]}}`

func (f fakes) withDefaults() fakes {
	return f.withResourceDefaults().withTargetDefaults()
}

// withResourceDefaults fills the resource-API replies: the classic POST
// series, and GET replies routed by path so concurrent fetches stay
// deterministic — each mirrors its real API's response shape.
func (f fakes) withResourceDefaults() fakes {
	if f.resource == nil {
		f.resource = func(int64) reply { return reply{200, defaultSeries, ""} }
	}
	if f.resourceGET == nil {
		f.resourceGET = func(path string, _ int64) reply {
			switch {
			case strings.HasPrefix(path, "/point"):
				return reply{200, weatherBody, ""}
			case strings.HasPrefix(path, "/buildings"):
				return reply{200, `[{"object_id":"DEHB01AL3AU0004T","number_of_storeys":2,"area_total_wall":214.5,"area_total_roof":98.2,"tabula_variant_code":"DE.N.SFH.04.Gen.ReEx.001.001"}]`, ""}
			case strings.HasPrefix(path, "/data/"):
				return reply{200, `{"country":"DE","variant_code":"DE.N.SFH.04.Gen.ReEx.001.001","expected_q_h_nd":112.4,"tabula_data":{"u_wall":1.6,"u_roof":1.2}}`, ""}
			case strings.HasPrefix(path, "/seriescalc"):
				// PVGIS seriescalc, verified against the public API: the
				// hourly rows sit under outputs.hourly with P in watts.
				return reply{200, `{"inputs":{"location":{"latitude":48.83,"longitude":12.95,"elevation":316.0},"meteo_data":{"radiation_db":"PVGIS-SARAH3","meteo_db":"ERA5","year_min":2020,"year_max":2020},"pv_module":{"technology":"c-Si","peak_power":12.5,"system_loss":14.0}},"outputs":{"hourly":[{"time":"20200101:0010","P":0.0,"G(i)":0.0,"T2m":-1.76},{"time":"20200101:1210","P":3812.4,"G(i)":401.2,"T2m":3.1},{"time":"20200101:1310","P":4105.0,"G(i)":433.9,"T2m":3.4}]},"meta":{"outputs":{"hourly":{"type":"time series","variables":{"P":{"description":"PV system power","units":"W"}}}}}}`, ""}
			default:
				return reply{404, `{"error":"unknown resource path"}`, ""}
			}
		}
	}
	return f
}

// withTargetDefaults fills every target reply with the service's documented
// happy-path response.
func (f fakes) withTargetDefaults() fakes {
	if f.direct == nil {
		f.direct = func(int64) reply { return reply{200, `{"ok":true}`, ""} }
	}
	if f.accept == nil {
		f.accept = func(int64) reply { return reply{202, `{"id":"m-golden-1","state":"queued"}`, ""} }
	}
	if f.status == nil {
		f.status = func(int64) reply { return reply{200, `{"id":"m-golden-1","state":"succeeded"}`, ""} }
	}
	if f.result == nil {
		f.result = func(int64) reply { return reply{200, `{"id":"m-golden-1","state":"succeeded","objective":1234.5}`, ""} }
	}
	if f.gateway == nil {
		// One result entry per building, in request order — buem-gateway's
		// documented response shape for POST /api/v1/buem/buildings.
		f.gateway = func(int64) reply {
			return reply{200, `[{"id":"b-1","buem":{"thermal_load_profile":{"summary":{"heating":{"total":{"value":12345.6,"unit":"kWh"}}}}}}]`, ""}
		}
	}
	if f.building == nil {
		// The single-building endpoint returns one enriched buem block —
		// the object shape a target-backed resolvent substitutes from.
		f.building = func(int64) reply {
			return reply{200, `{"id":"b-1","buem":{"thermal_load_profile":{"timeseries":{"unit":"kW","timestamps":["2018-01-01T00:00:00Z","2018-01-01T01:00:00Z"],"heating":[19.01,19.16]}},"model_metadata":{"buem_version":"golden"}}}`, ""}
		}
	}
	if f.calculate == nil {
		// ignis's documented calculate response: annual heating demand for
		// the variant, with optional overrides applied.
		f.calculate = func(int64) reply {
			return reply{200, `{"variant_code":"DE.N.SFH.04.Gen.ReEx.001.001","q_h_nd":112.4,"unit":"kWh/(m2a)"}`, ""}
		}
	}
	return f
}

// harness owns one isolated full tentacron stack plus the recording of every
// observable interaction into an ordered transcript.
type harness struct {
	t   *testing.T
	st  *store.Store
	api *httptest.Server

	resourceCalls, demoCalls, acceptCalls, statusCalls, resultCalls, gatewayCalls, buildingCalls, calculateCalls atomic.Int64

	mu               sync.Mutex
	lastForwarded    map[string][]byte   // keyed by target name
	lastTargetAuth   map[string]string   // X-API-Key seen per endpoint
	lastTargetPath   map[string]string   // request path seen per endpoint
	callOrder        map[string][]string // per endpoint: the "who" marker of each payload, in arrival order
	lastResourceAuth string
	lastResourceBody []byte
	resourceGETs     map[string]string // path -> "GET path?query" of the last GET per path
	unexpected       []string          // any upstream request the fakes did not script

	steps []map[string]any
	ids   []string // job ids in discovery order -> «job-N»
}

func writeReply(w http.ResponseWriter, r reply) {
	if r.ct != "" {
		w.Header().Set("Content-Type", r.ct)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(r.status)
	_, _ = io.WriteString(w, r.body)
}

// newHarness boots the full stack against the scripted fakes. mod may adjust
// the config (attempt limits, poll deadline) before the stack starts.
func newHarness(t *testing.T, f fakes, mod func(*config.Config)) *harness {
	t.Helper()
	f = f.withDefaults()
	h := &harness{
		t:              t,
		lastForwarded:  map[string][]byte{},
		lastTargetAuth: map[string]string{},
		lastTargetPath: map[string]string{},
		callOrder:      map[string][]string{},
		resourceGETs:   map[string]string{},
	}
	resource := h.startResourceFake(f)
	target := h.startTargetFake(f)
	cfg := goldenConfig(t, resource.URL, target.URL)
	if mod != nil {
		mod(cfg)
	}
	h.startStack(cfg)
	return h
}

// startResourceFake serves the resource APIs: POST "/" for the classic
// resolvents and GET paths for the weather/city2tabula/ignis contracts,
// recording the auth header, body and exact request line of each call.
func (h *harness) startResourceFake(f fakes) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/":
			body, _ := io.ReadAll(r.Body)
			h.mu.Lock()
			h.lastResourceAuth = r.Header.Get("X-API-Key")
			h.lastResourceBody = body
			h.mu.Unlock()
			writeReply(w, f.resource(h.resourceCalls.Add(1)))
		case r.Method == http.MethodGet:
			h.mu.Lock()
			h.lastResourceAuth = r.Header.Get("X-API-Key")
			h.resourceGETs[r.URL.Path] = "GET " + r.URL.RequestURI()
			h.mu.Unlock()
			writeReply(w, f.resourceGET(r.URL.Path, h.resourceCalls.Add(1)))
		default:
			h.noteUnexpected(r)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	h.t.Cleanup(srv.Close)
	return srv
}

// startTargetFake serves every target endpoint on one mux. The catch-all is
// a tripwire: traffic the fakes did not script (an escaped job id rewriting
// the poll path, a wrong method, an unforeseen extra call) must surface as
// a golden diff, not vanish into a silent 404.
func (h *harness) startTargetFake(f fakes) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /run", h.capturing("demo", &h.demoCalls, f.direct, f.directDelay))
	mux.HandleFunc("POST /simulate", h.capturing("meme", &h.acceptCalls, f.accept, nil))
	mux.HandleFunc("POST /api/v1/buem/buildings", h.capturing("buem", &h.gatewayCalls, f.gateway, nil))
	mux.HandleFunc("POST /api/v1/buem/building", h.capturing("buem-building", &h.buildingCalls, f.building, nil))
	mux.HandleFunc("POST /api/v1/calculate/{code}", h.capturing("ignis-calculate", &h.calculateCalls, f.calculate, nil))
	mux.HandleFunc("GET /jobs/{id}/status", func(w http.ResponseWriter, _ *http.Request) {
		writeReply(w, f.status(h.statusCalls.Add(1)))
	})
	mux.HandleFunc("GET /jobs/{id}", func(w http.ResponseWriter, _ *http.Request) {
		writeReply(w, f.result(h.resultCalls.Add(1)))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.noteUnexpected(r)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	h.t.Cleanup(srv.Close)
	return srv
}

// capturing builds a target handler that records the body, auth header and
// path it received under endpoint, counts the call, notes the payload's
// "who" marker for call-order assertions, waits out the scripted latency
// (giving up when the client hangs up) and answers with the scripted reply.
func (h *harness) capturing(endpoint string, calls *atomic.Int64, replyFor func(int64) reply, delayFor func(int64) time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var marker struct {
			Who string `json:"who"`
		}
		_ = json.Unmarshal(body, &marker)
		h.mu.Lock()
		h.lastForwarded[endpoint] = body
		h.lastTargetAuth[endpoint] = r.Header.Get("X-API-Key")
		h.lastTargetPath[endpoint] = r.URL.Path
		if marker.Who != "" {
			h.callOrder[endpoint] = append(h.callOrder[endpoint], marker.Who)
		}
		h.mu.Unlock()
		call := calls.Add(1)
		if delayFor != nil {
			if delay := delayFor(call); delay > 0 {
				select {
				case <-time.After(delay):
				case <-r.Context().Done():
					return
				}
			}
		}
		writeReply(w, replyFor(call))
	}
}

// targetCallOrder records the sequence in which one endpoint received
// payloads, by their "who" marker — the observable claim order.
func (h *harness) targetCallOrder(label, endpoint string) {
	h.mu.Lock()
	order := append([]string{}, h.callOrder[endpoint]...)
	h.mu.Unlock()
	h.record(map[string]any{"step": label, "endpoint": endpoint, "target_call_order": order})
}

func dur(d time.Duration) config.Duration { return config.Duration(d) }

// goldenConfig is the fixed stack configuration every scenario starts from.
func goldenConfig(t *testing.T, resourceURL, targetURL string) *config.Config {
	return &config.Config{
		Server:   config.Server{MaxBodyBytes: 4096},
		Upstream: config.Upstream{MaxResponseBytes: 4096},
		Auth: config.Auth{APIKeys: []config.APIKey{
			{Name: "golden", Key: clientKey, Role: config.RoleClient},
			{Name: "second", Key: secondClientKey, Role: config.RoleClient},
			{Name: "ops", Key: adminKey, Role: config.RoleAdmin},
		}},
		// One worker where a scenario needs an observable claim order; the
		// default of two keeps the corpus fast.
		Storage: config.Storage{ResultsDir: t.TempDir(), Retention: dur(time.Hour), MaxResultBytes: 1 << 20},
		Worker: config.Worker{
			Count: 2, ResolventConcurrency: 4,
			PollInterval: dur(10 * time.Millisecond), MaxAttempts: 3,
			BackoffBase: dur(5 * time.Millisecond), BackoffMax: dur(10 * time.Millisecond),
			// Generous: a job timeout firing under CI load would change
			// attempts/audit lines and flake the goldens.
			JobTimeout: dur(60 * time.Second),
		},
		Cache:      config.Cache{DefaultTTL: dur(time.Hour), CleanupInterval: dur(time.Hour)},
		Targets:    goldenTargets(targetURL),
		Resolvents: goldenResolvents(resourceURL),
	}
}

// goldenTargets wires every target contract the corpus exercises against
// the fake target server.
func goldenTargets(base string) map[string]config.Target {
	return map[string]config.Target{
		// The generic direct-mode fixture: default "time-series" array
		// container, header key injection.
		"demo": {
			URL: base + "/run", Method: "POST", Timeout: dur(2 * time.Second),
			TimeseriesPath: "time-series",
			APIKey:         demoSecret, APIKeyInject: config.InjectHeader, APIKeyHeader: "X-API-Key",
			Response: config.Response{Mode: config.ModeDirect},
		},
		// The real buem-gateway contract: synchronous batch endpoint,
		// X-Api-Key via the reverse proxy, the weather time series at
		// the payload root, and no tentacron marker in the forwarded
		// payload (BuEM's schema must receive weather unchanged).
		"buem": {
			URL: base + "/api/v1/buem/buildings", Method: "POST", Timeout: dur(2 * time.Second),
			TimeseriesPath: config.RootTimeseriesPath, AttachResolvent: boolPtr(false),
			APIKey: buemSecret, APIKeyInject: config.InjectHeader, APIKeyHeader: "X-Api-Key",
			Response: config.Response{Mode: config.ModeDirect},
		},
		// The single-building endpoint; backs resolvent-buem so a BuEM
		// simulation can feed another target's payload.
		"buem-building": {
			URL: base + "/api/v1/buem/building", Method: "POST", Timeout: dur(2 * time.Second),
			TimeseriesPath: config.RootTimeseriesPath, AttachResolvent: boolPtr(false),
			APIKey: buemSecret, APIKeyInject: config.InjectHeader, APIKeyHeader: "X-Api-Key",
			Response: config.Response{Mode: config.ModeDirect},
		},
		// The proxy-target exemplar: ignis's calculate endpoint. The
		// payload is handed through unresolved; the {code} placeholder
		// is filled from (and stripped out of) the payload.
		"ignis-calculate": {
			URL: base + "/api/v1/calculate/{code}", Method: "POST", Timeout: dur(2 * time.Second),
			Proxy:  true,
			APIKey: ignisSecret, APIKeyInject: config.InjectHeader, APIKeyHeader: "X-Api-Key",
			Response: config.Response{Mode: config.ModeDirect},
		},
		// meme's verified poll contract: id in "id", status at
		// /jobs/{id}/status with state queued|running|succeeded|failed,
		// the result bundle at /jobs/{id}.
		"meme": {
			URL: base + "/simulate", Method: "POST", Timeout: dur(2 * time.Second),
			TimeseriesPath: "model.timeseries",
			APIKey:         memeSecret, APIKeyInject: config.InjectBodyField, APIKeyField: "api_key",
			Response: config.Response{Mode: config.ModePoll, Poll: &config.Poll{
				IDJSONPath: "id", URLTemplate: base + "/jobs/{id}/status",
				ResultURLTemplate: base + "/jobs/{id}",
				StatusJSONPath:    "state", DoneValues: []string{"succeeded"}, FailedValues: []string{"failed"},
				Interval: dur(10 * time.Millisecond), Timeout: dur(2 * time.Second),
			}},
		},
	}
}

// goldenResolvents wires the POST resolvents, the GET resolvents against the
// verified weather/city2tabula/ignis contracts, and the target-backed
// resolvent-buem against the fake resource server.
func goldenResolvents(base string) map[string]config.Resolvent {
	return map[string]config.Resolvent{
		"resolvent-pv1": {URL: base, Method: "POST", APIKey: resourceSecret,
			APIKeyHeader: "X-API-Key", Timeout: dur(2 * time.Second), CacheTTL: dur(time.Hour)},
		"resolvent-wind": {URL: base, Method: "POST", APIKey: resourceSecret,
			APIKeyHeader: "X-API-Key", Timeout: dur(2 * time.Second), CacheTTL: dur(time.Hour)},
		"resolvent-weather": {URL: base + "/point?format=json", Method: "GET",
			APIKey: resourceSecret, APIKeyHeader: "X-API-Key",
			Timeout: dur(2 * time.Second), CacheTTL: dur(time.Hour)},
		"resolvent-city2tabula": {URL: base + "/buildings", Method: "GET",
			Timeout: dur(2 * time.Second), CacheTTL: dur(time.Hour), ResponsePath: "0"},
		"resolvent-ignis": {URL: base + "/data/{code}", Method: "GET",
			APIKey: resourceSecret, APIKeyHeader: "X-Api-Key",
			Timeout: dur(2 * time.Second), CacheTTL: dur(time.Hour)},
		// The verified PVGIS contract: resolvent fields renamed onto the
		// API's parameters (query_map), the hourly rows reshaped into a
		// time-series object (response_map), power rescaled to kW.
		"resolvent-pvgis": {URL: base + "/seriescalc?outputformat=json&pvcalculation=1", Method: "GET",
			Timeout: dur(2 * time.Second), CacheTTL: dur(time.Hour),
			QueryMap: map[string]string{"capacity_kw": "peakpower", "tilt": "angle", "azimuth": "aspect", "from_year": "startyear", "to_year": "endyear"},
			ResponseMap: map[string]any{
				"type": "time-series", "unit": "kW", "resolution": "PT1H",
				"index":  ".outputs.hourly[*].time",
				"values": map[string]any{"path": ".outputs.hourly[*].P", "scale": 0.001},
				"source": ".inputs.meteo_data.radiation_db",
			}},
		// Target composition: resolved by forwarding the resolvent's
		// "payload" field through the buem-building target and
		// extracting the load-profile timeseries from the response.
		"resolvent-buem": {Target: "buem-building", PayloadField: "payload",
			ResponsePath: "buem.thermal_load_profile.timeseries", CacheTTL: dur(time.Hour)},
	}
}

// startStack opens the store, runs the worker pool and serves the API,
// tearing everything down at test cleanup.
func (h *harness) startStack(cfg *config.Config) {
	st, err := store.Open(filepath.Join(h.t.TempDir(), "golden.db"))
	if err != nil {
		h.t.Fatal(err)
	}
	h.st = st
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	nudge := make(chan struct{}, 1)
	pool := worker.New(cfg, st, upstream.New(cfg.Upstream.MaxResponseBytes, cfg.UpstreamSecrets()), logger, nudge)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		pool.Run(ctx)
	}()
	h.api = httptest.NewServer(api.New(cfg, st, logger, nudge).Handler())
	h.t.Cleanup(func() {
		h.api.Close()
		cancel()
		<-done
		_ = st.Close()
	})
}

func (h *harness) noteUnexpected(r *http.Request) {
	h.mu.Lock()
	h.unexpected = append(h.unexpected, r.Method+" "+r.URL.RequestURI())
	h.mu.Unlock()
}

// --- recording -------------------------------------------------------------

func (h *harness) record(step map[string]any) {
	h.steps = append(h.steps, step)
}

// decodeAny decodes JSON preserving number fidelity (json.Number re-marshals
// verbatim), so goldens show exactly the bytes on the wire. Non-JSON bodies
// are recorded as plain strings.
func decodeAny(b []byte) any {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return string(b)
	}
	return v
}

// post sends a request body to POST /v1/requests and records the response.
// The returned job id (when accepted) feeds the «job-N» normalization.
func (h *harness) post(label, body string, hdr map[string]string) string {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.api.URL+"/v1/requests", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	h.record(map[string]any{
		"step": label, "request": "POST /v1/requests",
		"status": resp.StatusCode, "response": decodeAny(respBody),
	})
	var accepted struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(respBody, &accepted)
	if accepted.ID != "" && !slices.Contains(h.ids, accepted.ID) {
		h.ids = append(h.ids, accepted.ID)
	}
	return accepted.ID
}

// validate sends a create-shaped body to the dry-run endpoint and records
// the response.
func (h *harness) validate(label, body string, hdr map[string]string) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.api.URL+"/v1/requests/validate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	h.record(map[string]any{
		"step": label, "request": "POST /v1/requests/validate",
		"status": resp.StatusCode, "response": decodeAny(respBody),
	})
}

// postRaw sends an arbitrary request (custom headers/content type) and
// records the response — for the validation-error scenarios.
func (h *harness) postRaw(label, body, contentType string, hdr map[string]string) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.api.URL+"/v1/requests", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	h.record(map[string]any{
		"step": label, "request": "POST /v1/requests",
		"status": resp.StatusCode, "response": decodeAny(respBody),
	})
}

// await polls the status endpoint until the job is terminal and records the
// final response with volatile timestamps scrubbed.
func (h *harness) await(label, id string) {
	h.t.Helper()
	h.awaitState(label, id, store.StateCompleted, store.StateFailed)
}

// awaitState polls until the job reaches one of the wanted states and records
// the response — usable mid-flight (e.g. awaiting_target) as well as for
// terminal states. It polls with the admin key: reads are scoped per client
// and a scenario may await another client's job. The recorded response does
// not depend on the key.
func (h *harness) awaitState(label, id string, want ...string) {
	h.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		status, body := h.getOnce("/v1/requests/"+id, map[string]string{"X-API-Key": adminKey})
		var doc struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(body, &doc)
		for _, w := range want {
			if doc.State == w {
				h.record(map[string]any{
					"step": label, "request": "GET /v1/requests/{id}",
					"status": status, "response": scrubTimes(decodeAny(body)),
				})
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("job %s never reached %v", id, want)
}

// get performs a recorded GET (status endpoint mid-flight, result endpoint,
// list endpoint, ...). Binary bodies land in the golden as escaped strings.
func (h *harness) get(label, path string, hdr map[string]string) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.api.URL+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	h.record(map[string]any{
		"step": label, "request": "GET " + normalizePath(path, h.ids),
		"status": resp.StatusCode, "content_type": resp.Header.Get("Content-Type"),
		"response": scrubTimes(decodeAny(body)),
	})
}

// getJSON is get for steps whose response the scenario needs to read back
// (a page cursor, say); it records the step like get and returns the
// decoded body.
func (h *harness) getJSON(label, path string, hdr map[string]string) map[string]any {
	h.t.Helper()
	status, body := h.getOnce(path, hdr)
	decoded := decodeAny(body)
	h.record(map[string]any{
		"step": label, "request": "GET " + normalizePath(path, h.ids),
		"status": status, "response": scrubTimes(decoded),
	})
	doc, _ := decoded.(map[string]any)
	return doc
}

func (h *harness) getOnce(path string, hdr map[string]string) (int, []byte) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.api.URL+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// forwarded records the payload and auth header one target endpoint actually
// received — the heart of most goldens: it proves substitution, pass-through,
// and both key-injection modes. endpoint is "direct" (buem, header key) or
// "accept" (meme, body-field key). The capture is cleared after recording so
// a later step can only show bytes produced since this one.
func (h *harness) forwarded(label, endpoint string) {
	h.mu.Lock()
	body, hit := h.lastForwarded[endpoint]
	auth := h.lastTargetAuth[endpoint]
	path := h.lastTargetPath[endpoint]
	delete(h.lastForwarded, endpoint)
	delete(h.lastTargetAuth, endpoint)
	delete(h.lastTargetPath, endpoint)
	h.mu.Unlock()

	var received any = "«target never called»"
	if hit {
		received = decodeAny(body)
	}
	h.record(map[string]any{
		"step": label, "endpoint": endpoint, "target_path": path,
		"target_auth_header": auth, "target_received": received,
	})
}

// resourceReceived records the resolvent object and auth header the resource
// API saw. Only meaningful in scenarios with exactly one unique resolvent —
// concurrent fetches would make "the last request" nondeterministic.
func (h *harness) resourceReceived(label string) {
	h.mu.Lock()
	body := h.lastResourceBody
	auth := h.lastResourceAuth
	h.mu.Unlock()
	var received any = "«resource never called»"
	if body != nil {
		received = decodeAny(body)
	}
	h.record(map[string]any{
		"step": label, "resource_auth_header": auth, "resource_received": received,
	})
}

// events records the job's audit trail.
func (h *harness) events(label, id string) {
	h.t.Helper()
	events, err := h.st.ListEvents(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	lines := make([]string, 0, len(events))
	for _, e := range events {
		from := e.FromState
		if from == "" {
			from = "∅"
		}
		lines = append(lines, fmt.Sprintf("%s → %s: %s", from, e.ToState, e.Detail))
	}
	h.record(map[string]any{"step": label, "audit_trail": lines})
}

// counts records how often each upstream endpoint was hit, plus the tripwire
// list of unscripted requests (frozen empty in every golden). The status and
// result counts are load-dependent (overlapping poll ticks) and deliberately
// not asserted here.
func (h *harness) counts() {
	h.record(h.countsStep(false))
}

// countsWithPolls additionally asserts the numeric status/result counts.
// Only valid in scenarios where the job structurally never enters
// awaiting_target — there zero polls is guaranteed, whereas elsewhere the
// counts depend on timing.
func (h *harness) countsWithPolls() {
	h.record(h.countsStep(true))
}

func (h *harness) countsStep(withPolls bool) map[string]any {
	h.mu.Lock()
	unexpected := append([]string{}, h.unexpected...)
	h.mu.Unlock()
	step := map[string]any{
		"step":                         "upstream call counts",
		"resource_calls":               h.resourceCalls.Load(),
		"demo_calls":                   h.demoCalls.Load(),
		"meme_accept_calls":            h.acceptCalls.Load(),
		"buem_calls":                   h.gatewayCalls.Load(),
		"buem_building_calls":          h.buildingCalls.Load(),
		"ignis_calculate_calls":        h.calculateCalls.Load(),
		"unexpected_upstream_requests": unexpected,
	}
	if withPolls {
		step["meme_status_calls"] = h.statusCalls.Load()
		step["meme_result_calls"] = h.resultCalls.Load()
	} else {
		step["meme_status_calls"] = "not asserted (timing-dependent)"
		step["meme_result_calls"] = "not asserted (timing-dependent)"
	}
	return step
}

// resourceRequests records the exact GET request line each resource path
// received (sorted by path) — the frozen proof of the query-parameter and
// path-template mapping.
func (h *harness) resourceRequests(label string) {
	h.mu.Lock()
	lines := make([]string, 0, len(h.resourceGETs))
	for _, path := range slices.Sorted(maps.Keys(h.resourceGETs)) {
		lines = append(lines, h.resourceGETs[path])
	}
	h.mu.Unlock()
	h.record(map[string]any{"step": label, "resource_get_requests": lines})
}

// --- normalization and comparison ------------------------------------------

// scrubTimes masks the volatile timestamps of the job-response envelope: the
// top-level object and each element of an "items" list. Deliberately shallow —
// upstream-controlled bytes (result payloads, target_received) must stay
// frozen verbatim so real drift is never hidden behind a «ts» placeholder.
func scrubTimes(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	scrub := func(obj map[string]any) {
		for _, k := range []string{"created_at", "updated_at", "completed_at"} {
			if _, present := obj[k]; present {
				obj[k] = "«ts»"
			}
		}
	}
	scrub(m)
	if items, ok := m["items"].([]any); ok {
		for _, it := range items {
			if obj, ok := it.(map[string]any); ok {
				scrub(obj)
			}
		}
	}
	return m
}

func normalizePath(path string, ids []string) string {
	for i, id := range ids {
		path = strings.ReplaceAll(path, id, fmt.Sprintf("«job-%d»", i+1))
	}
	return path
}

var (
	// Anchored multi-unit pattern: compound Go durations ("1m30s") must
	// normalize whole, or the jittered residue would flake the golden.
	backoffPattern  = regexp.MustCompile(`retrying in (?:[0-9]+(?:\.[0-9]+)?(?:ns|µs|us|ms|s|m|h))+`)
	addrPattern     = regexp.MustCompile(`http://127\.0\.0\.1:[0-9]+`)
	bareAddrPattern = regexp.MustCompile(`127\.0\.0\.1:[0-9]+`)
	// Page cursors encode creation timestamps and are volatile by nature.
	cursorParam = regexp.MustCompile(`cursor=[A-Za-z0-9_-]+`)
	cursorField = regexp.MustCompile(`"next_cursor": "[A-Za-z0-9_-]+"`)
)

// normalize replaces run-specific values (job ids, jittered backoff
// durations, ephemeral upstream ports) with stable placeholders so goldens
// compare byte-for-byte.
func (h *harness) normalize(s string) string {
	for i, id := range h.ids {
		s = strings.ReplaceAll(s, id, fmt.Sprintf("«job-%d»", i+1))
	}
	s = addrPattern.ReplaceAllString(s, "«upstream»")
	s = bareAddrPattern.ReplaceAllString(s, "«addr»")
	s = cursorParam.ReplaceAllString(s, "cursor=«cursor»")
	s = cursorField.ReplaceAllString(s, `"next_cursor": "«cursor»"`)
	return backoffPattern.ReplaceAllString(s, "retrying in «dur»")
}

// golden compares the recorded transcript against testdata/golden/<name>,
// or rewrites it under -update.
func (h *harness) golden(name string) {
	h.t.Helper()
	raw, err := json.MarshalIndent(h.steps, "", "  ")
	if err != nil {
		h.t.Fatal(err)
	}
	got := h.normalize(string(raw)) + "\n"

	path := filepath.Join("testdata", "golden", name+".golden.json")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			h.t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatalf("missing golden file %s (run `make golden-update`): %v", path, err)
	}
	if got != string(want) {
		h.t.Errorf("transcript deviates from %s\n--- got ---\n%s\n--- want ---\n%s\n"+
			"(accept an intended change with `make golden-update`)", path, got, want)
	}
}

// scenario is one golden end-to-end case.
type scenario struct {
	name  string
	fakes fakes
	mod   func(*config.Config)
	run   func(t *testing.T, h *harness)
}

func runScenarios(t *testing.T, scenarios []scenario) {
	t.Helper()
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			h := newHarness(t, sc.fakes, sc.mod)
			sc.run(t, h)
			h.golden(sc.name)
		})
	}
}

// allScenarios concatenates the package's static scenario slices; the corpus
// gate (TestGoldenCorpusMatchesScenarios) checks golden files against it.
func allScenarios() []scenario {
	var all []scenario
	all = append(all, exampleScenarios...)
	all = append(all, resolutionScenarios...)
	all = append(all, containerIsResolventScenarios...)
	all = append(all, targetProtocolScenarios...)
	all = append(all, apiContractScenarios...)
	all = append(all, schedulingScenarios...)
	all = append(all, edgeScenarios...)
	return all
}

// requestBody builds a minimal valid request inline.
func requestBody(target, payload string) string {
	return fmt.Sprintf(`{"api_key":%q,"target":%q,"payload":%s}`, clientKey, target, payload)
}

func boolPtr(b bool) *bool { return &b }

// exampleRequest loads a file from examples/ and swaps the placeholder
// api_key for the harness client key — number-safe via RawMessage, so the
// example bytes otherwise reach the API verbatim.
func exampleRequest(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", file))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("examples/%s is not a JSON object: %v", file, err)
	}
	key, _ := json.Marshal(clientKey)
	doc["api_key"] = key
	patched, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(patched)
}

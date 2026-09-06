package metrics

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

type fakeStates struct {
	counts map[string]int
	err    error
}

func (f fakeStates) CountByState(context.Context) (map[string]int, error) { return f.counts, f.err }

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("scrape status %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

func TestMetricsAreExposedWithDocumentedNames(t *testing.T) {
	m := New(fakeStates{counts: map[string]int{"received": 3, "awaiting_target": 1}})
	m.JobFinished("demo", "completed", "")
	m.JobFinished("demo", "failed", "target_error")
	m.CacheLookup(true)
	m.CacheLookup(false)
	m.CacheLookup(false)
	m.ObserveUpstream("target", "meme", 503, errors.New("HTTP 503"), 250*time.Millisecond)
	m.ObserveUpstream("resource", "resolvent-pv1", 200, nil, 20*time.Millisecond)
	m.ObserveUpstream("poll", "meme", 0, errors.New("dial tcp: refused"), time.Second)
	m.SetBuildInfo("v0.3.0-alpha", "abc123", "go1.26")
	m.LongPollStarted()
	m.LongPollStarted()
	m.LongPollEnded()

	out := scrape(t, m)
	for _, want := range []string{
		`tentacron_jobs_total{outcome="completed",target="demo"} 1`,
		`tentacron_jobs_total{outcome="failed",target="demo"} 1`,
		`tentacron_job_failures_total{code="target_error",target="demo"} 1`,
		`tentacron_series_cache_lookups_total{result="hit"} 1`,
		`tentacron_series_cache_lookups_total{result="miss"} 2`,
		`tentacron_upstream_requests_total{class="5xx",kind="target",name="meme"} 1`,
		`tentacron_upstream_requests_total{class="2xx",kind="resource",name="resolvent-pv1"} 1`,
		`tentacron_upstream_requests_total{class="error",kind="poll",name="meme"} 1`,
		`tentacron_upstream_request_duration_seconds_count{kind="target",name="meme"} 1`,
		`tentacron_jobs_in_state{state="received"} 3`,
		`tentacron_jobs_in_state{state="awaiting_target"} 1`,
		`tentacron_long_polls 1`,
		`tentacron_build_info{go="go1.26",revision="abc123",version="v0.3.0-alpha"} 1`,
		`go_goroutines`,
		`process_start_time_seconds`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scrape lacks %q", want)
		}
	}
}

func TestStateCollectorCountsErrorsInsteadOfFailingTheScrape(t *testing.T) {
	m := New(fakeStates{err: errors.New("db closed")})
	out := scrape(t, m)
	if strings.Contains(out, "tentacron_jobs_in_state{") {
		t.Error("no state gauges must be emitted when the store cannot be read")
	}
	// Collectors are gathered concurrently, so the increment made during a
	// scrape shows up on the next one.
	out = scrape(t, m)
	if !regexp.MustCompile(`tentacron_metrics_scrape_errors_total [12]\n`).MatchString(out) {
		t.Errorf("scrape errors must be counted:\n%s", out)
	}
}

func TestNilMetricsAreANoOp(t *testing.T) {
	var m *Metrics
	m.JobFinished("demo", "completed", "")
	m.CacheLookup(true)
	m.ObserveUpstream("target", "demo", 200, nil, time.Millisecond)
}

func TestStatusClass(t *testing.T) {
	cases := map[string]string{
		statusClass(200, nil): "2xx", statusClass(302, nil): "3xx", statusClass(404, errors.New("x")): "4xx",
		statusClass(500, nil): "5xx", statusClass(0, errors.New("dial")): "error", statusClass(0, nil): "unknown",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("statusClass = %q, want %q", got, want)
		}
	}
}

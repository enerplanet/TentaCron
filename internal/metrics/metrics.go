// Package metrics defines tentacron's Prometheus metrics: job outcomes,
// upstream call latency and status classes, series-cache lookups, and a
// collector that reports queue depth per job state at scrape time. A nil
// *Metrics is a valid no-op receiver, so packages instrumented with it need
// no wiring in tests.
package metrics

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// StateCounter reports how many jobs sit in each state; the store implements it.
type StateCounter interface {
	CountByState(ctx context.Context) (map[string]int, error)
}

// Metrics owns a private registry with tentacron's collectors plus the Go
// runtime and process collectors.
type Metrics struct {
	registry         *prometheus.Registry
	jobsTotal        *prometheus.CounterVec
	failuresTotal    *prometheus.CounterVec
	upstreamDuration *prometheus.HistogramVec
	upstreamTotal    *prometheus.CounterVec
	cacheLookups     *prometheus.CounterVec
	scheduleRuns     *prometheus.CounterVec
	callbacks        *prometheus.CounterVec
	scrapeErrors     prometheus.Counter
}

// New builds the registry. states may be nil, in which case queue depth is
// not reported.
func New(states StateCounter) *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		jobsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tentacron_jobs_total", Help: "Jobs that reached a terminal state, by target and outcome (completed or failed).",
		}, []string{"target", "outcome"}),
		failuresTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tentacron_job_failures_total", Help: "Failed jobs by target and error code.",
		}, []string{"target", "code"}),
		upstreamDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "tentacron_upstream_request_duration_seconds", Help: "Outbound call latency by kind (resource, target, poll, result) and name.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}, []string{"kind", "name"}),
		upstreamTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tentacron_upstream_requests_total", Help: "Outbound calls by kind, name and status class (2xx, 3xx, 4xx, 5xx, error).",
		}, []string{"kind", "name", "class"}),
		cacheLookups: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tentacron_series_cache_lookups_total", Help: "Series cache lookups by result (hit or miss).",
		}, []string{"result"}),
		scheduleRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tentacron_schedule_runs_total", Help: "Runs materialised from schedules, by target.",
		}, []string{"target"}),
		callbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tentacron_callback_deliveries_total", Help: "Completion-callback attempts by outcome (delivered, retry, failed).",
		}, []string{"outcome"}),
		scrapeErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tentacron_metrics_scrape_errors_total", Help: "Scrapes on which the queue depth could not be read from the store.",
		}),
	}
	m.registry.MustRegister(m.jobsTotal, m.failuresTotal, m.upstreamDuration, m.upstreamTotal, m.cacheLookups, m.scheduleRuns, m.callbacks, m.scrapeErrors,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	if states != nil {
		m.registry.MustRegister(&stateCollector{states: states, errors: m.scrapeErrors})
	}
	return m
}

// Handler serves the registry in the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// JobFinished records a terminal outcome; code is the job error code for
// failures and empty for completions.
func (m *Metrics) JobFinished(target, outcome, code string) {
	if m == nil {
		return
	}
	m.jobsTotal.WithLabelValues(target, outcome).Inc()
	if code != "" {
		m.failuresTotal.WithLabelValues(target, code).Inc()
	}
}

// ScheduleRun records a run materialised from a schedule.
func (m *Metrics) ScheduleRun(target string) {
	if m == nil {
		return
	}
	m.scheduleRuns.WithLabelValues(target).Inc()
}

// CallbackDelivery records one delivery attempt's outcome.
func (m *Metrics) CallbackDelivery(outcome string) {
	if m == nil {
		return
	}
	m.callbacks.WithLabelValues(outcome).Inc()
}

// CacheLookup records a series-cache hit or miss.
func (m *Metrics) CacheLookup(hit bool) {
	if m == nil {
		return
	}
	result := "miss"
	if hit {
		result = "hit"
	}
	m.cacheLookups.WithLabelValues(result).Inc()
}

// ObserveUpstream records one outbound call: its latency and status class.
// status 0 with a non-nil err is a transport-level failure ("error").
func (m *Metrics) ObserveUpstream(kind, name string, status int, err error, d time.Duration) {
	if m == nil {
		return
	}
	m.upstreamDuration.WithLabelValues(kind, name).Observe(d.Seconds())
	m.upstreamTotal.WithLabelValues(kind, name, statusClass(status, err)).Inc()
}

func statusClass(status int, err error) string {
	if status < 100 {
		if err != nil {
			return "error"
		}
		return "unknown"
	}
	return strconv.Itoa(status/100) + "xx"
}

// stateCollector reports the current queue depth per state on every scrape.
type stateCollector struct {
	states StateCounter
	errors prometheus.Counter
}

var stateDesc = prometheus.NewDesc("tentacron_jobs_in_state", "Jobs currently in each state.", []string{"state"}, nil)

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) { ch <- stateDesc }

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	counts, err := c.states.CountByState(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		c.errors.Inc()
		return
	}
	for state, n := range counts {
		ch <- prometheus.MustNewConstMetric(stateDesc, prometheus.GaugeValue, float64(n), state)
	}
}

// Package worker drives job processing: claiming queued jobs, resolving
// resolvent objects via resource APIs, forwarding payloads to targets,
// polling async targets to completion, and housekeeping sweeps.
package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/metrics"
	"github.com/enerplanet/tentacron/internal/notify"
	"github.com/enerplanet/tentacron/internal/store"
	"github.com/enerplanet/tentacron/internal/upstream"
)

// Pool runs the worker goroutines and the housekeeping sweeper.
type Pool struct {
	cfgp     *config.Provider
	store    *store.Store
	client   *upstream.Client
	logger   *slog.Logger
	nudge    <-chan struct{}
	metrics  *metrics.Metrics // nil-safe: a nil receiver records nothing
	notifier *notify.Hub      // nil-safe: wakes long-polling reads on terminal transitions
	clock    Clock            // the scheduler's notion of now; injectable for tests
}

// WithClock replaces the wall clock the scheduler reads.
func (p *Pool) WithClock(c Clock) *Pool {
	p.clock = c
	return p
}

// WithNotifier wakes long-polling API reads when a job ends.
func (p *Pool) WithNotifier(h *notify.Hub) *Pool {
	p.notifier = h
	return p
}

// New builds a Pool. nudge wakes an idle worker when the API accepts a job.
func New(cfg *config.Provider, st *store.Store, client *upstream.Client, logger *slog.Logger, nudge <-chan struct{}) *Pool {
	return &Pool{cfgp: cfg, store: st, client: client, logger: logger, nudge: nudge, clock: realClock{}}
}

// config is the configuration current right now; loops read it per tick.
// The worker count and poll cadence are read once at Run, so changing them
// takes a restart.
func (p *Pool) config() *config.Config { return p.cfgp.Current() }

// run processes one claimed job under the configuration current at claim
// time, so a reload never changes a job's targets, resolvents or limits
// midway.
type run struct {
	*Pool
	cfg *config.Config
}

// WithMetrics records job outcomes and series-cache lookups.
func (p *Pool) WithMetrics(m *metrics.Metrics) *Pool {
	p.metrics = m
	return p
}

// Run recovers interrupted jobs, then processes work until ctx is cancelled
// and all workers have drained. It blocks for the pool's lifetime.
func (p *Pool) Run(ctx context.Context) {
	if n, err := p.store.RecoverInFlight(ctx); err != nil {
		p.logger.Error("startup recovery failed", "error", err)
	} else if n > 0 {
		p.logger.Info("recovered in-flight jobs after restart", "count", n)
	}

	var wg sync.WaitGroup
	for i := 0; i < p.config().Worker.Count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.workerLoop(ctx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.sweeperLoop(ctx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.schedulerLoop(ctx)
	}()
	wg.Wait()
}

func (p *Pool) workerLoop(ctx context.Context) {
	ticker := time.NewTicker(p.config().Worker.PollInterval.Std())
	defer ticker.Stop()
	for {
		p.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-p.nudge:
		case <-ticker.C:
		}
	}
}

// drain claims and processes eligible jobs until the queue is empty.
func (p *Pool) drain(ctx context.Context) {
	for ctx.Err() == nil {
		job, err := p.store.ClaimNext(ctx, p.claimPolicy())
		if err != nil {
			if ctx.Err() == nil {
				p.logger.Error("claim failed", "error", err)
			}
			return
		}
		if job == nil {
			return
		}
		(&run{Pool: p, cfg: p.config()}).process(ctx, job)
	}
}

// claimPolicy hands the store the poll cadence and, only when a key sets a
// ceiling, the per-client in-flight limit — so a deployment without
// ceilings never pays for the in-flight count on every claim.
func (p *Pool) claimPolicy() store.ClaimPolicy {
	cfg := p.config()
	policy := store.ClaimPolicy{PollInterval: p.pollInterval}
	if cfg.HasConcurrencyCeilings() {
		policy.MaxConcurrent = cfg.MaxConcurrentFor
	}
	return policy
}

// pollInterval supplies the per-target poll cadence used by the store when it
// claims a poll tick (pushing next_attempt_at forward). The Worker.PollInterval
// fallback only applies when an awaiting_target job's target lost its poll
// config; it keeps the job claimable so processPoll can fail it promptly.
func (p *Pool) pollInterval(target string) time.Duration {
	if t, ok := p.config().Targets[target]; ok && t.Response.Mode == config.ModePoll && t.Response.Poll != nil {
		return t.Response.Poll.Interval.Std()
	}
	return p.config().Worker.PollInterval.Std()
}

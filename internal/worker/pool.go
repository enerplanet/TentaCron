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
	"github.com/enerplanet/tentacron/internal/store"
	"github.com/enerplanet/tentacron/internal/upstream"
)

// Pool runs the worker goroutines and the housekeeping sweeper.
type Pool struct {
	cfg    *config.Config
	store  *store.Store
	client *upstream.Client
	logger *slog.Logger
	nudge  <-chan struct{}
}

// New builds a Pool. nudge wakes an idle worker when the API accepts a job.
func New(cfg *config.Config, st *store.Store, client *upstream.Client, logger *slog.Logger, nudge <-chan struct{}) *Pool {
	return &Pool{cfg: cfg, store: st, client: client, logger: logger, nudge: nudge}
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
	for i := 0; i < p.cfg.Worker.Count; i++ {
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
	wg.Wait()
}

func (p *Pool) workerLoop(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.Worker.PollInterval.Std())
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
		job, err := p.store.ClaimNext(ctx, p.pollInterval)
		if err != nil {
			if ctx.Err() == nil {
				p.logger.Error("claim failed", "error", err)
			}
			return
		}
		if job == nil {
			return
		}
		p.process(ctx, job)
	}
}

// pollInterval supplies the per-target poll cadence used by the store when it
// claims a poll tick (pushing next_attempt_at forward).
func (p *Pool) pollInterval(target string) time.Duration {
	if t, ok := p.cfg.Targets[target]; ok && t.Response.Mode == config.ModePoll && t.Response.Poll != nil {
		return t.Response.Poll.Interval.Std()
	}
	return p.cfg.Worker.PollInterval.Std()
}

package worker

import (
	"context"
	"time"

	"github.com/enerplanet/tentacron/internal/schedule"
	"github.com/enerplanet/tentacron/internal/store"
)

// Clock is the scheduler's source of time, so tests can move it without
// sleeping.
type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// dueBatch bounds how many schedules one tick materialises.
const dueBatch = 100

// defaultSchedulerInterval applies when a hand-built config (tests,
// embedders) never went through config.Load's defaults.
const defaultSchedulerInterval = 30 * time.Second

// schedulerLoop materialises due schedules into jobs every
// worker.scheduler_interval until ctx ends.
func (p *Pool) schedulerLoop(ctx context.Context) {
	interval := p.cfg.Worker.SchedulerInterval.Std()
	if interval <= 0 {
		interval = defaultSchedulerInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.runDueSchedules(ctx)
		}
	}
}

// runDueSchedules turns every due schedule into one job for its due time.
// The job's idempotency key names the due time, so a second scheduler (a
// restart racing the old process, or two ticks) replays the same job; and
// the schedule advances with a compare-and-set, so exactly one of them moves
// it on. Missed due times during downtime collapse into one run: the next
// due time is computed from now, not from the missed one.
func (p *Pool) runDueSchedules(ctx context.Context) {
	now := p.clock.Now()
	due, err := p.store.DueSchedules(ctx, now, dueBatch)
	if err != nil {
		if ctx.Err() == nil {
			p.logger.Error("list due schedules failed", "error", err)
		}
		return
	}
	for _, sc := range due {
		if ctx.Err() != nil {
			return
		}
		p.materialize(ctx, sc, now)
	}
}

// materialize creates the run of sc that is due and advances the schedule.
func (p *Pool) materialize(ctx context.Context, sc *store.Schedule, now time.Time) {
	spec, err := schedule.Parse(sc.Cron, sc.Timezone)
	if err != nil {
		// Validated at creation; only a config-free library change could
		// get here. Skipping keeps the schedule visible for the operator.
		p.logger.Error("schedule has an unparsable cron expression", "schedule_id", sc.ID, "error", err)
		return
	}
	due := *sc.NextRunAt
	id, err := store.NewID()
	if err != nil {
		p.logger.Error("id generation failed", "error", err)
		return
	}
	options := sc.Options
	if options.Cache == "" {
		// A recurring run exists to pick up fresh inputs.
		options.Cache = store.CacheRefresh
	}
	job := &store.Job{
		ID: id, Client: sc.Client, IdempotencyKey: store.RunKey(sc.ID, due),
		Target: sc.Target, MaxAttempts: p.cfg.MaxAttemptsFor(sc.Target),
		Payload: sc.Payload, Priority: sc.Priority, Options: options,
	}
	created, stored, err := p.store.CreateJob(ctx, job)
	if err != nil {
		p.logger.Error("materialise schedule failed", "schedule_id", sc.ID, "error", err)
		return
	}
	next := spec.Next(now)
	if next.Before(due) || next.Equal(due) {
		next = spec.Next(due)
	}
	advanced, err := p.store.AdvanceSchedule(ctx, sc.ID, due, stored.ID, next)
	if err != nil {
		p.logger.Error("advance schedule failed", "schedule_id", sc.ID, "error", err)
		return
	}
	if created && advanced {
		p.metrics.ScheduleRun(sc.Target)
		p.logger.Info("schedule run created", "schedule_id", sc.ID, "job_id", stored.ID, "target", sc.Target, "due", due.UTC().Format(time.RFC3339), "next", next.Format(time.RFC3339))
	}
}

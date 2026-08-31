package worker

import (
	"context"
	"os"
	"time"
)

// sweeperLoop periodically purges expired series-cache rows, rescues jobs
// stranded by failed bookkeeping writes, and prunes terminal jobs (plus their
// result files) past the retention window.
func (p *Pool) sweeperLoop(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.Cache.CleanupInterval.Std())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.sweep(ctx)
		}
	}
}

func (p *Pool) sweep(ctx context.Context) {
	if n, err := p.store.PurgeExpiredSeries(ctx); err != nil {
		p.logger.Error("series cache purge failed", "error", err)
	} else if n > 0 {
		p.logger.Info("purged expired cached series", "count", n)
	}

	// A failed bookkeeping write leaves a job in resolving/forwarding with no
	// schedule — unclaimable forever. Anything untouched for well over a full
	// processing attempt cannot still be in flight; requeue it.
	stuckCutoff := time.Now().Add(-2 * p.cfg.Worker.JobTimeout.Std())
	if n, err := p.store.RescueStuck(ctx, stuckCutoff); err != nil {
		p.logger.Error("stuck job rescue failed", "error", err)
	} else if n > 0 {
		p.logger.Warn("rescued stuck jobs", "count", n)
	}

	// Retention: remove result files before their rows — a crash in between
	// leaves rows for the next sweep to retry, never orphaned files.
	cutoff := time.Now().Add(-p.cfg.Storage.Retention.Std())
	ids, paths, err := p.store.TerminalBefore(ctx, cutoff)
	if err != nil {
		p.logger.Error("job retention scan failed", "error", err)
		return
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			p.logger.Warn("could not delete pruned result file", "path", path, "error", err)
		}
	}
	if err := p.store.DeleteJobs(ctx, ids); err != nil {
		p.logger.Error("job retention prune failed", "error", err)
		return
	}
	if len(ids) > 0 {
		p.logger.Info("pruned terminal jobs past retention", "jobs", len(ids), "result_files", len(paths))
	}
}

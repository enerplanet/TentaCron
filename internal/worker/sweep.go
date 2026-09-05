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
	ticker := time.NewTicker(p.config().Cache.CleanupInterval.Std())
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
	p.purgeExpiredSeries(ctx)
	p.rescueStuckJobs(ctx)
	p.pruneRetention(ctx)
	p.compact(ctx)
}

// compact hands pages freed by the purges above back to the filesystem and
// checkpoints the WAL, so the database file tracks the live data instead of
// its historical peak.
func (p *Pool) compact(ctx context.Context) {
	if err := p.store.Vacuum(ctx); err != nil {
		p.logger.Error("database compaction failed", "error", err)
	}
}

func (p *Pool) purgeExpiredSeries(ctx context.Context) {
	n, err := p.store.PurgeExpiredSeries(ctx)
	if err != nil {
		p.logger.Error("series cache purge failed", "error", err)
		return
	}
	if n > 0 {
		p.logger.Info("purged expired cached series", "count", n)
	}
}

// rescueStuckJobs requeues jobs a failed bookkeeping write left in
// resolving/forwarding with no schedule — unclaimable forever otherwise.
// Anything untouched for well over a full processing attempt cannot still
// be in flight.
func (p *Pool) rescueStuckJobs(ctx context.Context) {
	cutoff := time.Now().Add(-2 * p.config().Worker.JobTimeout.Std())
	n, err := p.store.RescueStuck(ctx, cutoff)
	if err != nil {
		p.logger.Error("stuck job rescue failed", "error", err)
		return
	}
	if n > 0 {
		p.logger.Warn("rescued stuck jobs", "count", n)
	}
}

// pruneBatch bounds one retention pass. Files are removed and rows deleted
// per batch, so a large backlog never becomes one huge statement or a
// long-held write lock, and a crash between batches only leaves rows the
// next pass re-selects. A variable so tests can exercise multiple passes
// without thousands of rows.
var pruneBatch = 1000

// pruneRetention removes terminal jobs past the retention window, batch by
// batch until a pass comes back short. Result files go before their rows:
// a crash in between leaves rows for the next sweep to retry, never
// orphaned files.
func (p *Pool) pruneRetention(ctx context.Context) {
	cutoff := time.Now().Add(-p.config().Storage.Retention.Std())
	jobs, files := 0, 0
	for ctx.Err() == nil {
		ids, paths, err := p.store.TerminalBefore(ctx, cutoff, pruneBatch)
		if err != nil {
			p.logger.Error("job retention scan failed", "error", err)
			return
		}
		if len(ids) == 0 {
			break
		}
		p.removeResultFiles(paths)
		if err := p.store.DeleteJobs(ctx, ids); err != nil {
			p.logger.Error("job retention prune failed", "error", err, "pruned_so_far", jobs)
			return
		}
		jobs += len(ids)
		files += len(paths)
		if len(ids) < pruneBatch {
			break
		}
	}
	if jobs > 0 {
		p.logger.Info("pruned terminal jobs past retention", "jobs", jobs, "result_files", files)
	}
}

// removeResultFiles unlinks pruned result files; a file that is already
// gone is fine, anything else is logged and left for the operator.
func (p *Pool) removeResultFiles(paths []string) {
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			p.logger.Warn("could not delete pruned result file", "path", path, "error", err)
		}
	}
}

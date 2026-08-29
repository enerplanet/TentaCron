package worker

import (
	"context"
	"os"
	"time"
)

// sweeperLoop periodically purges expired series-cache rows and prunes
// terminal jobs (plus their result files) past the retention window.
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

	cutoff := time.Now().Add(-p.cfg.Storage.Retention.Std())
	paths, err := p.store.PruneTerminal(ctx, cutoff)
	if err != nil {
		p.logger.Error("job retention prune failed", "error", err)
		return
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			p.logger.Warn("could not delete pruned result file", "path", path, "error", err)
		}
	}
	if len(paths) > 0 {
		p.logger.Info("pruned terminal jobs past retention", "result_files", len(paths))
	}
}

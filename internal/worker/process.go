package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/resolver"
	"github.com/enerplanet/tentacron/internal/store"
	"github.com/enerplanet/tentacron/internal/upstream"
)

// Job failure codes (stored on the job, surfaced via the API).
const (
	errUnknownTarget    = "unknown_target"
	errInvalidPayload   = "invalid_payload"
	errUnknownResolvent = "unknown_resolvent"
	errInvalidResource  = "invalid_resource_response"
	errResourceError    = "resource_error"
	errTargetError      = "target_error"
	errTargetJobFailed  = "target_job_failed"
	errTargetTimeout    = "target_timeout"
	errMaxAttempts      = "max_attempts_exceeded"
	errInternal         = "internal"
)

// Results up to this size that are valid JSON stay inline in the database;
// anything bigger or binary goes to the results directory.
const inlineResultLimit = 256 << 10

func (p *Pool) process(ctx context.Context, job *store.Job) {
	// Bookkeeping writes use an uncancellable context so a shutdown or
	// deadline can never strand a job in a half-written state.
	bg := context.WithoutCancel(ctx)
	defer func() {
		if r := recover(); r != nil {
			p.logger.Error("panic while processing job", "job_id", job.ID, "panic", r)
			p.failJob(bg, job, errInternal, fmt.Sprintf("panic: %v", r))
		}
	}()

	actx, cancel := context.WithTimeout(ctx, p.cfg.Worker.JobTimeout.Std())
	defer cancel()

	switch job.State {
	case store.StateResolving:
		p.processNew(actx, bg, job)
	case store.StateAwaitingTarget:
		p.processPoll(actx, bg, job)
	default:
		p.logger.Error("claimed job in unexpected state", "job_id", job.ID, "state", job.State)
	}
}

// processNew runs the resolve → forward pipeline for a freshly claimed job.
func (p *Pool) processNew(ctx, bg context.Context, job *store.Job) {
	tcfg, ok := p.cfg.Targets[job.Target]
	if !ok {
		p.failJob(bg, job, errUnknownTarget, fmt.Sprintf("target %q is no longer configured", job.Target))
		return
	}

	root, err := resolver.Parse(job.Payload)
	if err != nil {
		p.failJob(bg, job, errInvalidPayload, err.Error())
		return
	}
	found, err := resolver.Find(root, tcfg.TimeseriesPath)
	if err != nil {
		p.failJob(bg, job, errInvalidPayload, err.Error())
		return
	}
	for _, typ := range resolver.DistinctTypes(found) {
		if _, ok := p.cfg.Resolvents[typ]; !ok {
			p.failJob(bg, job, errUnknownResolvent, fmt.Sprintf("no resolvent config for type %q", typ))
			return
		}
	}

	seriesByHash, cached, err := p.fetchAll(ctx, bg, found)
	if err != nil {
		p.retryOrFail(bg, job, errResourceError, err)
		return
	}
	for _, f := range found {
		warnings, err := f.Substitute(seriesByHash[f.Hash])
		if err != nil {
			p.failJob(bg, job, errInvalidResource, err.Error())
			return
		}
		for _, w := range warnings {
			p.logger.Warn("resolvent substitution warning", "job_id", job.ID, "warning", w)
		}
	}

	resolved, err := resolver.Marshal(root)
	if err != nil {
		p.failJob(bg, job, errInternal, "re-encode resolved payload: "+err.Error())
		return
	}
	detail := fmt.Sprintf("resolved %d resolvent(s), %d from cache", len(found), cached)
	if err := p.store.SetResolved(bg, job.ID, resolved, detail); err != nil {
		p.logger.Error("persist resolved payload failed", "job_id", job.ID, "error", err)
		return
	}
	p.logger.Info("payload resolved", "job_id", job.ID, "resolvents", len(found), "cached", cached)

	p.forward(ctx, bg, job, tcfg, resolved)
}

// fetchAll retrieves the time series for every unique resolvent hash, from
// cache when possible, otherwise from the resource APIs with bounded
// concurrency. The first error cancels the remaining calls.
func (p *Pool) fetchAll(ctx, bg context.Context, found []*resolver.Found) (map[string][]byte, int, error) {
	unique := map[string]*resolver.Found{}
	for _, f := range found {
		if _, ok := unique[f.Hash]; !ok {
			unique[f.Hash] = f
		}
	}

	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, p.cfg.Worker.ResolventConcurrency)
	type result struct {
		hash   string
		body   []byte
		cached bool
		err    error
	}
	results := make(chan result, len(unique))

	for hash, f := range unique {
		go func(hash string, f *resolver.Found) {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-fctx.Done():
				results <- result{hash: hash, err: fctx.Err()}
				return
			}
			body, cached, err := p.fetchOne(fctx, bg, f)
			results <- result{hash: hash, body: body, cached: cached, err: err}
		}(hash, f)
	}

	seriesByHash := make(map[string][]byte, len(unique))
	cachedCount := 0
	var firstErr error
	for range unique {
		r := <-results
		switch {
		case r.err != nil:
			if firstErr == nil {
				firstErr = r.err
				cancel()
			}
		default:
			seriesByHash[r.hash] = r.body
			if r.cached {
				cachedCount++
			}
		}
	}
	if firstErr != nil {
		return nil, 0, firstErr
	}
	return seriesByHash, cachedCount, nil
}

func (p *Pool) fetchOne(ctx, bg context.Context, f *resolver.Found) (body []byte, cached bool, err error) {
	if body, hit, err := p.store.GetSeries(ctx, f.Hash); err == nil && hit {
		return body, true, nil
	}
	rcfg := p.cfg.Resolvents[f.Type]
	objBytes, err := json.Marshal(f.Object)
	if err != nil {
		return nil, false, fmt.Errorf("encode resolvent %s: %w", f.Type, err)
	}
	body, err = p.client.ResolveResolvent(ctx, f.Type, rcfg, objBytes)
	if err != nil {
		return nil, false, err
	}
	var probe map[string]any
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, false, fmt.Errorf("resource %s returned a non-object response: %w", f.Type, err)
	}
	if err := p.store.PutSeries(bg, f.Hash, f.Type, body, rcfg.CacheTTL.Std()); err != nil {
		p.logger.Warn("series cache write failed", "type", f.Type, "error", err)
	}
	return body, false, nil
}

// forward sends the resolved payload to the target and either completes the
// job (direct mode) or parks it for polling (poll mode).
func (p *Pool) forward(ctx, bg context.Context, job *store.Job, tcfg config.Target, resolved []byte) {
	res, err := p.client.ForwardToTarget(ctx, job.Target, tcfg, resolved)
	if err != nil {
		p.retryOrFail(bg, job, errTargetError, err)
		return
	}

	if tcfg.Response.Mode == config.ModePoll {
		poll := tcfg.Response.Poll
		targetJobID, err := upstream.ExtractJobID(res.Body, poll)
		if err != nil {
			p.failJob(bg, job, errTargetError, err.Error())
			return
		}
		now := time.Now()
		if err := p.store.MarkAwaitingTarget(bg, job.ID, targetJobID, res.Status, res.Body,
			now.Add(poll.Interval.Std()), now.Add(poll.Timeout.Std())); err != nil {
			p.logger.Error("persist awaiting_target failed", "job_id", job.ID, "error", err)
			return
		}
		p.logger.Info("target accepted, polling", "job_id", job.ID, "target", job.Target, "target_job_id", targetJobID)
		return
	}

	p.complete(bg, job, res.Status, "", res.Body,
		fmt.Sprintf("target responded with status %d", res.Status))
}

// processPoll runs one poll tick for a job awaiting its target's result.
func (p *Pool) processPoll(ctx, bg context.Context, job *store.Job) {
	tcfg, ok := p.cfg.Targets[job.Target]
	if !ok || tcfg.Response.Mode != config.ModePoll || tcfg.Response.Poll == nil {
		p.failJob(bg, job, errUnknownTarget, fmt.Sprintf("target %q is no longer configured for polling", job.Target))
		return
	}
	if job.PollDeadline != nil && time.Now().After(*job.PollDeadline) {
		p.failJob(bg, job, errTargetTimeout, fmt.Sprintf(
			"target job %s did not finish before the poll deadline", job.TargetJobID))
		return
	}

	st, err := p.client.PollTarget(ctx, job.Target, tcfg, job.TargetJobID)
	if err != nil {
		if upstream.IsTransient(err) {
			// The next poll tick is already scheduled; just note the miss.
			p.logger.Warn("poll attempt failed", "job_id", job.ID, "error", err)
			return
		}
		p.failJob(bg, job, errTargetError, err.Error())
		return
	}

	switch {
	case st.Failed:
		p.failJob(bg, job, errTargetJobFailed, fmt.Sprintf(
			"target job %s reported status %q", job.TargetJobID, st.Raw))
	case st.Done:
		ct, body, err := p.client.FetchResult(ctx, job.Target, tcfg, job.TargetJobID)
		if err != nil {
			if upstream.IsTransient(err) {
				p.logger.Warn("result fetch failed, will retry next tick", "job_id", job.ID, "error", err)
				return
			}
			p.failJob(bg, job, errTargetError, err.Error())
			return
		}
		p.complete(bg, job, 200, ct, body, fmt.Sprintf(
			"target job %s finished with status %q", job.TargetJobID, st.Raw))
	default:
		p.logger.Debug("target job still running", "job_id", job.ID, "status", st.Raw)
	}
}

// complete stores the final result: small JSON inline, everything else as a
// file in the results directory.
func (p *Pool) complete(bg context.Context, job *store.Job, status int, contentType string, body []byte, detail string) {
	// Trust the bytes over the declared content type: upstream servers are
	// often sloppy about Content-Type on JSON responses.
	if json.Valid(body) {
		contentType = "application/json"
	} else if contentType == "" {
		contentType = "application/octet-stream"
	}
	if len(body) <= inlineResultLimit && json.Valid(body) {
		if err := p.store.MarkCompleted(bg, job.ID, status, body, "", "", detail); err != nil {
			p.logger.Error("mark completed failed", "job_id", job.ID, "error", err)
		}
		p.logger.Info("job completed", "job_id", job.ID, "target", job.Target)
		return
	}

	path := filepath.Join(p.cfg.Storage.ResultsDir, job.ID+resultExt(contentType))
	if err := os.MkdirAll(p.cfg.Storage.ResultsDir, 0o750); err != nil {
		p.failJob(bg, job, errInternal, "create results dir: "+err.Error())
		return
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		p.failJob(bg, job, errInternal, "write result file: "+err.Error())
		return
	}
	if err := p.store.MarkCompleted(bg, job.ID, status, nil, path, contentType, detail); err != nil {
		p.logger.Error("mark completed failed", "job_id", job.ID, "error", err)
		return
	}
	p.logger.Info("job completed with file result", "job_id", job.ID, "target", job.Target, "result_path", path)
}

func resultExt(contentType string) string {
	switch {
	case strings.Contains(contentType, "zip"):
		return ".zip"
	case strings.Contains(contentType, "json"):
		return ".json"
	default:
		return ".bin"
	}
}

// retryOrFail requeues after a transient error (with capped exponential
// backoff and jitter) and fails the job on permanent errors or exhausted
// attempts.
func (p *Pool) retryOrFail(bg context.Context, job *store.Job, permanentCode string, err error) {
	if !upstream.IsTransient(err) && !isContextErr(err) {
		p.failJob(bg, job, permanentCode, err.Error())
		return
	}
	if job.Attempts >= job.MaxAttempts {
		p.failJob(bg, job, errMaxAttempts, fmt.Sprintf(
			"gave up after %d attempts, last error: %v", job.Attempts, err))
		return
	}
	backoff := p.cfg.Worker.BackoffBase.Std() << (job.Attempts - 1)
	if maxB := p.cfg.Worker.BackoffMax.Std(); backoff > maxB || backoff <= 0 {
		backoff = maxB
	}
	// Full ±20% jitter avoids thundering-herd retries.
	backoff = time.Duration(float64(backoff) * (0.8 + 0.4*rand.Float64())) //nolint:gosec // G404: jitter is scheduling, not security
	detail := fmt.Sprintf("attempt %d/%d failed (%v), retrying in %s",
		job.Attempts, job.MaxAttempts, err, backoff.Round(time.Millisecond))
	if reqErr := p.store.Requeue(bg, job.ID, time.Now().Add(backoff), detail); reqErr != nil {
		p.logger.Error("requeue failed", "job_id", job.ID, "error", reqErr)
		return
	}
	p.logger.Warn("job requeued", "job_id", job.ID, "attempt", job.Attempts, "backoff", backoff, "error", err)
}

// isContextErr treats cancellation/deadline as transient: a shutdown or job
// timeout parks the job for a clean retry instead of failing it permanently.
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (p *Pool) failJob(bg context.Context, job *store.Job, code, message string) {
	if err := p.store.MarkFailed(bg, job.ID, code, message); err != nil {
		p.logger.Error("mark failed failed", "job_id", job.ID, "error", err)
		return
	}
	p.logger.Warn("job failed", "job_id", job.ID, "target", job.Target, "code", code, "message", message)
}

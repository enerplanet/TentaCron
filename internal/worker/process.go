package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
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

// errBadResourceBody marks a resource API response whose body is not a JSON
// object; processNew maps it to the invalid_resource_response job code
// instead of the HTTP-level resource_error.
var errBadResourceBody = errors.New("resource response is not a JSON object")

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
	// ClaimNext already moved "received" jobs to "resolving", so a freshly
	// claimed job arrives here in StateResolving, never StateReceived.
	case store.StateResolving:
		p.processNew(actx, bg, job)
	case store.StateAwaitingTarget:
		p.processPoll(actx, bg, job)
	default:
		p.logger.Error("claimed job in unexpected state", "job_id", job.ID, "state", job.State)
	}
}

// processNew runs the resolve → forward pipeline for a freshly claimed job.
// ctx bounds the job's upstream I/O (job timeout, shutdown); bg is the
// uncancellable bookkeeping context from process — every store write below
// uses bg so cancellation can never strand the job mid-transition.
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
	// The "." sentinel scans the whole payload (resolver treats "" as
	// root-scan; the config layer reserves "" for "use the default path").
	path := tcfg.TimeseriesPath
	if path == config.RootTimeseriesPath {
		path = ""
	}
	found, err := resolver.Find(root, path)
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
		if errors.Is(err, errBadResourceBody) {
			p.failJob(bg, job, errInvalidResource, err.Error())
			return
		}
		p.retryOrFail(bg, job, errResourceError, err)
		return
	}
	// nil means "not configured": config.Load defaults it to true, but a
	// hand-built config (tests, embedders) must get the same default.
	attach := tcfg.AttachResolvent == nil || *tcfg.AttachResolvent
	for _, f := range found {
		warnings, err := f.Substitute(seriesByHash[f.Hash], attach)
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
// cache when possible, otherwise from the resource APIs. A fixed worker set
// bounds goroutines — not just concurrent HTTP calls — so a payload with tens
// of thousands of resolvents cannot spawn a goroutine each. The first error
// cancels the remaining work.
func (p *Pool) fetchAll(ctx, bg context.Context, found []*resolver.Found) (map[string][]byte, int, error) {
	seen := map[string]bool{}
	unique := make([]*resolver.Found, 0, len(found))
	for _, f := range found {
		if !seen[f.Hash] {
			seen[f.Hash] = true
			unique = append(unique, f)
		}
	}

	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		hash   string
		body   []byte
		cached bool
		err    error
	}
	results := make(chan result, len(unique))

	feed := make(chan *resolver.Found)
	go func() {
		defer close(feed)
		for _, f := range unique {
			select {
			case feed <- f:
			case <-fctx.Done():
				results <- result{hash: f.Hash, err: fctx.Err()}
			}
		}
	}()
	for range min(p.cfg.Worker.ResolventConcurrency, len(unique)) {
		go func() {
			for f := range feed {
				body, cached, err := p.fetchOne(fctx, bg, f)
				results <- result{hash: f.Hash, body: body, cached: cached, err: err}
			}
		}()
	}

	// Collect exactly len(unique) results: each unique resolvent is either
	// handed to a worker (which always sends one result) or reported as
	// cancelled by the feeder, so this loop always drains and cannot deadlock.
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
	// A store error on the cache read is deliberately treated as a miss:
	// fall through and fetch fresh rather than fail the job over a cache
	// problem.
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
		return nil, false, fmt.Errorf("resource %s returned malformed JSON (%v): %w", f.Type, err, errBadResourceBody)
	}
	if probe == nil {
		// json.Unmarshal accepts "null" into a map without error; caching it
		// would poison every job sharing this resolvent for the TTL.
		return nil, false, fmt.Errorf("resource %s returned null instead of a time-series object: %w", f.Type, errBadResourceBody)
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
		p.parkForPolling(bg, job, tcfg.Response.Poll, res)
		return
	}
	p.complete(bg, job, res.Status, "", res.Body,
		fmt.Sprintf("target responded with status %d", res.Status))
}

// parkForPolling extracts the target's job id from its accept response and
// parks the job in awaiting_target with its poll schedule.
func (p *Pool) parkForPolling(bg context.Context, job *store.Job, poll *config.Poll, res *upstream.ForwardResult) {
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
}

// processPoll runs one poll tick for a job awaiting its target's result.
// The poll deadline is evaluated after the status call, so a target job that
// finished just as the deadline passed still completes instead of failing
// with a wrong target_timeout. ctx bounds upstream I/O; bg is for store
// writes that must survive cancellation.
func (p *Pool) processPoll(ctx, bg context.Context, job *store.Job) {
	tcfg, ok := p.cfg.Targets[job.Target]
	if !ok || tcfg.Response.Mode != config.ModePoll || tcfg.Response.Poll == nil {
		p.failJob(bg, job, errUnknownTarget, fmt.Sprintf("target %q is no longer configured for polling", job.Target))
		return
	}

	st, err := p.client.PollTarget(ctx, job.Target, tcfg, job.TargetJobID)
	pastDeadline := job.PollDeadline != nil && time.Now().After(*job.PollDeadline)
	if err != nil {
		switch {
		case upstream.IsTransient(err) && !pastDeadline:
			// The next poll tick is already scheduled; just note the miss.
			p.logger.Warn("poll attempt failed", "job_id", job.ID, "error", err)
		case upstream.IsTransient(err):
			p.failJob(bg, job, errTargetTimeout, fmt.Sprintf(
				"target job %s did not finish before the poll deadline (status endpoint unreachable: %v)",
				job.TargetJobID, err))
		default:
			p.failJob(bg, job, errTargetError, err.Error())
		}
		return
	}

	switch {
	case st.Failed:
		p.failJob(bg, job, errTargetJobFailed, fmt.Sprintf(
			"target job %s reported status %q", job.TargetJobID, st.Raw))
	case st.Done:
		p.fetchAndComplete(ctx, bg, job, tcfg, st.Raw, pastDeadline)
	case pastDeadline:
		p.failJob(bg, job, errTargetTimeout, fmt.Sprintf(
			"target job %s did not finish before the poll deadline", job.TargetJobID))
	default:
		p.logger.Debug("target job still running", "job_id", job.ID, "status", st.Raw)
	}
}

// fetchAndComplete retrieves the finished target job's result and completes
// the job. A transient fetch error leaves the job parked for the next tick —
// unless the poll deadline has passed, in which case the job fails with an
// accurate target_error (the target job itself finished).
func (p *Pool) fetchAndComplete(ctx, bg context.Context, job *store.Job, tcfg config.Target, rawStatus string, pastDeadline bool) {
	ct, body, err := p.client.FetchResult(ctx, job.Target, tcfg, job.TargetJobID)
	if err != nil {
		if upstream.IsTransient(err) && !pastDeadline {
			p.logger.Warn("result fetch failed, will retry next tick", "job_id", job.ID, "error", err)
			return
		}
		p.failJob(bg, job, errTargetError, err.Error())
		return
	}
	// FetchResult only returns on a 2xx response and does not surface the
	// exact status, so record the canonical 200 (the status stored at accept
	// time belonged to the forward call, not the result fetch).
	p.complete(bg, job, http.StatusOK, ct, body, fmt.Sprintf(
		"target job %s finished with status %q", job.TargetJobID, rawStatus))
}

// complete stores the final result: small JSON inline, everything else as a
// file in the results directory.
func (p *Pool) complete(bg context.Context, job *store.Job, status int, contentType string, body []byte, detail string) {
	// Trust the bytes over the declared content type: upstream servers are
	// often sloppy about Content-Type on JSON responses.
	isJSON := json.Valid(body)
	if isJSON {
		contentType = "application/json"
	} else if contentType == "" {
		contentType = "application/octet-stream"
	}
	if isJSON && len(body) <= inlineResultLimit {
		p.markCompleted(bg, job, status, body, "", "", detail)
		return
	}

	path, err := p.writeResultFile(job.ID, contentType, body)
	if err != nil {
		p.failJob(bg, job, errInternal, err.Error())
		return
	}
	p.markCompleted(bg, job, status, nil, path, contentType, detail)
}

func (p *Pool) markCompleted(bg context.Context, job *store.Job, status int, body []byte, path, contentType, detail string) {
	err := p.store.MarkCompleted(bg, job.ID, status, body, path, contentType, detail)
	switch {
	case errors.Is(err, store.ErrTerminalState):
		// An overlapping poll tick finished the job first; this outcome is
		// redundant, not wrong.
		p.logger.Debug("job already terminal, dropping duplicate completion", "job_id", job.ID)
	case err != nil:
		p.logger.Error("mark completed failed", "job_id", job.ID, "error", err)
	case path != "":
		p.logger.Info("job completed with file result", "job_id", job.ID, "target", job.Target, "result_path", path)
	default:
		p.logger.Info("job completed", "job_id", job.ID, "target", job.Target)
	}
}

// writeResultFile stores an oversized or non-JSON result in the results dir.
// The write goes to a temp file first and is renamed into place, so a client
// streaming the previous file (or a concurrent duplicate completion) never
// observes a truncated result.
func (p *Pool) writeResultFile(jobID, contentType string, body []byte) (string, error) {
	if err := os.MkdirAll(p.cfg.Storage.ResultsDir, 0o750); err != nil {
		return "", fmt.Errorf("create results dir: %w", err)
	}
	path := filepath.Join(p.cfg.Storage.ResultsDir, jobID+resultExt(contentType))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return "", fmt.Errorf("write result file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("write result file: %w", err)
	}
	return path, nil
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
	// Attempts is >= 1 here (incremented at claim). The backoff <= 0 branch
	// below is not dead code: the shift overflows to a non-positive value
	// once the attempt count grows large.
	backoff := p.cfg.Worker.BackoffBase.Std() << (job.Attempts - 1)
	if maxB := p.cfg.Worker.BackoffMax.Std(); backoff > maxB || backoff <= 0 {
		backoff = maxB
	}
	// ±20% jitter avoids thundering-herd retries.
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
		if errors.Is(err, store.ErrTerminalState) {
			// A faster overlapping worker finished the job; a client may
			// already have seen that outcome, so this late failure is dropped.
			p.logger.Debug("job already terminal, dropping late failure", "job_id", job.ID, "code", code)
			return
		}
		p.logger.Error("mark failed failed", "job_id", job.ID, "error", err)
		return
	}
	p.logger.Warn("job failed", "job_id", job.ID, "target", job.Target, "code", code, "message", message)
}

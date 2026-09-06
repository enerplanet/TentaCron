package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/plan"
	"github.com/enerplanet/tentacron/internal/resolver"
	"github.com/enerplanet/tentacron/internal/store"
	"github.com/enerplanet/tentacron/internal/upstream"
)

// errBadResourceBody marks a resource API response whose body is not a JSON
// object; processNew maps it to the invalid_resource_response job code
// instead of the HTTP-level resource_error.
var errBadResourceBody = errors.New("resource response is not a JSON object")

// Results up to this size that are valid JSON stay inline in the database;
// anything bigger or binary goes to the results directory.
const inlineResultLimit = 256 << 10

func (p *run) process(ctx context.Context, job *store.Job) {
	// Bookkeeping writes use an uncancellable context so a shutdown or
	// deadline can never strand a job in a half-written state.
	bg := context.WithoutCancel(ctx)
	defer func() {
		if r := recover(); r != nil {
			p.logger.Error("panic while processing job", "job_id", job.ID, "panic", r)
			p.failJob(bg, job, store.JobCodeInternal, fmt.Sprintf("panic: %v", r))
		}
	}()

	switch job.State {
	// ClaimNext already moved "received" jobs to "resolving", so a freshly
	// claimed job arrives here in StateResolving, never StateReceived.
	case store.StateResolving:
		// The attempt deadline covers resolution and forwarding.
		actx, cancel := context.WithTimeout(ctx, p.cfg.JobTimeoutFor(job.Target))
		defer cancel()
		p.processNew(actx, bg, job)
	case store.StateAwaitingTarget:
		// A poll tick is not an attempt: the status call is bounded by the
		// target's timeout and the result download by its own two bounds
		// (see upstream.FetchResult), never by job_timeout, which was sized
		// for resolution plus one forward.
		p.processPoll(ctx, bg, job)
	default:
		p.logger.Error("claimed job in unexpected state", "job_id", job.ID, "state", job.State)
	}
}

// failure is a permanent job failure together with its API error code.
type failure struct {
	code string
	err  error
}

// processNew runs the resolve → forward pipeline for a freshly claimed job.
// ctx bounds the job's upstream I/O (job timeout, shutdown); bg is the
// uncancellable bookkeeping context from process — every store write below
// uses bg so cancellation can never strand the job mid-transition.
func (p *run) processNew(ctx, bg context.Context, job *store.Job) {
	tcfg, ok := p.cfg.Targets[job.Target]
	if !ok {
		p.failJob(bg, job, store.JobCodeUnknownTarget, fmt.Sprintf("target %q is no longer configured", job.Target))
		return
	}
	if tcfg.Proxy {
		p.handThrough(ctx, bg, job, tcfg)
		return
	}
	// The same inspection the dry-run endpoint answers with: the worker and
	// the API can never disagree about what a payload contains.
	pl := plan.Inspect(p.cfg, job.Target, job.Payload)
	if !pl.OK() {
		p.failJob(bg, job, pl.Problems[0].Code, pl.Problems[0].Message)
		return
	}
	seriesByHash, cached, err := p.fetchChain(ctx, bg, pl.Levels, job.Options.Cache)
	if err != nil {
		p.failResolution(bg, job, err)
		return
	}
	resolved, fail := p.substitute(job.ID, pl.Root, pl.Found, seriesByHash, attachResolvent(tcfg))
	if fail != nil {
		p.failJob(bg, job, fail.code, fail.err.Error())
		return
	}
	p.forwardResolved(ctx, bg, job, tcfg, resolved, len(pl.Found), cached)
}

// handThrough forwards a proxy target's payload unresolved — even objects
// that look like resolvents stay untouched. Storing the payload as
// resolved_payload keeps the audit trail's shape identical to a resolved
// job's.
func (p *run) handThrough(ctx, bg context.Context, job *store.Job, tcfg config.Target) {
	if err := p.store.SetResolved(bg, job.ID, job.Payload, "proxy target: payload handed through unresolved"); err != nil {
		p.logger.Error("persist proxied payload failed", "job_id", job.ID, "error", err)
		return
	}
	p.logger.Info("payload handed through unresolved", "job_id", job.ID, "target", job.Target)
	p.forward(ctx, bg, job, tcfg, job.Payload)
}

// attachResolvent reports the target's marker policy. nil means "not
// configured": config.Load defaults it to true, but a hand-built config
// (tests, embedders) must get the same default.
func attachResolvent(tcfg config.Target) bool {
	return tcfg.AttachResolvent == nil || *tcfg.AttachResolvent
}

// failResolution maps a fetch error onto the job outcome: a malformed
// resource body is the permanent invalid_resource_response, a reference
// that does not fit the series it points to the permanent invalid_payload,
// anything else goes through the retry classification as a resource_error.
func (p *run) failResolution(bg context.Context, job *store.Job, err error) {
	switch {
	case errors.Is(err, errBadResourceBody):
		p.failJob(bg, job, store.JobCodeInvalidResourceResponse, err.Error())
	case errors.Is(err, resolver.ErrReference):
		p.failJob(bg, job, plan.CodeInvalidPayload, err.Error())
	default:
		p.retryOrFail(bg, job, store.JobCodeResourceError, err)
	}
}

// fetchChain resolves the plan level by level. Before a level is fetched,
// its references are filled from the series of the levels before it and its
// cache keys are recomputed from the filled inputs, so a chained resolvent
// caches under the parameters it really sent. Failure attribution stays
// deterministic: the first failing level, document order within it.
func (p *run) fetchChain(ctx, bg context.Context, levels [][]*resolver.Found, cacheMode string) (map[string][]byte, int, error) {
	all := map[string][]byte{}
	cached := 0
	for _, level := range levels {
		for _, f := range level {
			filled, err := f.Fill(func(dep *resolver.Found) []byte { return all[dep.Hash] })
			if err != nil {
				return nil, 0, err
			}
			if filled {
				if err := f.Rehash(p.cfg.Resolvents[f.Type].CacheIgnore()); err != nil {
					return nil, 0, fmt.Errorf("%w: %w", resolver.ErrReference, err)
				}
			}
		}
		series, c, err := p.fetchAll(ctx, bg, level, cacheMode)
		if err != nil {
			return nil, 0, err
		}
		maps.Copy(all, series)
		cached += c
	}
	return all, cached, nil
}

// substitute splices every fetched series into its slot and re-encodes the
// document.
func (p *run) substitute(jobID string, root map[string]any, found []*resolver.Found, seriesByHash map[string][]byte, attach bool) ([]byte, *failure) {
	for _, f := range found {
		warnings, err := f.Substitute(seriesByHash[f.Hash], attach)
		if err != nil {
			return nil, &failure{store.JobCodeInvalidResourceResponse, err}
		}
		for _, w := range warnings {
			p.logger.Warn("resolvent substitution warning", "job_id", jobID, "warning", w)
		}
	}
	resolved, err := resolver.Marshal(root)
	if err != nil {
		return nil, &failure{store.JobCodeInternal, fmt.Errorf("re-encode resolved payload: %w", err)}
	}
	return resolved, nil
}

// forwardResolved persists the resolved payload and hands it to the target.
func (p *run) forwardResolved(ctx, bg context.Context, job *store.Job, tcfg config.Target, resolved []byte, count, cached int) {
	detail := fmt.Sprintf("resolved %d resolvent(s), %d from cache", count, cached)
	if err := p.store.SetResolved(bg, job.ID, resolved, detail); err != nil {
		p.logger.Error("persist resolved payload failed", "job_id", job.ID, "error", err)
		return
	}
	p.logger.Info("payload resolved", "job_id", job.ID, "resolvents", count, "cached", cached)
	p.forward(ctx, bg, job, tcfg, resolved)
}

// fetchResult is one resolvent fetch outcome, keyed by parameter hash.
type fetchResult struct {
	hash   string
	body   []byte
	cached bool
	err    error
}

// fetchAll retrieves the time series for every unique resolvent hash, from
// cache when possible, otherwise from the resource APIs. A fixed worker set
// bounds goroutines — not just concurrent HTTP calls — so a payload with tens
// of thousands of resolvents cannot spawn a goroutine each.
//
// On the first error no new fetches start, but in-flight calls run to
// completion (their series still land in the cache, keeping retries cheap).
// Deliberately no cancellation: aborting a sibling would turn its genuine
// failure into a cancellation, making it a scheduling race which resolvent
// the job's error names. Because feeding follows document order, the first
// failing resolvent in document order always records its true error.
func (p *run) fetchAll(ctx, bg context.Context, found []*resolver.Found, cacheMode string) (map[string][]byte, int, error) {
	unique := uniqueByHash(found)
	results := make(chan fetchResult, len(unique))
	stop := make(chan struct{})
	feed := feedResolvents(ctx, unique, stop, results)
	for range min(p.cfg.Worker.ResolventConcurrency, len(unique)) {
		go p.fetchLoop(ctx, bg, feed, results, cacheMode)
	}
	return collectFetches(unique, results, stop)
}

// uniqueByHash keeps the first occurrence of each parameter hash, in
// document order.
func uniqueByHash(found []*resolver.Found) []*resolver.Found {
	seen := map[string]bool{}
	unique := make([]*resolver.Found, 0, len(found))
	for _, f := range found {
		if !seen[f.Hash] {
			seen[f.Hash] = true
			unique = append(unique, f)
		}
	}
	return unique
}

// feedResolvents hands resolvents to the fetchers in document order. Once
// stop closes (a sibling failed) or ctx ends, the remaining resolvents are
// reported as skipped instead, so the collector always receives exactly one
// result per unique resolvent.
func feedResolvents(ctx context.Context, unique []*resolver.Found, stop <-chan struct{}, results chan<- fetchResult) <-chan *resolver.Found {
	feed := make(chan *resolver.Found)
	go func() {
		defer close(feed)
		for _, f := range unique {
			select {
			case feed <- f:
			case <-stop:
				results <- fetchResult{hash: f.Hash, err: fmt.Errorf("fetch skipped after another resolvent failed: %w", context.Canceled)}
			case <-ctx.Done():
				results <- fetchResult{hash: f.Hash, err: ctx.Err()}
			}
		}
	}()
	return feed
}

// fetchLoop is one bounded fetcher: it resolves resolvents from feed until
// the feed closes, sending exactly one result per resolvent.
func (p *run) fetchLoop(ctx, bg context.Context, feed <-chan *resolver.Found, results chan<- fetchResult, cacheMode string) {
	for f := range feed {
		body, cached, err := p.fetchOne(ctx, bg, f, cacheMode)
		results <- fetchResult{hash: f.Hash, body: body, cached: cached, err: err}
	}
}

// collectFetches drains exactly len(unique) results — each unique resolvent
// is either fetched by a worker or reported as skipped by the feeder, so the
// loop cannot deadlock — and closes stop on the first error.
func collectFetches(unique []*resolver.Found, results <-chan fetchResult, stop chan<- struct{}) (map[string][]byte, int, error) {
	seriesByHash := make(map[string][]byte, len(unique))
	errByHash := map[string]error{}
	cached := 0
	for range unique {
		r := <-results
		if r.err != nil {
			if len(errByHash) == 0 {
				close(stop)
			}
			errByHash[r.hash] = r.err
			continue
		}
		seriesByHash[r.hash] = r.body
		if r.cached {
			cached++
		}
	}
	if len(errByHash) > 0 {
		return nil, 0, pickResolveError(unique, errByHash)
	}
	return seriesByHash, cached, nil
}

// pickResolveError chooses which of several fetch failures the job reports:
// the first genuine error in document order. Skips and cancellations (both
// context-flavored) only surface when nothing failed for a real reason —
// e.g. the job timeout fired — and then retryOrFail treats them as
// transient, parking the job for a clean retry.
func pickResolveError(unique []*resolver.Found, errByHash map[string]error) error {
	var fallback error
	for _, f := range unique {
		err := errByHash[f.Hash]
		if err == nil {
			continue
		}
		if !isContextErr(err) {
			return err
		}
		if fallback == nil {
			fallback = err
		}
	}
	return fallback
}

// fetchOne resolves one resolvent under the job's cache mode: "use" reads
// and writes the series cache, "refresh" skips the read, "bypass" skips both.
func (p *run) fetchOne(ctx, bg context.Context, f *resolver.Found, cacheMode string) (body []byte, cached bool, err error) {
	if cacheMode != store.CacheBypass && cacheMode != store.CacheRefresh {
		// A store error on the cache read is deliberately treated as a miss:
		// fall through and fetch fresh rather than fail the job over a cache
		// problem.
		if body, hit, err := p.store.GetSeries(ctx, f.Hash); err == nil && hit {
			p.metrics.CacheLookup(true)
			return body, true, nil
		}
		p.metrics.CacheLookup(false)
	}
	rcfg := p.cfg.Resolvents[f.Type]
	body, err = p.callResolventBackend(ctx, f, rcfg)
	if err != nil {
		return nil, false, err
	}
	if rcfg.ResponsePath != "" {
		extracted, err := upstream.ExtractPath(body, rcfg.ResponsePath)
		if err != nil {
			return nil, false, fmt.Errorf("resource %s response has no %q (%w): %w",
				f.Type, rcfg.ResponsePath, err, errBadResourceBody)
		}
		body = extracted
	}
	if len(rcfg.ResponseMap) > 0 {
		// The adapter for backends that do not speak the series contract;
		// the mapped object is what gets cached and substituted.
		mapped, err := resolver.ApplyMap(body, rcfg.ResponseMap)
		if err != nil {
			return nil, false, fmt.Errorf("resource %s response does not fit its response_map (%w): %w", f.Type, err, errBadResourceBody)
		}
		body = mapped
	}
	var probe map[string]any
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, false, fmt.Errorf("resource %s returned malformed JSON (%w): %w", f.Type, err, errBadResourceBody)
	}
	if probe == nil {
		// json.Unmarshal accepts "null" into a map without error; caching it
		// would poison every job sharing this resolvent for the TTL.
		return nil, false, fmt.Errorf("resource %s returned null instead of a time-series object: %w", f.Type, errBadResourceBody)
	}
	if cacheMode == store.CacheBypass {
		return body, false, nil
	}
	if err := p.store.PutSeries(bg, f.Hash, f.Type, body, rcfg.CacheTTL.Std()); err != nil {
		p.logger.Warn("series cache write failed", "type", f.Type, "error", err)
	}
	return body, false, nil
}

// callResolventBackend performs the outbound call for one resolvent: a call
// to its resource URL (JSON body for POST-style methods; URL path/query
// mapping for GET — see upstream.ResolveResolvent), or — for a target-backed
// resolvent — a forward through the named direct-mode target (tentacron
// composing its own targets, with the target's url/auth/timeout applying).
// The payload is the resolvent's input — the object itself with any
// references already filled — or the object under payload_field when
// configured; it is never scanned for resolvent objects, so the only
// nesting is the explicit, acyclic chain of references.
func (p *run) callResolventBackend(ctx context.Context, f *resolver.Found, rcfg config.Resolvent) ([]byte, error) {
	payload := f.Input
	if rcfg.PayloadField != "" {
		nested, ok := f.Input[rcfg.PayloadField].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("resolvent %s: field %q must hold the payload object for the backend call",
				f.Type, rcfg.PayloadField)
		}
		payload = nested
	}

	if rcfg.Target != "" {
		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode resolvent %s: %w", f.Type, err)
		}
		tcfg, ok := p.cfg.Targets[rcfg.Target]
		if !ok {
			return nil, fmt.Errorf("resolvent %s: backing target %q is no longer configured", f.Type, rcfg.Target)
		}
		res, err := p.client.ForwardToTarget(ctx, rcfg.Target, tcfg, payloadBytes)
		if err != nil {
			return nil, err
		}
		return res.Body, nil
	}
	return p.client.ResolveResolvent(ctx, f.Type, rcfg, payload)
}

// forward sends the resolved payload to the target and either completes the
// job (direct mode) or parks it for polling (poll mode).
func (p *run) forward(ctx, bg context.Context, job *store.Job, tcfg config.Target, resolved []byte) {
	res, err := p.client.ForwardToTarget(ctx, job.Target, tcfg, resolved)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && !tcfg.RetriesOnTimeout() {
			// The target may still be working on this request; a retry
			// would submit the same work again. Fail instead of requeueing.
			p.failJob(bg, job, store.JobCodeTargetTimeout, fmt.Sprintf(
				"target %s did not answer before the deadline (call timeout %s); not retried because retry_on_timeout is false",
				job.Target, tcfg.Timeout.Std()))
			return
		}
		p.retryOrFail(bg, job, store.JobCodeTargetError, err)
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
func (p *run) parkForPolling(bg context.Context, job *store.Job, poll *config.Poll, res *upstream.ForwardResult) {
	targetJobID, err := upstream.ExtractJobID(res.Body, poll)
	if err != nil {
		p.failJob(bg, job, store.JobCodeTargetError, err.Error())
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
func (p *run) processPoll(ctx, bg context.Context, job *store.Job) {
	tcfg, ok := p.cfg.Targets[job.Target]
	if !ok || tcfg.Response.Mode != config.ModePoll || tcfg.Response.Poll == nil {
		p.failJob(bg, job, store.JobCodeUnknownTarget, fmt.Sprintf("target %q is no longer configured for polling", job.Target))
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
			p.failJob(bg, job, store.JobCodeTargetTimeout, fmt.Sprintf(
				"target job %s did not finish before the poll deadline (status endpoint unreachable: %v)",
				job.TargetJobID, err))
		default:
			p.failJob(bg, job, store.JobCodeTargetError, err.Error())
		}
		return
	}

	switch {
	case st.Failed:
		p.failJob(bg, job, store.JobCodeTargetJobFailed, fmt.Sprintf(
			"target job %s reported status %q", job.TargetJobID, st.Raw))
	case st.Done:
		p.fetchAndComplete(ctx, bg, job, tcfg, st.Raw, pastDeadline)
	case pastDeadline:
		p.failJob(bg, job, store.JobCodeTargetTimeout, fmt.Sprintf(
			"target job %s did not finish before the poll deadline", job.TargetJobID))
	default:
		p.logger.Debug("target job still running", "job_id", job.ID, "status", st.Raw)
	}
}

// fetchAndComplete retrieves the finished target job's result and completes
// the job. The download streams to a spool file under
// storage.max_result_bytes, so a bundle far larger than the JSON response
// cap never sits in memory. A transient fetch error leaves the job parked
// for the next tick — unless the poll deadline has passed, in which case the
// job fails with an accurate target_error (the target job itself finished).
func (p *run) fetchAndComplete(ctx, bg context.Context, job *store.Job, tcfg config.Target, rawStatus string, pastDeadline bool) {
	stream, err := p.client.FetchResult(ctx, job.Target, tcfg, job.TargetJobID)
	if err == nil {
		var spool spooledResult
		spool, err = p.spoolResult(job, stream)
		_ = stream.Close()
		if err == nil {
			// FetchResult only returns on a 2xx response; record the
			// canonical 200 (the status stored at accept time belonged to
			// the forward call, not the result fetch).
			p.completeSpooled(bg, job, spool, fmt.Sprintf("target job %s finished with status %q", job.TargetJobID, rawStatus))
			return
		}
	}
	if upstream.IsTransient(err) && !pastDeadline {
		p.logger.Warn("result fetch failed, will retry next tick", "job_id", job.ID, "error", err)
		return
	}
	p.failJob(bg, job, store.JobCodeTargetError, err.Error())
}

// spooledResult is a downloaded result parked in the results directory.
type spooledResult struct {
	path        string
	size        int64
	contentType string
}

// spoolResult streams the download into a uniquely named temp file, capped
// at storage.max_result_bytes. Exceeding the cap is permanent (the result
// will not shrink); any other failure is transient and retried next tick.
func (p *run) spoolResult(job *store.Job, stream *upstream.ResultStream) (spooledResult, error) {
	op := "result " + job.Target
	if err := os.MkdirAll(p.cfg.Storage.ResultsDir, 0o750); err != nil {
		return spooledResult{}, &upstream.Error{Op: op, Transient: true, Err: fmt.Errorf("create results dir: %w", err)}
	}
	tmp, err := os.CreateTemp(p.cfg.Storage.ResultsDir, job.ID+".*.tmp")
	if err != nil {
		return spooledResult{}, &upstream.Error{Op: op, Transient: true, Err: fmt.Errorf("create spool file: %w", err)}
	}
	limit := p.cfg.Storage.MaxResultBytes
	n, err := io.Copy(tmp, io.LimitReader(stream.Body, limit+1))
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	switch {
	case err != nil:
		_ = os.Remove(tmp.Name())
		return spooledResult{}, &upstream.Error{Op: op, Transient: true, Err: fmt.Errorf("download result: %w", err)}
	case n > limit:
		_ = os.Remove(tmp.Name())
		return spooledResult{}, &upstream.Error{Op: op, Transient: false,
			Err: fmt.Errorf("result exceeds storage.max_result_bytes (%d bytes)", limit)}
	}
	return spooledResult{path: tmp.Name(), size: n, contentType: stream.ContentType}, nil
}

// completeSpooled finishes the job from the spool file. A result within the
// upstream response cap takes the in-memory path (JSON detection, inline
// storage below the inline limit); anything larger is renamed into place
// as a file with the content type the target declared — no sniffing of
// gigabytes.
func (p *run) completeSpooled(bg context.Context, job *store.Job, spool spooledResult, detail string) {
	if spool.size <= p.cfg.Upstream.MaxResponseBytes {
		body, err := os.ReadFile(spool.path)
		_ = os.Remove(spool.path)
		if err != nil {
			p.failJob(bg, job, store.JobCodeInternal, "read spooled result: "+err.Error())
			return
		}
		p.complete(bg, job, http.StatusOK, spool.contentType, body, detail)
		return
	}
	ct := spool.contentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	path := filepath.Join(p.cfg.Storage.ResultsDir, job.ID+resultExt(ct))
	if err := os.Rename(spool.path, path); err != nil {
		_ = os.Remove(spool.path)
		p.failJob(bg, job, store.JobCodeInternal, "store result file: "+err.Error())
		return
	}
	p.markCompleted(bg, job, http.StatusOK, nil, path, ct, detail)
}

// complete stores the final result: small JSON inline, everything else as a
// file in the results directory.
func (p *run) complete(bg context.Context, job *store.Job, status int, contentType string, body []byte, detail string) {
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
		p.failJob(bg, job, store.JobCodeInternal, err.Error())
		return
	}
	p.markCompleted(bg, job, status, nil, path, contentType, detail)
}

func (p *run) markCompleted(bg context.Context, job *store.Job, status int, body []byte, path, contentType, detail string) {
	err := p.store.MarkCompleted(bg, job.ID, status, body, path, contentType, detail)
	switch {
	case errors.Is(err, store.ErrTerminalState):
		// An overlapping poll tick finished the job first, or the client
		// cancelled it meanwhile; this outcome is redundant, not wrong.
		p.logger.Debug("job already terminal, dropping duplicate completion", "job_id", job.ID)
		p.discardLateResult(bg, job.ID, path)
	case err != nil:
		p.logger.Error("mark completed failed", "job_id", job.ID, "error", err)
	case path != "":
		p.metrics.JobFinished(job.Target, "completed", "")
		p.notifier.Notify(job.ID)
		p.logger.Info("job completed with file result", "job_id", job.ID, "target", job.Target, "result_path", path)
	default:
		p.metrics.JobFinished(job.Target, "completed", "")
		p.notifier.Notify(job.ID)
		p.logger.Info("job completed", "job_id", job.ID, "target", job.Target)
	}
}

// discardLateResult removes the file a dropped completion had already
// renamed into place, so a job that ended first — cancelled, or completed by
// an overlapping tick under another name — never leaves a file that no row
// references and retention would never prune. The one file kept is the
// job's own stored result: an overlapping tick that won with the same path
// must not lose it. When the job cannot be read the file stays; a leak is
// recoverable, a deleted winner is not.
func (p *run) discardLateResult(bg context.Context, jobID, path string) {
	if path == "" {
		return
	}
	current, err := p.store.GetJob(bg, jobID)
	if err != nil || current.ResultPath == path {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		p.logger.Warn("could not remove the result file of a dropped completion", "job_id", jobID, "path", path, "error", err)
	}
}

// writeResultFile stores an oversized or non-JSON result in the results dir.
// The write goes to a uniquely named temp file first and is renamed into
// place, so a client streaming the previous file never observes a truncated
// result, and two overlapping poll ticks completing the same job (each with
// its own temp file) can never interleave writes into one another.
func (p *run) writeResultFile(jobID, contentType string, body []byte) (string, error) {
	if err := os.MkdirAll(p.cfg.Storage.ResultsDir, 0o750); err != nil {
		return "", fmt.Errorf("create results dir: %w", err)
	}
	path := filepath.Join(p.cfg.Storage.ResultsDir, jobID+resultExt(contentType))
	tmp, err := os.CreateTemp(p.cfg.Storage.ResultsDir, jobID+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("write result file: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("write result file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("write result file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
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
func (p *run) retryOrFail(bg context.Context, job *store.Job, permanentCode string, err error) {
	if !upstream.IsTransient(err) && !isContextErr(err) {
		p.failJob(bg, job, permanentCode, err.Error())
		return
	}
	if job.Attempts >= job.MaxAttempts {
		p.failJob(bg, job, store.JobCodeMaxAttemptsExceeded, fmt.Sprintf(
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

func (p *run) failJob(bg context.Context, job *store.Job, code, message string) {
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
	p.metrics.JobFinished(job.Target, "failed", code)
	p.notifier.Notify(job.ID)
	p.logger.Warn("job failed", "job_id", job.ID, "target", job.Target, "code", code, "message", message)
}

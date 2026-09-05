package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/enerplanet/tentacron/internal/callback"
	"github.com/enerplanet/tentacron/internal/jobview"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/plan"
	"github.com/enerplanet/tentacron/internal/store"
)

type createRequest struct {
	APIKey  string          `json:"api_key"`
	Target  string          `json:"target"`
	Payload json.RawMessage `json:"payload"`
	// Priority orders claims (-10..10, default 0); capped per client key.
	Priority *int `json:"priority"`
	// Options are per-request processing choices.
	Options *requestOptions `json:"options"`
	// NotBefore delays the run: RFC 3339, at most 30 days ahead.
	NotBefore string `json:"not_before,omitempty"`
	// CallbackURL receives the job document once the request is terminal;
	// https, host allow-listed in callbacks.allowed_hosts.
	CallbackURL string `json:"callback_url,omitempty"`
}

// maxNotBeforeAhead bounds how far a delayed run may be scheduled.
const maxNotBeforeAhead = 30 * 24 * time.Hour

type requestOptions struct {
	// Cache is use (default), bypass or refresh.
	Cache string `json:"cache,omitempty"`
}

// jobOptions converts the request's options into the stored form.
func (r createRequest) jobOptions() store.JobOptions {
	if r.Options == nil || r.Options.Cache == store.CacheUse {
		return store.JobOptions{}
	}
	return store.JobOptions{Cache: r.Options.Cache}
}

type createResponse struct {
	ID    string            `json:"id"`
	State string            `json:"state"`
	Links map[string]string `json:"links"`
}

// maxIdempotencyKeyLen bounds the Idempotency-Key header, which is stored
// verbatim and indexed.
const maxIdempotencyKeyLen = 255

// decodeCreateRequest parses and validates the create-request body, writing
// the client error response itself when the request is unusable. The API
// key comes from the X-API-Key header when present (the preferred form) and
// from the body's api_key field otherwise.
func (s *Server) decodeCreateRequest(w http.ResponseWriter, r *http.Request) (createRequest, bool) {
	var req createRequest
	if !requireJSON(w, r) {
		return req, false
	}
	if len(r.Header.Get("Idempotency-Key")) > maxIdempotencyKeyLen {
		writeError(w, http.StatusBadRequest, CodeInvalidParameter, "Idempotency-Key must be at most 255 bytes")
		return req, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg().Server.MaxBodyBytes)
	if !decodeSingleObject(w, r.Body, &req) {
		return req, false
	}
	if header := r.Header.Get("X-API-Key"); header != "" {
		req.APIKey = header
	}
	if status, code, msg := validateCreateRequest(req); code != "" {
		writeError(w, status, code, msg)
		return req, false
	}
	return req, true
}

// requireJSON enforces the documented Content-Type. An absent header takes
// the same 415 path as a wrong one: mime.ParseMediaType("") errors.
func requireJSON(w http.ResponseWriter, r *http.Request) bool {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType,
			"Content-Type must be application/json")
		return false
	}
	return true
}

// decodeSingleObject decodes exactly one JSON value from body: an oversized
// body is 413, a malformed one or trailing data 400.
func decodeSingleObject(w http.ResponseWriter, body io.Reader, dst any) bool {
	dec := json.NewDecoder(body)
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
				"request body exceeds the configured limit")
			return false
		}
		writeError(w, http.StatusBadRequest, CodeInvalidJSON, "request body is not valid JSON")
		return false
	}
	// Decode stops after the first JSON value; anything but EOF behind it
	// means the body was not a single JSON object.
	if err := dec.Decode(new(struct{})); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, CodeInvalidJSON, "request body contains trailing data")
		return false
	}
	return true
}

// validateCreateRequest checks the required fields; an empty code means the
// request is well-formed.
func validateCreateRequest(req createRequest) (status int, code, msg string) {
	if req.APIKey == "" {
		return http.StatusBadRequest, CodeMissingField, "an API key is required: send the X-API-Key header (or the api_key field)"
	}
	return validateSubmission(req)
}

// validateSubmission checks the fields of one submission — target, payload,
// priority, options, not_before — shared by create and every batch item.
func validateSubmission(req createRequest) (status int, code, msg string) {
	if req.NotBefore != "" {
		t, err := time.Parse(time.RFC3339, req.NotBefore)
		switch {
		case err != nil:
			return http.StatusBadRequest, CodeInvalidParameter, "not_before must be an RFC 3339 timestamp"
		case time.Until(t) > maxNotBeforeAhead:
			return http.StatusBadRequest, CodeInvalidParameter, "not_before must be at most 30 days ahead"
		}
	}
	switch {
	case req.Target == "":
		return http.StatusBadRequest, CodeMissingField, "target is required"
	case len(req.Payload) == 0 || string(req.Payload) == "null":
		return http.StatusBadRequest, CodeMissingField, "payload is required"
	case !strings.HasPrefix(strings.TrimSpace(string(req.Payload)), "{"):
		return http.StatusBadRequest, CodeInvalidJSON, "payload must be a JSON object"
	case req.Priority != nil && (*req.Priority < config.MinPriority || *req.Priority > config.MaxPriority):
		return http.StatusBadRequest, CodeInvalidParameter,
			fmt.Sprintf("priority must be between %d and %d", config.MinPriority, config.MaxPriority)
	case req.Options != nil && !validCacheMode(req.Options.Cache):
		return http.StatusBadRequest, CodeInvalidParameter, "options.cache must be use, bypass or refresh"
	}
	return 0, "", ""
}

func validCacheMode(mode string) bool {
	switch mode {
	case "", store.CacheUse, store.CacheBypass, store.CacheRefresh:
		return true
	}
	return false
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	req, id, ok := s.acceptedCreate(w, r)
	if !ok {
		return
	}
	s.acceptJob(w, r, req, id.name)
}

// acceptedCreate decodes and authenticates a create-shaped request and checks
// its target and priority — everything create and the dry run share before
// they diverge into persisting or inspecting. Errors are written here.
func (s *Server) acceptedCreate(w http.ResponseWriter, r *http.Request) (createRequest, identity, bool) {
	req, ok := s.decodeCreateRequest(w, r)
	if !ok {
		return req, identity{}, false
	}
	id, ok := s.authenticate(req.APIKey)
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "invalid API key")
		return req, identity{}, false
	}
	noteClient(r, id.name)
	if _, ok := s.cfg().Targets[req.Target]; !ok {
		writeError(w, http.StatusUnprocessableEntity, CodeUnknownTarget,
			"target "+strconv.Quote(req.Target)+" is not configured")
		return req, id, false
	}
	if limit := s.cfg().MaxPriorityFor(id.name); req.Priority != nil && *req.Priority > limit {
		writeError(w, http.StatusBadRequest, CodeInvalidParameter,
			fmt.Sprintf("priority %d exceeds this key's maximum of %d", *req.Priority, limit))
		return req, id, false
	}
	if req.CallbackURL != "" {
		if err := callback.Check(s.cfg().Callbacks, req.CallbackURL); err != nil {
			writeError(w, http.StatusUnprocessableEntity, CodeCallbackNotAllowed, err.Error())
			return req, id, false
		}
	}
	return req, id, true
}

type validateResponse struct {
	OK         bool                `json:"ok"`
	Target     string              `json:"target"`
	Resolvents []validateResolvent `json:"resolvents"`
	Problems   []plan.Problem      `json:"problems"`
}

type validateResolvent struct {
	Type      string   `json:"type"`
	Path      string   `json:"path"`
	Name      string   `json:"name,omitempty"`
	Cached    bool     `json:"cached"`
	DependsOn []string `json:"depends_on,omitempty"`
}

// handleValidate is the dry run: the request is decoded, authenticated and
// inspected exactly as the worker would start it — which resolvents it
// contains and which problems would fail it — without persisting anything or
// calling any upstream. Only the series cache is consulted. The answer is 200
// whenever the request itself is well-formed; ok says whether the job could
// start.
func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	req, _, ok := s.acceptedCreate(w, r)
	if !ok {
		return
	}
	pl := plan.Inspect(s.cfg(), req.Target, req.Payload)
	resp := validateResponse{OK: pl.OK(), Target: req.Target,
		Resolvents: make([]validateResolvent, 0, len(pl.Found)), Problems: make([]plan.Problem, 0, len(pl.Problems))}
	resp.Problems = append(resp.Problems, pl.Problems...)
	for _, f := range pl.Found {
		v := validateResolvent{Type: f.Type, Path: f.Path, Name: f.Name, DependsOn: f.DependsOn()}
		// A chained resolvent's cache key depends on series not fetched
		// yet, so only an independent one can be looked up.
		if len(v.DependsOn) == 0 {
			_, cached, err := s.store.GetSeries(r.Context(), f.Hash)
			v.Cached = err == nil && cached
		}
		resp.Resolvents = append(resp.Resolvents, v)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleTargets lists the configured targets: routing knobs, never URLs or
// credentials, so a frontend can offer them without reading the server's
// configuration.
func (s *Server) handleTargets(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authFromHeader(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": plan.Targets(s.cfg())})
}

// handleResolvents lists the configured resolvent types and their backend kind.
func (s *Server) handleResolvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authFromHeader(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": plan.Resolvents(s.cfg())})
}

// acceptJob persists the request as a new job — or replays the stored one
// under its Idempotency-Key — and answers 202 with the job's current state.
func (s *Server) acceptJob(w http.ResponseWriter, r *http.Request, req createRequest, client string) {
	stored, created, err := s.storeSubmission(r.Context(), req, client, r.Header.Get("Idempotency-Key"))
	if errors.Is(err, store.ErrIdempotencyConflict) {
		writeError(w, http.StatusConflict, CodeIdempotencyConflict,
			"Idempotency-Key was already used with a different target or payload")
		return
	}
	if err != nil {
		s.internalError(w, "create job failed", err)
		return
	}
	if created {
		s.wakeWorkers()
	}
	writeJSON(w, http.StatusAccepted, accepted(stored))
}

// storeSubmission turns one validated submission into a stored job, or
// replays the job stored under its idempotency key. The client's API key
// lives outside the payload and is never stored.
func (s *Server) storeSubmission(ctx context.Context, req createRequest, client, idempotencyKey string) (stored *store.Job, created bool, err error) {
	id, err := store.NewID()
	if err != nil {
		return nil, false, fmt.Errorf("id generation failed: %w", err)
	}
	job := &store.Job{
		ID:             id,
		Client:         client,
		IdempotencyKey: idempotencyKey,
		Target:         req.Target,
		MaxAttempts:    s.cfg().MaxAttemptsFor(req.Target),
		Payload:        req.Payload,
		Options:        req.jobOptions(),
		CallbackURL:    req.CallbackURL,
	}
	if req.Priority != nil {
		job.Priority = *req.Priority
	}
	if req.NotBefore != "" {
		t, _ := time.Parse(time.RFC3339, req.NotBefore) // validated by validateSubmission
		job.NotBefore = &t
	}
	created, stored, err = s.store.CreateJob(ctx, job)
	if err != nil {
		return nil, false, err
	}
	if created {
		s.logger.Info("job accepted", "job_id", stored.ID, "target", stored.Target, "client", client)
	}
	return stored, created, nil
}

func accepted(j *store.Job) createResponse {
	return createResponse{ID: j.ID, State: j.State, Links: map[string]string{"self": "/v1/requests/" + j.ID}}
}

// maxBatchItems bounds one batch submission.
const maxBatchItems = 100

// batchItem is one submission inside a batch; the idempotency key travels in
// the body because a header cannot be per item.
type batchItem struct {
	createRequest
	IdempotencyKey string `json:"idempotency_key"`
}

type batchRequest struct {
	APIKey   string      `json:"api_key"`
	Requests []batchItem `json:"requests"`
}

// batchResult is one item's outcome: an accepted (or replayed) request, or
// the error that item alone produced.
type batchResult struct {
	ID    string            `json:"id,omitempty"`
	State string            `json:"state,omitempty"`
	Links map[string]string `json:"links,omitempty"`
	Error *errorDetail      `json:"error,omitempty"`
}

// handleBatch submits up to maxBatchItems requests in one call. Items are
// validated and stored independently: the answer lists a result per item in
// order, 202 when at least one was accepted, 400 when none was.
func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	var req batchRequest
	if !requireJSON(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg().Server.MaxBodyBytes)
	if !decodeSingleObject(w, r.Body, &req) {
		return
	}
	if header := r.Header.Get("X-API-Key"); header != "" {
		req.APIKey = header
	}
	id, ok := s.authenticate(req.APIKey)
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "invalid API key")
		return
	}
	noteClient(r, id.name)
	if len(req.Requests) == 0 || len(req.Requests) > maxBatchItems {
		writeError(w, http.StatusBadRequest, CodeInvalidParameter,
			fmt.Sprintf("requests must hold between 1 and %d items", maxBatchItems))
		return
	}
	results := make([]batchResult, 0, len(req.Requests))
	acceptedCount := 0
	for _, item := range req.Requests {
		res := s.submitBatchItem(r.Context(), item, id)
		if res.Error == nil {
			acceptedCount++
		}
		results = append(results, res)
	}
	if acceptedCount > 0 {
		s.wakeWorkers()
	}
	status := http.StatusAccepted
	if acceptedCount == 0 {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{"items": results})
}

// submitBatchItem validates and stores one item, mapping every failure onto
// the same code the single endpoint would answer with.
func (s *Server) submitBatchItem(ctx context.Context, item batchItem, id identity) batchResult {
	if _, code, msg := validateSubmission(item.createRequest); code != "" {
		return batchResult{Error: &errorDetail{Code: code, Message: msg}}
	}
	if _, ok := s.cfg().Targets[item.Target]; !ok {
		return batchResult{Error: &errorDetail{Code: CodeUnknownTarget, Message: "target " + strconv.Quote(item.Target) + " is not configured"}}
	}
	if limit := s.cfg().MaxPriorityFor(id.name); item.Priority != nil && *item.Priority > limit {
		return batchResult{Error: &errorDetail{Code: CodeInvalidParameter, Message: fmt.Sprintf("priority %d exceeds this key's maximum of %d", *item.Priority, limit)}}
	}
	if len(item.IdempotencyKey) > maxIdempotencyKeyLen {
		return batchResult{Error: &errorDetail{Code: CodeInvalidParameter, Message: "idempotency_key must be at most 255 bytes"}}
	}
	if item.CallbackURL != "" {
		if err := callback.Check(s.cfg().Callbacks, item.CallbackURL); err != nil {
			return batchResult{Error: &errorDetail{Code: CodeCallbackNotAllowed, Message: err.Error()}}
		}
	}
	stored, _, err := s.storeSubmission(ctx, item.createRequest, id.name, item.IdempotencyKey)
	switch {
	case errors.Is(err, store.ErrIdempotencyConflict):
		return batchResult{Error: &errorDetail{Code: CodeIdempotencyConflict, Message: "idempotency_key was already used with a different target or payload"}}
	case err != nil:
		s.logger.Error("create job failed", "error", err)
		return batchResult{Error: &errorDetail{Code: CodeInternal, Message: "internal server error"}}
	}
	a := accepted(stored)
	return batchResult{ID: a.ID, State: a.State, Links: a.Links}
}

// wakeWorkers nudges the pool without blocking: a full channel means a
// wake-up is already pending.
func (s *Server) wakeWorkers() {
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

// The job document is shared with completion callbacks (see jobview).
type (
	jobResponse = jobview.Job
	jobOptions  = jobview.Options
)

// jobFromPath loads the job addressed by the {id} path segment, writing the
// 404/500 response itself when it cannot.
func (s *Server) jobFromPath(w http.ResponseWriter, r *http.Request) (*store.Job, bool) {
	job, err := s.store.GetJob(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such request")
		return nil, false
	}
	if err != nil {
		s.internalError(w, "get job failed", err)
		return nil, false
	}
	return job, true
}

// jobForCaller loads the addressed job and enforces read scoping: a job the
// caller may not see answers 404 exactly like an unknown id, so ids cannot
// be probed across clients.
func (s *Server) jobForCaller(w http.ResponseWriter, r *http.Request, id identity) (*store.Job, bool) {
	job, ok := s.jobFromPath(w, r)
	if !ok {
		return nil, false
	}
	if !id.mayRead(job) {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such request")
		return nil, false
	}
	return job, true
}

// internalError logs the cause and answers with the opaque 500 error body.
func (s *Server) internalError(w http.ResponseWriter, logMsg string, err error) {
	s.logger.Error(logMsg, "error", err)
	writeError(w, http.StatusInternalServerError, CodeInternal, "internal server error")
}

// handleGet reports a request. With ?wait=DURATION it blocks until the
// request is terminal or the wait elapses (clamped below the write timeout),
// then answers with the freshest state — one call instead of a polling loop.
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authFromHeader(w, r)
	if !ok {
		return
	}
	job, ok := s.jobForCaller(w, r, id)
	if !ok {
		return
	}
	wait, ok := waitParam(w, r, s.maxWait())
	if !ok {
		return
	}
	if wait > 0 && !store.IsTerminal(job.State) {
		job = s.awaitTerminal(r.Context(), job, wait)
	}
	resp := toJobResponse(job)
	resp.Callback = s.callbackInfo(r, job)
	writeJSON(w, http.StatusOK, resp)
}

// callbackInfo describes a request's completion callback: pending until the
// request is terminal and delivered, then delivered or failed.
func (s *Server) callbackInfo(r *http.Request, job *store.Job) *jobview.CallbackInfo {
	if job.CallbackURL == "" {
		return nil
	}
	info := &jobview.CallbackInfo{URL: job.CallbackURL, State: store.DeliveryPending}
	del, err := s.store.GetDelivery(r.Context(), job.ID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.logger.Error("get callback delivery failed", "job_id", job.ID, "error", err)
		}
		return info
	}
	info.State, info.Attempts, info.LastStatus, info.LastError = del.State(), del.Attempts, del.LastStatus, del.LastError
	return info
}

// maxWait bounds a long poll safely below the server's write timeout, which
// would otherwise cut the response off; without a configured timeout the
// cap is a minute.
func (s *Server) maxWait() time.Duration {
	if wt := s.cfg().Server.WriteTimeout.Std(); wt > 0 {
		return max(wt-5*time.Second, time.Second)
	}
	return time.Minute
}

// waitParam parses ?wait=; longer waits are clamped to the cap.
func waitParam(w http.ResponseWriter, r *http.Request, maxWait time.Duration) (time.Duration, bool) {
	raw := r.URL.Query().Get("wait")
	if raw == "" {
		return 0, true
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		writeError(w, http.StatusBadRequest, CodeInvalidParameter, "wait must be a non-negative duration such as 25s")
		return 0, false
	}
	return min(d, maxWait), true
}

// awaitTerminal blocks until the job is terminal, the wait elapses or the
// client goes away, re-reading the job on every wake-up. The notifier is
// registered before each read, so a transition in between cannot be missed;
// a one-second fallback re-read covers a missing notifier.
func (s *Server) awaitTerminal(ctx context.Context, job *store.Job, wait time.Duration) *store.Job {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	fallback := time.NewTicker(time.Second)
	defer fallback.Stop()
	for {
		woke := s.notifier.Wait(job.ID)
		fresh, err := s.store.GetJob(ctx, job.ID)
		if err == nil {
			job = fresh
		}
		if err != nil || store.IsTerminal(job.State) {
			s.notifier.Forget(job.ID, woke)
			return job
		}
		select {
		case <-woke:
		case <-fallback.C:
		case <-deadline.C:
			s.notifier.Forget(job.ID, woke)
			return job
		case <-ctx.Done():
			s.notifier.Forget(job.ID, woke)
			return job
		}
		s.notifier.Forget(job.ID, woke)
	}
}

type listResponse struct {
	Items []jobResponse `json:"items"`
	// NextCursor is present only when more items exist; pass it back as the
	// cursor query parameter to fetch the next page.
	NextCursor string `json:"next_cursor,omitempty"`
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authFromHeader(w, r)
	if !ok {
		return
	}
	filter, ok := listParams(w, r, id)
	if !ok {
		return
	}
	s.writeJobPage(w, r, filter)
}

// writeJobPage answers one page of jobs for the filter, with the cursor of
// the next page when there is one.
func (s *Server) writeJobPage(w http.ResponseWriter, r *http.Request, filter store.ListFilter) {
	pageSize := filter.Limit
	filter.Limit++ // one extra row tells whether a next page exists
	jobs, err := s.store.ListJobs(r.Context(), filter)
	if err != nil {
		s.internalError(w, "list jobs failed", err)
		return
	}
	resp := listResponse{Items: make([]jobResponse, 0, len(jobs))}
	if len(jobs) > pageSize {
		jobs = jobs[:pageSize]
		resp.NextCursor = encodeCursor(jobs[pageSize-1])
	}
	for _, j := range jobs {
		resp.Items = append(resp.Items, toJobResponse(j))
	}
	writeJSON(w, http.StatusOK, resp)
}

// listParams validates the list query and builds the store filter. A client
// key lists only its own requests; the client parameter is for admin keys.
func listParams(w http.ResponseWriter, r *http.Request, id identity) (store.ListFilter, bool) {
	q := r.URL.Query()
	f := store.ListFilter{State: q.Get("state"), Target: q.Get("target"), Client: id.name, Limit: 50}
	switch f.State {
	case "", store.StateReceived, store.StateResolving, store.StateForwarding,
		store.StateAwaitingTarget, store.StateCompleted, store.StateFailed, store.StateCancelled:
	default:
		writeError(w, http.StatusBadRequest, CodeInvalidParameter, "unknown state filter "+strconv.Quote(f.State))
		return f, false
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, CodeInvalidParameter, "limit must be a positive integer")
			return f, false
		}
		f.Limit = min(n, 200)
	}
	if client := q.Get("client"); client != "" {
		if !id.admin() && client != id.name {
			writeError(w, http.StatusBadRequest, CodeInvalidParameter, "the client filter requires an admin key")
			return f, false
		}
		f.Client = client
	} else if id.admin() {
		f.Client = "" // admins list every client's requests
	}
	return listWindow(w, q, f)
}

// listWindow parses the time bounds and the page cursor.
func listWindow(w http.ResponseWriter, q url.Values, f store.ListFilter) (store.ListFilter, bool) {
	for name, dst := range map[string]**time.Time{"since": &f.Since, "until": &f.Until} {
		raw := q.Get(name)
		if raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidParameter, name+" must be an RFC 3339 timestamp")
			return f, false
		}
		*dst = &t
	}
	if token := q.Get("cursor"); token != "" {
		c, err := decodeCursor(token)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidParameter, "cursor is not a valid page token")
			return f, false
		}
		f.Before = c
	}
	return f, true
}

// encodeCursor renders a list position as an opaque token: the last item's
// creation time in Unix milliseconds (the store's precision) and its
// insertion sequence.
func encodeCursor(j *store.Job) string {
	raw := strconv.FormatInt(j.CreatedAt.UnixMilli(), 10) + "." + strconv.FormatInt(j.Seq, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(token string) (*store.Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, err
	}
	msPart, seqPart, ok := strings.Cut(string(raw), ".")
	if !ok {
		return nil, errors.New("cursor: missing separator")
	}
	ms, err := strconv.ParseInt(msPart, 10, 64)
	if err != nil {
		return nil, err
	}
	seq, err := strconv.ParseInt(seqPart, 10, 64)
	if err != nil {
		return nil, err
	}
	return &store.Cursor{CreatedAt: time.UnixMilli(ms).UTC(), Seq: seq}, nil
}

func (s *Server) handleResult(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authFromHeader(w, r)
	if !ok {
		return
	}
	job, ok := s.jobForCaller(w, r, id)
	if !ok {
		return
	}
	if job.State != store.StateCompleted {
		writeError(w, http.StatusNotFound, CodeNotFound, "result not available (state: "+job.State+")")
		return
	}
	switch {
	case job.ResultPath != "":
		s.serveResultFile(w, r, job)
	case len(job.TargetResponse) > 0 && json.Valid(job.TargetResponse):
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(job.TargetResponse)))
		_, _ = w.Write(job.TargetResponse) // #nosec G705 -- served as application/json, never rendered as HTML
	default:
		writeError(w, http.StatusNotFound, CodeNotFound, "job completed without a stored result body")
	}
}

// serveResultFile streams a file-backed result with its recorded content
// type. ServeContent supplies Content-Length, Accept-Ranges, Range (206)
// and conditional requests, so a client can resume a large bundle; HEAD
// gets the headers alone. The retention sweeper removes files just before
// their rows; a request landing in that window gets a 404, not a 500.
func (s *Server) serveResultFile(w http.ResponseWriter, r *http.Request, job *store.Job) {
	f, err := os.Open(job.ResultPath) // #nosec G304 G703 -- path is written by the worker, never taken from request input
	if errors.Is(err, fs.ErrNotExist) {
		s.logger.Warn("result file already pruned", "job_id", job.ID, "path", job.ResultPath)
		writeError(w, http.StatusNotFound, CodeNotFound, "result no longer available")
		return
	}
	if err != nil {
		s.logger.Error("result file unreadable", "job_id", job.ID, "path", job.ResultPath, "error", err)
		writeError(w, http.StatusInternalServerError, CodeInternal, "stored result is unavailable")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		s.logger.Error("result file unreadable", "job_id", job.ID, "path", job.ResultPath, "error", err)
		writeError(w, http.StatusInternalServerError, CodeInternal, "stored result is unavailable")
		return
	}
	ct := job.ResultContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	// A pre-set Content-Type keeps ServeContent from sniffing the file.
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(job.ResultPath)}))
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// handleCancel ends a request at the client's wish. A queued request is
// cancelled at once; one awaiting its target is cancelled and, when the
// target has a cancel_url_template, the target is told to stop its job (best
// effort, in the background). A request a worker is processing right now
// answers 409 not_cancellable — retry once it is queued again or awaiting
// the target — and a finished request answers 409 as well.
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authFromHeader(w, r)
	if !ok {
		return
	}
	job, ok := s.jobForCaller(w, r, id)
	if !ok {
		return
	}
	err := s.store.MarkCancelled(r.Context(), job.ID, "cancelled by client "+id.name)
	switch {
	case errors.Is(err, store.ErrNotCancellable):
		writeError(w, http.StatusConflict, CodeNotCancellable,
			"the request is being processed right now (state "+job.State+"); retry once it is queued again or awaiting its target")
		return
	case errors.Is(err, store.ErrTerminalState):
		writeError(w, http.StatusConflict, CodeNotCancellable, "the request already finished (state "+job.State+")")
		return
	case err != nil:
		s.internalError(w, "cancel failed", err)
		return
	}
	s.logger.Info("job cancelled", "job_id", job.ID, "target", job.Target, "client", id.name, "was", job.State)
	s.notifier.Notify(job.ID)
	if job.State == store.StateAwaitingTarget {
		s.notifyTargetCancel(job)
	}
	updated, err := s.store.GetJob(r.Context(), job.ID)
	if err != nil {
		s.internalError(w, "get job failed", err)
		return
	}
	writeJSON(w, http.StatusOK, toJobResponse(updated))
}

// notifyTargetCancel tells a poll-mode target to stop the job tentacron just
// cancelled, when the target offers a cancel URL. Fire and forget, bounded by
// the target's timeout: the request is cancelled whatever the target says.
func (s *Server) notifyTargetCancel(job *store.Job) {
	tcfg, ok := s.cfg().Targets[job.Target]
	if !ok || s.upstream == nil || tcfg.Response.Poll == nil || tcfg.Response.Poll.CancelURLTemplate == "" || job.TargetJobID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), tcfg.Timeout.Std())
		defer cancel()
		if err := s.upstream.CancelTarget(ctx, job.Target, tcfg, job.TargetJobID); err != nil {
			s.logger.Warn("target cancel failed", "job_id", job.ID, "target", job.Target, "target_job_id", job.TargetJobID, "error", err)
			return
		}
		s.logger.Info("target job cancelled", "job_id", job.ID, "target", job.Target, "target_job_id", job.TargetJobID)
	}()
}

// eventTimeLayout keeps the store's millisecond precision: several
// transitions of one job often fall into the same second.
const eventTimeLayout = "2006-01-02T15:04:05.000Z07:00"

type eventResponse struct {
	FromState string `json:"from_state,omitempty"`
	ToState   string `json:"to_state"`
	Detail    string `json:"detail"`
	CreatedAt string `json:"created_at"`
}

// handleEvents serves a job's audit trail in chronological order, under the
// same read scoping as the job itself.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authFromHeader(w, r)
	if !ok {
		return
	}
	job, ok := s.jobForCaller(w, r, id)
	if !ok {
		return
	}
	events, err := s.store.ListEvents(r.Context(), job.ID)
	if err != nil {
		s.internalError(w, "list events failed", err)
		return
	}
	items := make([]eventResponse, 0, len(events))
	for _, e := range events {
		items = append(items, eventResponse{
			FromState: e.FromState, ToState: e.ToState, Detail: e.Detail,
			CreatedAt: e.CreatedAt.UTC().Format(eventTimeLayout),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.Build.Version})
}

// handleVersion is unauthenticated: it tells an operator which build answers
// without revealing configuration.
func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Build)
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal, "database not reachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func toJobResponse(j *store.Job) jobResponse { return jobview.From(j) }

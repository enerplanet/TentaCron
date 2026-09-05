package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
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

	"github.com/enerplanet/tentacron/internal/store"
)

type createRequest struct {
	APIKey  string          `json:"api_key"`
	Target  string          `json:"target"`
	Payload json.RawMessage `json:"payload"`
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
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Server.MaxBodyBytes)
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
	switch {
	case req.APIKey == "":
		return http.StatusBadRequest, CodeMissingField, "an API key is required: send the X-API-Key header (or the api_key field)"
	case req.Target == "":
		return http.StatusBadRequest, CodeMissingField, "target is required"
	case len(req.Payload) == 0 || string(req.Payload) == "null":
		return http.StatusBadRequest, CodeMissingField, "payload is required"
	case !strings.HasPrefix(strings.TrimSpace(string(req.Payload)), "{"):
		return http.StatusBadRequest, CodeInvalidJSON, "payload must be a JSON object"
	}
	return 0, "", ""
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	req, ok := s.decodeCreateRequest(w, r)
	if !ok {
		return
	}
	id, ok := s.authenticate(req.APIKey)
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "invalid API key")
		return
	}
	noteClient(r, id.name)
	if _, ok := s.cfg.Targets[req.Target]; !ok {
		writeError(w, http.StatusUnprocessableEntity, CodeUnknownTarget,
			"target "+strconv.Quote(req.Target)+" is not configured")
		return
	}
	s.acceptJob(w, r, req, id.name)
}

// acceptJob persists the request as a new job — or replays the stored one
// under its Idempotency-Key — and answers 202 with the job's current state.
func (s *Server) acceptJob(w http.ResponseWriter, r *http.Request, req createRequest, client string) {
	id, err := store.NewID()
	if err != nil {
		s.internalError(w, "id generation failed", err)
		return
	}
	job := &store.Job{
		ID:             id,
		Client:         client,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
		Target:         req.Target,
		MaxAttempts:    s.cfg.MaxAttemptsFor(req.Target),
		Payload:        req.Payload, // client api_key lives outside payload and is never stored
	}
	created, stored, err := s.store.CreateJob(r.Context(), job)
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
		s.logger.Info("job accepted", "job_id", stored.ID, "target", stored.Target, "client", client)
		s.wakeWorkers()
	}
	writeJSON(w, http.StatusAccepted, createResponse{
		ID:    stored.ID,
		State: stored.State,
		Links: map[string]string{"self": "/v1/requests/" + stored.ID},
	})
}

// wakeWorkers nudges the pool without blocking: a full channel means a
// wake-up is already pending.
func (s *Server) wakeWorkers() {
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

type jobResponse struct {
	ID          string      `json:"id"`
	Target      string      `json:"target"`
	State       string      `json:"state"`
	Attempts    int         `json:"attempts"`
	TargetJobID string      `json:"target_job_id,omitempty"`
	CreatedAt   string      `json:"created_at"`
	UpdatedAt   string      `json:"updated_at"`
	CompletedAt *string     `json:"completed_at,omitempty"`
	Result      *resultInfo `json:"result"`
	Error       *errorInfo  `json:"error"`
}

type resultInfo struct {
	TargetStatus   int             `json:"target_status"`
	TargetResponse json.RawMessage `json:"target_response,omitempty"`
	Href           string          `json:"href,omitempty"`
	ContentType    string          `json:"content_type,omitempty"`
}

type errorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

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

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authFromHeader(w, r)
	if !ok {
		return
	}
	job, ok := s.jobForCaller(w, r, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, toJobResponse(job))
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
		store.StateAwaitingTarget, store.StateCompleted, store.StateFailed:
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

func toJobResponse(j *store.Job) jobResponse {
	resp := jobResponse{
		ID:          j.ID,
		Target:      j.Target,
		State:       j.State,
		Attempts:    j.Attempts,
		TargetJobID: j.TargetJobID,
		CreatedAt:   j.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   j.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if j.CompletedAt != nil {
		s := j.CompletedAt.UTC().Format(time.RFC3339)
		resp.CompletedAt = &s
	}
	if j.State == store.StateCompleted {
		resp.Result = resultInfoFor(j)
	}
	if j.State == store.StateFailed {
		resp.Error = &errorInfo{Code: j.ErrorCode, Message: j.ErrorMessage}
	}
	return resp
}

// resultInfoFor describes a completed job's result: a file-backed result is
// referenced by href, inline JSON is embedded.
func resultInfoFor(j *store.Job) *resultInfo {
	if j.TargetStatus == nil {
		return nil
	}
	res := &resultInfo{TargetStatus: *j.TargetStatus}
	switch {
	case j.ResultPath != "":
		res.Href = "/v1/requests/" + j.ID + "/result"
		res.ContentType = j.ResultContentType
	case len(j.TargetResponse) > 0:
		res.TargetResponse = rawOrQuoted(j.TargetResponse)
	}
	return res
}

// rawOrQuoted embeds upstream bytes as-is when they are valid JSON and as a
// JSON string otherwise, so our own response never becomes malformed.
func rawOrQuoted(b []byte) json.RawMessage {
	if json.Valid(b) {
		return b
	}
	quoted, _ := json.Marshal(string(b))
	return quoted
}

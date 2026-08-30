package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
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

// decodeCreateRequest parses and validates the create-request body, writing
// the client error response itself when the request is unusable.
func (s *Server) decodeCreateRequest(w http.ResponseWriter, r *http.Request) (createRequest, bool) {
	var req createRequest
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType,
				"Content-Type must be application/json")
			return req, false
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Server.MaxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
				"request body exceeds the configured limit")
			return req, false
		}
		writeError(w, http.StatusBadRequest, CodeInvalidJSON, "request body is not valid JSON")
		return req, false
	}

	switch {
	case req.APIKey == "":
		writeError(w, http.StatusBadRequest, CodeMissingField, "api_key is required")
		return req, false
	case req.Target == "":
		writeError(w, http.StatusBadRequest, CodeMissingField, "target is required")
		return req, false
	case len(req.Payload) == 0 || string(req.Payload) == "null":
		writeError(w, http.StatusBadRequest, CodeMissingField, "payload is required")
		return req, false
	}
	if trimmed := strings.TrimSpace(string(req.Payload)); !strings.HasPrefix(trimmed, "{") {
		writeError(w, http.StatusBadRequest, CodeInvalidJSON, "payload must be a JSON object")
		return req, false
	}
	return req, true
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	req, ok := s.decodeCreateRequest(w, r)
	if !ok {
		return
	}
	client, ok := s.authenticate(req.APIKey)
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "invalid api_key")
		return
	}
	if _, ok := s.cfg.Targets[req.Target]; !ok {
		writeError(w, http.StatusUnprocessableEntity, CodeUnknownTarget,
			"target "+strconv.Quote(req.Target)+" is not configured")
		return
	}

	id, err := store.NewID()
	if err != nil {
		s.internalError(w, "id generation failed", err)
		return
	}
	job := &store.Job{
		ID:             id,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
		Target:         req.Target,
		MaxAttempts:    s.cfg.Worker.MaxAttempts,
		Payload:        req.Payload, // client api_key lives outside payload and is never stored
	}
	created, stored, err := s.store.CreateJob(r.Context(), job)
	if err != nil {
		s.internalError(w, "create job failed", err)
		return
	}
	if created {
		s.logger.Info("job accepted", "job_id", stored.ID, "target", stored.Target, "client", client)
		select {
		case s.nudge <- struct{}{}:
		default:
		}
	}
	writeJSON(w, http.StatusAccepted, createResponse{
		ID:    stored.ID,
		State: stored.State,
		Links: map[string]string{"self": "/v1/requests/" + stored.ID},
	})
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

// internalError logs the cause and answers with the opaque 500 error body.
func (s *Server) internalError(w http.ResponseWriter, logMsg string, err error) {
	s.logger.Error(logMsg, "error", err)
	writeError(w, http.StatusInternalServerError, CodeInternal, "internal server error")
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	if !s.authFromHeader(w, r) {
		return
	}
	job, ok := s.jobFromPath(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, toJobResponse(job))
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	if !s.authFromHeader(w, r) {
		return
	}
	state := r.URL.Query().Get("state")
	switch state {
	case "", store.StateReceived, store.StateResolving, store.StateForwarding,
		store.StateAwaitingTarget, store.StateCompleted, store.StateFailed:
	default:
		writeError(w, http.StatusBadRequest, CodeInvalidParameter, "unknown state filter "+strconv.Quote(state))
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, CodeInvalidParameter, "limit must be a positive integer")
			return
		}
		limit = min(n, 200)
	}
	jobs, err := s.store.ListJobs(r.Context(), state, limit)
	if err != nil {
		s.internalError(w, "list jobs failed", err)
		return
	}
	items := make([]jobResponse, 0, len(jobs))
	for _, j := range jobs {
		items = append(items, toJobResponse(j))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleResult(w http.ResponseWriter, r *http.Request) {
	if !s.authFromHeader(w, r) {
		return
	}
	job, ok := s.jobFromPath(w, r)
	if !ok {
		return
	}
	if job.State != store.StateCompleted {
		writeError(w, http.StatusNotFound, CodeNotFound, "result not available (state: "+job.State+")")
		return
	}
	if job.ResultPath != "" {
		f, err := os.Open(job.ResultPath) // #nosec G304 G703 -- path is written by the worker, never taken from request input
		if err != nil {
			s.logger.Error("result file missing", "job_id", job.ID, "path", job.ResultPath, "error", err)
			writeError(w, http.StatusInternalServerError, CodeInternal, "stored result is unavailable")
			return
		}
		defer f.Close()
		ct := job.ResultContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
		_, _ = io.Copy(w, f)
		return
	}
	if len(job.TargetResponse) > 0 && json.Valid(job.TargetResponse) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(job.TargetResponse) // #nosec G705 -- served as application/json, never rendered as HTML
		return
	}
	writeError(w, http.StatusNotFound, CodeNotFound, "job completed without a stored result body")
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
	if j.State == store.StateCompleted && j.TargetStatus != nil {
		res := &resultInfo{TargetStatus: *j.TargetStatus}
		if j.ResultPath != "" {
			res.Href = "/v1/requests/" + j.ID + "/result"
			res.ContentType = j.ResultContentType
		} else if len(j.TargetResponse) > 0 {
			res.TargetResponse = rawOrQuoted(j.TargetResponse)
		}
		resp.Result = res
	}
	if j.State == store.StateFailed {
		resp.Error = &errorInfo{Code: j.ErrorCode, Message: j.ErrorMessage}
	}
	return resp
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

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/enerplanet/tentacron/internal/plan"
	"github.com/enerplanet/tentacron/internal/schedule"
	"github.com/enerplanet/tentacron/internal/store"
)

// maxSchedulesListed bounds one schedule listing; schedules are few by
// nature, so there is no cursor.
const maxSchedulesListed = 100

// scheduleRequest creates a recurring submission.
type scheduleRequest struct {
	APIKey   string          `json:"api_key"`
	Target   string          `json:"target"`
	Payload  json.RawMessage `json:"payload"`
	Cron     string          `json:"cron"`
	Timezone string          `json:"timezone"`
	Priority *int            `json:"priority"`
	Options  *requestOptions `json:"options"`
}

type scheduleResponse struct {
	ID        string            `json:"id"`
	Target    string            `json:"target"`
	Cron      string            `json:"cron"`
	Timezone  string            `json:"timezone"`
	Priority  int               `json:"priority,omitempty"`
	Options   *jobOptions       `json:"options,omitempty"`
	NextRunAt string            `json:"next_run_at"`
	LastRunAt *string           `json:"last_run_at,omitempty"`
	LastJobID string            `json:"last_job_id,omitempty"`
	CreatedAt string            `json:"created_at"`
	UpdatedAt string            `json:"updated_at"`
	Links     map[string]string `json:"links"`
}

func toScheduleResponse(sc *store.Schedule) scheduleResponse {
	resp := scheduleResponse{
		ID: sc.ID, Target: sc.Target, Cron: sc.Cron, Timezone: sc.Timezone, Priority: sc.Priority,
		LastJobID: sc.LastJobID,
		CreatedAt: sc.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: sc.UpdatedAt.UTC().Format(time.RFC3339),
		Links:     map[string]string{"self": "/v1/schedules/" + sc.ID, "runs": "/v1/schedules/" + sc.ID + "/runs"},
	}
	if sc.NextRunAt != nil {
		resp.NextRunAt = sc.NextRunAt.UTC().Format(time.RFC3339)
	}
	if sc.LastRunAt != nil {
		s := sc.LastRunAt.UTC().Format(time.RFC3339)
		resp.LastRunAt = &s
	}
	if !sc.Options.IsZero() {
		resp.Options = &jobOptions{Cache: sc.Options.Cache}
	}
	return resp
}

// handleCreateSchedule validates a recurring submission exactly like a single
// one — plus its cron expression and time zone — and stores it with its
// first due time. The payload is inspected as a dry run would, so a schedule
// whose every run would fail is refused up front.
func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	var req scheduleRequest
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
	spec, ok := s.validateSchedule(w, req, id)
	if !ok {
		return
	}
	scID, err := store.NewID()
	if err != nil {
		s.internalError(w, "id generation failed", err)
		return
	}
	tz := req.Timezone
	if tz == "" {
		tz = "UTC"
	}
	next := spec.Next(time.Now())
	sc := &store.Schedule{
		ID: scID, Client: id.name, Target: req.Target, Payload: req.Payload, Cron: req.Cron, Timezone: tz,
		Options: createRequest{Options: req.Options}.jobOptions(), NextRunAt: &next,
	}
	if req.Priority != nil {
		sc.Priority = *req.Priority
	}
	if err := s.store.CreateSchedule(r.Context(), sc); err != nil {
		s.internalError(w, "create schedule failed", err)
		return
	}
	s.logger.Info("schedule created", "schedule_id", sc.ID, "target", sc.Target, "client", id.name, "cron", sc.Cron)
	writeJSON(w, http.StatusCreated, toScheduleResponse(sc))
}

// validateSchedule applies the single-submission checks, the key's priority
// cap, the cron syntax and the payload inspection.
func (s *Server) validateSchedule(w http.ResponseWriter, req scheduleRequest, id identity) (schedule.Spec, bool) {
	sub := createRequest{Target: req.Target, Payload: req.Payload, Priority: req.Priority, Options: req.Options}
	if status, code, msg := validateSubmission(sub); code != "" {
		writeError(w, status, code, msg)
		return schedule.Spec{}, false
	}
	if _, ok := s.cfg().Targets[req.Target]; !ok {
		writeError(w, http.StatusUnprocessableEntity, CodeUnknownTarget, "target "+strconv.Quote(req.Target)+" is not configured")
		return schedule.Spec{}, false
	}
	if limit := s.cfg().MaxPriorityFor(id.name); req.Priority != nil && *req.Priority > limit {
		writeError(w, http.StatusBadRequest, CodeInvalidParameter, fmt.Sprintf("priority %d exceeds this key's maximum of %d", *req.Priority, limit))
		return schedule.Spec{}, false
	}
	spec, err := schedule.Parse(req.Cron, req.Timezone)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidParameter, err.Error())
		return schedule.Spec{}, false
	}
	if pl := plan.Inspect(s.cfg(), req.Target, req.Payload); !pl.OK() {
		writeError(w, http.StatusBadRequest, pl.Problems[0].Code, pl.Problems[0].Message)
		return schedule.Spec{}, false
	}
	return spec, true
}

// handleListSchedules lists the caller's schedules, oldest first; an admin
// key lists every client's, or one client's with ?client=.
func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authFromHeader(w, r)
	if !ok {
		return
	}
	client := id.name
	if id.admin() {
		client = r.URL.Query().Get("client")
	}
	schedules, err := s.store.ListSchedules(r.Context(), client, maxSchedulesListed)
	if err != nil {
		s.internalError(w, "list schedules failed", err)
		return
	}
	items := make([]scheduleResponse, 0, len(schedules))
	for _, sc := range schedules {
		items = append(items, toScheduleResponse(sc))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleGetSchedule(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authFromHeader(w, r)
	if !ok {
		return
	}
	sc, ok := s.scheduleForCaller(w, r, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, toScheduleResponse(sc))
}

// handleDeleteSchedule removes a schedule; runs already created stay.
func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authFromHeader(w, r)
	if !ok {
		return
	}
	sc, ok := s.scheduleForCaller(w, r, id)
	if !ok {
		return
	}
	if err := s.store.DeleteSchedule(r.Context(), sc.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, "delete schedule failed", err)
		return
	}
	s.logger.Info("schedule deleted", "schedule_id", sc.ID, "client", id.name)
	w.WriteHeader(http.StatusNoContent)
}

// handleScheduleRuns lists the jobs a schedule created, newest first, with
// the same filters and cursor paging as the request list.
func (s *Server) handleScheduleRuns(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authFromHeader(w, r)
	if !ok {
		return
	}
	sc, ok := s.scheduleForCaller(w, r, id)
	if !ok {
		return
	}
	filter, ok := listParams(w, r, id)
	if !ok {
		return
	}
	filter.Client, filter.Target, filter.IdempotencyPrefix = sc.Client, "", store.RunKeyPrefix(sc.ID)
	s.writeJobPage(w, r, filter)
}

// scheduleForCaller loads the schedule in the path, hiding other clients'
// schedules behind 404 like requests.
func (s *Server) scheduleForCaller(w http.ResponseWriter, r *http.Request, id identity) (*store.Schedule, bool) {
	sc, err := s.store.GetSchedule(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) || (err == nil && !id.admin() && sc.Client != id.name) {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such schedule")
		return nil, false
	}
	if err != nil {
		s.internalError(w, "get schedule failed", err)
		return nil, false
	}
	return sc, true
}

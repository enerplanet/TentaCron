// Package jobview renders a stored job as the JSON object the API answers
// for GET /v1/requests/{id}. It lives outside the API so a completion
// callback can deliver exactly the same document.
package jobview

import (
	"encoding/json"
	"time"

	"github.com/enerplanet/tentacron/internal/store"
)

// Job is the public shape of a request.
type Job struct {
	ID          string        `json:"id"`
	Target      string        `json:"target"`
	State       string        `json:"state"`
	Attempts    int           `json:"attempts"`
	Priority    int           `json:"priority,omitempty"`
	Options     *Options      `json:"options,omitempty"`
	TargetJobID string        `json:"target_job_id,omitempty"`
	NotBefore   *string       `json:"not_before,omitempty"`
	CreatedAt   string        `json:"created_at"`
	UpdatedAt   string        `json:"updated_at"`
	CompletedAt *string       `json:"completed_at,omitempty"`
	Result      *Result       `json:"result"`
	Error       *Error        `json:"error"`
	Callback    *CallbackInfo `json:"callback,omitempty"`
}

// Options are the processing choices that differ from their defaults.
type Options struct {
	Cache string `json:"cache,omitempty"`
}

// Result describes a completed job's result: inline JSON, or a file
// referenced by href.
type Result struct {
	TargetStatus   int             `json:"target_status"`
	TargetResponse json.RawMessage `json:"target_response,omitempty"`
	Href           string          `json:"href,omitempty"`
	ContentType    string          `json:"content_type,omitempty"`
}

// Error is a failed job's outcome.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// CallbackInfo reports the completion callback of a request: pending until
// the request is terminal and delivered, delivered, or failed after the last
// attempt. It is filled by the API and absent from the callback body itself.
type CallbackInfo struct {
	URL        string `json:"url"`
	State      string `json:"state"`
	Attempts   int    `json:"attempts"`
	LastStatus *int   `json:"last_status,omitempty"`
	LastError  string `json:"last_error,omitempty"`
}

// From renders a job.
func From(j *store.Job) Job {
	resp := Job{
		ID:          j.ID,
		Target:      j.Target,
		State:       j.State,
		Attempts:    j.Attempts,
		Priority:    j.Priority,
		TargetJobID: j.TargetJobID,
		CreatedAt:   j.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   j.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if j.CompletedAt != nil {
		s := j.CompletedAt.UTC().Format(time.RFC3339)
		resp.CompletedAt = &s
	}
	if j.NotBefore != nil {
		s := j.NotBefore.UTC().Format(time.RFC3339)
		resp.NotBefore = &s
	}
	if !j.Options.IsZero() {
		resp.Options = &Options{Cache: j.Options.Cache}
	}
	if j.State == store.StateCompleted {
		resp.Result = ResultFor(j)
	}
	if j.State == store.StateFailed {
		resp.Error = &Error{Code: j.ErrorCode, Message: j.ErrorMessage}
	}
	return resp
}

// ResultFor describes a completed job's result: a file-backed result is
// referenced by href, inline JSON is embedded.
func ResultFor(j *store.Job) *Result {
	if j.TargetStatus == nil {
		return nil
	}
	res := &Result{TargetStatus: *j.TargetStatus}
	switch {
	case j.ResultPath != "":
		res.Href = "/v1/requests/" + j.ID + "/result"
		res.ContentType = j.ResultContentType
	case len(j.TargetResponse) > 0:
		res.TargetResponse = RawOrQuoted(j.TargetResponse)
	}
	return res
}

// RawOrQuoted embeds upstream bytes as-is when they are valid JSON and as a
// JSON string otherwise, so our own document never becomes malformed.
func RawOrQuoted(b []byte) json.RawMessage {
	if json.Valid(b) {
		return b
	}
	quoted, _ := json.Marshal(string(b))
	return quoted
}

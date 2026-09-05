package api

import (
	"encoding/json"
	"net/http"
)

// Client-facing error codes.
const (
	CodeInvalidJSON          = "invalid_json"
	CodeMissingField         = "missing_field"
	CodeUnauthorized         = "unauthorized"
	CodeUnknownTarget        = "unknown_target"
	CodeIdempotencyConflict  = "idempotency_conflict"
	CodePayloadTooLarge      = "payload_too_large"
	CodeUnsupportedMediaType = "unsupported_media_type"
	CodeNotFound             = "not_found"
	CodeMethodNotAllowed     = "method_not_allowed"
	CodeNotCancellable       = "not_cancellable"
	CodeCallbackNotAllowed   = "callback_not_allowed"
	CodeInvalidParameter     = "invalid_parameter"
	CodeInternal             = "internal"
)

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Encoding of our own response types cannot fail; ignore the error to
	// avoid writing a second status line.
	_ = json.NewEncoder(w).Encode(body)
}

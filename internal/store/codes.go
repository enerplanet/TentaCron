package store

// Job error codes: the terminal outcome a failed job carries and the API
// surfaces. Declared once here so the worker, the dry run and the OpenAPI
// description share one list; a test keeps the description's enum equal to
// JobErrorCodes.
const (
	JobCodeInvalidPayload          = "invalid_payload"
	JobCodeUnknownTarget           = "unknown_target"
	JobCodeUnknownResolvent        = "unknown_resolvent"
	JobCodeResourceError           = "resource_error"
	JobCodeInvalidResourceResponse = "invalid_resource_response"
	JobCodeTargetError             = "target_error"
	JobCodeTargetJobFailed         = "target_job_failed"
	JobCodeTargetTimeout           = "target_timeout"
	JobCodeMaxAttemptsExceeded     = "max_attempts_exceeded"
	JobCodeInternal                = "internal"
)

// JobErrorCodes lists every code a failed job can carry, in declaration
// order.
func JobErrorCodes() []string {
	return []string{
		JobCodeInvalidPayload, JobCodeUnknownTarget, JobCodeUnknownResolvent,
		JobCodeResourceError, JobCodeInvalidResourceResponse, JobCodeTargetError,
		JobCodeTargetJobFailed, JobCodeTargetTimeout, JobCodeMaxAttemptsExceeded, JobCodeInternal,
	}
}

// Package upstream performs all outbound HTTP: resource-API resolution calls,
// target forwarding and target polling. It owns timeout handling, response
// size limits and the transient/permanent error classification the worker's
// retry logic builds on.
package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Error is a classified upstream failure.
type Error struct {
	Op        string // e.g. "resource resolvent-pv1", "target meme"
	Status    int    // 0 for transport-level failures
	Transient bool
	Body      string // truncated response excerpt for diagnostics
	Err       error  // underlying transport error, if any
}

func (e *Error) Error() string {
	switch {
	case e.Err != nil:
		return fmt.Sprintf("%s: %v", e.Op, e.Err)
	case e.Body != "":
		return fmt.Sprintf("%s: HTTP %d: %s", e.Op, e.Status, e.Body)
	default:
		return fmt.Sprintf("%s: HTTP %d", e.Op, e.Status)
	}
}

func (e *Error) Unwrap() error { return e.Err }

// IsTransient reports whether err is worth retrying.
func IsTransient(err error) bool {
	var ue *Error
	if errors.As(err, &ue) {
		return ue.Transient
	}
	return false
}

const errBodyExcerpt = 512

// Client is the shared outbound HTTP client.
type Client struct {
	http    *http.Client
	maxBody int64
	logger  *slog.Logger
}

// New builds a Client. maxBody caps every upstream response body.
func New(maxBody int64, logger *slog.Logger) *Client {
	return &Client{
		// Per-call deadlines come from contexts; the transport-level timeout
		// is a safety net against connections that hang forever.
		http:    &http.Client{Timeout: 10 * time.Minute},
		maxBody: maxBody,
		logger:  logger,
	}
}

// do performs one HTTP call and classifies the outcome. 2xx returns the
// (size-capped) body; anything else returns an *Error.
func (c *Client) do(ctx context.Context, op, method, url string, body []byte, headers map[string]string, timeout time.Duration) (int, []byte, error) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(callCtx, method, url, reader)
	if err != nil {
		return 0, nil, &Error{Op: op, Transient: false, Err: err}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Timeouts, refused connections, DNS failures: all retryable.
		return 0, nil, &Error{Op: op, Transient: true, Err: err}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		return 0, nil, &Error{Op: op, Transient: true, Err: fmt.Errorf("read response: %w", err)}
	}
	if int64(len(respBody)) > c.maxBody {
		return 0, nil, &Error{Op: op, Status: resp.StatusCode, Transient: false,
			Err: fmt.Errorf("response exceeds the %d byte limit", c.maxBody)}
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return resp.StatusCode, respBody, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return resp.StatusCode, nil, &Error{Op: op, Status: resp.StatusCode, Transient: true, Body: excerpt(respBody)}
	default:
		return resp.StatusCode, nil, &Error{Op: op, Status: resp.StatusCode, Transient: false, Body: excerpt(respBody)}
	}
}

func newGetRequest(ctx context.Context, url string, headers map[string]string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

func readCapped(r io.Reader, maxBody int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if int64(len(body)) > maxBody {
		return nil, fmt.Errorf("response exceeds the %d byte limit", maxBody)
	}
	return body, nil
}

// excerpt truncates an upstream body for logs and error messages.
func excerpt(b []byte) string {
	s := strings.ToValidUTF8(string(b), "")
	if len(s) > errBodyExcerpt {
		s = s[:errBodyExcerpt] + "…"
	}
	return s
}

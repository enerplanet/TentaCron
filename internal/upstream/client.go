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
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/enerplanet/tentacron/internal/metrics"
)

// Error is a classified upstream failure.
type Error struct {
	Op        string // e.g. "resource resolvent-pv1", "target meme"
	Status    int    // 0 for transport-level failures
	Transient bool
	Body      string // truncated, redacted response excerpt for diagnostics
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

// errBodyTooLarge marks a response over the configured size cap — the one
// read failure that must never be retried (the body will not shrink).
var errBodyTooLarge = errors.New("response exceeds the size limit")

const errBodyExcerpt = 512

// Client is the shared outbound HTTP client.
type Client struct {
	http    *http.Client
	maxBody int64
	secrets []string
	metrics *metrics.Metrics // nil-safe: a nil receiver records nothing
}

// WithMetrics records latency and status class of every outbound call.
func (c *Client) WithMetrics(m *metrics.Metrics) *Client {
	c.metrics = m
	return c
}

// observe reports one finished call. op is "<kind> <name>" ("resource
// resolvent-pv1", "target meme", "poll meme", "result meme").
func (c *Client) observe(op string, status int, err error, d time.Duration) {
	kind, name, _ := strings.Cut(op, " ")
	c.metrics.ObserveUpstream(kind, name, status, err, d)
}

// New builds a Client. maxBody caps every upstream response body; secrets
// lists credential values (target/resource API keys) that must never appear
// in error excerpts — upstream error bodies often echo the request back.
func New(maxBody int64, secrets []string) *Client {
	return &Client{
		http: &http.Client{
			// Per-call deadlines come from contexts; the transport-level
			// timeout is a safety net against connections that hang forever.
			Timeout: 10 * time.Minute,
			// Never follow redirects: Go's default policy forwards custom
			// headers (our injected API keys) to cross-origin redirect
			// targets. A 3xx surfaces as a permanent error instead.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		maxBody: maxBody,
		secrets: secrets,
	}
}

// send builds and performs one HTTP request, classifying transport-level
// failures (request build = permanent, network = transient). The caller owns
// the response body.
func (c *Client) send(ctx context.Context, op, method, url string, body []byte, headers map[string]string) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, &Error{Op: op, Transient: false, Err: err}
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
		return nil, &Error{Op: op, Transient: true, Err: err}
	}
	return resp, nil
}

// do performs one HTTP call, classifies the outcome and records it. 2xx
// returns the (size-capped) body; anything else returns an *Error.
func (c *Client) do(ctx context.Context, op, method, url string, body []byte, headers map[string]string, timeout time.Duration) (int, []byte, error) {
	start := time.Now()
	status, respBody, err := c.call(ctx, op, method, url, body, headers, timeout)
	c.observe(op, status, err, time.Since(start))
	return status, respBody, err
}

func (c *Client) call(ctx context.Context, op, method, url string, body []byte, headers map[string]string, timeout time.Duration) (int, []byte, error) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := c.send(callCtx, op, method, url, body, headers)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	respBody, err := c.readCapped(resp.Body)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			return 0, nil, &Error{Op: op, Status: resp.StatusCode, Transient: false, Err: err}
		}
		return 0, nil, &Error{Op: op, Transient: true, Err: err}
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return resp.StatusCode, respBody, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return resp.StatusCode, nil, &Error{Op: op, Status: resp.StatusCode, Transient: true, Body: c.excerpt(respBody)}
	default:
		return resp.StatusCode, nil, &Error{Op: op, Status: resp.StatusCode, Transient: false, Body: c.excerpt(respBody)}
	}
}

// readCapped reads a response body up to the configured cap. Over-cap
// failures wrap errBodyTooLarge; other errors are plain read failures.
func (c *Client) readCapped(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, c.maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if int64(len(body)) > c.maxBody {
		return nil, fmt.Errorf("response exceeds the %d byte limit: %w", c.maxBody, errBodyTooLarge)
	}
	return body, nil
}

// excerpt prepares an upstream body for logs, stored error messages and API
// responses: configured credentials are redacted before the excerpt is
// truncated, so a secret can never survive by straddling the cut. The cut
// falls on a rune boundary, so the excerpt stays valid UTF-8, and
// surrounding whitespace (the newline http.Error appends) is dropped.
func (c *Client) excerpt(b []byte) string {
	s := strings.ToValidUTF8(string(b), "")
	for _, secret := range c.secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	s = strings.TrimSpace(s)
	if len(s) > errBodyExcerpt {
		cut := errBodyExcerpt
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

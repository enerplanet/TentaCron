// Package callback delivers completion callbacks: once a request is
// terminal, its job document — the same JSON GET /v1/requests/{id} answers —
// is POSTed to the callback_url the request named, signed with HMAC-SHA256,
// and retried with the worker backoff until delivered or given up.
package callback

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/jobview"
	"github.com/enerplanet/tentacron/internal/metrics"
	"github.com/enerplanet/tentacron/internal/notify"
	"github.com/enerplanet/tentacron/internal/store"
)

// Delivery headers.
const (
	HeaderEvent     = "X-Tentacron-Event"
	HeaderSignature = "X-Tentacron-Signature"
	HeaderRequestID = "X-Tentacron-Request-Id"
	HeaderAttempt   = "X-Tentacron-Attempt"

	// maxResponseBytes caps how much of a receiver's answer is read.
	maxResponseBytes = 1024
	dueBatch         = 50
)

// Deliverer runs the delivery loop.
type Deliverer struct {
	cfgp     *config.Provider
	store    *store.Store
	logger   *slog.Logger
	client   *http.Client
	metrics  *metrics.Metrics
	notifier *notify.Hub
	now      func() time.Time
}

// New builds a deliverer with a client that times out per attempt and
// never follows redirects (a redirect could move the signed document to a
// host that was never allow-listed).
func New(cfg *config.Provider, st *store.Store, logger *slog.Logger) *Deliverer {
	d := &Deliverer{cfgp: cfg, store: st, logger: logger, now: time.Now}
	d.WithHTTPClient(&http.Client{})
	return d
}

// config is the configuration current right now, read once per tick.
func (d *Deliverer) config() *config.Config { return d.cfgp.Current() }

// WithHTTPClient replaces the client (tests trust their own certificates);
// the no-redirect policy is enforced on whatever client is given. The
// per-attempt timeout comes from the configuration at call time.
func (d *Deliverer) WithHTTPClient(c *http.Client) *Deliverer {
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	d.client = c
	return d
}

// WithMetrics records delivery outcomes.
func (d *Deliverer) WithMetrics(m *metrics.Metrics) *Deliverer {
	d.metrics = m
	return d
}

// WithNotifier wakes the loop as soon as any job ends.
func (d *Deliverer) WithNotifier(h *notify.Hub) *Deliverer {
	d.notifier = h
	return d
}

// Sign computes the signature header value for a body.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a signature header value against a body in constant time.
func Verify(secret string, body []byte, signature string) bool {
	return hmac.Equal([]byte(Sign(secret, body)), []byte(signature))
}

// Run delivers due callbacks until ctx ends: on every terminal notification
// and, as a fallback for retries, every worker poll interval.
func (d *Deliverer) Run(ctx context.Context) {
	ticker := time.NewTicker(d.config().Worker.PollInterval.Std())
	defer ticker.Stop()
	for {
		d.deliverDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-d.notifier.Terminals():
		case <-ticker.C:
		}
	}
}

// deliverDue attempts every due delivery once, in due order. Each attempt
// re-checks the URL against the allow-list in force: a reload that removed
// the host, or disabled callbacks, turns the attempt into a failed one with
// the reason, so the delivery keeps its backoff schedule — reversible by
// restoring the host — and is given up only when the attempt budget runs
// out, never left pending forever.
func (d *Deliverer) deliverDue(ctx context.Context) {
	due, err := d.store.DueDeliveries(ctx, d.now(), dueBatch)
	if err != nil {
		if ctx.Err() == nil {
			d.logger.Error("list due callbacks failed", "error", err)
		}
		return
	}
	cfg := d.config()
	for _, del := range due {
		if ctx.Err() != nil {
			return
		}
		if err := Check(cfg.Callbacks, del.URL); err != nil {
			d.refuse(ctx, del, err)
			continue
		}
		d.attempt(ctx, del)
	}
}

// refuse records an attempt that was not made because the allow-list in
// force no longer covers the URL.
func (d *Deliverer) refuse(ctx context.Context, del *store.Delivery, reason error) {
	outcome, next := d.classify(del.Attempts+1, 0, reason)
	if err := d.store.RecordAttempt(ctx, del.JobID, del.Attempts, nil, reason.Error(), false, next); err != nil {
		d.logger.Error("record callback attempt failed", "job_id", del.JobID, "error", err)
		return
	}
	d.metrics.CallbackDelivery(outcome)
	d.logger.Warn("callback not sent: the allow-list no longer covers its host",
		"job_id", del.JobID, "event", del.Event, "attempt", del.Attempts+1, "outcome", outcome, "error", reason)
}

// attempt performs one delivery and records its outcome.
func (d *Deliverer) attempt(ctx context.Context, del *store.Delivery) {
	job, err := d.store.GetJob(ctx, del.JobID)
	if err != nil {
		d.logger.Error("callback job vanished", "job_id", del.JobID, "error", err)
		return
	}
	body, err := json.Marshal(jobview.From(job))
	if err != nil {
		d.logger.Error("encode callback body failed", "job_id", del.JobID, "error", err)
		return
	}
	status, err := d.post(ctx, del, body)
	outcome, next := d.classify(del.Attempts+1, status, err)
	var statusPtr *int
	if status != 0 {
		statusPtr = &status
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	if recErr := d.store.RecordAttempt(ctx, del.JobID, del.Attempts, statusPtr, errText, outcome == "delivered", next); recErr != nil {
		d.logger.Error("record callback attempt failed", "job_id", del.JobID, "error", recErr)
		return
	}
	d.metrics.CallbackDelivery(outcome)
	attrs := []any{"job_id", del.JobID, "event", del.Event, "attempt", del.Attempts + 1, "status", status, "outcome", outcome}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	if outcome == "delivered" {
		d.logger.Info("callback delivered", attrs...)
	} else {
		d.logger.Warn("callback attempt failed", attrs...)
	}
}

// post sends the signed document; status is 0 on a transport failure, err
// describes any non-2xx answer or transport error.
func (d *Deliverer) post(ctx context.Context, del *store.Delivery, body []byte) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, d.config().Callbacks.Timeout.Std())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, del.URL, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderEvent, del.Event)
	req.Header.Set(HeaderRequestID, del.JobID)
	req.Header.Set(HeaderAttempt, strconv.Itoa(del.Attempts+1))
	req.Header.Set(HeaderSignature, Sign(d.config().Callbacks.SigningSecret, body))
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

// classify decides what an attempt's result means: delivered on 2xx; given
// up ("failed") on a 4xx other than 408/429, on a redirect, or once the
// attempt budget is spent; otherwise a retry after the worker backoff.
func (d *Deliverer) classify(attempts, status int, err error) (outcome string, next *time.Time) {
	switch {
	case err == nil:
		return "delivered", nil
	case status >= 300 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests:
		return "failed", nil
	case attempts >= d.config().Callbacks.MaxAttempts:
		return "failed", nil
	}
	at := d.now().Add(d.backoff(attempts))
	return "retry", &at
}

// backoff mirrors the worker's: base doubled per attempt, capped, ±20 %.
func (d *Deliverer) backoff(attempts int) time.Duration {
	w := d.config().Worker
	b := w.BackoffBase.Std() << (attempts - 1)
	if maxB := w.BackoffMax.Std(); b > maxB || b <= 0 {
		b = maxB
	}
	return time.Duration(float64(b) * (0.8 + 0.4*rand.Float64())) //nolint:gosec // G404: jitter is scheduling, not security
}

// ErrDisabled is returned by Check when callbacks are not configured.
var ErrDisabled = errors.New("callbacks are not enabled on this deployment")

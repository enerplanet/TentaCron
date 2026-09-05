package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/enerplanet/tentacron/internal/config"
)

// ForwardResult is the target's response to the forwarded payload.
type ForwardResult struct {
	Status int
	Body   []byte
}

// authHeaders returns the outbound header map for a target call, carrying the
// API key when the target uses header injection.
func authHeaders(tcfg config.Target) map[string]string {
	headers := map[string]string{}
	if tcfg.APIKeyInject == config.InjectHeader {
		headers[tcfg.APIKeyHeader] = tcfg.APIKey
	}
	return headers
}

// ForwardToTarget sends the resolved payload to the target API. The target's
// API key is injected into the outbound bytes only — stored payloads stay
// credential-free. {field} placeholders in the target URL are filled from
// the payload's top-level fields (ignis-style path parameters); consumed
// fields are stripped from the forwarded body, since they address the call
// rather than belong to it. A target with neither placeholders nor
// body-field injection forwards the payload byte-exact.
func (c *Client) ForwardToTarget(ctx context.Context, name string, tcfg config.Target, payload []byte) (*ForwardResult, error) {
	op := "target " + name
	callURL, outbound, err := prepareOutbound(tcfg, payload)
	if err != nil {
		return nil, &Error{Op: op, Transient: false, Err: err}
	}
	status, body, err := c.do(ctx, op, tcfg.Method, callURL, outbound, authHeaders(tcfg), tcfg.Timeout.Std())
	if err != nil {
		return nil, err
	}
	return &ForwardResult{Status: status, Body: body}, nil
}

// CheckTargetPayload reports whether the payload can be forwarded to the
// target at all — its top-level fields fill every {field} placeholder of the
// target URL — without sending anything. The dry-run endpoint uses it for
// proxy targets, whose payloads are never scanned for resolvents.
func CheckTargetPayload(tcfg config.Target, payload []byte) error {
	_, _, err := prepareOutbound(tcfg, payload)
	return err
}

// prepareOutbound applies the two rewrites a target may need — URL
// templating and body-field key injection — and returns the call URL and
// body. Without either, the payload goes out byte-exact. Both rewrites work
// on map[string]json.RawMessage so nested values pass through byte-for-byte:
// a decode into map[string]any would round large integers through float64
// and silently corrupt model data.
func prepareOutbound(tcfg config.Target, payload []byte) (callURL string, body []byte, err error) {
	templated := placeholderPattern.MatchString(tcfg.URL)
	inject := tcfg.APIKeyInject == config.InjectBodyField
	if !templated && !inject {
		return tcfg.URL, payload, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(payload, &doc); err != nil || doc == nil {
		return "", nil, fmt.Errorf("payload not an object for url templating or key injection: %w", errNotObject(err))
	}
	callURL = tcfg.URL
	if templated {
		if callURL, err = fillTargetURL(tcfg.URL, doc); err != nil {
			return "", nil, err
		}
	}
	if inject {
		keyJSON, err := json.Marshal(tcfg.APIKey)
		if err != nil {
			return "", nil, err
		}
		doc[tcfg.APIKeyField] = keyJSON
	}
	if body, err = json.Marshal(doc); err != nil {
		return "", nil, err
	}
	return callURL, body, nil
}

// errNotObject names the decode failure, or the fact that a valid "null"
// decoded into no object at all.
func errNotObject(err error) error {
	if err != nil {
		return err
	}
	return errors.New("got null instead of an object")
}

// fillTargetURL replaces {field} placeholders with the path-escaped value of
// the payload's top-level field — a JSON string (unquoted) or number — and
// deletes consumed fields from doc. A placeholder may repeat; null, booleans
// and containers never fill one.
func fillTargetURL(rawURL string, doc map[string]json.RawMessage) (string, error) {
	var missing []string
	consumed := map[string]bool{}
	filled := placeholderPattern.ReplaceAllStringFunc(rawURL, func(match string) string {
		field := match[1 : len(match)-1]
		s, ok := scalarFromRaw(doc[field])
		if !ok {
			if !slices.Contains(missing, field) {
				missing = append(missing, field)
			}
			return match
		}
		consumed[field] = true
		return url.PathEscape(s)
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("url placeholder(s) %v need string or number fields at the payload's top level", missing)
	}
	for field := range consumed {
		delete(doc, field)
	}
	return filled, nil
}

// scalarFromRaw renders a raw JSON string or number for use in a URL.
// Anything else — absent, null, booleans, objects, arrays — is rejected:
// json.Unmarshal would happily decode "null" into an empty string and
// silently produce an empty path segment.
func scalarFromRaw(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	switch {
	case raw[0] == '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", false
		}
		return s, true
	case raw[0] == '-' || (raw[0] >= '0' && raw[0] <= '9'):
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
			return "", false
		}
		return n.String(), true
	}
	return "", false
}

// PollStatus is the outcome of one poll tick against the target's job.
type PollStatus struct {
	Done   bool
	Failed bool
	Raw    string // the status value as reported by the target
	Body   []byte
}

// jobIDPattern bounds what a target-supplied job id may look like before it
// is substituted into the poll/result URL templates: the id comes from the
// target's response, and URL metacharacters in it would rewrite the
// configured request path.
var jobIDPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,256}$`)

// ExtractJobID pulls the target's job id out of its accept response using the
// configured JSON path.
func ExtractJobID(acceptBody []byte, poll *config.Poll) (string, error) {
	v, err := jsonPath(acceptBody, poll.IDJSONPath)
	if err != nil {
		return "", fmt.Errorf("extract job id at %q: %w", poll.IDJSONPath, err)
	}
	var id string
	switch typed := v.(type) {
	case string:
		id = typed
	case json.Number:
		id = typed.String()
	default:
		return "", fmt.Errorf("job id at %q has unsupported type %T", poll.IDJSONPath, v)
	}
	if !jobIDPattern.MatchString(id) {
		return "", fmt.Errorf("job id at %q is empty or contains characters unsafe for URL templates", poll.IDJSONPath)
	}
	return id, nil
}

// PollTarget checks the state of the target's job once.
func (c *Client) PollTarget(ctx context.Context, name string, tcfg config.Target, targetJobID string) (*PollStatus, error) {
	poll := tcfg.Response.Poll
	pollURL := strings.ReplaceAll(poll.URLTemplate, "{id}", url.PathEscape(targetJobID))
	_, body, err := c.do(ctx, "poll "+name, http.MethodGet, pollURL, nil, authHeaders(tcfg), tcfg.Timeout.Std())
	if err != nil {
		return nil, err
	}

	v, err := jsonPath(body, poll.StatusJSONPath)
	if err != nil {
		return nil, &Error{Op: "poll " + name, Transient: false,
			Err: fmt.Errorf("extract status at %q: %w", poll.StatusJSONPath, err)}
	}
	raw := fmt.Sprintf("%v", v)
	return &PollStatus{
		Raw:    raw,
		Body:   body,
		Done:   slices.Contains(poll.DoneValues, raw),
		Failed: slices.Contains(poll.FailedValues, raw),
	}, nil
}

// CancelTarget asks a poll-mode target to stop a job, with a DELETE on the
// configured cancel_url_template and the target's auth. Best effort: the
// caller logs the outcome and the request stays cancelled either way.
func (c *Client) CancelTarget(ctx context.Context, name string, tcfg config.Target, targetJobID string) error {
	poll := tcfg.Response.Poll
	cancelURL := strings.ReplaceAll(poll.CancelURLTemplate, "{id}", url.PathEscape(targetJobID))
	_, _, err := c.do(ctx, "cancel "+name, http.MethodDelete, cancelURL, nil, authHeaders(tcfg), tcfg.Timeout.Std())
	return err
}

// ResultStream is an open result download. The body is not capped by the
// client — the worker streams it to disk under storage.max_result_bytes —
// and Close releases the connection together with the call's deadline.
type ResultStream struct {
	Status      int
	ContentType string
	Body        io.ReadCloser
	cancel      context.CancelFunc
}

// Close ends the download.
func (r *ResultStream) Close() error {
	r.cancel()
	return r.Body.Close()
}

// FetchResult opens the finished target job's result for streaming. It
// bypasses c.do because a result bundle may be far larger than the JSON
// response cap and its Content-Type must be kept for storage. Non-2xx
// statuses are classified like any other call; the target's timeout bounds
// the whole download.
func (c *Client) FetchResult(ctx context.Context, name string, tcfg config.Target, targetJobID string) (stream *ResultStream, err error) {
	poll := tcfg.Response.Poll
	resultURL := strings.ReplaceAll(poll.ResultURLTemplate, "{id}", url.PathEscape(targetJobID))
	start, status := time.Now(), 0
	defer func() { c.observe("result "+name, status, err, time.Since(start)) }()

	callCtx, cancel := context.WithTimeout(ctx, tcfg.Timeout.Std())
	resp, err := c.send(callCtx, "result "+name, http.MethodGet, resultURL, nil, authHeaders(tcfg))
	if err != nil {
		cancel()
		return nil, err
	}
	status = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		cancel()
		transient := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return nil, &Error{Op: "result " + name, Status: resp.StatusCode, Transient: transient}
	}
	return &ResultStream{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: resp.Body, cancel: cancel}, nil
}

// ExtractPath returns the sub-document at the dot-separated path of a JSON
// body, re-encoded with number fidelity (json.Number round-trips verbatim).
// An empty path returns the body unchanged.
func ExtractPath(body []byte, path string) ([]byte, error) {
	if path == "" {
		return body, nil
	}
	v, err := jsonPath(body, path)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// jsonPath navigates a dot-separated path through a JSON document. Numbers
// are decoded as json.Number so large integer ids survive verbatim. A
// numeric segment indexes an array — "0" selects the first element of a
// list response (e.g. city2tabula's building list).
func jsonPath(body []byte, path string) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("response is not JSON: %w", err)
	}
	cur := doc
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			var ok bool
			if cur, ok = node[seg]; !ok {
				return nil, fmt.Errorf("segment %q: not found", seg)
			}
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil {
				return nil, fmt.Errorf("segment %q: response is an array, expected a numeric index", seg)
			}
			if idx < 0 || idx >= len(node) {
				return nil, fmt.Errorf("segment %q: index out of range (array has %d elements)", seg, len(node))
			}
			cur = node[idx]
		default:
			return nil, fmt.Errorf("segment %q: not an object or array", seg)
		}
	}
	return cur, nil
}

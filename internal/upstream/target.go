package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

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
// credential-free.
func (c *Client) ForwardToTarget(ctx context.Context, name string, tcfg config.Target, payload []byte) (*ForwardResult, error) {
	op := "target " + name
	headers := authHeaders(tcfg)
	outbound := payload
	if tcfg.APIKeyInject == config.InjectBodyField {
		// Inject via RawMessage so nested values pass through byte-for-byte:
		// a full decode into map[string]any would round large integers
		// through float64 and silently corrupt model data.
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(payload, &doc); err != nil {
			return nil, &Error{Op: op, Transient: false, Err: fmt.Errorf("payload not an object for key injection: %w", err)}
		}
		keyJSON, err := json.Marshal(tcfg.APIKey)
		if err != nil {
			return nil, &Error{Op: op, Transient: false, Err: err}
		}
		doc[tcfg.APIKeyField] = keyJSON
		if outbound, err = json.Marshal(doc); err != nil {
			return nil, &Error{Op: op, Transient: false, Err: err}
		}
	}

	status, body, err := c.do(ctx, op, tcfg.Method, tcfg.URL, outbound, headers, tcfg.Timeout.Std())
	if err != nil {
		return nil, err
	}
	return &ForwardResult{Status: status, Body: body}, nil
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

// FetchResult retrieves the finished target job's result. It bypasses c.do
// because the response's Content-Type header must be captured for storage
// alongside the body.
func (c *Client) FetchResult(ctx context.Context, name string, tcfg config.Target, targetJobID string) (contentType string, body []byte, err error) {
	poll := tcfg.Response.Poll
	resultURL := strings.ReplaceAll(poll.ResultURLTemplate, "{id}", url.PathEscape(targetJobID))

	callCtx, cancel := context.WithTimeout(ctx, tcfg.Timeout.Std())
	defer cancel()
	resp, err := c.send(callCtx, "result "+name, http.MethodGet, resultURL, nil, authHeaders(tcfg))
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		transient := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return "", nil, &Error{Op: "result " + name, Status: resp.StatusCode, Transient: transient}
	}
	body, err = c.readCapped(resp.Body)
	if err != nil {
		// A mid-download failure is retryable next tick; only the size cap
		// is final (the body will not shrink).
		return "", nil, &Error{Op: "result " + name, Transient: !errors.Is(err, errBodyTooLarge), Err: err}
	}
	return resp.Header.Get("Content-Type"), body, nil
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

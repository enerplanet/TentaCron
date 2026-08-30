package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
		var doc map[string]any
		if err := json.Unmarshal(payload, &doc); err != nil {
			return nil, &Error{Op: op, Transient: false, Err: fmt.Errorf("payload not an object for key injection: %w", err)}
		}
		doc[tcfg.APIKeyField] = tcfg.APIKey
		var err error
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

// ExtractJobID pulls the target's job id out of its accept response using the
// configured JSON path.
func ExtractJobID(acceptBody []byte, poll *config.Poll) (string, error) {
	v, err := jsonPath(acceptBody, poll.IDJSONPath)
	if err != nil {
		return "", fmt.Errorf("extract job id at %q: %w", poll.IDJSONPath, err)
	}
	switch id := v.(type) {
	case string:
		if id == "" {
			return "", fmt.Errorf("job id at %q is empty", poll.IDJSONPath)
		}
		return id, nil
	case float64:
		return strconv.FormatFloat(id, 'f', -1, 64), nil
	default:
		return "", fmt.Errorf("job id at %q has unsupported type %T", poll.IDJSONPath, v)
	}
}

// PollTarget checks the state of the target's job once.
func (c *Client) PollTarget(ctx context.Context, name string, tcfg config.Target, targetJobID string) (*PollStatus, error) {
	poll := tcfg.Response.Poll
	url := strings.ReplaceAll(poll.URLTemplate, "{id}", targetJobID)
	_, body, err := c.do(ctx, "poll "+name, http.MethodGet, url, nil, authHeaders(tcfg), tcfg.Timeout.Std())
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
	url := strings.ReplaceAll(poll.ResultURLTemplate, "{id}", targetJobID)

	callCtx, cancel := context.WithTimeout(ctx, tcfg.Timeout.Std())
	defer cancel()
	resp, err := c.send(callCtx, "result "+name, http.MethodGet, url, nil, authHeaders(tcfg))
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		transient := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return "", nil, &Error{Op: "result " + name, Status: resp.StatusCode, Transient: transient}
	}
	body, err = readCapped(resp.Body, c.maxBody)
	if err != nil {
		return "", nil, &Error{Op: "result " + name, Transient: false, Err: err}
	}
	return resp.Header.Get("Content-Type"), body, nil
}

// jsonPath navigates a dot-separated path through a JSON object.
func jsonPath(body []byte, path string) (any, error) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("response is not JSON: %w", err)
	}
	cur := doc
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("segment %q: not an object", seg)
		}
		cur, ok = m[seg]
		if !ok {
			return nil, fmt.Errorf("segment %q: not found", seg)
		}
	}
	return cur, nil
}

package upstream

import (
	"context"

	"github.com/enerplanet/tentacron/internal/config"
)

// ResolveResolvent sends one resolvent object to its resource API and returns
// the time-series response body. The resolvent object's own properties are
// the request payload — they carry everything the resource API needs.
func (c *Client) ResolveResolvent(ctx context.Context, typ string, rcfg config.Resolvent, resolventObj []byte) ([]byte, error) {
	headers := map[string]string{}
	if rcfg.APIKey != "" {
		headers[rcfg.APIKeyHeader] = rcfg.APIKey
	}
	_, body, err := c.do(ctx, "resource "+typ, rcfg.Method, rcfg.URL, resolventObj, headers, rcfg.Timeout.Std())
	return body, err
}

package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/enerplanet/tentacron/internal/config"
)

// ResolveResolvent calls the resource API for one resolvent. POST-style
// resolvents send the payload object as a JSON body (the classic contract).
// GET-style resolvents map the object onto the request URL instead: {field}
// placeholders in the configured URL are filled from the object (ignis-style
// path parameters), and every remaining field becomes a query parameter
// (weather/city2tabula-style APIs) — see buildResolventURL for the rules.
func (c *Client) ResolveResolvent(ctx context.Context, typ string, rcfg config.Resolvent, payload map[string]any) ([]byte, error) {
	headers := map[string]string{}
	if rcfg.APIKey != "" {
		headers[rcfg.APIKeyHeader] = rcfg.APIKey
	}

	if rcfg.Method == http.MethodGet {
		callURL, err := buildResolventURL(rcfg.URL, payload, rcfg.QueryMap)
		if err != nil {
			return nil, &Error{Op: "resource " + typ, Transient: false, Err: err}
		}
		_, body, err := c.do(ctx, "resource "+typ, http.MethodGet, callURL, nil, headers, rcfg.Timeout.Std())
		return body, err
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, &Error{Op: "resource " + typ, Transient: false, Err: fmt.Errorf("encode resolvent object: %w", err)}
	}
	_, body, err := c.do(ctx, "resource "+typ, rcfg.Method, rcfg.URL, bodyBytes, headers, rcfg.Timeout.Std())
	return body, err
}

var placeholderPattern = regexp.MustCompile(`\{([A-Za-z0-9_]+)\}`)

// buildResolventURL maps a resolvent object onto a GET URL:
//   - {field} placeholders in the configured URL are replaced with the
//     path-escaped value of that field, which is then consumed;
//   - every remaining field becomes a query parameter, appended to any
//     query already fixed in the configured URL (e.g. ?format=json), under
//     the name query_map assigns it or its own name otherwise;
//   - the "type" and "name" fields are tentacron's own (the marker and the
//     object's label) and never sent;
//   - values must be scalars (strings, numbers, booleans) or arrays of
//     scalars, which join comma-separated (the convention of the weather
//     and city2tabula APIs); nested objects are an authoring error;
//   - parameters are appended in sorted field order, so the produced URL —
//     and anything derived from it (logs, goldens) — is deterministic.
func buildResolventURL(rawURL string, payload map[string]any, queryMap map[string]string) (string, error) {
	// type (the marker) and name (the label) are tentacron's own, never sent.
	return buildGetURL(rawURL, payload, queryMap, map[string]bool{"type": true, "name": true})
}

// buildGetURL maps a flat field map onto a GET URL: {field} placeholders
// take the path-escaped scalar value of that field; every field not consumed
// by a placeholder or already in preConsumed becomes a query parameter
// (renamed by queryMap), appended to any query already in rawURL, in sorted
// field order for a deterministic result.
//
// Shared by GET resolvents and GET proxy targets so both escape query values
// the same way and honour query_map; a proxy target passes an empty
// preConsumed (it has no markers of its own).
func buildGetURL(rawURL string, fields map[string]any, queryMap map[string]string, preConsumed map[string]bool) (string, error) {
	templated, consumed, err := fillPathPlaceholders(rawURL, fields, preConsumed)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(templated)
	if err != nil {
		return "", fmt.Errorf("get url: %w", err)
	}
	q := u.Query()
	if err := addQueryFields(q, fields, consumed, queryMap); err != nil {
		return "", err
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// fillPathPlaceholders replaces every {field} placeholder with the
// path-escaped scalar value of that field and reports the consumed fields.
// preConsumed seeds fields that must never be sent (a resolvent's type/name).
func fillPathPlaceholders(rawURL string, payload map[string]any, preConsumed map[string]bool) (string, map[string]bool, error) {
	consumed := maps.Clone(preConsumed)
	if consumed == nil {
		consumed = map[string]bool{}
	}
	var missing []string
	filled := placeholderPattern.ReplaceAllStringFunc(rawURL, func(match string) string {
		field := match[1 : len(match)-1]
		s, err := scalarString(payload[field])
		if err != nil {
			missing = append(missing, field)
			return match
		}
		consumed[field] = true
		return url.PathEscape(s)
	})
	if len(missing) > 0 {
		return "", nil, fmt.Errorf("url placeholder(s) %v need a scalar (string, number or bool) field", missing)
	}
	return filled, consumed, nil
}

// addQueryFields sets every unconsumed field as a query parameter — under
// its query_map name when one is configured — in sorted field order so the
// outbound URL is deterministic.
func addQueryFields(q url.Values, payload map[string]any, consumed map[string]bool, queryMap map[string]string) error {
	for _, field := range slices.Sorted(maps.Keys(payload)) {
		if consumed[field] {
			continue
		}
		s, err := queryString(payload[field])
		if err != nil {
			return fmt.Errorf("field %q: %w", field, err)
		}
		param := field
		if mapped, ok := queryMap[field]; ok {
			param = mapped
		}
		q.Set(param, s)
	}
	return nil
}

func scalarString(v any) (string, error) {
	switch typed := v.(type) {
	case string:
		return typed, nil
	case json.Number:
		return typed.String(), nil
	case bool:
		return strconvBool(typed), nil
	default:
		return "", fmt.Errorf("value of type %T is not a scalar", v)
	}
}

// queryString renders a query value: a scalar, or an array of scalars joined
// comma-separated (variables=T,GHI / osm_ids=123,456).
func queryString(v any) (string, error) {
	if list, ok := v.([]any); ok {
		parts := make([]string, len(list))
		for i, elem := range list {
			s, err := scalarString(elem)
			if err != nil {
				return "", fmt.Errorf("array element %d: %w", i, err)
			}
			parts[i] = s
		}
		return strings.Join(parts, ","), nil
	}
	s, err := scalarString(v)
	if err != nil {
		return "", fmt.Errorf("%w (GET resolvents take flat fields; nest complex data under a POST resolvent instead)", err)
	}
	return s, nil
}

func strconvBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

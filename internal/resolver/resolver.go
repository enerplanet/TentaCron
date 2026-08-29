// Package resolver locates resolvent objects inside a payload's time-series
// container and substitutes them with resolved time-series objects. It is
// pure: no I/O, no configuration — callers supply the resource-API responses.
package resolver

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	typeKey = "type"

	// TimeSeriesType marks a genuine time-series object.
	TimeSeriesType = "time-series"
	// TypePrefix marks a resolvent object's type value, e.g. "resolvent-pv1".
	TypePrefix = "resolvent-"
	// ResolventKey is where the original resolvent object is preserved on the
	// substituted time-series object.
	ResolventKey = "resolvent"
)

// Found is one resolvent object located in the payload, with the plumbing to
// replace it in place.
type Found struct {
	// Type is the resolvent type, e.g. "resolvent-pv1".
	Type string
	// Hash is the canonical parameter hash, used as series-cache key and to
	// deduplicate identical resolvents within one payload.
	Hash string
	// Object is the resolvent object itself (the resource-API parameters).
	Object map[string]any

	replace func(series map[string]any)
}

// Parse decodes a payload into a mutable document root.
func Parse(payload []byte) (map[string]any, error) {
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, fmt.Errorf("payload is not a JSON object: %w", err)
	}
	return root, nil
}

// Marshal re-encodes the (possibly mutated) document root.
func Marshal(root map[string]any) ([]byte, error) {
	return json.Marshal(root)
}

// Find navigates the dot-separated path to the time-series container and
// collects all resolvent objects inside it. A missing container is not an
// error — payloads without a time-series property simply have nothing to
// resolve. An empty path scans the whole document.
func Find(root map[string]any, path string) ([]*Found, error) {
	container := any(root)
	if path != "" {
		for _, seg := range strings.Split(path, ".") {
			m, ok := container.(map[string]any)
			if !ok {
				return nil, nil
			}
			container, ok = m[seg]
			if !ok {
				return nil, nil
			}
		}
	}
	var found []*Found
	var err error
	walk(container, func(obj map[string]any, replace func(map[string]any)) {
		typ := obj[typeKey].(string) // walk only reports objects with a string type
		var hash string
		if err == nil {
			hash, err = paramHash(typ, obj)
		}
		found = append(found, &Found{Type: typ, Hash: hash, Object: obj, replace: replace})
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// walk visits arrays and objects below node, reporting each resolvent object
// together with a closure that overwrites its slot in the parent container.
// It does not descend into resolvent objects (their properties are opaque
// resource-API parameters) nor into genuine time-series objects (data).
func walk(node any, report func(obj map[string]any, replace func(map[string]any))) {
	switch v := node.(type) {
	case []any:
		for i, elem := range v {
			if obj, ok := elem.(map[string]any); ok && isResolvent(obj) {
				report(obj, func(series map[string]any) { v[i] = series })
				continue
			}
			walk(elem, report)
		}
	case map[string]any:
		if typ, _ := v[typeKey].(string); typ == TimeSeriesType {
			return
		}
		for k, elem := range v {
			if obj, ok := elem.(map[string]any); ok && isResolvent(obj) {
				report(obj, func(series map[string]any) { v[k] = series })
				continue
			}
			walk(elem, report)
		}
	}
}

func isResolvent(obj map[string]any) bool {
	typ, ok := obj[typeKey].(string)
	return ok && strings.HasPrefix(typ, TypePrefix)
}

// Substitute replaces the found resolvent with the resource API's response.
// The response must be a JSON object; it gets the original resolvent object
// attached under "resolvent" and overwrites the resolvent's slot in the
// payload. Non-fatal oddities are returned as warnings.
func (f *Found) Substitute(seriesBody []byte) (warnings []string, err error) {
	var series map[string]any
	if err := json.Unmarshal(seriesBody, &series); err != nil {
		return nil, fmt.Errorf("resource response for %s is not a JSON object: %w", f.Type, err)
	}
	if typ, _ := series[typeKey].(string); typ != TimeSeriesType {
		warnings = append(warnings, fmt.Sprintf(
			"resource response for %s has type %q, expected %q", f.Type, typ, TimeSeriesType))
	}
	if _, exists := series[ResolventKey]; exists {
		warnings = append(warnings, fmt.Sprintf(
			"resource response for %s already contains a %q key; it was overwritten", f.Type, ResolventKey))
	}
	series[ResolventKey] = f.Object
	f.replace(series)
	return warnings, nil
}

// DistinctTypes returns the unique resolvent types over the found set.
func DistinctTypes(found []*Found) []string {
	seen := map[string]bool{}
	var types []string
	for _, f := range found {
		if !seen[f.Type] {
			seen[f.Type] = true
			types = append(types, f.Type)
		}
	}
	return types
}

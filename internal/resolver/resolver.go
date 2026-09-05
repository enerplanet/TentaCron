// Package resolver locates resolvent objects inside a payload's time-series
// container and substitutes them with resolved time-series objects. It is
// pure: no I/O, no configuration — callers supply the resource-API responses.
package resolver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
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
	// Object is the resolvent object as it appears in the payload — with
	// its references, when it has any. It is what the marker preserves.
	Object map[string]any
	// Input is what is sent upstream and hashed for the cache: Object
	// itself, or — after Fill — a copy with every reference replaced by
	// the value it points to.
	Input map[string]any
	// Path locates the object in the payload as a JSON pointer (RFC 6901),
	// e.g. "/model/timeseries/pv_cf" or "/time-series/0".
	Path string
	// Name identifies the object to humans and to references: the object's
	// own "name" field when it has one, otherwise its key in a registry
	// object; empty for an unnamed array element.
	Name string

	replace func(series map[string]any)
	deps    map[string]*Found // referenced resolvents by name, set by Chain
}

// Parse decodes a payload into a mutable document root. Numbers are decoded
// as json.Number so Marshal re-emits them verbatim — a float64 round-trip
// would silently corrupt integers above 2^53 in the forwarded payload.
func Parse(payload []byte) (map[string]any, error) {
	root, err := decodeObject(payload)
	if err != nil {
		return nil, fmt.Errorf("payload is not a JSON object: %w", err)
	}
	return root, nil
}

// decodeObject decodes b into a non-nil object with number fidelity.
func decodeObject(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, fmt.Errorf("got null instead of an object")
	}
	return obj, nil
}

// Marshal re-encodes the (possibly mutated) document root.
func Marshal(root map[string]any) ([]byte, error) {
	return json.Marshal(root)
}

// Find navigates the dot-separated path to the time-series container and
// collects all resolvent objects inside it — or the container itself, when
// the path addresses a single resolvent object. A missing container is not
// an error: payloads without a time-series property simply have nothing to
// resolve. An empty path scans the whole document.
func Find(root map[string]any, path string) ([]*Found, error) {
	container, parent, key, ok := locateContainer(root, path)
	if !ok {
		return nil, nil
	}
	var found []*Found
	var err error
	report := func(typ string, obj map[string]any, ptr, name string, replace func(map[string]any)) {
		if err != nil {
			return // a hash already failed; Find returns the error and discards found
		}
		var hash string
		hash, err = paramHash(typ, obj)
		found = append(found, &Found{Type: typ, Hash: hash, Object: obj, Input: obj, Path: ptr, Name: nameOf(obj, name), replace: replace})
	}
	prefix := pointerOf(path)
	if obj, isObj := container.(map[string]any); isObj && parent != nil {
		if typ, isRes := resolventType(obj); isRes {
			// The container is the resolvent; its slot is the parent's key.
			report(typ, obj, prefix, key, func(series map[string]any) { parent[key] = series })
			return found, err
		}
	}
	walk(container, prefix, report)
	if err != nil {
		return nil, err
	}
	return found, nil
}

// pointerOf renders a dot-separated container path as a JSON pointer prefix
// ("" for the root).
func pointerOf(path string) string {
	if path == "" {
		return ""
	}
	var b strings.Builder
	for _, seg := range strings.Split(path, ".") {
		b.WriteString("/")
		b.WriteString(escapePointer(seg))
	}
	return b.String()
}

// escapePointer applies RFC 6901 escaping to one reference token.
func escapePointer(token string) string {
	return strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1")
}

// nameOf prefers the object's own "name" field over the key it sits under.
func nameOf(obj map[string]any, key string) string {
	if n, ok := obj["name"].(string); ok && n != "" {
		return n
	}
	return key
}

// locateContainer follows the dot-separated path from root and returns the
// node it addresses together with the parent map and key that hold it
// (nil and "" for the root itself). ok is false when the path misses.
func locateContainer(root map[string]any, path string) (node any, parent map[string]any, key string, ok bool) {
	node = root
	if path == "" {
		return node, nil, "", true
	}
	for _, seg := range strings.Split(path, ".") {
		m, isMap := node.(map[string]any)
		if !isMap {
			return nil, nil, "", false
		}
		if node, ok = m[seg]; !ok {
			return nil, nil, "", false
		}
		parent, key = m, seg
	}
	return node, parent, key, true
}

// walk visits arrays and objects below node, reporting each resolvent object
// together with its type, JSON pointer, the key it sits under and a closure
// that overwrites its slot in the parent container. It does not descend into
// resolvent objects (their properties are opaque resource-API parameters)
// nor into genuine time-series objects (data). Reporting order is
// deterministic: array order for arrays, sorted key order for objects — so
// error messages and processing order never depend on map iteration
// randomness.
func walk(node any, prefix string, report func(typ string, obj map[string]any, ptr, key string, replace func(map[string]any))) {
	switch v := node.(type) {
	case []any:
		for i, elem := range v {
			ptr := prefix + "/" + strconv.Itoa(i)
			if obj, ok := elem.(map[string]any); ok {
				if typ, ok := resolventType(obj); ok {
					report(typ, obj, ptr, "", func(series map[string]any) { v[i] = series })
					continue
				}
			}
			walk(elem, ptr, report)
		}
	case map[string]any:
		if typ, _ := v[typeKey].(string); typ == TimeSeriesType {
			return
		}
		for _, k := range slices.Sorted(maps.Keys(v)) {
			elem := v[k]
			ptr := prefix + "/" + escapePointer(k)
			if obj, ok := elem.(map[string]any); ok {
				if typ, ok := resolventType(obj); ok {
					report(typ, obj, ptr, k, func(series map[string]any) { v[k] = series })
					continue
				}
			}
			walk(elem, ptr, report)
		}
	}
}

// resolventType reports obj's resolvent type, if obj is a resolvent object.
func resolventType(obj map[string]any) (string, bool) {
	typ, ok := obj[typeKey].(string)
	if !ok || !strings.HasPrefix(typ, TypePrefix) {
		return "", false
	}
	return typ, true
}

// Substitute replaces the found resolvent with the resource API's response.
// The response must be a JSON object; it overwrites the resolvent's slot in
// the payload and — when attachResolvent is set — gets the original resolvent
// object attached under "resolvent" for traceability. Targets whose schema
// rejects unknown keys pass false; the substituted series then carries no
// tentacron marker at all. Non-fatal oddities are returned as warnings.
// Each call decodes seriesBody afresh, so duplicate resolvents fed the same
// cached body never share the substituted map.
func (f *Found) Substitute(seriesBody []byte, attachResolvent bool) (warnings []string, err error) {
	// decodeObject keeps number fidelity (the series lands in the forwarded
	// payload) and rejects "null", which would otherwise decode into a nil
	// map and panic on the resolvent-key assignment below.
	series, err := decodeObject(seriesBody)
	if err != nil {
		return nil, fmt.Errorf("resource response for %s is not a JSON object: %w", f.Type, err)
	}
	if typ, _ := series[typeKey].(string); typ != TimeSeriesType {
		warnings = append(warnings, fmt.Sprintf(
			"resource response for %s has type %q, expected %q", f.Type, typ, TimeSeriesType))
	}
	if attachResolvent {
		if _, exists := series[ResolventKey]; exists {
			warnings = append(warnings, fmt.Sprintf(
				"resource response for %s already contains a %q key; it was overwritten", f.Type, ResolventKey))
		}
		series[ResolventKey] = f.Object
	}
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

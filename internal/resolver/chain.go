package resolver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// A field inside a resolvent object may be a reference to a sibling
// resolvent's resolved series instead of a literal:
//
//	{"$from": "building", "path": "tabula_variant_code"}
//
// "$from" names the sibling by its name field or registry key, "path" (a
// response_map-style path, the leading dot optional; absent means the whole
// series) selects the value. Chained resolvents resolve level by level, and a
// resolvent's cache key is computed from its filled input, so it reflects the
// real parameters sent upstream.
const (
	refKey  = "$from"
	pathKey = "path"

	// MaxChainDepth bounds how many levels a chain may have.
	MaxChainDepth = 8
)

// ErrReference marks a reference that cannot be filled from the series it
// points to — a permanent payload problem, not an upstream failure.
var ErrReference = errors.New("reference cannot be filled")

// Chain orders the found resolvents into dependency levels: level 0 holds
// the resolvents without references, every later level those whose
// references all point into earlier levels. Document order is kept within a
// level. References to unknown or ambiguous names, self references, cycles
// and chains deeper than MaxChainDepth are rejected. A payload without
// references yields a single level.
func Chain(found []*Found) ([][]*Found, error) {
	byName := map[string][]*Found{}
	for _, f := range found {
		if f.Name != "" {
			byName[f.Name] = append(byName[f.Name], f)
		}
	}
	for _, f := range found {
		f.deps = map[string]*Found{}
		err := walkRefs(f.Object, f.Path, func(ref map[string]any, ptr string, _ func(any)) error {
			from, _, err := parseRef(ref)
			if err != nil {
				return fmt.Errorf("reference at %s: %w", ptr, err)
			}
			return f.bind(from, ptr, byName[from])
		})
		if err != nil {
			return nil, err
		}
	}
	depth := map[*Found]int{}
	for _, f := range found {
		if _, err := f.level(depth, map[*Found]bool{}, nil); err != nil {
			return nil, err
		}
	}
	var levels [][]*Found
	for _, f := range found {
		for len(levels) <= depth[f] {
			levels = append(levels, nil)
		}
		levels[depth[f]] = append(levels[depth[f]], f)
	}
	return levels, nil
}

// bind records the resolvent one reference points to.
func (f *Found) bind(from, ptr string, candidates []*Found) error {
	switch {
	case len(candidates) == 0:
		return fmt.Errorf("reference at %s: no resolvent named %q", ptr, from)
	case len(candidates) > 1:
		return fmt.Errorf("reference at %s: %d resolvents are named %q; a referenced name must be unique", ptr, len(candidates), from)
	case candidates[0] == f:
		return fmt.Errorf("reference at %s: resolvent %q references itself", ptr, from)
	}
	f.deps[from] = candidates[0]
	return nil
}

// level computes f's depth (0 for a resolvent without references), detecting
// cycles along the way; trail is the chain of names being followed.
func (f *Found) level(depth map[*Found]int, visiting map[*Found]bool, trail []string) (int, error) {
	if d, done := depth[f]; done {
		return d, nil
	}
	if visiting[f] {
		return 0, fmt.Errorf("resolvent references form a cycle: %s", strings.Join(append(trail, f.label()), " -> "))
	}
	visiting[f] = true
	defer delete(visiting, f)
	d := 0
	for _, name := range slices.Sorted(maps.Keys(f.deps)) {
		dd, err := f.deps[name].level(depth, visiting, append(trail, f.label()))
		if err != nil {
			return 0, err
		}
		d = max(d, dd+1)
	}
	if d >= MaxChainDepth {
		return 0, fmt.Errorf("resolvent chain through %s is deeper than %d levels", f.label(), MaxChainDepth)
	}
	depth[f] = d
	return d, nil
}

func (f *Found) label() string {
	if f.Name != "" {
		return f.Name
	}
	return f.Path
}

// DependsOn lists the names of the resolvents f references, sorted; empty
// for a resolvent without references.
func (f *Found) DependsOn() []string {
	return slices.Sorted(maps.Keys(f.deps))
}

// Fill replaces every reference in f with the value it points to, read from
// the referenced resolvent's series, and stores the result as f.Input — the
// parameters sent upstream and hashed for the cache. The payload keeps the
// original object with its references, which is what the marker shows.
// series returns the body a dependency resolved to. filled is false when f
// holds no references.
func (f *Found) Fill(series func(dep *Found) []byte) (filled bool, err error) {
	if len(f.deps) == 0 {
		return false, nil
	}
	docs := map[*Found]any{}
	input := deepCopy(f.Object).(map[string]any)
	err = walkRefs(input, f.Path, func(ref map[string]any, ptr string, set func(any)) error {
		from, path, err := parseRef(ref)
		if err != nil {
			return err // Chain already rejected it; kept for a direct caller
		}
		dep := f.deps[from]
		doc, ok := docs[dep]
		if !ok {
			if doc, err = decodeSeries(series(dep)); err != nil {
				return fmt.Errorf("reference at %s: series of %q: %w", ptr, from, err)
			}
			docs[dep] = doc
		}
		v, err := path.Eval(doc)
		if err != nil {
			return fmt.Errorf("reference at %s: series of %q: %w", ptr, from, err)
		}
		set(v)
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrReference, err)
	}
	f.Input = input
	return true, nil
}

// decodeSeries decodes a resolved series with number fidelity.
func decodeSeries(body []byte) (any, error) {
	if body == nil {
		return nil, errors.New("not resolved")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// parseRef validates one reference object and parses its path.
func parseRef(ref map[string]any) (from string, path PathExpr, err error) {
	from, ok := ref[refKey].(string)
	if !ok || from == "" {
		return "", PathExpr{}, fmt.Errorf("%s must be a non-empty string", refKey)
	}
	expr := pathMarker
	if raw, has := ref[pathKey]; has {
		s, isStr := raw.(string)
		if !isStr {
			return "", PathExpr{}, fmt.Errorf("%s must be a string", pathKey)
		}
		if !strings.HasPrefix(s, pathMarker) {
			s = pathMarker + s
		}
		expr = s
	}
	for _, k := range slices.Sorted(maps.Keys(ref)) {
		if k != refKey && k != pathKey {
			return "", PathExpr{}, fmt.Errorf("unknown key %q next to %s (only %s is allowed)", k, refKey, pathKey)
		}
	}
	if path, err = ParsePath(expr); err != nil {
		return "", PathExpr{}, err
	}
	return from, path, nil
}

// walkRefs visits every reference object below node, in deterministic
// order, handing visit the object, its JSON pointer and a setter that
// overwrites its slot. It does not descend into a reference.
func walkRefs(node any, ptr string, visit func(ref map[string]any, ptr string, set func(any)) error) error {
	switch n := node.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(n)) {
			if err := visitChild(n[k], ptr+"/"+escapePointer(k), func(v any) { n[k] = v }, visit); err != nil {
				return err
			}
		}
	case []any:
		for i, el := range n {
			if err := visitChild(el, ptr+"/"+strconv.Itoa(i), func(v any) { n[i] = v }, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

func visitChild(child any, ptr string, set func(any), visit func(ref map[string]any, ptr string, set func(any)) error) error {
	if m, ok := child.(map[string]any); ok {
		if _, isRef := m[refKey]; isRef {
			return visit(m, ptr, set)
		}
	}
	return walkRefs(child, ptr, visit)
}

// deepCopy clones decoded JSON so a filled input never aliases the payload.
func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = deepCopy(e)
		}
		return m
	case []any:
		s := make([]any, len(x))
		for i, e := range x {
			s[i] = deepCopy(e)
		}
		return s
	default:
		return v
	}
}

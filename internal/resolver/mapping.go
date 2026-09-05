package resolver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strconv"
	"strings"
)

// pathMarker introduces a path expression in a response_map value (jq
// style: ".outputs.hourly[*].P"); any other string is a literal. A leading
// dot, not "$": configuration files interpolate ${VAR} over the whole file,
// and a plain YAML scalar may start with a dot.
const pathMarker = "."

// PathExpr is a parsed response_map path such as ".outputs.hourly[*].P":
// dot-separated segments navigating objects (by key) and arrays (by index),
// at most one of which projects over every element of an array with [*].
// "." alone selects the whole response.
type PathExpr struct {
	segs []pathSeg
}

type pathSeg struct {
	name    string
	project bool
}

// ParsePath parses a path expression.
func ParsePath(expr string) (PathExpr, error) {
	if !strings.HasPrefix(expr, pathMarker) {
		return PathExpr{}, fmt.Errorf("path %q must start with %q", expr, pathMarker)
	}
	rest := strings.TrimPrefix(expr, pathMarker)
	if rest == "" {
		return PathExpr{}, nil
	}
	var p PathExpr
	projections := 0
	for _, raw := range strings.Split(rest, ".") {
		seg := pathSeg{name: raw}
		if strings.HasSuffix(raw, "[*]") {
			seg.name, seg.project = strings.TrimSuffix(raw, "[*]"), true
			projections++
		}
		if seg.name == "" {
			return PathExpr{}, fmt.Errorf("path %q has an empty segment", expr)
		}
		p.segs = append(p.segs, seg)
	}
	if projections > 1 {
		return PathExpr{}, fmt.Errorf("path %q: at most one [*] projection is supported", expr)
	}
	return p, nil
}

// Eval navigates a decoded response. Numbers stay json.Number, so values
// reach the substituted series verbatim.
func (p PathExpr) Eval(doc any) (any, error) { return evalSegs(doc, p.segs) }

func evalSegs(node any, segs []pathSeg) (any, error) {
	for i, seg := range segs {
		var err error
		if node, err = child(node, seg.name); err != nil {
			return nil, err
		}
		if seg.project {
			arr, ok := node.([]any)
			if !ok {
				return nil, fmt.Errorf("segment %q: [*] needs an array, got %T", seg.name, node)
			}
			out := make([]any, len(arr))
			for j, el := range arr {
				if out[j], err = evalSegs(el, segs[i+1:]); err != nil {
					return nil, fmt.Errorf("element %d: %w", j, err)
				}
			}
			return out, nil
		}
	}
	return node, nil
}

func child(node any, name string) (any, error) {
	switch n := node.(type) {
	case map[string]any:
		v, ok := n[name]
		if !ok {
			return nil, fmt.Errorf("segment %q: not found", name)
		}
		return v, nil
	case []any:
		idx, err := strconv.Atoi(name)
		if err != nil {
			return nil, fmt.Errorf("segment %q: response is an array, expected a numeric index", name)
		}
		if idx < 0 || idx >= len(n) {
			return nil, fmt.Errorf("segment %q: index out of range (array has %d elements)", name, len(n))
		}
		return n[idx], nil
	default:
		return nil, fmt.Errorf("segment %q: not an object or array", name)
	}
}

// ApplyMap builds the substituted object from a backend response according
// to a response_map: every key of the map becomes a key of the result. A
// ".path" string is evaluated against the response; an object {path, scale}
// additionally multiplies the selected number(s); {value: v} copies v as a
// literal (the escape hatch for a literal string starting with "."); any
// other value is a literal. Keys are processed in sorted order, so the first
// error reported is deterministic.
func ApplyMap(body []byte, m map[string]any) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("response is not JSON: %w", err)
	}
	out := make(map[string]any, len(m))
	for _, key := range slices.Sorted(maps.Keys(m)) {
		v, err := mapValue(doc, m[key])
		if err != nil {
			return nil, fmt.Errorf("response_map %q: %w", key, err)
		}
		out[key] = v
	}
	return json.Marshal(out)
}

func mapValue(doc any, spec any) (any, error) {
	switch s := spec.(type) {
	case string:
		if !strings.HasPrefix(s, pathMarker) {
			return s, nil
		}
		p, err := ParsePath(s)
		if err != nil {
			return nil, err
		}
		return p.Eval(doc)
	case map[string]any:
		if v, ok := s["value"]; ok {
			return v, nil
		}
		raw, ok := s["path"]
		if !ok {
			return s, nil // a literal object
		}
		pathStr, _ := raw.(string)
		p, err := ParsePath(pathStr)
		if err != nil {
			return nil, err
		}
		v, err := p.Eval(doc)
		if err != nil {
			return nil, err
		}
		if sc, ok := s["scale"]; ok {
			factor, err := scaleFactor(sc)
			if err != nil {
				return nil, err
			}
			return scaleValue(v, factor)
		}
		return v, nil
	default:
		return spec, nil
	}
}

// scaleValue multiplies a number or every number of an array; null stays
// null (a missing reading), anything else cannot be scaled.
func scaleValue(v any, factor *big.Rat) (any, error) {
	switch n := v.(type) {
	case json.Number:
		return scaleNumber(n, factor)
	case []any:
		out := make([]any, len(n))
		for i, el := range n {
			var err error
			if out[i], err = scaleValue(el, factor); err != nil {
				return nil, fmt.Errorf("element %d: %w", i, err)
			}
		}
		return out, nil
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("cannot scale a %T", v)
	}
}

// scaleNumber multiplies exactly in decimal arithmetic, so 612.4 × 0.001 is
// 0.6124 rather than the binary float64 product 0.6123999999999999, and
// renders the result with exactly the decimals it needs.
func scaleNumber(n json.Number, factor *big.Rat) (json.Number, error) {
	value, ok := new(big.Rat).SetString(n.String())
	if !ok {
		return "", fmt.Errorf("%q is not a number", n)
	}
	return json.Number(formatRat(value.Mul(value, factor))), nil
}

// formatRat renders a rational as the shortest exact decimal; a
// non-terminating result (a scale like 1/3) is rounded to 16 decimals.
func formatRat(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	twos, fives := 0, 0
	rest := new(big.Int).Set(r.Denom())
	two, five, zero := big.NewInt(2), big.NewInt(5), big.NewInt(0)
	for new(big.Int).Mod(rest, two).Cmp(zero) == 0 {
		rest.Div(rest, two)
		twos++
	}
	for new(big.Int).Mod(rest, five).Cmp(zero) == 0 {
		rest.Div(rest, five)
		fives++
	}
	decimals := max(twos, fives)
	if rest.Cmp(big.NewInt(1)) != 0 {
		decimals = 16 // non-terminating
	}
	return strings.TrimRight(strings.TrimRight(r.FloatString(decimals), "0"), ".")
}

// scaleFactor reads the scale from the map (a YAML int or float, or a
// json.Number) as an exact rational.
func scaleFactor(v any) (*big.Rat, error) {
	var text string
	switch n := v.(type) {
	case int:
		text = strconv.Itoa(n)
	case int64:
		text = strconv.FormatInt(n, 10)
	case float64:
		text = strconv.FormatFloat(n, 'g', -1, 64)
	case json.Number:
		text = n.String()
	default:
		return nil, fmt.Errorf("scale must be a number, got %T", v)
	}
	r, ok := new(big.Rat).SetString(text)
	if !ok {
		return nil, fmt.Errorf("scale %q is not a number", text)
	}
	return r, nil
}

// ValidateResponseMap checks a response_map's shape and path syntax without
// a response: keys must be non-empty, "." strings must parse, and the
// {path, scale} form must carry a string path, a numeric scale and nothing
// else.
func ValidateResponseMap(m map[string]any) error {
	if len(m) == 0 {
		return fmt.Errorf("response_map must not be empty")
	}
	for _, key := range slices.Sorted(maps.Keys(m)) {
		if key == "" {
			return fmt.Errorf("response_map has an empty key")
		}
		if err := validateMapSpec(m[key]); err != nil {
			return fmt.Errorf("response_map %q: %w", key, err)
		}
	}
	return nil
}

func validateMapSpec(spec any) error {
	switch s := spec.(type) {
	case string:
		if strings.HasPrefix(s, pathMarker) {
			_, err := ParsePath(s)
			return err
		}
	case map[string]any:
		raw, isPath := s["path"]
		if !isPath {
			return nil // {value: …} or a literal object
		}
		pathStr, ok := raw.(string)
		if !ok {
			return fmt.Errorf("path must be a string, got %T", raw)
		}
		if _, err := ParsePath(pathStr); err != nil {
			return err
		}
		for k, v := range s {
			switch k {
			case "path":
			case "scale":
				if _, err := scaleFactor(v); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unknown key %q next to path (only scale is allowed)", k)
			}
		}
	}
	return nil
}

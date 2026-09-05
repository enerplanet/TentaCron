// Package plan inspects a request without calling any upstream: which
// resolvents a payload contains for a given target, and which problems would
// fail the job before the first outbound call. The worker uses it to start
// processing and the API to answer dry runs, so both can never disagree.
package plan

import (
	"fmt"
	"maps"
	"slices"

	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/resolver"
	"github.com/enerplanet/tentacron/internal/upstream"
)

// Job failure codes a plan can predict. They match the worker's.
const (
	CodeUnknownTarget    = "unknown_target"
	CodeInvalidPayload   = "invalid_payload"
	CodeUnknownResolvent = "unknown_resolvent"
	CodeTargetError      = "target_error"
)

// Problem is one reason the job would fail before any upstream call.
type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (p Problem) Error() string { return p.Message }

// Plan is the result of inspecting a payload for a target.
type Plan struct {
	// Target is the target's configuration when it exists.
	Target *config.Target
	// Root is the decoded payload, ready for substitution (nil for proxy
	// targets and invalid payloads).
	Root map[string]any
	// Found lists the resolvent objects in document order.
	Found []*resolver.Found
	// Problems lists what would fail the job, in the order the worker would
	// hit them; empty means the job can start.
	Problems []Problem
}

// OK reports whether processing could start.
func (p Plan) OK() bool { return len(p.Problems) == 0 }

// Inspect parses the payload as the worker would for the named target and
// locates its resolvents, refusing unknown targets and resolvent types. A
// proxy target's payload is checked against the target's URL placeholders
// instead of being scanned.
func Inspect(cfg *config.Config, target string, payload []byte) Plan {
	tcfg, ok := cfg.Targets[target]
	if !ok {
		return Plan{Problems: []Problem{{CodeUnknownTarget, fmt.Sprintf("target %q is not configured", target)}}}
	}
	p := Plan{Target: &tcfg}
	if tcfg.Proxy {
		if err := upstream.CheckTargetPayload(tcfg, payload); err != nil {
			p.Problems = append(p.Problems, Problem{CodeTargetError, err.Error()})
		}
		return p
	}
	root, err := resolver.Parse(payload)
	if err != nil {
		p.Problems = append(p.Problems, Problem{CodeInvalidPayload, err.Error()})
		return p
	}
	found, err := resolver.Find(root, ContainerPath(tcfg))
	if err != nil {
		p.Problems = append(p.Problems, Problem{CodeInvalidPayload, err.Error()})
		return p
	}
	p.Root, p.Found = root, found
	for _, typ := range resolver.DistinctTypes(found) {
		if _, ok := cfg.Resolvents[typ]; !ok {
			p.Problems = append(p.Problems, Problem{CodeUnknownResolvent, fmt.Sprintf("no resolvent config for type %q", typ)})
		}
	}
	return p
}

// ContainerPath translates the "." sentinel into the resolver's root scan:
// the resolver treats "" as root, while the config layer reserves "" for
// "use the default path".
func ContainerPath(tcfg config.Target) string {
	if tcfg.TimeseriesPath == config.RootTimeseriesPath {
		return ""
	}
	return tcfg.TimeseriesPath
}

// TargetInfo is what discovery reveals about a target: routing knobs, never
// URLs or credentials.
type TargetInfo struct {
	Name            string `json:"name"`
	ResponseMode    string `json:"response_mode"`
	Proxy           bool   `json:"proxy"`
	TimeseriesPath  string `json:"timeseries_path,omitempty"`
	AttachResolvent *bool  `json:"attach_resolvent,omitempty"`
}

// ResolventInfo is what discovery reveals about a resolvent type.
type ResolventInfo struct {
	Type     string `json:"type"`
	Backend  string `json:"backend"` // "get", "post" (any body method) or "target"
	Target   string `json:"target,omitempty"`
	CacheTTL string `json:"cache_ttl"`
}

// Targets lists the configured targets, sorted by name.
func Targets(cfg *config.Config) []TargetInfo {
	out := make([]TargetInfo, 0, len(cfg.Targets))
	for _, name := range slices.Sorted(maps.Keys(cfg.Targets)) {
		t := cfg.Targets[name]
		info := TargetInfo{Name: name, ResponseMode: t.Response.Mode, Proxy: t.Proxy}
		if !t.Proxy {
			info.TimeseriesPath = t.TimeseriesPath
			attach := t.AttachResolvent == nil || *t.AttachResolvent
			info.AttachResolvent = &attach
		}
		out = append(out, info)
	}
	return out
}

// Resolvents lists the configured resolvent types, sorted.
func Resolvents(cfg *config.Config) []ResolventInfo {
	out := make([]ResolventInfo, 0, len(cfg.Resolvents))
	for _, typ := range slices.Sorted(maps.Keys(cfg.Resolvents)) {
		r := cfg.Resolvents[typ]
		info := ResolventInfo{Type: typ, CacheTTL: r.CacheTTL.Std().String()}
		switch {
		case r.Target != "":
			info.Backend, info.Target = "target", r.Target
		case r.Method == "GET":
			info.Backend = "get"
		default:
			info.Backend = "post"
		}
		out = append(out, info)
	}
	return out
}

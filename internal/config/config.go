// Package config loads and validates the tentacron YAML configuration.
package config

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so YAML values like "30s" parse directly.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Std returns the wrapped time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Config is the root configuration.
type Config struct {
	Server     Server               `yaml:"server"`
	Auth       Auth                 `yaml:"auth"`
	Storage    Storage              `yaml:"storage"`
	Worker     Worker               `yaml:"worker"`
	Cache      Cache                `yaml:"cache"`
	Targets    map[string]Target    `yaml:"targets"`
	Resolvents map[string]Resolvent `yaml:"resolvents"`
}

// Server holds HTTP server settings.
type Server struct {
	Addr          string   `yaml:"addr"`
	ReadTimeout   Duration `yaml:"read_timeout"`
	WriteTimeout  Duration `yaml:"write_timeout"`
	ShutdownGrace Duration `yaml:"shutdown_grace"`
	MaxBodyBytes  int64    `yaml:"max_body_bytes"`
}

// APIKey is one named client credential.
type APIKey struct {
	Name string `yaml:"name"`
	Key  string `yaml:"key"`
}

// Auth lists accepted client API keys.
type Auth struct {
	APIKeys []APIKey `yaml:"api_keys"`
}

// Storage holds persistence settings.
type Storage struct {
	Path       string   `yaml:"path"`
	ResultsDir string   `yaml:"results_dir"`
	Retention  Duration `yaml:"retention"`
}

// Worker holds job-processing settings.
type Worker struct {
	Count                int      `yaml:"count"`
	ResolventConcurrency int      `yaml:"resolvent_concurrency"`
	PollInterval         Duration `yaml:"poll_interval"`
	MaxAttempts          int      `yaml:"max_attempts"`
	BackoffBase          Duration `yaml:"backoff_base"`
	BackoffMax           Duration `yaml:"backoff_max"`
	JobTimeout           Duration `yaml:"job_timeout"`
}

// Cache holds resolved-series cache settings.
type Cache struct {
	DefaultTTL      Duration `yaml:"default_ttl"`
	CleanupInterval Duration `yaml:"cleanup_interval"`
}

// API-key injection modes for outbound target calls.
const (
	InjectNone      = "none"
	InjectBodyField = "body_field"
	InjectHeader    = "header"
)

// Response modes for targets.
const (
	ModeDirect = "direct"
	ModePoll   = "poll"
)

// RootTimeseriesPath is the timeseries_path value that scans the whole
// payload for resolvent objects instead of one named container — for targets
// like buem-gateway whose time series (the weather block) sits at the payload
// root. ("" cannot express this: it means "use the default".)
const RootTimeseriesPath = "."

// Target describes one downstream target workflow (e.g. meme, buem).
type Target struct {
	URL            string   `yaml:"url"`
	Method         string   `yaml:"method"`
	APIKey         string   `yaml:"api_key"`
	APIKeyInject   string   `yaml:"api_key_inject"`
	APIKeyField    string   `yaml:"api_key_field"`
	APIKeyHeader   string   `yaml:"api_key_header"`
	Timeout        Duration `yaml:"timeout"`
	TimeseriesPath string   `yaml:"timeseries_path"`
	// AttachResolvent controls whether the original resolvent object is
	// preserved under the substituted series' "resolvent" key (tentacron's
	// default traceability contract). Set false for targets whose schema
	// validation rejects unknown keys (e.g. buem-gateway forwarding to
	// BuEM's GeoJSON validator); the audit store keeps full traceability
	// either way.
	AttachResolvent *bool    `yaml:"attach_resolvent"`
	Response        Response `yaml:"response"`
}

// Response describes how a target reports its result.
type Response struct {
	Mode string `yaml:"mode"`
	Poll *Poll  `yaml:"poll"`
}

// Poll describes how to poll an async target job to completion.
type Poll struct {
	IDJSONPath        string   `yaml:"id_json_path"`
	URLTemplate       string   `yaml:"url_template"`
	ResultURLTemplate string   `yaml:"result_url_template"`
	StatusJSONPath    string   `yaml:"status_json_path"`
	DoneValues        []string `yaml:"done_values"`
	FailedValues      []string `yaml:"failed_values"`
	Interval          Duration `yaml:"interval"`
	Timeout           Duration `yaml:"timeout"`
}

// Resolvent describes the backend for one resolvent type: either a resource
// API (URL) or a configured target (Target).
type Resolvent struct {
	URL          string   `yaml:"url"`
	Method       string   `yaml:"method"`
	APIKey       string   `yaml:"api_key"`
	APIKeyHeader string   `yaml:"api_key_header"`
	Timeout      Duration `yaml:"timeout"`
	CacheTTL     Duration `yaml:"cache_ttl"`

	// Target names a configured direct-mode target that backs this resolvent
	// instead of a raw resource URL — tentacron composing its own targets,
	// e.g. a BuEM simulation feeding a MEME model. The nested payload is
	// forwarded to that target as-is (never re-resolved), so resolvent loops
	// are impossible by construction; the target's url/auth/timeout apply.
	// Mutually exclusive with URL.
	Target string `yaml:"target"`
	// PayloadField selects which field of the resolvent object is sent as
	// the call's payload. Empty sends the whole resolvent object (the
	// classic resource-API contract).
	PayloadField string `yaml:"payload_field"`
	// ResponsePath optionally extracts a sub-object of the response (dot
	// separated) as the substituted series — e.g.
	// "buem.thermal_load_profile.timeseries". Empty substitutes the whole
	// response.
	ResponsePath string `yaml:"response_path"`
}

// Load reads the YAML file at path, interpolates ${ENV} references, applies
// defaults and validates the result. Referencing an unset environment
// variable is an error so the service never starts with empty credentials.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- path comes from the operator's -config flag
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	expanded, err := expandEnv(string(raw))
	if err != nil {
		return nil, err
	}

	dec := yaml.NewDecoder(strings.NewReader(expanded))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// expandEnv substitutes ${VAR} and $VAR. "$$" escapes a literal "$".
// Unset variables are collected and reported as one error.
func expandEnv(s string) (string, error) {
	var missing []string
	out := os.Expand(s, func(name string) string {
		if name == "$" {
			return "$"
		}
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			return ""
		}
		return v
	})
	if len(missing) > 0 {
		slices.Sort(missing)
		missing = slices.Compact(missing)
		return "", fmt.Errorf("config references unset environment variables: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	setDur(&c.Server.ReadTimeout, 10*time.Second)
	setDur(&c.Server.WriteTimeout, 30*time.Second)
	setDur(&c.Server.ShutdownGrace, 20*time.Second)
	if c.Server.MaxBodyBytes == 0 {
		c.Server.MaxBodyBytes = 10 << 20
	}

	if c.Storage.Path == "" {
		c.Storage.Path = "./data/tentacron.db"
	}
	if c.Storage.ResultsDir == "" {
		c.Storage.ResultsDir = "./data/results"
	}
	setDur(&c.Storage.Retention, 720*time.Hour)

	if c.Worker.Count == 0 {
		c.Worker.Count = 4
	}
	if c.Worker.ResolventConcurrency == 0 {
		c.Worker.ResolventConcurrency = 4
	}
	setDur(&c.Worker.PollInterval, 2*time.Second)
	if c.Worker.MaxAttempts == 0 {
		c.Worker.MaxAttempts = 5
	}
	setDur(&c.Worker.BackoffBase, 2*time.Second)
	setDur(&c.Worker.BackoffMax, 60*time.Second)
	setDur(&c.Worker.JobTimeout, 5*time.Minute)

	setDur(&c.Cache.DefaultTTL, 6*time.Hour)
	setDur(&c.Cache.CleanupInterval, 15*time.Minute)

	for name, t := range c.Targets {
		t.applyDefaults()
		c.Targets[name] = t
	}
	for name, r := range c.Resolvents {
		r.applyDefaults(c.Cache.DefaultTTL)
		c.Resolvents[name] = r
	}
}

func (t *Target) applyDefaults() {
	if t.Method == "" {
		t.Method = "POST"
	}
	if t.TimeseriesPath == "" {
		t.TimeseriesPath = "time-series"
	}
	if t.AttachResolvent == nil {
		attach := true
		t.AttachResolvent = &attach
	}
	setDur(&t.Timeout, 60*time.Second)
	if t.APIKeyInject == "" {
		if t.APIKey == "" {
			t.APIKeyInject = InjectNone
		} else {
			t.APIKeyInject = InjectHeader
		}
	}
	if t.APIKeyInject == InjectBodyField && t.APIKeyField == "" {
		t.APIKeyField = "api_key"
	}
	if t.APIKeyInject == InjectHeader && t.APIKeyHeader == "" {
		t.APIKeyHeader = "X-API-Key"
	}
	if t.Response.Mode == "" {
		t.Response.Mode = ModeDirect
	}
	if t.Response.Mode == ModePoll && t.Response.Poll != nil {
		p := t.Response.Poll
		setDur(&p.Interval, 10*time.Second)
		setDur(&p.Timeout, 30*time.Minute)
		if p.ResultURLTemplate == "" {
			p.ResultURLTemplate = p.URLTemplate
		}
	}
}

func (r *Resolvent) applyDefaults(defaultTTL Duration) {
	// URL-call defaults only apply to URL-backed resolvents: a target-backed
	// resolvent inherits transport settings from its backing target, and
	// defaulted-but-dead fields here would only mislead the operator.
	if r.Target == "" {
		if r.Method == "" {
			r.Method = "POST"
		}
		if r.APIKeyHeader == "" {
			r.APIKeyHeader = "X-API-Key"
		}
		setDur(&r.Timeout, 30*time.Second)
	}
	if r.CacheTTL == 0 {
		r.CacheTTL = defaultTTL
	}
}

func setDur(d *Duration, def time.Duration) {
	if *d == 0 {
		*d = Duration(def)
	}
}

// UpstreamSecrets returns every configured downstream credential (target and
// resolvent API keys). The upstream client redacts these from error excerpts
// so a service echoing a request back can never leak a key into logs, the
// audit store, or API responses. The list is ordered longest-first so a
// shorter credential can never split a longer one during redaction.
func (c *Config) UpstreamSecrets() []string {
	var secrets []string
	for _, t := range c.Targets {
		if t.APIKey != "" {
			secrets = append(secrets, t.APIKey)
		}
	}
	for _, r := range c.Resolvents {
		if r.APIKey != "" {
			secrets = append(secrets, r.APIKey)
		}
	}
	sort.Slice(secrets, func(i, j int) bool {
		if len(secrets[i]) != len(secrets[j]) {
			return len(secrets[i]) > len(secrets[j])
		}
		return secrets[i] < secrets[j]
	})
	return secrets
}

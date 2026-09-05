// Package config loads and validates the tentacron YAML configuration.
package config

import (
	"fmt"
	"log/slog"
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
	// MetricsAddr, when set, serves Prometheus metrics on a second listener
	// that answers /metrics only, so they never share the public listener.
	MetricsAddr string `yaml:"metrics_addr"`
	// LogLevel is the minimum level written to the structured log: debug,
	// info (default), warn or error.
	LogLevel string `yaml:"log_level"`
}

// SlogLevel maps the configured log level onto slog; an unknown value (which
// validation rejects) falls back to info.
func (s Server) SlogLevel() slog.Level {
	switch s.LogLevel {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Roles a client key can hold.
const (
	RoleClient = "client" // reads only the requests it submitted
	RoleAdmin  = "admin"  // reads every request and may filter lists by client
)

// APIKey is one named client credential.
type APIKey struct {
	Name string `yaml:"name"`
	Key  string `yaml:"key"`
	// Role scopes what the key may read: a client sees only the requests it
	// submitted, an admin sees all of them. Defaults to client.
	Role string `yaml:"role"`
}

// Auth lists accepted client API keys.
type Auth struct {
	APIKeys []APIKey `yaml:"api_keys"`
}

func (a *Auth) applyDefaults() {
	for i := range a.APIKeys {
		if a.APIKeys[i].Role == "" {
			a.APIKeys[i].Role = RoleClient
		}
	}
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
	URL          string   `yaml:"url"`
	Method       string   `yaml:"method"`
	APIKey       string   `yaml:"api_key"`
	APIKeyInject string   `yaml:"api_key_inject"`
	APIKeyField  string   `yaml:"api_key_field"`
	APIKeyHeader string   `yaml:"api_key_header"`
	Timeout      Duration `yaml:"timeout"`
	// JobTimeout bounds one processing attempt (resolution plus the
	// forward) for jobs of this target; zero inherits worker.job_timeout.
	// Validation requires it to cover Timeout plus the longest resolvent
	// timeout, or a slow forward is cut off by the job deadline, classified
	// transient, and the target's work is submitted again.
	JobTimeout Duration `yaml:"job_timeout"`
	// MaxAttempts caps processing attempts for jobs of this target; zero
	// inherits worker.max_attempts.
	MaxAttempts int `yaml:"max_attempts"`
	// RetryOnTimeout decides what a deadline hit on the forward means: nil
	// or true requeues the job like any transient failure; false fails it
	// with target_timeout. Set false for synchronous targets whose work is
	// expensive or not idempotent — the target may still be processing the
	// request, and a retry would run it twice.
	RetryOnTimeout *bool  `yaml:"retry_on_timeout"`
	TimeseriesPath string `yaml:"timeseries_path"`
	// AttachResolvent controls whether the original resolvent object is
	// preserved under the substituted series' "resolvent" key (tentacron's
	// default traceability contract). Set false for targets whose schema
	// validation rejects unknown keys (e.g. buem-gateway forwarding to
	// BuEM's GeoJSON validator); the audit store keeps full traceability
	// either way.
	AttachResolvent *bool `yaml:"attach_resolvent"`
	// Proxy hands the payload through unresolved: no resolvent scanning,
	// no substitution — tentacron contributes auth, persistence, audit and
	// retries only. Without URL placeholders the payload is forwarded
	// byte-exact; {field} placeholders in the URL are filled from top-level
	// payload fields, which are then stripped from the forwarded body
	// (they address the call, they are not payload).
	Proxy    bool     `yaml:"proxy"`
	Response Response `yaml:"response"`
}

// RetriesOnTimeout reports whether a deadline hit on the forward requeues
// the job (the default) or fails it with target_timeout.
func (t Target) RetriesOnTimeout() bool { return t.RetryOnTimeout == nil || *t.RetryOnTimeout }

// JobTimeoutFor returns the attempt deadline for jobs of target: the
// target's own job_timeout when set, otherwise worker.job_timeout (which
// also covers a target that is no longer configured).
func (c *Config) JobTimeoutFor(target string) time.Duration {
	if t, ok := c.Targets[target]; ok && t.JobTimeout > 0 {
		return t.JobTimeout.Std()
	}
	return c.Worker.JobTimeout.Std()
}

// MaxAttemptsFor returns the attempt ceiling for jobs of target: the
// target's own max_attempts when set, otherwise worker.max_attempts.
func (c *Config) MaxAttemptsFor(target string) int {
	if t, ok := c.Targets[target]; ok && t.MaxAttempts > 0 {
		return t.MaxAttempts
	}
	return c.Worker.MaxAttempts
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
	c.Server.applyDefaults()
	c.Auth.applyDefaults()
	c.Storage.applyDefaults()
	c.Worker.applyDefaults()
	c.Cache.applyDefaults()
	for name, t := range c.Targets {
		t.applyDefaults()
		c.Targets[name] = t
	}
	for name, r := range c.Resolvents {
		r.applyDefaults(c.Cache.DefaultTTL)
		c.Resolvents[name] = r
	}
}

func (s *Server) applyDefaults() {
	if s.Addr == "" {
		s.Addr = ":8080"
	}
	setDur(&s.ReadTimeout, 10*time.Second)
	setDur(&s.WriteTimeout, 30*time.Second)
	setDur(&s.ShutdownGrace, 20*time.Second)
	if s.MaxBodyBytes == 0 {
		s.MaxBodyBytes = 10 << 20
	}
	if s.LogLevel == "" {
		s.LogLevel = "info"
	}
}

func (s *Storage) applyDefaults() {
	if s.Path == "" {
		s.Path = "./data/tentacron.db"
	}
	if s.ResultsDir == "" {
		s.ResultsDir = "./data/results"
	}
	setDur(&s.Retention, 720*time.Hour)
}

func (w *Worker) applyDefaults() {
	if w.Count == 0 {
		w.Count = 4
	}
	if w.ResolventConcurrency == 0 {
		w.ResolventConcurrency = 4
	}
	setDur(&w.PollInterval, 2*time.Second)
	if w.MaxAttempts == 0 {
		w.MaxAttempts = 5
	}
	setDur(&w.BackoffBase, 2*time.Second)
	setDur(&w.BackoffMax, 60*time.Second)
	setDur(&w.JobTimeout, 5*time.Minute)
}

func (c *Cache) applyDefaults() {
	setDur(&c.DefaultTTL, 6*time.Hour)
	setDur(&c.CleanupInterval, 15*time.Minute)
}

func (t *Target) applyDefaults() {
	if t.Method == "" {
		t.Method = "POST"
	}
	setDur(&t.Timeout, 60*time.Second)
	t.applyResolutionDefaults()
	t.applyAuthDefaults()
	t.applyResponseDefaults()
}

// applyResolutionDefaults fills the resolution knobs. A proxy target gets
// none: they have no meaning there, defaulting them would only mislead the
// operator, and validation rejects explicit ones.
func (t *Target) applyResolutionDefaults() {
	if t.Proxy {
		return
	}
	if t.TimeseriesPath == "" {
		t.TimeseriesPath = "time-series"
	}
	if t.AttachResolvent == nil {
		attach := true
		t.AttachResolvent = &attach
	}
}

// applyAuthDefaults derives the injection mode from whether a key is set,
// then the field or header name for that mode.
func (t *Target) applyAuthDefaults() {
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
}

func (t *Target) applyResponseDefaults() {
	if t.Response.Mode == "" {
		t.Response.Mode = ModeDirect
	}
	if t.Response.Mode != ModePoll || t.Response.Poll == nil {
		return
	}
	p := t.Response.Poll
	setDur(&p.Interval, 10*time.Second)
	setDur(&p.Timeout, 30*time.Minute)
	if p.ResultURLTemplate == "" {
		p.ResultURLTemplate = p.URLTemplate
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

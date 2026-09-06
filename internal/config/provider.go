package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
)

// Provider hands out the current configuration and swaps it on reload. The
// API reads it per request and the worker per job, so a reload changes
// keys, targets, resolvents, cache rules and callbacks without a restart
// while a job in flight keeps the configuration it started with.
type Provider struct {
	path    string
	mu      sync.Mutex // serialises reloads
	current atomic.Pointer[Config]
	hash    atomic.Pointer[string]
}

// NewProvider loads path and serves it until the next successful Reload.
func NewProvider(path string) (*Provider, error) {
	p := &Provider{path: path}
	cfg, hash, err := loadFile(path)
	if err != nil {
		return nil, err
	}
	p.swap(cfg, hash)
	return p, nil
}

// Static wraps an already-built configuration (tests, embedders); Reload is
// a no-op on it.
func Static(cfg *Config) *Provider {
	p := &Provider{}
	p.swap(cfg, "")
	return p
}

func (p *Provider) swap(cfg *Config, hash string) {
	p.current.Store(cfg)
	p.hash.Store(&hash)
}

// Current returns the configuration in force. Callers read it once per unit
// of work and keep that pointer, never re-reading midway.
func (p *Provider) Current() *Config { return p.current.Load() }

// Hash identifies the loaded file's bytes (SHA-256, hex), so operators can
// tell from the logs which configuration a process runs.
func (p *Provider) Hash() string { return *p.hash.Load() }

// ReloadResult reports what a reload did.
type ReloadResult struct {
	Hash string
	// Changed is false when the file's bytes were identical.
	Changed bool
	// RestartRequired names settings that changed but are only read at
	// startup; the process must be restarted for them to take effect.
	RestartRequired []string
}

// Reload re-reads the file, validates it and swaps it in. An invalid file
// leaves the running configuration untouched and returns the error, so a
// typo can never take a running service down.
func (p *Provider) Reload() (ReloadResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.path == "" {
		return ReloadResult{Hash: p.Hash()}, nil
	}
	cfg, hash, err := loadFile(p.path)
	if err != nil {
		return ReloadResult{}, err
	}
	old := p.Current()
	res := ReloadResult{Hash: hash, Changed: hash != p.Hash(), RestartRequired: restartRequired(old, cfg)}
	p.swap(cfg, hash)
	return res, nil
}

// loadFile reads, expands, parses and validates a file, returning the hash
// of its raw bytes.
func loadFile(path string) (*Config, string, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- path comes from the operator's -config flag
	if err != nil {
		return nil, "", fmt.Errorf("read config: %w", err)
	}
	cfg, err := parse(raw)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	return cfg, hex.EncodeToString(sum[:]), nil
}

// restartRequired lists the startup-only settings that differ between two
// configurations: listeners, timeouts and the body limit of the server
// (everything but the log level and the browser policy, which the API
// swaps in place), storage, the worker's loop shape and the sweep cadence.
// Everything else is read per request or per job.
func restartRequired(old, cur *Config) []string {
	var out []string
	oldServer, curServer := old.Server, cur.Server
	oldServer.LogLevel, curServer.LogLevel = "", ""
	oldServer.CORS, curServer.CORS = CORS{}, CORS{}
	if !reflect.DeepEqual(oldServer, curServer) {
		out = append(out, "server")
	}
	if !reflect.DeepEqual(old.Storage, cur.Storage) {
		out = append(out, "storage")
	}
	for _, f := range []struct {
		name     string
		old, cur any
	}{
		{"worker.count", old.Worker.Count, cur.Worker.Count},
		{"worker.poll_interval", old.Worker.PollInterval, cur.Worker.PollInterval},
		{"worker.scheduler_interval", old.Worker.SchedulerInterval, cur.Worker.SchedulerInterval},
		{"cache.cleanup_interval", old.Cache.CleanupInterval, cur.Cache.CleanupInterval},
	} {
		if f.old != f.cur {
			out = append(out, f.name)
		}
	}
	return out
}

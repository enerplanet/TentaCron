// Command tentacron runs the tentacron orchestration and resolvent API.
//
//	tentacron [serve] -config config.yaml   start the service (the default)
//	tentacron validate -config config.yaml  load and validate a configuration
//	tentacron version                       print build information
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // schedules name IANA zones; the distroless image has no zoneinfo

	"github.com/enerplanet/tentacron/internal/api"
	"github.com/enerplanet/tentacron/internal/callback"
	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/metrics"
	"github.com/enerplanet/tentacron/internal/notify"
	"github.com/enerplanet/tentacron/internal/store"
	"github.com/enerplanet/tentacron/internal/upstream"
	"github.com/enerplanet/tentacron/internal/worker"
)

var version = "dev" // overridden at build time via -ldflags

const usage = `Usage:
  tentacron [serve] [-config FILE]        start the service (default command)
  tentacron validate [-config FILE]       load, interpolate and validate a configuration
  tentacron backup [-config FILE] DEST    write a consistent copy of the database to DEST
  tentacron healthcheck [-addr HOST:PORT] exit 0 when the service at HOST:PORT is ready
  tentacron version                       print build information

FILE defaults to config.yaml. Exit codes: 0 ok, 1 invalid configuration or
runtime failure, 2 usage error.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches on the first argument. A leading flag means "serve", so the
// original `tentacron -config …` invocation keeps working unchanged.
func run(args []string, stdout, stderr io.Writer) int {
	cmd, rest := "serve", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, rest = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return runServe(rest, stdout, stderr)
	case "validate":
		return runValidate(rest, stdout, stderr)
	case "backup":
		return runBackup(rest, stdout, stderr)
	case "healthcheck":
		return runHealthcheck(rest, stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "tentacron %s %s %s/%s\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "tentacron: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

// configFlag parses the shared -config flag and exactly positional
// arguments after it; a help request prints usage and reports done.
func configFlag(name string, args []string, positional int, stdout, stderr io.Writer) (path string, rest []string, done bool, code int) {
	fs := flag.NewFlagSet("tentacron "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&path, "config", "config.yaml", "path to the YAML configuration file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage)
			return "", nil, true, 0
		}
		return "", nil, true, 2
	}
	if fs.NArg() != positional {
		fmt.Fprintf(stderr, "tentacron %s: expected %d argument(s), got %d\n\n%s", name, positional, fs.NArg(), usage)
		return "", nil, true, 2
	}
	return path, fs.Args(), false, 0
}

// runBackup copies the configured database to DEST with VACUUM INTO — a
// consistent snapshot that is safe to take while the service runs, of the
// schema as it is: the command never migrates.
func runBackup(args []string, stdout, stderr io.Writer) int {
	path, rest, done, code := configFlag("backup", args, 1, stdout, stderr)
	if done {
		return code
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", path, err)
		return 1
	}
	// Never migrate the database being backed up: the copy must be of what
	// runs today, and a database from a newer release is refused.
	st, err := store.OpenForBackup(cfg.Storage.Path)
	if err != nil {
		fmt.Fprintf(stderr, "open %s: %v\n", cfg.Storage.Path, err)
		return 1
	}
	defer func() { _ = st.Close() }()
	if err := st.BackupTo(context.Background(), rest[0]); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	info, err := os.Stat(rest[0])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "backup written: %s (%d bytes)\n", rest[0], info.Size())
	return 0
}

// runHealthcheck asks the service at -addr whether it is ready and exits 0
// when it is. It exists for the release image, which has no shell and no
// curl for a Docker HEALTHCHECK to run; a Kubernetes probe reads /readyz
// directly.
func runHealthcheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tentacron healthcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:8080", "host:port the service listens on")
	timeout := fs.Duration("timeout", 5*time.Second, "how long to wait for the answer")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage)
			return 0
		}
		return 2
	}
	if strings.HasPrefix(*addr, ":") {
		*addr = "127.0.0.1" + *addr
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+*addr+"/readyz", http.NoBody)
	if err != nil {
		fmt.Fprintf(stderr, "not ready: %v\n", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "not ready: %v\n", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "not ready: /readyz answered %d\n", resp.StatusCode)
		return 1
	}
	fmt.Fprintln(stdout, "ready")
	return 0
}

// runValidate loads the configuration exactly as serve would — file, ${ENV}
// interpolation, defaults, validation — and prints what it found or every
// problem at once. Meant for deploy pipelines and CI.
func runValidate(args []string, stdout, stderr io.Writer) int {
	path, _, done, code := configFlag("validate", args, 0, stdout, stderr)
	if done {
		return code
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "%s: configuration is invalid:\n", path)
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintf(stderr, "  - %s\n", line)
		}
		return 1
	}
	fmt.Fprint(stdout, config.Describe(cfg))
	return 0
}

func runServe(args []string, stdout, stderr io.Writer) int {
	path, _, done, code := configFlag("serve", args, 0, stdout, stderr)
	if done {
		return code
	}
	// Info until the configuration is loaded and says otherwise; a config
	// error is always logged.
	level := new(slog.LevelVar)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	if err := serveFromConfig(path, logger, level); err != nil {
		logger.Error("fatal", "error", err)
		return 1
	}
	return 0
}

// process is a configured service: its store, its HTTP servers, its
// background loops and what a reload does. run drives it under a context —
// runServe supplies the signal-bound one, a test supplies its own — so the
// lifecycle (listen, serve, reload, drain, park) is exercised the same way
// in both.
type process struct {
	cfg      *config.Config
	provider *config.Provider
	store    *store.Store
	logger   *slog.Logger
	servers  []*http.Server
	api      *api.Server
	loops    []runner
	reload   func()
	// started is closed once every server listens; bound then holds their
	// addresses, which is how a test finds the port it asked for with :0.
	started chan struct{}
	bound   []string
}

// newProcess wires the store, the worker pool, the callback deliverer, the
// API server and the optional metrics listener from the configuration file.
func newProcess(path string, logger *slog.Logger, level *slog.LevelVar) (*process, error) {
	provider, err := config.NewProvider(path)
	if err != nil {
		return nil, err
	}
	cfg := provider.Current()
	level.Set(cfg.Server.SlogLevel())
	logger.Info("configuration loaded", "path", path, "hash", provider.Hash()[:12])
	st, err := store.Open(cfg.Storage.Path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.Storage.ResultsDir, 0o750); err != nil {
		_ = st.Close()
		return nil, err
	}
	schema, err := st.SchemaVersion(context.Background())
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	// An upgrade leaves its trace here: the migrations this start applied.
	logger.Info("database opened", "path", cfg.Storage.Path, "schema", schema, "migrations_applied", st.MigrationsApplied())

	m := metrics.New(st)
	hub := notify.New()
	nudge := make(chan struct{}, 1)
	client := upstream.New(cfg.Upstream.MaxResponseBytes, cfg.UpstreamSecrets()).
		WithConnectionPool(cfg.Worker.Count * cfg.Worker.ResolventConcurrency).WithMetrics(m)
	pool := worker.New(provider, st, client, logger, nudge).WithMetrics(m).WithNotifier(hub)
	deliverer := callback.New(provider, st, logger).WithMetrics(m).WithNotifier(hub)
	apiServer := api.New(provider, st, logger, nudge).WithUpstream(client).WithNotifier(hub).WithMetrics(m)
	apiServer.Build = buildInfo()
	m.SetBuildInfo(apiServer.Build.Version, apiServer.Build.Revision, apiServer.Build.Go)
	servers := []*http.Server{newHTTPServer(cfg, apiServer.Handler())}
	if ms := newMetricsServer(cfg, m); ms != nil {
		servers = append(servers, ms)
	}
	return &process{
		cfg: cfg, provider: provider, store: st, logger: logger, servers: servers, api: apiServer,
		loops:   []runner{pool, deliverer},
		reload:  func() { reloadConfig(provider, logger, level, client, apiServer) },
		started: make(chan struct{}),
	}, nil
}

// serveFromConfig builds the process and runs it until a shutdown signal;
// SIGHUP reloads the configuration.
func serveFromConfig(path string, logger *slog.Logger, level *slog.LevelVar) error {
	p, err := newProcess(path, logger, level)
	if err != nil {
		return err
	}
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, hand the signals back to Go's default
	// handling, so a second one terminates a shutdown that hangs.
	context.AfterFunc(rootCtx, stop)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	reloads := make(chan struct{}, 1)
	go func() {
		for range hup {
			select {
			case reloads <- struct{}{}:
			default: // a reload is already pending
			}
		}
	}()
	return p.run(rootCtx, reloads)
}

// reloadConfig re-reads the configuration on SIGHUP. A file that fails to
// parse or validate is reported and the running configuration kept; a good
// one is swapped in, the log level, the redaction list and the response cap
// follow it, and the settings that still need a restart are named.
func reloadConfig(provider *config.Provider, logger *slog.Logger, level *slog.LevelVar, client *upstream.Client, api *api.Server) {
	res, err := provider.Reload()
	if err != nil {
		logger.Error("configuration reload failed; keeping the running configuration", "error", err)
		return
	}
	cfg := provider.Current()
	level.Set(cfg.Server.SlogLevel())
	client.SetSecrets(cfg.UpstreamSecrets())
	client.SetMaxBody(cfg.Upstream.MaxResponseBytes)
	api.UpdateCORS(cfg.Server.CORS.Policy())
	attrs := []any{"hash", res.Hash[:12], "changed", res.Changed,
		"targets", len(cfg.Targets), "resolvents", len(cfg.Resolvents), "keys", len(cfg.Auth.APIKeys),
		"cors", config.DescribeCORS(cfg.Server.CORS)}
	if len(res.RestartRequired) > 0 {
		attrs = append(attrs, "restart_required", res.RestartRequired)
		logger.Warn("configuration reloaded; some changes need a restart", attrs...)
		return
	}
	logger.Info("configuration reloaded", attrs...)
}

// runner is a background loop that stops when its context ends.
type runner interface{ Run(context.Context) }

// buildInfo combines the -ldflags version with the VCS stamp Go embeds when
// the binary is built inside the repository.
func buildInfo() api.BuildInfo {
	b := api.BuildInfo{Version: version, Go: runtime.Version()}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				b.Revision = s.Value
			case "vcs.time":
				b.Built = s.Value
			}
		}
	}
	return b
}

// newMetricsServer serves /metrics on its own listener, so scrapes never
// share the public API port; nil when server.metrics_addr is unset.
func newMetricsServer(cfg *config.Config, m *metrics.Metrics) *http.Server {
	if cfg.Server.MetricsAddr == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	return &http.Server{
		Addr:              cfg.Server.MetricsAddr,
		Handler:           mux,
		ReadTimeout:       cfg.Server.ReadTimeout.Std(),
		ReadHeaderTimeout: cfg.Server.ReadTimeout.Std(),
		WriteTimeout:      cfg.Server.WriteTimeout.Std(),
	}
}

func newHTTPServer(cfg *config.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           handler,
		ReadTimeout:       cfg.Server.ReadTimeout.Std(),
		ReadHeaderTimeout: cfg.Server.ReadTimeout.Std(), // deliberately the same knob; no separate header timeout in the config
		WriteTimeout:      cfg.Server.WriteTimeout.Std(),
	}
}

// run serves until ctx ends or a server fails, then stops in the documented
// order: long-polls answer, HTTP drains, workers are cancelled, background
// notifications are waited for. Every value on reloads re-reads the
// configuration. The store is closed on return.
func (p *process) run(ctx context.Context, reloads <-chan struct{}) error {
	defer func() { _ = p.store.Close() }()
	// workerCtx is deliberately not derived from ctx: on shutdown the
	// workers must keep finishing in-flight jobs while the HTTP server
	// drains, and are only cancelled explicitly afterwards (or immediately
	// on a fatal server error).
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()
	poolDone := make(chan struct{})
	go func() {
		defer close(poolDone)
		var wg sync.WaitGroup
		for _, l := range p.loops {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l.Run(workerCtx)
			}()
		}
		wg.Wait()
	}()
	serverErr := make(chan error, len(p.servers))
	if err := p.listenAndServe(serverErr); err != nil {
		cancelWorkers()
		<-poolDone
		return err
	}
	p.logger.Info("tentacron started", "version", version, "addr", p.bound[0],
		"metrics_addr", p.cfg.Server.MetricsAddr, "log_level", p.cfg.Server.LogLevel,
		"targets", len(p.cfg.Targets), "resolvents", len(p.cfg.Resolvents),
		"cors", config.DescribeCORS(p.cfg.Server.CORS))

	for {
		select {
		case <-reloads:
			p.reload()
			continue
		case <-ctx.Done():
			p.logger.Info("shutdown signal received")
		case err := <-serverErr:
			cancelWorkers()
			<-poolDone
			return err
		}
		break
	}
	// Long-polls answer now, so the drain is not held open by waits that
	// may be longer than the grace window; every other request completes.
	p.api.BeginDrain()
	drainHTTP(p.cfg, p.logger, p.servers)
	cancelWorkers()
	<-poolDone
	p.api.WaitBackground()
	p.logger.Info("tentacron stopped")
	return nil
}

// listenAndServe binds every server first — so a taken port fails before
// anything runs and a ":0" address reports the port it got — then serves
// each on its own goroutine, reporting a fatal error on errc; a clean
// Shutdown is not an error.
func (p *process) listenAndServe(errc chan<- error) error {
	listeners := make([]net.Listener, 0, len(p.servers))
	for _, srv := range p.servers {
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			for _, open := range listeners {
				_ = open.Close()
			}
			return fmt.Errorf("listen %s: %w", srv.Addr, err)
		}
		listeners = append(listeners, ln)
		p.bound = append(p.bound, ln.Addr().String())
	}
	for i, srv := range p.servers {
		go func() {
			if err := srv.Serve(listeners[i]); !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("serve %s: %w", srv.Addr, err)
			}
		}()
	}
	close(p.started)
	return nil
}

// drainHTTP stops accepting requests and waits out in-flight ones within the
// grace window. Workers are cancelled by the caller afterwards: in-flight
// jobs abort their upstream calls and park themselves back to "received"
// for a clean retry after restart.
func drainHTTP(cfg *config.Config, logger *slog.Logger, servers []*http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownGrace.Std())
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil {
			logger.Warn("http shutdown incomplete", "addr", srv.Addr, "error", err)
		}
	}
}

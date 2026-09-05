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
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/enerplanet/tentacron/internal/api"
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
// consistent snapshot that is safe to take while the service runs.
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
	st, err := store.Open(cfg.Storage.Path)
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

// serveFromConfig wires the store, the worker pool, the API server and the
// optional metrics listener from the configuration file and runs them until
// shutdown.
func serveFromConfig(path string, logger *slog.Logger, level *slog.LevelVar) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	level.Set(cfg.Server.SlogLevel())
	st, err := store.Open(cfg.Storage.Path)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	if err := os.MkdirAll(cfg.Storage.ResultsDir, 0o750); err != nil {
		return err
	}

	m := metrics.New(st)
	hub := notify.New()
	nudge := make(chan struct{}, 1)
	client := upstream.New(cfg.Upstream.MaxResponseBytes, cfg.UpstreamSecrets()).WithMetrics(m)
	pool := worker.New(cfg, st, client, logger, nudge).WithMetrics(m).WithNotifier(hub)
	apiServer := api.New(cfg, st, logger, nudge).WithUpstream(client).WithNotifier(hub)
	apiServer.Build = buildInfo()
	servers := []*http.Server{newHTTPServer(cfg, apiServer.Handler())}
	if ms := newMetricsServer(cfg, m); ms != nil {
		servers = append(servers, ms)
	}
	return serve(cfg, logger, servers, pool)
}

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

// serve runs the worker pool and the HTTP servers until a shutdown signal or
// a fatal server error, then stops them in the documented order: HTTP
// drains first, workers are cancelled afterwards.
func serve(cfg *config.Config, logger *slog.Logger, servers []*http.Server, pool *worker.Pool) error {
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// workerCtx is deliberately not derived from rootCtx: on shutdown the
	// workers must keep finishing in-flight jobs while the HTTP server
	// drains, and are only cancelled explicitly afterwards (or immediately
	// on a fatal server error).
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()
	poolDone := make(chan struct{})
	go func() {
		defer close(poolDone)
		pool.Run(workerCtx)
	}()
	serverErr := make(chan error, len(servers))
	for _, srv := range servers {
		listenAndServe(srv, serverErr)
	}
	logger.Info("tentacron started", "version", version, "addr", cfg.Server.Addr,
		"metrics_addr", cfg.Server.MetricsAddr, "log_level", cfg.Server.LogLevel,
		"targets", len(cfg.Targets), "resolvents", len(cfg.Resolvents))

	select {
	case <-rootCtx.Done():
		logger.Info("shutdown signal received")
	case err := <-serverErr:
		cancelWorkers()
		<-poolDone
		return err
	}
	drainHTTP(cfg, logger, servers)
	cancelWorkers()
	<-poolDone
	logger.Info("tentacron stopped")
	return nil
}

// listenAndServe starts one HTTP server and reports a fatal listen error on
// errc; a clean Shutdown is not an error.
func listenAndServe(srv *http.Server, errc chan<- error) {
	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("listen %s: %w", srv.Addr, err)
		}
	}()
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

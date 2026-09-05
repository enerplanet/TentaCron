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
	"strings"
	"syscall"

	"github.com/enerplanet/tentacron/internal/api"
	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/store"
	"github.com/enerplanet/tentacron/internal/upstream"
	"github.com/enerplanet/tentacron/internal/worker"
)

var version = "dev" // overridden at build time via -ldflags

const usage = `Usage:
  tentacron [serve] [-config FILE]   start the service (default command)
  tentacron validate [-config FILE]  load, interpolate and validate a configuration
  tentacron version                  print build information

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

// configFlag parses the shared -config flag; a help request prints usage
// and reports done.
func configFlag(name string, args []string, stdout, stderr io.Writer) (path string, done bool, code int) {
	fs := flag.NewFlagSet("tentacron "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&path, "config", "config.yaml", "path to the YAML configuration file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage)
			return "", true, 0
		}
		return "", true, 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "tentacron %s: unexpected argument %q\n\n%s", name, fs.Arg(0), usage)
		return "", true, 2
	}
	return path, false, 0
}

// runValidate loads the configuration exactly as serve would — file, ${ENV}
// interpolation, defaults, validation — and prints what it found or every
// problem at once. Meant for deploy pipelines and CI.
func runValidate(args []string, stdout, stderr io.Writer) int {
	path, done, code := configFlag("validate", args, stdout, stderr)
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
	path, done, code := configFlag("serve", args, stdout, stderr)
	if done {
		return code
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	if err := serveFromConfig(path, logger); err != nil {
		logger.Error("fatal", "error", err)
		return 1
	}
	return 0
}

// serveFromConfig wires the store, the worker pool and the HTTP server from
// the configuration file and runs them until shutdown.
func serveFromConfig(path string, logger *slog.Logger) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.Storage.Path)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	if err := os.MkdirAll(cfg.Storage.ResultsDir, 0o750); err != nil {
		return err
	}

	nudge := make(chan struct{}, 1)
	client := upstream.New(cfg.Server.MaxBodyBytes, cfg.UpstreamSecrets())
	pool := worker.New(cfg, st, client, logger, nudge)
	httpServer := newHTTPServer(cfg, api.New(cfg, st, logger, nudge).Handler())
	return serve(cfg, logger, httpServer, pool)
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

// serve runs the worker pool and the HTTP server until a shutdown signal or
// a fatal server error, then stops them in the documented order: HTTP
// drains first, workers are cancelled afterwards.
func serve(cfg *config.Config, logger *slog.Logger, httpServer *http.Server, pool *worker.Pool) error {
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
	serverErr := listenAndServe(httpServer)
	logger.Info("tentacron started", "version", version, "addr", cfg.Server.Addr,
		"targets", len(cfg.Targets), "resolvents", len(cfg.Resolvents))

	select {
	case <-rootCtx.Done():
		logger.Info("shutdown signal received")
	case err := <-serverErr:
		cancelWorkers()
		<-poolDone
		return err
	}
	drainHTTP(cfg, logger, httpServer)
	cancelWorkers()
	<-poolDone
	logger.Info("tentacron stopped")
	return nil
}

// listenAndServe starts the HTTP server and reports a fatal listen error on
// the returned channel; a clean Shutdown is not an error.
func listenAndServe(srv *http.Server) <-chan error {
	errc := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	return errc
}

// drainHTTP stops accepting requests and waits out in-flight ones within the
// grace window. Workers are cancelled by the caller afterwards: in-flight
// jobs abort their upstream calls and park themselves back to "received"
// for a clean retry after restart.
func drainHTTP(cfg *config.Config, logger *slog.Logger, srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownGrace.Std())
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Warn("http shutdown incomplete", "error", err)
	}
}

// Command tentacron runs the tentacron orchestration and resolvent API.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/enerplanet/tentacron/internal/api"
	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/store"
	"github.com/enerplanet/tentacron/internal/upstream"
	"github.com/enerplanet/tentacron/internal/worker"
)

var version = "dev" // overridden at build time via -ldflags

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.yaml", "path to the YAML configuration file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
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

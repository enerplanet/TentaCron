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
	client := upstream.New(cfg.Server.MaxBodyBytes, logger)
	pool := worker.New(cfg, st, client, logger, nudge)
	server := api.New(cfg, st, logger, nudge)

	httpServer := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           server.Handler(),
		ReadTimeout:       cfg.Server.ReadTimeout.Std(),
		ReadHeaderTimeout: cfg.Server.ReadTimeout.Std(),
		WriteTimeout:      cfg.Server.WriteTimeout.Std(),
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()

	poolDone := make(chan struct{})
	go func() {
		defer close(poolDone)
		pool.Run(workerCtx)
	}()

	serverErr := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()
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

	// Drain HTTP first, then stop workers: in-flight jobs abort their
	// upstream calls and park themselves back to "received" for a clean
	// retry after restart.
	shutCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownGrace.Std())
	defer cancel()
	if err := httpServer.Shutdown(shutCtx); err != nil {
		logger.Warn("http shutdown incomplete", "error", err)
	}
	cancelWorkers()
	<-poolDone
	logger.Info("tentacron stopped")
	return nil
}

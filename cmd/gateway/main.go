package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/maximilyin/gateway/internal/config"
	"github.com/maximilyin/gateway/internal/gateway"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "configs/gateway.yaml", "path to the YAML config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.Log.Level}))

	gw, err := gateway.New(cfg, logger)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           gw,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		ReadTimeout:       cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout,
		IdleTimeout:       cfg.Server.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	// ctx is canceled on the first SIGINT/SIGTERM. Docker and Kubernetes send SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("gateway listening", "addr", srv.Addr,
			"upstreams", len(cfg.Upstreams), "routes", len(cfg.Routes))
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		// ListenAndServe only returns early on a real failure, e.g. the port is taken.
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	// Restore default signal handling: a second Ctrl+C kills the process at once.
	stop()
	logger.Info("shutting down", "timeout", cfg.Server.ShutdownTimeout.String())
	gw.SetReady(false)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	// Shutdown stops accepting connections and waits for in-flight requests.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}

	logger.Info("gateway stopped")
	return nil
}

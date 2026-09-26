/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

// lake-sqlite serves one SQLite database over HTTP for lake's sqlite service kind. The Orama
// operator runs it as a one-pod StatefulSet per service (docs/lake/lake-sqlite.md).
//
// Environment:
//
//	LAKE_SQLITE_TOKEN         the bearer token (required; mounted from the service's Secret)
//	LAKE_SQLITE_PATH          the database file (default /data/main.db)
//	LAKE_SQLITE_ADDR          the listen address (default :8080)
//	LAKE_SQLITE_MEMORY_LIMIT  the container memory limit in bytes (optional); the Go heap is held
//	                          to 3/4 of it and SQLite's page cache to 1/4
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"github.com/oramasearch/orama-operator/cmd/lake-sqlite/internal/server"
)

const (
	defaultPath = "/data/main.db"
	defaultAddr = ":8080"
	// shutdownGrace is how long in-flight requests get after SIGTERM: the request timeout, so a
	// request that started just before the signal still finishes. The pod's
	// terminationGracePeriodSeconds is longer.
	shutdownGrace = server.DefaultRequestTimeout
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("lake-sqlite stopped", "err", err.Error())
		os.Exit(1)
	}
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func run(log *slog.Logger) error {
	token := os.Getenv("LAKE_SQLITE_TOKEN")
	if token == "" {
		return errors.New("LAKE_SQLITE_TOKEN is not set")
	}
	path := env("LAKE_SQLITE_PATH", defaultPath)
	addr := env("LAKE_SQLITE_ADDR", defaultAddr)
	memoryLimit, err := parseMemoryLimit(os.Getenv("LAKE_SQLITE_MEMORY_LIMIT"))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	openCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	srv, err := server.Open(openCtx, server.Config{Path: path, Token: token, Logger: log})
	cancel()
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() {
		if err := srv.Close(); err != nil {
			log.Error("closing the database", "err", err.Error())
		}
	}()
	if memoryLimit > 0 {
		// Right-size both halves: the Go runtime's heap and SQLite's own allocator, which is not
		// Go heap, share the container limit.
		debug.SetMemoryLimit(memoryLimit / 4 * 3)
		if _, err := srv.DB().ExecContext(ctx, fmt.Sprintf("PRAGMA soft_heap_limit=%d", memoryLimit/4)); err != nil {
			return fmt.Errorf("set the SQLite soft heap limit: %w", err)
		}
	}

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       server.DefaultRequestTimeout,
		// The handler's own deadline is the request timeout; the write deadline leaves room to
		// send the answer after it.
		WriteTimeout: server.DefaultRequestTimeout + 15*time.Second,
		IdleTimeout:  2 * time.Minute,
	}
	errs := make(chan error, 1)
	go func() {
		log.Info("lake-sqlite listening", "addr", addr, "path", path)
		errs <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	log.Info("lake-sqlite shutting down")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancelShutdown()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

func parseMemoryLimit(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	limit, err := strconv.ParseInt(value, 10, 64)
	if err != nil || limit < 0 {
		return 0, fmt.Errorf("LAKE_SQLITE_MEMORY_LIMIT must be a byte count, got %q", value)
	}
	return limit, nil
}

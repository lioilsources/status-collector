package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ol1n/status-collector/internal/api"
	"github.com/ol1n/status-collector/internal/checker"
	"github.com/ol1n/status-collector/internal/storage"
)

func main() {
	dbPath := flag.String("db", "/var/lib/ol1n-status/status.db", "SQLite database path")
	addr := flag.String("addr", ":8765", "HTTP API listen address")
	interval := flag.Duration("interval", 1*time.Hour, "Check interval")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	db, err := storage.New(*dbPath)
	if err != nil {
		logger.Error("failed to open db", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	endpoints := checker.DefaultEndpoints()

	chk := checker.New(endpoints, db, logger)
	srv := api.New(db, logger)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Run first check immediately on startup
	logger.Info("running initial check")
	chk.RunOnce(ctx)

	// Schedule periodic checks
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	go func() {
		for {
			select {
			case <-ticker.C:
				chk.RunOnce(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()

	httpSrv := &http.Server{
		Addr:         *addr,
		Handler:      srv,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	go func() {
		logger.Info("starting HTTP API", "addr", *addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server error", "err", err)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

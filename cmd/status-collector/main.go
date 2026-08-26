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
	"github.com/ol1n/status-collector/internal/comfy"
	"github.com/ol1n/status-collector/internal/storage"
)

func main() {
	dbPath := flag.String("db", "/var/lib/ol1n-status/status.db", "SQLite database path")
	addr := flag.String("addr", ":8765", "HTTP API listen address")
	interval := flag.Duration("interval", 1*time.Hour, "Availability check interval")
	comfyBase := flag.String("comfy", "", "ComfyUI base URL (e.g. http://192.168.1.50:8188); empty disables ComfyUI monitoring")
	sampleInterval := flag.Duration("sample-interval", 60*time.Second, "ComfyUI gauge sampling interval (queue depth, VRAM)")
	drainInterval := flag.Duration("drain-interval", 5*time.Minute, "ComfyUI job history drain interval")
	snapshotDir := flag.String("snapshot-dir", "", "Directory to write status.json/comfy.json into; empty disables snapshots")
	snapshotInterval := flag.Duration("snapshot-interval", 5*time.Minute, "How often to rewrite the snapshot files")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	db, err := storage.New(*dbPath)
	if err != nil {
		logger.Error("failed to open db", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	endpoints := checker.DefaultEndpoints(*comfyBase)
	chk := checker.New(endpoints, db, logger)

	// ComfyUI metrics are opt-in: without -comfy the collector behaves exactly
	// as before, and /api/comfy reports enabled=false.
	var poller *comfy.Poller
	if *comfyBase != "" {
		poller = comfy.New(*comfyBase, db, logger)
		logger.Info("comfyui monitoring enabled", "base", *comfyBase,
			"sample_interval", sampleInterval.String(), "drain_interval", drainInterval.String())
	} else {
		logger.Info("comfyui monitoring disabled (no -comfy flag)")
	}

	srv := api.New(db, logger, endpoints, poller)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	writeSnapshot := func() {
		if *snapshotDir == "" {
			return
		}
		if err := srv.WriteSnapshot(*snapshotDir); err != nil {
			logger.Warn("snapshot write failed", "dir", *snapshotDir, "err", err)
		}
	}

	sampleComfy := func(ctx context.Context) {
		if poller == nil {
			return
		}
		if err := poller.SampleGauges(ctx); err != nil {
			logger.Warn("comfy gauge sample failed", "err", err)
		}
	}

	drainComfy := func(ctx context.Context) {
		if poller == nil {
			return
		}
		if err := poller.DrainJobs(ctx); err != nil {
			logger.Warn("comfy job drain failed", "err", err)
		}
	}

	// Run everything once on startup so the API has data immediately.
	logger.Info("running initial check")
	chk.RunOnce(ctx)
	sampleComfy(ctx)
	drainComfy(ctx)
	writeSnapshot()

	// Availability probes and ComfyUI gauges run on separate clocks: an hourly
	// tick says nothing useful about a queue that drains in minutes.
	every(ctx, *interval, func() {
		chk.RunOnce(ctx)
		writeSnapshot()
	})
	if poller != nil {
		every(ctx, *sampleInterval, func() { sampleComfy(ctx) })
		every(ctx, *drainInterval, func() { drainComfy(ctx) })
	}
	if *snapshotDir != "" {
		every(ctx, *snapshotInterval, writeSnapshot)
		logger.Info("snapshot enabled", "dir", *snapshotDir, "interval", snapshotInterval.String())
	}

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

// every runs fn on a ticker until ctx is cancelled.
func every(ctx context.Context, d time.Duration, fn func()) {
	go func() {
		ticker := time.NewTicker(d)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				fn()
			case <-ctx.Done():
				return
			}
		}
	}()
}

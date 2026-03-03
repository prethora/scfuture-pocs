package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"scfuture/internal/coordinator"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	listenAddr := os.Getenv("LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = "0.0.0.0:8080"
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		fmt.Fprintf(os.Stderr, "DATABASE_URL environment variable is required\n")
		os.Exit(1)
	}

	b2Bucket := os.Getenv("B2_BUCKET_NAME")

	// ── Step 1: Create coordinator (connects to Postgres, runs schema migration) ──
	coord, err := coordinator.NewCoordinator(databaseURL, b2Bucket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create coordinator: %v\n", err)
		os.Exit(1)
	}

	// ── Step 2: Acquire advisory lock (singleton enforcement) ──
	if err := coord.GetStore().AcquireAdvisoryLock(); err != nil {
		fmt.Fprintf(os.Stderr, "Advisory lock failed: %v\n", err)
		os.Exit(1)
	}

	// ── Step 3: Run reconciliation (BEFORE goroutines and HTTP server) ──
	coord.Reconcile()

	// ── Step 4: Start background goroutines ──
	ctx, cancel := context.WithCancel(context.Background())
	coord.SetCancelFunc(cancel)

	coordinator.StartHealthChecker(coord.GetStore(), coordinator.NewMachineClient(""), coord)
	coordinator.StartReformer(coord.GetStore(), coord)
	coordinator.StartRetentionEnforcer(coord.GetStore(), coord)
	coordinator.StartRebalancer(coord.GetStore(), coord)

	// ── Step 5: Start HTTP server ──
	mux := http.NewServeMux()
	coord.RegisterRoutes(mux)

	server := &http.Server{Addr: listenAddr, Handler: mux}

	// Graceful shutdown on SIGTERM/SIGINT
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigChan
		slog.Info("Shutdown signal received")
		cancel() // stop background goroutines
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		server.Shutdown(shutdownCtx)
		coord.GetStore().Close()
	}()

	slog.Info("Coordinator ready",
		"listen_addr", listenAddr,
		"b2_bucket", b2Bucket,
	)

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "HTTP server failed: %v\n", err)
		os.Exit(1)
	}

	_ = ctx // referenced by background goroutines
}

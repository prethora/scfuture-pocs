package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"scfuture/internal/coordinator"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	listenAddr := os.Getenv("LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = "0.0.0.0:8080"
	}

	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "/data"
	}

	b2Bucket := os.Getenv("B2_BUCKET_NAME")

	coord := coordinator.NewCoordinator(dataDir, b2Bucket)

	// Start health checker
	coordinator.StartHealthChecker(coord.GetStore(), coordinator.NewMachineClient(""), coord)

	// Start reformer
	coordinator.StartReformer(coord.GetStore(), coord)

	// Start retention enforcer
	coordinator.StartRetentionEnforcer(coord.GetStore(), coord)

	mux := http.NewServeMux()
	coord.RegisterRoutes(mux)

	slog.Info("Coordinator ready",
		"listen_addr", listenAddr,
		"data_dir", dataDir,
		"b2_bucket", b2Bucket,
	)

	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "HTTP server failed: %v\n", err)
		os.Exit(1)
	}
}

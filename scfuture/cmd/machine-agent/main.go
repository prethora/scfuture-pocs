package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"scfuture/internal/machineagent"
)

func main() {
	// JSON structured logging
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	nodeID := os.Getenv("NODE_ID")
	if nodeID == "" {
		fmt.Fprintln(os.Stderr, "NODE_ID environment variable is required")
		os.Exit(1)
	}

	listenAddr := os.Getenv("LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = "0.0.0.0:8080"
	}

	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "/data"
	}

	nodeAddress := os.Getenv("NODE_ADDRESS")
	coordinatorURL := os.Getenv("COORDINATOR_URL")

	agent := machineagent.NewAgent(nodeID, dataDir)

	// Discover existing state
	agent.Discover()

	// Ensure container image
	if err := agent.EnsureContainerImage(); err != nil {
		slog.Warn("Container image build failed (may not be deployed yet)", "error", err)
	}

	// Register routes
	mux := http.NewServeMux()
	agent.RegisterRoutes(mux)

	// Start heartbeat if coordinator URL is set
	if coordinatorURL != "" {
		if nodeAddress == "" {
			nodeAddress = listenAddr
		}
		agent.StartHeartbeat(coordinatorURL, nodeAddress)
		slog.Info("Heartbeat started", "coordinator", coordinatorURL, "node_address", nodeAddress)
	}

	slog.Info("Machine agent ready",
		"node_id", nodeID,
		"listen_addr", listenAddr,
	)

	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		slog.Error("HTTP server failed", "error", err)
		os.Exit(1)
	}
}

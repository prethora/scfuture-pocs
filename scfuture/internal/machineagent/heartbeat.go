package machineagent

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"scfuture/internal/shared"
)

// StartHeartbeat begins the registration and heartbeat loop.
// Called from main.go only if COORDINATOR_URL is set.
func (a *Agent) StartHeartbeat(coordinatorURL, nodeAddress string) {
	go func() {
		client := &http.Client{Timeout: 10 * time.Second}

		// Register — retry until successful
		for {
			req := shared.FleetRegisterRequest{
				MachineID:   a.nodeID,
				Address:     nodeAddress,
				DiskTotalMB: getDiskTotalMB(),
				DiskUsedMB:  getDiskUsedMB(),
				RAMTotalMB:  getRAMTotalMB(),
				RAMUsedMB:   getRAMUsedMB(),
				MaxAgents:   200,
			}
			body, _ := json.Marshal(req)
			resp, err := client.Post(coordinatorURL+"/api/fleet/register", "application/json", bytes.NewReader(body))
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					slog.Info("Registered with coordinator", "component", "heartbeat", "coordinator", coordinatorURL)
					break
				}
			}
			slog.Warn("Registration failed, retrying in 5s", "component", "heartbeat", "error", err)
			time.Sleep(5 * time.Second)
		}

		// Heartbeat loop — every 10 seconds
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			users := a.allUsers()
			var running []string
			for id, u := range users {
				if u.ContainerRunning {
					running = append(running, id)
				}
			}

			req := shared.FleetHeartbeatRequest{
				MachineID:     a.nodeID,
				DiskTotalMB:   getDiskTotalMB(),
				DiskUsedMB:    getDiskUsedMB(),
				RAMTotalMB:    getRAMTotalMB(),
				RAMUsedMB:     getRAMUsedMB(),
				ActiveAgents:  len(running),
				RunningAgents: running,
			}
			body, _ := json.Marshal(req)
			resp, err := client.Post(coordinatorURL+"/api/fleet/heartbeat", "application/json", bytes.NewReader(body))
			if err != nil {
				slog.Warn("Heartbeat failed", "component", "heartbeat", "error", err)
				continue
			}
			resp.Body.Close()
		}
	}()
}

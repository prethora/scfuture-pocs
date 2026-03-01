package machineagent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"scfuture/internal/shared"
)

func (a *Agent) ContainerStart(userID string) (*shared.ContainerStartResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}

	containerName := userID + "-agent"

	// Check if already running
	if a.isContainerRunning(containerName) {
		slog.Info("Container already running", "component", "containers", "user", userID)
		return &shared.ContainerStartResponse{
			ContainerName:  containerName,
			AlreadyExisted: true,
		}, nil
	}

	// Remove any existing stopped container
	runCmd("docker", "rm", "-f", containerName)

	u := a.getUser(userID)
	if u == nil {
		return nil, fmt.Errorf("user %q not found in state", userID)
	}
	if u.DRBDDevice == "" {
		return nil, fmt.Errorf("no DRBD device for user %q", userID)
	}

	drbdDev := u.DRBDDevice

	result, err := runCmd("docker", "run", "-d",
		"--name", containerName,
		"--device", drbdDev,
		"--cap-drop", "ALL",
		"--cap-add", "SYS_ADMIN",
		"--cap-add", "SETUID",
		"--cap-add", "SETGID",
		"--security-opt", "apparmor=unconfined",
		"--network", "none",
		"--memory", "64m",
		"-e", "BLOCK_DEVICE="+drbdDev,
		"-e", "SUBVOL_NAME=workspace",
		"platform/app-container",
	)
	if err != nil {
		return nil, cmdError("docker run failed", "docker run "+containerName, result)
	}

	// Wait and verify container is running
	time.Sleep(2 * time.Second)
	if !a.isContainerRunning(containerName) {
		// Get logs for debugging
		logResult, _ := runCmd("docker", "logs", containerName)
		return nil, fmt.Errorf("container %s exited after start. logs: %s%s", containerName, logResult.Stdout, logResult.Stderr)
	}

	// Update state
	if u != nil {
		u.ContainerRunning = true
		u.ContainerName = containerName
		a.setUser(userID, u)
	}

	slog.Info("Container started", "component", "containers", "user", userID, "container", containerName)
	return &shared.ContainerStartResponse{ContainerName: containerName}, nil
}

func (a *Agent) ContainerStop(userID string) error {
	if err := validateUserID(userID); err != nil {
		return err
	}

	containerName := userID + "-agent"

	// Check if exists
	if !a.containerExists(containerName) {
		slog.Info("Container already removed", "component", "containers", "user", userID)
		return nil
	}

	runCmd("docker", "stop", "--time", "10", containerName)
	runCmd("docker", "rm", "-f", containerName)

	// Update state
	u := a.getUser(userID)
	if u != nil {
		u.ContainerRunning = false
		u.ContainerName = ""
		a.setUser(userID, u)
	}

	slog.Info("Container stopped", "component", "containers", "user", userID)
	return nil
}

func (a *Agent) ContainerStatus(userID string) (*shared.ContainerStatusResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}

	containerName := userID + "-agent"

	if !a.containerExists(containerName) {
		return &shared.ContainerStatusResponse{Exists: false, Running: false}, nil
	}

	running := a.isContainerRunning(containerName)
	resp := &shared.ContainerStatusResponse{
		Exists:        true,
		Running:       running,
		ContainerName: containerName,
	}

	// Get started_at if running
	if running {
		result, err := runCmd("docker", "inspect", containerName)
		if err == nil {
			var inspectData []struct {
				State struct {
					StartedAt string `json:"StartedAt"`
				} `json:"State"`
			}
			if json.Unmarshal([]byte(result.Stdout), &inspectData) == nil && len(inspectData) > 0 {
				resp.StartedAt = inspectData[0].State.StartedAt
			}
		}
	}

	return resp, nil
}

func (a *Agent) isContainerRunning(name string) bool {
	result, err := runCmd("docker", "inspect", "--format", "{{.State.Running}}", name)
	if err != nil {
		return false
	}
	return strings.TrimSpace(result.Stdout) == "true"
}

func (a *Agent) containerExists(name string) bool {
	_, err := runCmd("docker", "inspect", name)
	return err == nil
}

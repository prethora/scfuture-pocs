package machineagent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"scfuture/internal/shared"
)

func (a *Agent) FormatBtrfs(userID string) (*shared.FormatBtrfsResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}

	u := a.getUser(userID)
	if u == nil {
		return nil, fmt.Errorf("user %q not found in state", userID)
	}
	if u.DRBDDevice == "" {
		return nil, fmt.Errorf("no DRBD device for user %q", userID)
	}

	mountPath := a.mountPath(userID)
	drbdDev := u.DRBDDevice

	// Check if already formatted: try mounting and look for workspace subvol
	if err := os.MkdirAll(mountPath, 0755); err != nil {
		return nil, fmt.Errorf("mkdir mount path: %w", err)
	}

	// Try to mount — if it succeeds the device has a filesystem
	result, err := runCmd("mount", "-t", "btrfs", drbdDev, mountPath)
	if err == nil {
		// Check for workspace subvolume
		if _, statErr := os.Stat(mountPath + "/workspace"); statErr == nil {
			// Already formatted — unmount and return
			runCmd("umount", mountPath)
			slog.Info("Btrfs already formatted", "component", "btrfs", "user", userID)
			return &shared.FormatBtrfsResponse{AlreadyFormatted: true}, nil
		}
		// Mounted but no workspace subvol — unmount and reformat
		runCmd("umount", mountPath)
	}

	// Format
	result, err = runCmd("mkfs.btrfs", "-f", drbdDev)
	if err != nil {
		return nil, cmdError("mkfs.btrfs failed", cmdString("mkfs.btrfs", "-f", drbdDev), result)
	}

	// Mount
	result, err = runCmd("mount", "-t", "btrfs", drbdDev, mountPath)
	if err != nil {
		return nil, cmdError("mount failed", cmdString("mount", "-t", "btrfs", drbdDev, mountPath), result)
	}

	// Create workspace subvolume
	result, err = runCmd("btrfs", "subvolume", "create", mountPath+"/workspace")
	if err != nil {
		runCmd("umount", mountPath)
		return nil, cmdError("btrfs subvolume create failed", "btrfs subvolume create workspace", result)
	}

	// Create seed directories
	for _, dir := range []string{"memory", "apps", "data"} {
		os.MkdirAll(mountPath+"/workspace/"+dir, 0755)
	}

	// Write config.json
	configData := map[string]string{
		"created": time.Now().UTC().Format(time.RFC3339),
		"user":    userID,
	}
	configJSON, _ := json.Marshal(configData)
	os.WriteFile(mountPath+"/workspace/data/config.json", configJSON, 0644)

	// Create snapshots directory and layer-000 snapshot
	os.MkdirAll(mountPath+"/snapshots", 0755)
	result, err = runCmd("btrfs", "subvolume", "snapshot", "-r",
		mountPath+"/workspace", mountPath+"/snapshots/layer-000")
	if err != nil {
		slog.Warn("Snapshot creation failed (non-fatal)", "component", "btrfs", "user", userID, "error", err)
	}

	// Unmount — host does NOT keep Btrfs mounted
	result, err = runCmd("umount", mountPath)
	if err != nil {
		return nil, cmdError("umount after format failed", cmdString("umount", mountPath), result)
	}

	slog.Info("Btrfs formatted and provisioned", "component", "btrfs", "user", userID)
	return &shared.FormatBtrfsResponse{}, nil
}

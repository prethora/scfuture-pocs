package machineagent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"scfuture/internal/shared"
)

// Backup performs a full btrfs send of a snapshot, compresses with zstd, and uploads to B2.
// Requires B2_KEY_ID, B2_APP_KEY env vars to be set on the machine agent.
// The DRBD resource must be Primary.
func (a *Agent) Backup(userID string, req *shared.BackupRequest) (*shared.BackupResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}
	if req.SnapshotName == "" {
		return nil, fmt.Errorf("snapshot_name is required")
	}
	if req.BucketName == "" {
		return nil, fmt.Errorf("bucket_name is required")
	}

	u := a.getUser(userID)
	if u == nil {
		return nil, fmt.Errorf("user %q not found in state", userID)
	}
	if u.DRBDDevice == "" {
		return nil, fmt.Errorf("no DRBD device for user %q", userID)
	}

	// Check B2 credentials
	b2KeyID := os.Getenv("B2_KEY_ID")
	b2AppKey := os.Getenv("B2_APP_KEY")
	if b2KeyID == "" || b2AppKey == "" {
		return nil, fmt.Errorf("B2_KEY_ID and B2_APP_KEY environment variables are required")
	}

	// Authorize b2 account before any b2 operations
	result, err := runCmd("b2", "account", "authorize", b2KeyID, b2AppKey)
	if err != nil {
		return nil, cmdError("b2 authorize failed", "b2 account authorize", result)
	}

	mountPath := a.mountPath(userID)
	drbdDev := u.DRBDDevice
	snapPath := mountPath + "/snapshots/" + req.SnapshotName

	// Mount
	os.MkdirAll(mountPath, 0755)
	result, err = runCmd("mount", "-t", "btrfs", drbdDev, mountPath)
	if err != nil {
		return nil, cmdError("mount failed for backup", cmdString("mount", "-t", "btrfs", drbdDev, mountPath), result)
	}
	defer func() {
		runCmd("umount", mountPath)
	}()

	// Verify snapshot exists
	if _, err := os.Stat(snapPath); err != nil {
		return nil, fmt.Errorf("snapshot %q does not exist at %s", req.SnapshotName, snapPath)
	}

	// Create temp file for btrfs send output
	tmpDir := "/tmp"
	tmpFile := filepath.Join(tmpDir, fmt.Sprintf("%s-%s.btrfs.zst", userID, req.SnapshotName))
	defer os.Remove(tmpFile)

	// btrfs send | zstd > temp file
	sendCmd := fmt.Sprintf("btrfs send %s | zstd -o %s", snapPath, tmpFile)
	result, err = runCmd("bash", "-c", sendCmd)
	if err != nil {
		return nil, cmdError("btrfs send | zstd failed", sendCmd, result)
	}

	// Get file size
	info, err := os.Stat(tmpFile)
	if err != nil {
		return nil, fmt.Errorf("stat temp file: %w", err)
	}

	// Determine B2 key
	b2KeyPrefix := req.B2KeyPrefix
	if b2KeyPrefix == "" {
		b2KeyPrefix = "users/" + userID
	}
	b2Key := b2KeyPrefix + "/" + req.SnapshotName + ".btrfs.zst"

	// Upload to B2
	result, err = runCmd("b2", "file", "upload", req.BucketName, tmpFile, b2Key)
	if err != nil {
		return nil, cmdError("b2 upload failed", "b2 file upload "+req.BucketName+" "+b2Key, result)
	}

	// Upload manifest.json
	manifest := map[string]interface{}{
		"snapshot":  req.SnapshotName,
		"b2_key":    b2Key,
		"size":      info.Size(),
		"timestamp": strings.TrimSpace(result.Stdout),
	}
	manifestJSON, _ := json.Marshal(manifest)
	manifestPath := filepath.Join(tmpDir, fmt.Sprintf("%s-manifest.json", userID))
	os.WriteFile(manifestPath, manifestJSON, 0644)
	defer os.Remove(manifestPath)

	manifestB2Key := b2KeyPrefix + "/manifest.json"
	runCmd("b2", "file", "upload", req.BucketName, manifestPath, manifestB2Key)

	slog.Info("Backup complete", "component", "backup", "user", userID,
		"snapshot", req.SnapshotName, "b2_key", b2Key, "size", info.Size())

	return &shared.BackupResponse{
		B2Path:    b2Key,
		SizeBytes: info.Size(),
	}, nil
}

// Restore downloads a snapshot from B2, decompresses, and applies via btrfs receive.
// Then creates a writable workspace subvolume from the received snapshot.
// The DRBD resource must be Primary and the filesystem must already be formatted (bare mode OK).
func (a *Agent) Restore(userID string, req *shared.RestoreRequest) (*shared.RestoreResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}
	if req.BucketName == "" || req.B2Path == "" {
		return nil, fmt.Errorf("bucket_name and b2_path are required")
	}

	u := a.getUser(userID)
	if u == nil {
		return nil, fmt.Errorf("user %q not found in state", userID)
	}
	if u.DRBDDevice == "" {
		return nil, fmt.Errorf("no DRBD device for user %q", userID)
	}

	// Check B2 credentials
	b2KeyID := os.Getenv("B2_KEY_ID")
	b2AppKey := os.Getenv("B2_APP_KEY")
	if b2KeyID == "" || b2AppKey == "" {
		return nil, fmt.Errorf("B2_KEY_ID and B2_APP_KEY environment variables are required")
	}

	// Authorize b2 account before any b2 operations
	result, err := runCmd("b2", "account", "authorize", b2KeyID, b2AppKey)
	if err != nil {
		return nil, cmdError("b2 authorize failed", "b2 account authorize", result)
	}

	mountPath := a.mountPath(userID)
	drbdDev := u.DRBDDevice

	// Mount
	os.MkdirAll(mountPath, 0755)
	result, err = runCmd("mount", "-t", "btrfs", drbdDev, mountPath)
	if err != nil {
		return nil, cmdError("mount failed for restore", cmdString("mount", "-t", "btrfs", drbdDev, mountPath), result)
	}
	defer func() {
		runCmd("umount", mountPath)
	}()

	// Create snapshots directory if needed
	os.MkdirAll(mountPath+"/snapshots", 0755)

	// Download from B2
	tmpDir := "/tmp"
	tmpZst := filepath.Join(tmpDir, fmt.Sprintf("%s-restore.btrfs.zst", userID))
	tmpRaw := filepath.Join(tmpDir, fmt.Sprintf("%s-restore.btrfs", userID))
	defer os.Remove(tmpZst)
	defer os.Remove(tmpRaw)

	result, err = runCmd("b2", "file", "download", fmt.Sprintf("b2://%s/%s", req.BucketName, req.B2Path), tmpZst)
	if err != nil {
		return nil, cmdError("b2 download failed", "b2 file download "+req.B2Path, result)
	}

	// Decompress
	result, err = runCmd("zstd", "-d", tmpZst, "-o", tmpRaw)
	if err != nil {
		return nil, cmdError("zstd decompress failed", "zstd -d "+tmpZst, result)
	}

	// btrfs receive
	receiveCmd := fmt.Sprintf("btrfs receive %s/snapshots/ < %s", mountPath, tmpRaw)
	result, err = runCmd("bash", "-c", receiveCmd)
	if err != nil {
		return nil, cmdError("btrfs receive failed", receiveCmd, result)
	}

	// Determine snapshot name from the received subvolume
	// btrfs receive creates the subvolume with its original name
	snapName := req.SnapshotName
	if snapName == "" {
		// Try to discover from directory listing
		entries, _ := os.ReadDir(mountPath + "/snapshots")
		for _, e := range entries {
			if e.Name() != "layer-000" {
				snapName = e.Name()
				break
			}
		}
	}

	// Delete existing workspace subvolume if present (e.g., from a failed previous restore)
	if _, err := os.Stat(mountPath + "/workspace"); err == nil {
		runCmd("btrfs", "subvolume", "delete", mountPath+"/workspace")
	}

	// Create writable workspace from the received snapshot
	snapFullPath := mountPath + "/snapshots/" + snapName
	result, err = runCmd("btrfs", "subvolume", "snapshot",
		snapFullPath, mountPath+"/workspace")
	if err != nil {
		return nil, cmdError("create workspace from snapshot failed",
			"btrfs subvolume snapshot "+snapName+" workspace", result)
	}

	slog.Info("Restore complete", "component", "backup", "user", userID,
		"snapshot", snapName, "source", req.B2Path)

	return &shared.RestoreResponse{SnapshotName: snapName}, nil
}

// BackupStatus checks if a B2 backup exists for this user by checking for manifest.json.
func (a *Agent) BackupStatus(userID string) (*shared.BackupStatusResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}

	bucketName := os.Getenv("B2_BUCKET_NAME")
	if bucketName == "" {
		return &shared.BackupStatusResponse{Exists: false}, nil
	}

	b2KeyID := os.Getenv("B2_KEY_ID")
	b2AppKey := os.Getenv("B2_APP_KEY")
	if b2KeyID == "" || b2AppKey == "" {
		return &shared.BackupStatusResponse{Exists: false}, nil
	}

	// Authorize b2 account
	runCmd("b2", "account", "authorize", b2KeyID, b2AppKey)

	// Check for manifest.json
	manifestKey := "users/" + userID + "/manifest.json"
	result, err := runCmd("b2", "ls", "--recursive", fmt.Sprintf("b2://%s", bucketName), "--prefix", manifestKey)
	if err != nil {
		return &shared.BackupStatusResponse{Exists: false}, nil
	}

	exists := strings.TrimSpace(result.Stdout) != ""

	resp := &shared.BackupStatusResponse{Exists: exists}
	if exists {
		resp.B2Path = manifestKey
	}

	return resp, nil
}

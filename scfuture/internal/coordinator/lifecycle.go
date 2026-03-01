package coordinator

import (
	"fmt"
	"log/slog"
	"time"

	"scfuture/internal/shared"
)

// suspendUser drives the full suspension flow for a user.
func (coord *Coordinator) suspendUser(userID string) {
	start := time.Now()
	logger := slog.With("component", "lifecycle", "user_id", userID, "action", "suspend")

	user := coord.store.GetUser(userID)
	if user == nil {
		logger.Error("User not found")
		return
	}

	if user.Status != "running" && user.Status != "running_degraded" {
		logger.Warn("Cannot suspend — invalid status", "status", user.Status)
		return
	}

	opID := generateOpID()
	coord.store.CreateOperation(opID, "suspension", userID, map[string]interface{}{
		"primary_machine": user.PrimaryMachine,
		"previous_status": user.Status,
	})

	coord.store.SetUserStatus(userID, "suspending", "")

	// Find the primary machine
	primaryMachine := coord.store.GetMachine(user.PrimaryMachine)
	if primaryMachine == nil {
		logger.Error("Primary machine not found")
		coord.store.SetUserStatus(userID, user.Status, "primary machine not found for suspension")
		coord.store.FailOperation(opID, "primary machine not found")
		return
	}
	primaryClient := NewMachineClient(primaryMachine.Address)

	// ── Step 1: Stop container ──
	if err := primaryClient.ContainerStop(userID); err != nil {
		logger.Error("Container stop failed", "error", err)
		coord.store.SetUserStatus(userID, user.Status, "container stop failed: "+err.Error())
		coord.store.FailOperation(opID, "container stop: "+err.Error())
		coord.store.RecordLifecycleEvent(LifecycleEvent{
			UserID: userID, Type: "suspension", Success: false,
			Error: "container stop: " + err.Error(),
			DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
		})
		return
	}
	logger.Info("Container stopped")
	coord.step(opID, "suspend-container-stopped")

	// ── Step 2: Take final snapshot ──
	snapName := fmt.Sprintf("suspend-%s", time.Now().UTC().Format("20060102T150405Z"))
	_, err := primaryClient.Snapshot(userID, &shared.SnapshotRequest{SnapshotName: snapName})
	if err != nil {
		logger.Error("Snapshot failed", "error", err)
		logger.Warn("Continuing suspension without snapshot")
	} else {
		logger.Info("Snapshot created", "name", snapName)
	}
	coord.step(opID, "suspend-snapshot-created")

	// ── Step 3: Backup to B2 ──
	backupSuccess := false
	if coord.B2BucketName != "" {
		backupReq := &shared.BackupRequest{
			SnapshotName: snapName,
			BucketName:   coord.B2BucketName,
			B2KeyPrefix:  "users/" + userID,
		}
		backupResp, err := primaryClient.Backup(userID, backupReq)
		if err != nil {
			logger.Warn("B2 backup failed (non-fatal for suspension)", "error", err)
		} else {
			coord.store.SetUserBackup(userID, backupResp.B2Path, coord.B2BucketName)
			backupSuccess = true
			logger.Info("B2 backup complete", "b2_path", backupResp.B2Path, "size", backupResp.SizeBytes)
		}
	} else {
		logger.Warn("No B2 bucket configured — skipping backup")
	}
	coord.step(opID, "suspend-backed-up")

	// ── Step 4: Demote primary to Secondary ──
	_, err = primaryClient.DRBDDemote(userID)
	if err != nil {
		logger.Warn("DRBD demote failed (non-fatal)", "error", err)
	} else {
		logger.Info("DRBD demoted to Secondary")
	}
	coord.step(opID, "suspend-demoted")

	// ── Step 5: Set status → "suspended" ──
	coord.store.SetUserStatus(userID, "suspended", "")
	coord.store.SetUserDRBDDisconnected(userID, false)
	coord.store.CompleteOperation(opID)

	coord.store.RecordLifecycleEvent(LifecycleEvent{
		UserID: userID, Type: "suspension", Success: true,
		DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
	})

	logger.Info("Suspension complete",
		"backup_success", backupSuccess,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

// reactivateUser determines whether to use warm or cold path and proceeds.
func (coord *Coordinator) reactivateUser(userID string) {
	user := coord.store.GetUser(userID)
	if user == nil {
		slog.Error("[LIFECYCLE] User not found", "user_id", userID)
		return
	}

	switch user.Status {
	case "suspended":
		coord.warmReactivate(userID)
	case "evicted":
		coord.coldReactivate(userID)
	default:
		slog.Warn("[LIFECYCLE] Cannot reactivate — invalid status", "user_id", userID, "status", user.Status)
	}
}

// warmReactivate brings back a suspended user whose images are still on fleet.
func (coord *Coordinator) warmReactivate(userID string) {
	start := time.Now()
	logger := slog.With("component", "lifecycle", "user_id", userID, "action", "reactivate_warm")

	user := coord.store.GetUser(userID)
	if user == nil || user.Status != "suspended" {
		return
	}

	opID := generateOpID()
	coord.store.CreateOperation(opID, "reactivation_warm", userID, map[string]interface{}{
		"primary_machine": user.PrimaryMachine,
		"drbd_disconnected": user.DRBDDisconnected,
	})

	coord.store.SetUserStatus(userID, "reactivating", "")

	// Find bipods on active machines
	bipods := coord.store.GetBipods(userID)
	var activeBipods []*Bipod
	for _, b := range bipods {
		if b.Role != "stale" {
			m := coord.store.GetMachine(b.MachineID)
			if m != nil && m.Status == "active" {
				activeBipods = append(activeBipods, b)
			}
		}
	}

	if len(activeBipods) == 0 {
		logger.Error("No active bipods found — cannot warm reactivate")
		coord.store.SetUserStatus(userID, "suspended", "no active bipods for warm reactivation")
		coord.store.FailOperation(opID, "no active bipods")
		coord.store.RecordLifecycleEvent(LifecycleEvent{
			UserID: userID, Type: "reactivation_warm", Success: false,
			Error: "no active bipods",
			DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
		})
		return
	}

	// Pick primary
	var primaryBipod *Bipod
	for _, b := range activeBipods {
		if b.MachineID == user.PrimaryMachine {
			primaryBipod = b
			break
		}
	}
	if primaryBipod == nil {
		primaryBipod = activeBipods[0]
		coord.store.SetUserPrimary(userID, primaryBipod.MachineID)
	}

	primaryMachine := coord.store.GetMachine(primaryBipod.MachineID)
	primaryClient := NewMachineClient(primaryMachine.Address)

	// ── Step 1: Reconnect DRBD if disconnected ──
	if user.DRBDDisconnected {
		logger.Info("DRBD was disconnected — reconnecting")
		for _, b := range activeBipods {
			m := coord.store.GetMachine(b.MachineID)
			if m != nil {
				client := NewMachineClient(m.Address)
				if _, err := client.DRBDConnect(userID); err != nil {
					logger.Warn("DRBD connect failed on machine (non-fatal)", "machine", b.MachineID, "error", err)
				}
			}
		}
		time.Sleep(3 * time.Second)
	}
	coord.step(opID, "reactivate-warm-connected")

	// ── Step 2: Promote DRBD ──
	if _, err := primaryClient.DRBDPromote(userID); err != nil {
		logger.Error("DRBD promote failed", "error", err)
		coord.store.SetUserStatus(userID, "suspended", "promote failed: "+err.Error())
		coord.store.FailOperation(opID, "promote: "+err.Error())
		coord.store.RecordLifecycleEvent(LifecycleEvent{
			UserID: userID, Type: "reactivation_warm", Success: false,
			Error: "promote: " + err.Error(),
			DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
		})
		return
	}
	logger.Info("DRBD promoted")
	coord.step(opID, "reactivate-warm-promoted")

	// ── Step 3: Start container ──
	if _, err := primaryClient.ContainerStart(userID); err != nil {
		logger.Error("Container start failed", "error", err)
		coord.store.SetUserStatus(userID, "suspended", "container start failed: "+err.Error())
		coord.store.FailOperation(opID, "container start: "+err.Error())
		coord.store.RecordLifecycleEvent(LifecycleEvent{
			UserID: userID, Type: "reactivation_warm", Success: false,
			Error: "container start: " + err.Error(),
			DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
		})
		return
	}
	logger.Info("Container started")
	coord.step(opID, "reactivate-warm-container-started")

	// ── Step 4: Update state ──
	coord.store.SetBipodRole(userID, primaryBipod.MachineID, "primary")
	coord.store.SetUserStatus(userID, "running", "")
	coord.store.SetUserDRBDDisconnected(userID, false)
	coord.store.CompleteOperation(opID)

	coord.store.RecordLifecycleEvent(LifecycleEvent{
		UserID: userID, Type: "reactivation_warm", Success: true,
		DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
	})

	logger.Info("Warm reactivation complete", "duration_ms", time.Since(start).Milliseconds())
}

// coldReactivate provisions a user from B2 backup after eviction.
func (coord *Coordinator) coldReactivate(userID string) {
	start := time.Now()
	logger := slog.With("component", "lifecycle", "user_id", userID, "action", "reactivate_cold")

	user := coord.store.GetUser(userID)
	if user == nil || user.Status != "evicted" {
		return
	}

	if !user.BackupExists || user.BackupPath == "" || user.BackupBucket == "" {
		logger.Error("No B2 backup found — cannot cold reactivate")
		coord.store.RecordLifecycleEvent(LifecycleEvent{
			UserID: userID, Type: "reactivation_cold", Success: false,
			Error: "no B2 backup",
			DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
		})
		return
	}

	coord.store.SetUserStatus(userID, "reactivating", "")

	// Helper to fail
	fail := func(opID, step string, err error) {
		msg := fmt.Sprintf("%s: %v", step, err)
		logger.Error("Cold reactivation failed", "step", step, "error", err)
		coord.store.SetUserStatus(userID, "evicted", msg)
		coord.store.FailOperation(opID, msg)
		coord.store.RecordLifecycleEvent(LifecycleEvent{
			UserID: userID, Type: "reactivation_cold", Success: false,
			Error: msg,
			DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
		})
	}

	// ── Step 1: Select machines ──
	primary, secondary, err := coord.store.SelectMachines()
	if err != nil {
		opID := generateOpID()
		coord.store.CreateOperation(opID, "reactivation_cold", userID, map[string]interface{}{})
		fail(opID, "select_machines", err)
		return
	}

	port := coord.store.AllocatePort()
	primaryMinor := coord.store.AllocateMinor(primary.MachineID)
	secondaryMinor := coord.store.AllocateMinor(secondary.MachineID)

	coord.store.SetUserPrimary(userID, primary.MachineID)
	coord.store.SetUserPort(userID, port)
	coord.store.CreateBipod(userID, primary.MachineID, "primary", primaryMinor)
	coord.store.CreateBipod(userID, secondary.MachineID, "secondary", secondaryMinor)

	opID := generateOpID()
	coord.store.CreateOperation(opID, "reactivation_cold", userID, map[string]interface{}{
		"primary_machine":   primary.MachineID,
		"secondary_machine": secondary.MachineID,
		"primary_address":   primary.Address,
		"secondary_address": secondary.Address,
		"port":              port,
		"primary_minor":     primaryMinor,
		"secondary_minor":   secondaryMinor,
		"backup_path":       user.BackupPath,
		"backup_bucket":     user.BackupBucket,
	})

	logger.Info("Machines selected", "primary", primary.MachineID, "secondary", secondary.MachineID)
	coord.step(opID, "reactivate-cold-machines-selected")

	primaryClient := NewMachineClient(primary.Address)
	secondaryClient := NewMachineClient(secondary.Address)

	// ── Step 2: Create images ──
	primaryResp, err := primaryClient.CreateImage(userID, user.ImageSizeMB)
	if err != nil {
		fail(opID, "image_primary", err)
		return
	}
	secondaryResp, err := secondaryClient.CreateImage(userID, user.ImageSizeMB)
	if err != nil {
		fail(opID, "image_secondary", err)
		return
	}

	coord.store.SetBipodLoopDevice(userID, primary.MachineID, primaryResp.LoopDevice)
	coord.store.SetBipodLoopDevice(userID, secondary.MachineID, secondaryResp.LoopDevice)
	coord.step(opID, "reactivate-cold-images-created")

	// ── Step 3: Configure DRBD ──
	primaryAddr := stripPort(primary.Address)
	secondaryAddr := stripPort(secondary.Address)

	drbdReq := &shared.DRBDCreateRequest{
		ResourceName: "user-" + userID,
		Nodes: []shared.DRBDNode{
			{Hostname: primary.MachineID, Minor: primaryMinor, Disk: primaryResp.LoopDevice, Address: primaryAddr},
			{Hostname: secondary.MachineID, Minor: secondaryMinor, Disk: secondaryResp.LoopDevice, Address: secondaryAddr},
		},
		Port: port,
	}

	if _, err := primaryClient.DRBDCreate(userID, drbdReq); err != nil {
		fail(opID, "drbd_primary", err)
		return
	}
	if _, err := secondaryClient.DRBDCreate(userID, drbdReq); err != nil {
		fail(opID, "drbd_secondary", err)
		return
	}
	coord.step(opID, "reactivate-cold-drbd-configured")

	// ── Step 4: Promote primary ──
	if _, err := primaryClient.DRBDPromote(userID); err != nil {
		fail(opID, "drbd_promote", err)
		return
	}
	coord.step(opID, "reactivate-cold-promoted")

	// ── Step 5: Wait for DRBD sync ──
	syncTimeout := 60 * time.Second
	syncStart := time.Now()
	for {
		if time.Since(syncStart) > syncTimeout {
			fail(opID, "drbd_sync", fmt.Errorf("sync timeout"))
			return
		}
		status, err := primaryClient.DRBDStatus(userID)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		if status.PeerDiskState == "UpToDate" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	logger.Info("DRBD sync complete")
	coord.step(opID, "reactivate-cold-synced")

	// ── Step 6: Format Btrfs (bare mode) ──
	if _, err := primaryClient.FormatBtrfsBare(userID); err != nil {
		fail(opID, "format_btrfs_bare", err)
		return
	}
	logger.Info("Btrfs formatted (bare)")
	coord.step(opID, "reactivate-cold-formatted")

	// ── Step 7: Restore from B2 ──
	restoreReq := &shared.RestoreRequest{
		BucketName:   user.BackupBucket,
		B2Path:       user.BackupPath,
		SnapshotName: "",
	}
	restoreResp, err := primaryClient.Restore(userID, restoreReq)
	if err != nil {
		fail(opID, "restore", err)
		return
	}
	logger.Info("Restore complete", "snapshot", restoreResp.SnapshotName)
	coord.step(opID, "reactivate-cold-restored")

	// ── Step 8: Start container ──
	if _, err := primaryClient.ContainerStart(userID); err != nil {
		fail(opID, "container_start", err)
		return
	}
	logger.Info("Container started")
	coord.step(opID, "reactivate-cold-container-started")

	// ── Step 9: Update state ──
	coord.store.SetUserStatus(userID, "running", "")
	coord.store.SetUserDRBDDisconnected(userID, false)
	coord.store.CompleteOperation(opID)

	coord.store.RecordLifecycleEvent(LifecycleEvent{
		UserID: userID, Type: "reactivation_cold", Success: true,
		DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
	})

	logger.Info("Cold reactivation complete", "duration_ms", time.Since(start).Milliseconds())
}

// evictUser removes all fleet copies of a user's data after verifying B2 backup exists.
func (coord *Coordinator) evictUser(userID string) {
	start := time.Now()
	logger := slog.With("component", "lifecycle", "user_id", userID, "action", "evict")

	user := coord.store.GetUser(userID)
	if user == nil {
		logger.Error("User not found")
		return
	}

	if user.Status != "suspended" {
		logger.Warn("Cannot evict — user is not suspended", "status", user.Status)
		return
	}

	opID := generateOpID()
	coord.store.CreateOperation(opID, "eviction", userID, map[string]interface{}{
		"backup_exists": user.BackupExists,
	})

	// ── Safety check: B2 backup must exist ──
	if !user.BackupExists {
		logger.Warn("No B2 backup — attempting backup before eviction")

		bipods := coord.store.GetBipods(userID)
		backupDone := false
		for _, b := range bipods {
			if b.Role == "stale" {
				continue
			}
			m := coord.store.GetMachine(b.MachineID)
			if m == nil || m.Status != "active" {
				continue
			}

			client := NewMachineClient(m.Address)

			client.DRBDPromote(userID)

			snapName := fmt.Sprintf("evict-%s", time.Now().UTC().Format("20060102T150405Z"))
			if _, err := client.Snapshot(userID, &shared.SnapshotRequest{SnapshotName: snapName}); err != nil {
				logger.Warn("Pre-eviction snapshot failed", "error", err)
				client.DRBDDemote(userID)
				continue
			}

			if coord.B2BucketName != "" {
				backupReq := &shared.BackupRequest{
					SnapshotName: snapName,
					BucketName:   coord.B2BucketName,
					B2KeyPrefix:  "users/" + userID,
				}
				backupResp, err := client.Backup(userID, backupReq)
				if err != nil {
					logger.Warn("Pre-eviction backup failed", "error", err)
					client.DRBDDemote(userID)
					continue
				}
				coord.store.SetUserBackup(userID, backupResp.B2Path, coord.B2BucketName)
				backupDone = true
			}
			client.DRBDDemote(userID)
			if backupDone {
				break
			}
		}

		if !backupDone {
			logger.Error("Cannot evict — no B2 backup and backup attempt failed")
			coord.store.FailOperation(opID, "no backup exists and backup attempt failed")
			coord.store.RecordLifecycleEvent(LifecycleEvent{
				UserID: userID, Type: "eviction", Success: false,
				Error: "no backup exists and backup attempt failed",
				DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
			})
			return
		}
	}

	coord.step(opID, "evict-backup-verified")

	// ── Set status → "evicting" ──
	coord.store.SetUserStatus(userID, "evicting", "")

	// ── Destroy DRBD and delete images on all machines ──
	bipods := coord.store.GetBipods(userID)
	for _, b := range bipods {
		m := coord.store.GetMachine(b.MachineID)
		if m == nil || m.Status != "active" {
			logger.Warn("Skipping bipod cleanup on unavailable machine", "machine", b.MachineID)
			continue
		}
		client := NewMachineClient(m.Address)

		client.DRBDDisconnect(userID)

		if err := client.DRBDDestroy(userID); err != nil {
			logger.Warn("DRBD destroy failed (non-fatal)", "machine", b.MachineID, "error", err)
		}

		if err := client.DeleteUser(userID); err != nil {
			logger.Warn("Image delete failed (non-fatal)", "machine", b.MachineID, "error", err)
		}

		logger.Info("Cleaned up on machine", "machine", b.MachineID)
	}

	coord.step(opID, "evict-resources-cleaned")

	// ── Update state ──
	coord.store.ClearUserBipods(userID)
	coord.store.SetUserStatus(userID, "evicted", "")
	coord.store.SetUserDRBDDisconnected(userID, false)
	coord.store.CompleteOperation(opID)

	coord.store.RecordLifecycleEvent(LifecycleEvent{
		UserID: userID, Type: "eviction", Success: true,
		DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
	})

	logger.Info("Eviction complete", "duration_ms", time.Since(start).Milliseconds())
}

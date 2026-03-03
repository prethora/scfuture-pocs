package coordinator

import (
	"fmt"
	"log/slog"
	"time"

	"scfuture/internal/shared"
)

const (
	MigrationSyncTimeout = 300 * time.Second // 5 minutes for initial sync
)

// MigrateUser drives the live migration of a user from one machine to another.
// Runs in its own goroutine, launched by handleMigrateUser.
func (coord *Coordinator) MigrateUser(userID, sourceMachineID, targetMachineID, trigger string) {
	start := time.Now()
	logger := slog.With("user_id", userID, "component", "migrator",
		"source", sourceMachineID, "target", targetMachineID)

	// Recover from panics so the goroutine doesn't crash silently
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Migration goroutine panicked", "panic", r)
			coord.store.SetUserStatus(userID, "running", fmt.Sprintf("panic: %v", r))
		}
	}()

	var reconfigMethod string
	var containerStopped bool       // tracks if container was stopped (for fail handler recovery)
	var sourceClient *MachineClient // declared early so fail closure can access it
	var targetClient *MachineClient // declared early so fail closure can clean up target
	var stayerClient *MachineClient // declared early so fail closure can restore stayer
	var targetBipodCreated bool     // true after CreateBipod — guards RemoveBipod in fail handler
	var tripodConfigured bool       // true after source+stayer reconfigured to 3-node
	var originalBipodNodes []shared.DRBDNode
	var user *User
	var sourceBipod *Bipod

	fail := func(step string, err error) {
		msg := fmt.Sprintf("%s: %v", step, err)
		logger.Error("Migration failed", "step", step, "error", err)

		// If container was stopped during primary migration, try to restart it on source
		if containerStopped && sourceClient != nil {
			logger.Info("Migration failed after container stop — restarting container on source")
			if _, startErr := sourceClient.ContainerStart(userID); startErr != nil {
				logger.Error("Failed to restart container on source after migration failure", "error", startErr)
			}
		}

		// Clean up target bipod if it was created during this migration.
		if targetBipodCreated {
			if targetClient != nil {
				logger.Info("Cleaning up target machine after migration failure", "target", targetMachineID)
				if _, disconnErr := targetClient.DRBDDisconnect(userID); disconnErr != nil {
					logger.Warn("Target DRBD disconnect during cleanup failed (continuing)", "error", disconnErr)
				}
				if destroyErr := targetClient.DRBDDestroy(userID); destroyErr != nil {
					logger.Warn("Target DRBD destroy during cleanup failed (continuing)", "error", destroyErr)
				}
				if delErr := targetClient.DeleteUser(userID); delErr != nil {
					logger.Warn("Target user delete during cleanup failed (continuing)", "error", delErr)
				}
			}
			coord.store.RemoveBipod(userID, targetMachineID)
		}

		// If source/stayer were reconfigured to tripod (3-node), restore them to original 2-node.
		// This prevents stale 3-node configs that can cause minor collisions on future migrations.
		if tripodConfigured && len(originalBipodNodes) == 2 {
			logger.Info("Restoring source and stayer to original 2-node DRBD config")
			var srcRole, stRole string
			if sourceBipod.Role == "primary" {
				srcRole = "primary"
				stRole = "secondary"
			} else {
				srcRole = "secondary"
				stRole = "primary"
			}
			// Try adjust first (non-destructive), fall back to force only if adjust fails.
			// adjust can remove the third peer without taking the resource down.
			if sourceClient != nil {
				srcReq := &shared.DRBDReconfigureRequest{
					Nodes: originalBipodNodes, Port: user.DRBDPort, Force: false, Role: srcRole,
				}
				if _, rErr := sourceClient.DRBDReconfigure(userID, srcReq); rErr != nil {
					logger.Warn("Adjust failed restoring source, trying force", "error", rErr)
					srcReq.Force = true
					if _, rErr2 := sourceClient.DRBDReconfigure(userID, srcReq); rErr2 != nil {
						logger.Warn("Failed to restore source to 2-node config (continuing)", "error", rErr2)
					}
				}
			}
			if stayerClient != nil {
				stReq := &shared.DRBDReconfigureRequest{
					Nodes: originalBipodNodes, Port: user.DRBDPort, Force: false, Role: stRole,
				}
				if _, rErr := stayerClient.DRBDReconfigure(userID, stReq); rErr != nil {
					logger.Warn("Adjust failed restoring stayer, trying force", "error", rErr)
					stReq.Force = true
					if _, rErr2 := stayerClient.DRBDReconfigure(userID, stReq); rErr2 != nil {
						logger.Warn("Failed to restore stayer to 2-node config (continuing)", "error", rErr2)
					}
				}
			}
		}

		coord.store.SetUserStatus(userID, "running", msg)
		_ = coord.store.RecordMigrationEvent(MigrationEvent{
			UserID:        userID,
			SourceMachine: sourceMachineID,
			TargetMachine: targetMachineID,
			MigrationType: "",
			Success:       false,
			Error:         msg,
			Method:        reconfigMethod,
			Trigger:       trigger,
			DurationMS:    time.Since(start).Milliseconds(),
			Timestamp:     time.Now(),
		})
	}

	retry := func(step string, fn func() error) error {
		if err := fn(); err != nil {
			logger.Warn("Step failed, retrying in 2s", "step", step, "error", err)
			time.Sleep(2 * time.Second)
			return fn()
		}
		return nil
	}

	// ── Step 1: Determine migration type and gather metadata ──
	user = coord.store.GetUser(userID)
	if user == nil {
		fail("lookup", fmt.Errorf("user not found"))
		return
	}

	bipods := coord.store.GetBipods(userID)
	var stayerBipod *Bipod
	var stayerMachineID string
	var migrationType string

	for _, b := range bipods {
		if b.Role == "stale" {
			continue
		}
		if b.MachineID == sourceMachineID {
			bCopy := *b
			sourceBipod = &bCopy
		} else {
			bCopy := *b
			stayerBipod = &bCopy
			stayerMachineID = b.MachineID
		}
	}

	if sourceBipod == nil {
		fail("lookup", fmt.Errorf("source machine %s not found in user's bipod", sourceMachineID))
		return
	}
	if stayerBipod == nil {
		fail("lookup", fmt.Errorf("stayer machine not found in user's bipod"))
		return
	}

	if sourceBipod.Role == "primary" {
		migrationType = "primary"
	} else {
		migrationType = "secondary"
	}

	sourceMachine := coord.store.GetMachine(sourceMachineID)
	targetMachine := coord.store.GetMachine(targetMachineID)
	stayerMachine := coord.store.GetMachine(stayerMachineID)

	if sourceMachine == nil || targetMachine == nil || stayerMachine == nil {
		fail("lookup", fmt.Errorf("one or more machines not found in store"))
		return
	}

	targetMinor := coord.store.AllocateMinor(targetMachineID)

	// Create operation with all metadata needed for resumption
	opID := generateOpID()
	coord.store.CreateOperation(opID, "live_migration", userID, map[string]interface{}{
		"source_machine": sourceMachineID,
		"target_machine": targetMachineID,
		"stayer_machine": stayerMachineID,
		"source_address": sourceMachine.Address,
		"target_address": targetMachine.Address,
		"stayer_address": stayerMachine.Address,
		"port":           user.DRBDPort,
		"source_minor":   sourceBipod.DRBDMinor,
		"target_minor":   targetMinor,
		"stayer_minor":   stayerBipod.DRBDMinor,
		"source_loop":    sourceBipod.LoopDevice,
		"stayer_loop":    stayerBipod.LoopDevice,
		"migration_type": migrationType,
	})

	coord.store.CreateBipod(userID, targetMachineID, "secondary", targetMinor)
	targetBipodCreated = true

	logger.Info("Migration started",
		"type", migrationType,
		"stayer", stayerMachineID,
		"op_id", opID,
	)

	coord.step(opID, "migrate-target-selected")

	sourceAddr := stripPort(sourceMachine.Address)
	targetAddr := stripPort(targetMachine.Address)
	stayerAddr := stripPort(stayerMachine.Address)

	sourceClient = NewMachineClient(sourceMachine.Address)
	targetClient = NewMachineClient(targetMachine.Address)
	stayerClient = NewMachineClient(stayerMachine.Address)

	// Build original 2-node config for fail handler restoration
	originalBipodNodes = []shared.DRBDNode{
		{
			Hostname: sourceMachineID,
			Minor:    sourceBipod.DRBDMinor,
			Disk:     sourceBipod.LoopDevice,
			Address:  sourceAddr,
		},
		{
			Hostname: stayerMachineID,
			Minor:    stayerBipod.DRBDMinor,
			Disk:     stayerBipod.LoopDevice,
			Address:  stayerAddr,
		},
	}

	// ── Step 2: Create image on target ──
	var targetLoop string
	err := retry("target_image", func() error {
		resp, e := targetClient.CreateImage(userID, user.ImageSizeMB)
		if e != nil {
			return e
		}
		targetLoop = resp.LoopDevice
		return nil
	})
	if err != nil {
		fail("target_image", err)
		coord.store.FailOperation(opID, "target image: "+err.Error())
		return
	}
	coord.store.SetBipodLoopDevice(userID, targetMachineID, targetLoop)
	logger.Info("Image created on target", "loop", targetLoop)
	coord.step(opID, "migrate-image-created")

	// ── Step 3: Add target to DRBD (temporary tripod) ──
	tripodNodes := []shared.DRBDNode{
		{
			Hostname: sourceMachineID,
			Minor:    sourceBipod.DRBDMinor,
			Disk:     sourceBipod.LoopDevice,
			Address:  sourceAddr,
		},
		{
			Hostname: stayerMachineID,
			Minor:    stayerBipod.DRBDMinor,
			Disk:     stayerBipod.LoopDevice,
			Address:  stayerAddr,
		},
		{
			Hostname: targetMachineID,
			Minor:    targetMinor,
			Disk:     targetLoop,
			Address:  targetAddr,
		},
	}

	// Create DRBD on target (3-node config, fresh metadata)
	tripodCreateReq := &shared.DRBDCreateRequest{
		ResourceName: "user-" + userID,
		Nodes:        tripodNodes,
		Port:         user.DRBDPort,
	}
	err = retry("drbd_target_create", func() error {
		_, e := targetClient.DRBDCreate(userID, tripodCreateReq)
		return e
	})
	if err != nil {
		fail("drbd_target_create", err)
		coord.store.FailOperation(opID, "drbd target create: "+err.Error())
		return
	}

	// Determine roles for force reconfigure
	var sourceRole, stayerRoleInBipod string
	if migrationType == "primary" {
		sourceRole = "primary"
		stayerRoleInBipod = "secondary"
	} else {
		sourceRole = "secondary"
		stayerRoleInBipod = "primary"
	}

	// Reconfigure source and stayer to 3-node config
	reconfigMethod = "adjust"

	// Try adjust on source (Role not needed for adjust path)
	sourceReconfigReq := &shared.DRBDReconfigureRequest{
		Nodes: tripodNodes,
		Port:  user.DRBDPort,
		Force: false,
		Role:  sourceRole,
	}
	_, err = sourceClient.DRBDReconfigure(userID, sourceReconfigReq)
	if err != nil {
		logger.Warn("drbdadm adjust failed on source, using force fallback", "error", err)
		reconfigMethod = "down_up"

		if migrationType == "primary" {
			// Stop container, force reconfigure, restart
			if stopErr := sourceClient.ContainerStop(userID); stopErr != nil {
				fail("source_container_stop_for_reconfig", stopErr)
				coord.store.FailOperation(opID, "container stop for reconfig: "+stopErr.Error())
				return
			}
			containerStopped = true
		}

		sourceReconfigReq.Force = true
		_, err = sourceClient.DRBDReconfigure(userID, sourceReconfigReq)
		if err != nil {
			fail("source_force_reconfig", err)
			coord.store.FailOperation(opID, "source force reconfig: "+err.Error())
			return
		}

		if migrationType == "primary" {
			if _, startErr := sourceClient.ContainerStart(userID); startErr != nil {
				fail("source_container_restart_after_reconfig", startErr)
				coord.store.FailOperation(opID, "container restart after reconfig: "+startErr.Error())
				return
			}
			containerStopped = false // container restarted successfully
		}
	}

	// Source is now running 3-node config; mark so fail handler can restore if needed.
	tripodConfigured = true

	// Try adjust on stayer
	stayerReconfigReq := &shared.DRBDReconfigureRequest{
		Nodes: tripodNodes,
		Port:  user.DRBDPort,
		Force: false,
		Role:  stayerRoleInBipod,
	}
	_, err = stayerClient.DRBDReconfigure(userID, stayerReconfigReq)
	if err != nil {
		logger.Warn("drbdadm adjust failed on stayer, using force fallback", "error", err)
		if reconfigMethod == "adjust" {
			reconfigMethod = "down_up"
		}

		stayerReconfigReq.Force = true
		_, err = stayerClient.DRBDReconfigure(userID, stayerReconfigReq)
		if err != nil {
			fail("stayer_force_reconfig", err)
			coord.store.FailOperation(opID, "stayer force reconfig: "+err.Error())
			return
		}
	}

	logger.Info("DRBD tripod configured", "method", reconfigMethod)
	coord.step(opID, "migrate-drbd-added")

	// ── Step 4: Wait for target to sync ──
	syncStart := time.Now()
	for {
		if time.Since(syncStart) > MigrationSyncTimeout {
			fail("sync_timeout", fmt.Errorf("sync timeout after %v", MigrationSyncTimeout))
			coord.store.FailOperation(opID, "sync timeout")
			return
		}

		status, err := sourceClient.DRBDStatus(userID)
		if err != nil {
			logger.Warn("DRBD status check failed", "error", err)
			time.Sleep(2 * time.Second)
			continue
		}

		// Check if target peer is UpToDate
		targetSynced := false
		for _, peer := range status.Peers {
			if peer.Hostname == targetMachineID && peer.DiskState == "UpToDate" {
				targetSynced = true
				break
			}
		}

		if targetSynced {
			logger.Info("Target DRBD sync complete")
			break
		}

		// Log progress
		for _, peer := range status.Peers {
			if peer.Hostname == targetMachineID && peer.SyncProgress != nil {
				logger.Info("DRBD syncing to target", "progress", *peer.SyncProgress)
			}
		}
		time.Sleep(2 * time.Second)
	}
	coord.step(opID, "migrate-synced")

	// ── PRIMARY MIGRATION ONLY: Steps 5-8 ──
	if migrationType == "primary" {
		// Step 5: Stop container on source
		err = retry("container_stop", func() error {
			return sourceClient.ContainerStop(userID)
		})
		if err != nil {
			fail("container_stop", err)
			coord.store.FailOperation(opID, "container stop: "+err.Error())
			return
		}
		logger.Info("Container stopped on source")
		containerStopped = true
		coord.step(opID, "migrate-container-stopped")

		// Step 6: Demote source
		err = retry("drbd_demote", func() error {
			_, e := sourceClient.DRBDDemote(userID)
			return e
		})
		if err != nil {
			fail("drbd_demote", err)
			coord.store.FailOperation(opID, "drbd demote: "+err.Error())
			return
		}
		logger.Info("Source demoted to Secondary")
		coord.step(opID, "migrate-source-demoted")

		// Step 7: Promote target
		err = retry("drbd_promote", func() error {
			_, e := targetClient.DRBDPromote(userID)
			return e
		})
		if err != nil {
			fail("drbd_promote", err)
			coord.store.FailOperation(opID, "drbd promote: "+err.Error())
			return
		}
		logger.Info("Target promoted to Primary")
		coord.step(opID, "migrate-target-promoted")

		// Step 8: Start container on target
		err = retry("container_start", func() error {
			_, e := targetClient.ContainerStart(userID)
			return e
		})
		if err != nil {
			fail("container_start", err)
			coord.store.FailOperation(opID, "container start: "+err.Error())
			return
		}
		logger.Info("Container started on target")
		coord.step(opID, "migrate-container-started")

		// Update DB: primary machine changes
		coord.store.SetUserPrimary(userID, targetMachineID)
	}

	// ── Step 9: Clean up source (remove from tripod → back to bipod) ──
	// Disconnect source from peers
	if _, disconnErr := sourceClient.DRBDDisconnect(userID); disconnErr != nil {
		logger.Warn("DRBD disconnect on source failed (continuing)", "error", disconnErr)
	}

	// Destroy DRBD on source
	if destroyErr := sourceClient.DRBDDestroy(userID); destroyErr != nil {
		logger.Warn("DRBD destroy on source failed (continuing)", "error", destroyErr)
	}

	// Delete image on source
	if delErr := sourceClient.DeleteUser(userID); delErr != nil {
		logger.Warn("Delete user on source failed (continuing)", "error", delErr)
	}

	// Determine roles for reconfigure
	var targetRole, stayerRole string
	if migrationType == "primary" {
		targetRole = "primary"
		stayerRole = "secondary"
	} else {
		targetRole = "secondary"
		stayerRole = "primary"
	}

	// Build 2-node config (target + stayer)
	bipodNodes := []shared.DRBDNode{
		{
			Hostname: targetMachineID,
			Minor:    targetMinor,
			Disk:     targetLoop,
			Address:  targetAddr,
		},
		{
			Hostname: stayerMachineID,
			Minor:    stayerBipod.DRBDMinor,
			Disk:     stayerBipod.LoopDevice,
			Address:  stayerAddr,
		},
	}

	// Reconfigure target to 2-node
	bipodReconfigReq := &shared.DRBDReconfigureRequest{
		Nodes: bipodNodes,
		Port:  user.DRBDPort,
		Force: false,
		Role:  targetRole,
	}
	_, err = targetClient.DRBDReconfigure(userID, bipodReconfigReq)
	if err != nil {
		logger.Warn("adjust failed on target during cleanup, using force", "error", err)
		bipodReconfigReq.Force = true
		targetClient.DRBDReconfigure(userID, bipodReconfigReq)
	}

	// Reconfigure stayer to 2-node
	stayerReconfigReq = &shared.DRBDReconfigureRequest{
		Nodes: bipodNodes,
		Port:  user.DRBDPort,
		Force: false,
		Role:  stayerRole,
	}
	_, err = stayerClient.DRBDReconfigure(userID, stayerReconfigReq)
	if err != nil {
		logger.Warn("adjust failed on stayer during cleanup, using force", "error", err)
		stayerReconfigReq.Force = true
		stayerClient.DRBDReconfigure(userID, stayerReconfigReq)
	}

	// Update bipod records
	coord.store.RemoveBipod(userID, sourceMachineID)
	if migrationType == "primary" {
		coord.store.SetBipodRole(userID, targetMachineID, "primary")
	} else {
		coord.store.SetBipodRole(userID, targetMachineID, "secondary")
	}

	if migrationType == "primary" {
		coord.step(opID, "migrate-source-cleaned")
	} else {
		coord.step(opID, "migrate-secondary-cleaned")
	}

	// ── Step 10: Mark complete ──
	coord.store.SetUserStatus(userID, "running", "")
	if err := coord.store.CompleteOperation(opID); err != nil {
		logger.Error("Failed to complete operation in DB", "op_id", opID, "error", err)
	}

	// Update ActiveAgents so rebalancer sees correct counts before next heartbeat
	if migrationType == "primary" {
		coord.store.AdjustActiveAgents(sourceMachineID, targetMachineID)
	}

	if err := coord.store.RecordMigrationEvent(MigrationEvent{
		UserID:        userID,
		SourceMachine: sourceMachineID,
		TargetMachine: targetMachineID,
		MigrationType: migrationType,
		Success:       true,
		Method:        reconfigMethod,
		Trigger:       trigger,
		DurationMS:    time.Since(start).Milliseconds(),
		Timestamp:     time.Now(),
	}); err != nil {
		logger.Error("Failed to record migration event in DB", "error", err)
	}

	logger.Info("Migration complete",
		"type", migrationType,
		"method", reconfigMethod,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

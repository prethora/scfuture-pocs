package coordinator

import (
	"log/slog"
	"net/http"
	"time"

	"scfuture/internal/shared"
)

// Reconcile runs the five-phase startup reconciliation algorithm.
// Must be called BEFORE starting background goroutines or HTTP server.
func (coord *Coordinator) Reconcile() {
	start := time.Now()
	logger := slog.With("component", "reconciler")
	logger.Info("Starting reconciliation")

	// Phase 1: Discover Reality — probe all machines
	machineStatuses := coord.reconcilePhase1DiscoverReality(logger)

	// Phase 2: Reconcile DB with machine reality
	orphans := coord.reconcilePhase2ReconcileDB(logger, machineStatuses)

	// Phase 3: Resume interrupted operations
	resumed := coord.reconcilePhase3ResumeOperations(logger, machineStatuses)

	// Phase 3b: Clean up orphans
	coord.reconcilePhase3bCleanOrphans(logger, orphans)

	// Phase 3c: Clean up stale tripod bipods — running users should have exactly 2 non-stale bipods
	coord.reconcilePhase3cCleanStaleTripods(logger, machineStatuses)

	// Phase 4: Handle offline machines (failover for running users on dead machines)
	coord.reconcilePhase4HandleOffline(logger)

	// Phase 5: Ensure running users have containers on their primary machine
	coord.reconcilePhase5EnsureContainers(logger, machineStatuses)

	// Phase 6: Log completion
	logger.Info("Reconciliation complete",
		"duration_ms", time.Since(start).Milliseconds(),
		"operations_resumed", resumed,
		"orphans_cleaned", len(orphans),
	)
}

// machineReality holds the /status response from a machine probe.
type machineReality struct {
	Online    bool
	MachineID string
	Address   string
	Users     map[string]machineUserInfo
}

type machineUserInfo struct {
	ImageExists      bool
	DRBDRole         string
	ContainerRunning bool
}

// orphanEntry tracks a resource that exists on a machine but not in the DB.
type orphanEntry struct {
	MachineID string
	UserID    string
	Address   string
}

// reconcilePhase1DiscoverReality probes all machines in the DB.
func (coord *Coordinator) reconcilePhase1DiscoverReality(logger *slog.Logger) map[string]*machineReality {
	logger.Info("[Phase 1] Discovering machine reality")

	machines := coord.store.AllMachines()
	results := make(map[string]*machineReality)

	for _, m := range machines {
		mr := &machineReality{
			MachineID: m.MachineID,
			Address:   m.Address,
			Users:     make(map[string]machineUserInfo),
		}

		client := &MachineClient{
			address: m.Address,
			client:  &http.Client{Timeout: 5 * time.Second},
		}

		status, err := client.Status()
		if err != nil {
			logger.Warn("[Phase 1] Machine unreachable", "machine_id", m.MachineID, "error", err)
			coord.store.SetMachineStatus(m.MachineID, "dead")
			mr.Online = false
		} else {
			logger.Info("[Phase 1] Machine reachable", "machine_id", m.MachineID,
				"users", len(status.Users))
			coord.store.SetMachineStatus(m.MachineID, "active")
			mr.Online = true

			// Parse user status from machine — Users is map[string]*UserStatusDTO
			for userID, u := range status.Users {
				mr.Users[userID] = machineUserInfo{
					ImageExists:      u.ImageExists,
					DRBDRole:         u.DRBDRole,
					ContainerRunning: u.ContainerRunning,
				}
			}
		}

		results[m.MachineID] = mr
	}

	// Reload cache after status updates
	coord.store.ReloadCache()

	return results
}

// reconcilePhase2ReconcileDB cross-references machine reality with DB state.
func (coord *Coordinator) reconcilePhase2ReconcileDB(logger *slog.Logger, machineStatuses map[string]*machineReality) []orphanEntry {
	logger.Info("[Phase 2] Reconciling DB with machine reality")

	var orphans []orphanEntry

	users := coord.store.AllUsers()
	for _, u := range users {
		bipods := coord.store.GetBipods(u.UserID)
		for _, b := range bipods {
			mr, exists := machineStatuses[b.MachineID]
			if !exists || !mr.Online {
				// Machine is dead — mark bipod stale if not already
				if b.Role != "stale" {
					coord.store.SetBipodRole(u.UserID, b.MachineID, "stale")
					logger.Info("[Phase 2] Marked bipod stale (machine dead)",
						"user_id", u.UserID, "machine_id", b.MachineID)
				}
				continue
			}

			// Machine is online — check if it knows about this user
			_, machineKnows := mr.Users[u.UserID]
			if !machineKnows && b.Role != "stale" {
				// DB says resources exist, machine says they don't
				orphans = append(orphans, orphanEntry{
					MachineID: b.MachineID,
					UserID:    u.UserID,
					Address:   mr.Address,
				})
				logger.Info("[Phase 2] DB-only bipod (machine doesn't know user)",
					"user_id", u.UserID, "machine_id", b.MachineID)
			}
		}

		// User-level consistency
		if u.Status == "running" {
			// Count non-stale bipods where the machine actually knows about the user
			liveBipods := 0
			containerFound := false
			for _, b := range bipods {
				if b.Role == "stale" {
					continue
				}
				mr := machineStatuses[b.MachineID]
				if mr != nil && mr.Online {
					if info, ok := mr.Users[u.UserID]; ok {
						liveBipods++
						if info.ContainerRunning {
							containerFound = true
						}
					} else {
						// Machine is online but doesn't know about this user — mark bipod stale
						coord.store.SetBipodRole(u.UserID, b.MachineID, "stale")
						logger.Info("[Phase 2] Marking orphaned bipod stale (machine doesn't know user)",
							"user_id", u.UserID, "machine_id", b.MachineID)
					}
				}
			}
			if liveBipods == 0 {
				// All bipods are gone — user's resources have been fully destroyed
				// Mark as failed so it can be re-provisioned
				coord.store.SetUserStatus(u.UserID, "failed", "resources lost during crash recovery")
				logger.Warn("[Phase 2] Running user has no live bipods — marking failed",
					"user_id", u.UserID)
			} else if !containerFound {
				// Bipods exist but container not running — will be repaired by Phase 3 or Phase 4
				logger.Warn("[Phase 2] Running user has no container",
					"user_id", u.UserID)
			}
		}

		if u.Status == "evicted" {
			// Evicted users should have no non-stale bipods — clean up any leftover from crashed reactivation
			for _, b := range bipods {
				if b.Role != "stale" {
					mr := machineStatuses[b.MachineID]
					if mr != nil && mr.Online {
						client := NewMachineClient(mr.Address)
						client.DRBDDisconnect(u.UserID)
						client.DRBDDestroy(u.UserID)
						client.DeleteUser(u.UserID)
					}
					coord.store.SetBipodRole(u.UserID, b.MachineID, "stale")
					logger.Info("[Phase 2] Cleaned stale bipod on evicted user",
						"user_id", u.UserID, "machine_id", b.MachineID)
				}
			}
		}

		if u.Status == "provisioning" {
			// Check if container IS running (coordinator crashed after start but before status update)
			for _, b := range bipods {
				mr := machineStatuses[b.MachineID]
				if mr != nil && mr.Online {
					if info, ok := mr.Users[u.UserID]; ok && info.ContainerRunning {
						logger.Info("[Phase 2] Provisioning user has running container — updating to running",
							"user_id", u.UserID)
						coord.store.SetUserStatus(u.UserID, "running", "")
						break
					}
				}
			}
		}
	}

	// Check for orphans on machines (machine has user resources, DB doesn't)
	for machineID, mr := range machineStatuses {
		if !mr.Online {
			continue
		}
		for userID := range mr.Users {
			u := coord.store.GetUser(userID)
			if u == nil {
				// Machine has user that DB doesn't know about
				orphans = append(orphans, orphanEntry{
					MachineID: machineID,
					UserID:    userID,
					Address:   mr.Address,
				})
				logger.Info("[Phase 2] Orphaned resource (no DB user)",
					"user_id", userID, "machine_id", machineID)
			}
		}
	}

	return orphans
}

// reconcilePhase3ResumeOperations reads incomplete operations and resumes them.
func (coord *Coordinator) reconcilePhase3ResumeOperations(logger *slog.Logger, machineStatuses map[string]*machineReality) int {
	logger.Info("[Phase 3] Resuming interrupted operations")

	ops, err := coord.store.GetIncompleteOperations()
	if err != nil {
		logger.Error("[Phase 3] Failed to get incomplete operations", "error", err)
		return 0
	}

	if len(ops) == 0 {
		logger.Info("[Phase 3] No interrupted operations found")
	}

	resumed := 0
	for _, op := range ops {
		logger.Info("[Phase 3] Found interrupted operation",
			"op_id", op.OperationID, "type", op.Type,
			"user_id", op.UserID, "step", op.CurrentStep)

		switch op.Type {
		case "provision":
			coord.resumeProvision(op, machineStatuses)
		case "failover":
			coord.resumeFailover(op, machineStatuses)
		case "reformation":
			coord.resumeReformation(op, machineStatuses)
		case "suspension":
			coord.resumeSuspension(op, machineStatuses)
		case "reactivation_warm":
			coord.resumeWarmReactivation(op, machineStatuses)
		case "reactivation_cold":
			coord.resumeColdReactivation(op, machineStatuses)
		case "eviction":
			coord.resumeEviction(op, machineStatuses)
		case "live_migration":
			coord.resumeMigration(op, machineStatuses)
		default:
			logger.Warn("[Phase 3] Unknown operation type — cancelling", "type", op.Type)
			coord.store.CancelOperation(op.OperationID)
		}
		resumed++
	}

	// Handle users stuck in "provisioning" or "migrating" with no operation row
	users := coord.store.AllUsers()
	for _, u := range users {
		if u.Status == "provisioning" {
			// Check if there's an operation for this user
			ops, _ := coord.store.GetIncompleteOperations()
			hasOp := false
			for _, op := range ops {
				if op.UserID == u.UserID && op.Type == "provision" {
					hasOp = true
					break
				}
			}
			if !hasOp {
				logger.Info("[Phase 3] User stuck in provisioning with no operation — marking failed",
					"user_id", u.UserID)
				coord.store.SetUserStatus(u.UserID, "failed", "coordinator crashed before operation created")
			}
		}
		if u.Status == "migrating" {
			// Check if there's a migration operation for this user
			ops, _ := coord.store.GetIncompleteOperations()
			hasOp := false
			for _, op := range ops {
				if op.UserID == u.UserID && op.Type == "live_migration" {
					hasOp = true
					break
				}
			}
			if !hasOp {
				logger.Info("[Phase 3] User stuck in migrating with no operation — reverting to running",
					"user_id", u.UserID)
				coord.store.SetUserStatus(u.UserID, "running", "coordinator crashed before migration operation created")
			}
		}
	}

	return resumed
}

// resumeProvision resumes an interrupted provisioning operation.
func (coord *Coordinator) resumeProvision(op *Operation, machineStatuses map[string]*machineReality) {
	logger := slog.With("component", "reconciler", "op_id", op.OperationID, "user_id", op.UserID)

	primaryMachine := metaString(op.Metadata, "primary_machine")
	secondaryMachine := metaString(op.Metadata, "secondary_machine")

	// Check if required machines are online
	pm := machineStatuses[primaryMachine]
	sm := machineStatuses[secondaryMachine]

	if (pm == nil || !pm.Online) || (sm == nil || !sm.Online) {
		logger.Warn("Required machines offline — marking provision failed")
		coord.store.SetUserStatus(op.UserID, "failed", "machines offline during recovery")
		coord.store.FailOperation(op.OperationID, "machines offline during recovery")
		return
	}

	// Check if user already has a running container (completed but status not updated)
	if pm.Online {
		if info, ok := pm.Users[op.UserID]; ok && info.ContainerRunning {
			logger.Info("Container already running — completing provision")
			coord.store.SetUserStatus(op.UserID, "running", "")
			_ = coord.store.CompleteOperation(op.OperationID)
			return
		}
	}

	// Re-run provisioning from the beginning with the same parameters
	// Since all steps are idempotent, this is safe
	logger.Info("Restarting provision from beginning")
	coord.store.FailOperation(op.OperationID, "restarted after crash recovery")
	coord.store.SetUserStatus(op.UserID, "failed", "restarted after crash — re-provision needed")
}

// resumeFailover resumes an interrupted failover operation.
func (coord *Coordinator) resumeFailover(op *Operation, machineStatuses map[string]*machineReality) {
	logger := slog.With("component", "reconciler", "op_id", op.OperationID, "user_id", op.UserID)

	survivingMachine := metaString(op.Metadata, "surviving_machine")
	survivingAddress := metaString(op.Metadata, "surviving_address")

	mr := machineStatuses[survivingMachine]
	if mr == nil || !mr.Online {
		logger.Warn("Surviving machine offline — marking user unavailable")
		coord.store.SetUserStatus(op.UserID, "unavailable", "surviving machine offline during recovery")
		coord.store.FailOperation(op.OperationID, "surviving machine offline")
		return
	}

	// Check current state
	if info, ok := mr.Users[op.UserID]; ok && info.ContainerRunning {
		logger.Info("Container already running on survivor — completing failover")
		coord.store.SetBipodRole(op.UserID, survivingMachine, "primary")
		coord.store.SetUserPrimary(op.UserID, survivingMachine)
		coord.store.SetUserStatus(op.UserID, "running_degraded", "recovered after crash")
		_ = coord.store.CompleteOperation(op.OperationID)
		return
	}

	// Resume from current step
	client := NewMachineClient(survivingAddress)

	switch op.CurrentStep {
	case "failover-detected":
		// Need to promote and start container
		if _, err := client.DRBDPromote(op.UserID); err != nil {
			logger.Error("Resume DRBD promote failed", "error", err)
			coord.store.SetUserStatus(op.UserID, "unavailable", "drbd promote failed during recovery")
			coord.store.FailOperation(op.OperationID, "promote failed during recovery")
			return
		}
		coord.store.UpdateOperationStep(op.OperationID, "failover-promoted")
		fallthrough
	case "failover-promoted":
		if _, err := client.ContainerStart(op.UserID); err != nil {
			logger.Error("Resume container start failed", "error", err)
			coord.store.SetBipodRole(op.UserID, survivingMachine, "primary")
			coord.store.SetUserPrimary(op.UserID, survivingMachine)
			coord.store.SetUserStatus(op.UserID, "running_degraded", "container start failed during recovery")
			coord.store.FailOperation(op.OperationID, "container start failed during recovery")
			return
		}
		coord.store.UpdateOperationStep(op.OperationID, "failover-container-started")
		fallthrough
	case "failover-container-started":
		coord.store.SetBipodRole(op.UserID, survivingMachine, "primary")
		coord.store.SetUserPrimary(op.UserID, survivingMachine)
		coord.store.SetUserStatus(op.UserID, "running_degraded", "recovered after crash")
		_ = coord.store.CompleteOperation(op.OperationID)
	default:
		logger.Warn("Unknown failover step — marking failed", "step", op.CurrentStep)
		coord.store.SetUserStatus(op.UserID, "unavailable", "unknown step during recovery: "+op.CurrentStep)
		coord.store.FailOperation(op.OperationID, "unknown step: "+op.CurrentStep)
	}
}

// resumeReformation resumes an interrupted reformation operation.
func (coord *Coordinator) resumeReformation(op *Operation, machineStatuses map[string]*machineReality) {
	logger := slog.With("component", "reconciler", "op_id", op.OperationID, "user_id", op.UserID)
	logger.Info("Reformation interrupted — reverting to running_degraded")
	coord.store.SetUserStatus(op.UserID, "running_degraded", "reformation interrupted by crash — reformer will retry")
	coord.store.FailOperation(op.OperationID, "interrupted by crash")
}

// resumeSuspension resumes an interrupted suspension operation.
func (coord *Coordinator) resumeSuspension(op *Operation, machineStatuses map[string]*machineReality) {
	logger := slog.With("component", "reconciler", "op_id", op.OperationID, "user_id", op.UserID)

	previousStatus := metaString(op.Metadata, "previous_status")
	if previousStatus == "" {
		previousStatus = "running"
	}

	primaryMachine := metaString(op.Metadata, "primary_machine")
	mr := machineStatuses[primaryMachine]

	// If container is stopped and we're past the demote step, complete the suspension
	if mr != nil && mr.Online {
		if info, ok := mr.Users[op.UserID]; ok && !info.ContainerRunning {
			if op.CurrentStep == "suspend-demoted" || op.CurrentStep == "suspend-backed-up" {
				logger.Info("Container stopped, completing suspension")
				coord.store.SetUserStatus(op.UserID, "suspended", "")
				coord.store.SetUserDRBDDisconnected(op.UserID, false)
				_ = coord.store.CompleteOperation(op.OperationID)
				return
			}
		}
	}

	// Revert to previous status
	logger.Info("Reverting to previous status", "previous", previousStatus)
	coord.store.SetUserStatus(op.UserID, previousStatus, "suspension interrupted by crash")
	coord.store.FailOperation(op.OperationID, "interrupted by crash")

	// If reverting to running, ensure container is actually running
	if previousStatus == "running" && mr != nil && mr.Online {
		if info, ok := mr.Users[op.UserID]; !ok || !info.ContainerRunning {
			logger.Info("Restarting container after suspension revert", "machine", primaryMachine)
			client := NewMachineClient(mr.Address)
			if _, err := client.ContainerStart(op.UserID); err != nil {
				logger.Warn("Failed to restart container after suspension revert", "error", err)
			}
		}
	}
}

// resumeWarmReactivation resumes an interrupted warm reactivation.
func (coord *Coordinator) resumeWarmReactivation(op *Operation, machineStatuses map[string]*machineReality) {
	logger := slog.With("component", "reconciler", "op_id", op.OperationID, "user_id", op.UserID)

	primaryMachine := metaString(op.Metadata, "primary_machine")
	mr := machineStatuses[primaryMachine]

	// If container is already running, complete it
	if mr != nil && mr.Online {
		if info, ok := mr.Users[op.UserID]; ok && info.ContainerRunning {
			logger.Info("Container already running — completing warm reactivation")
			coord.store.SetBipodRole(op.UserID, primaryMachine, "primary")
			coord.store.SetUserStatus(op.UserID, "running", "")
			coord.store.SetUserDRBDDisconnected(op.UserID, false)
			_ = coord.store.CompleteOperation(op.OperationID)
			return
		}
	}

	// Revert to suspended
	logger.Info("Reverting to suspended")
	coord.store.SetUserStatus(op.UserID, "suspended", "warm reactivation interrupted by crash")
	coord.store.FailOperation(op.OperationID, "interrupted by crash")
}

// resumeColdReactivation resumes an interrupted cold reactivation.
func (coord *Coordinator) resumeColdReactivation(op *Operation, machineStatuses map[string]*machineReality) {
	logger := slog.With("component", "reconciler", "op_id", op.OperationID, "user_id", op.UserID)

	primaryMachine := metaString(op.Metadata, "primary_machine")

	// Check if container is already running
	if primaryMachine != "" {
		mr := machineStatuses[primaryMachine]
		if mr != nil && mr.Online {
			if info, ok := mr.Users[op.UserID]; ok && info.ContainerRunning {
				logger.Info("Container already running — completing cold reactivation")
				coord.store.SetUserStatus(op.UserID, "running", "")
				coord.store.SetUserDRBDDisconnected(op.UserID, false)
				_ = coord.store.CompleteOperation(op.OperationID)
				return
			}
		}
	}

	// Clean up partial resources and revert to evicted
	logger.Info("Reverting to evicted — cleaning partial resources")

	// Clean up any bipods created during the partial cold reactivation
	bipods := coord.store.GetBipods(op.UserID)
	for _, b := range bipods {
		if b.Role == "stale" {
			continue
		}
		mr := machineStatuses[b.MachineID]
		if mr != nil && mr.Online {
			client := NewMachineClient(mr.Address)
			client.DRBDDisconnect(op.UserID)
			client.DRBDDestroy(op.UserID)
			client.DeleteUser(op.UserID)
		}
		coord.store.SetBipodRole(op.UserID, b.MachineID, "stale")
	}

	coord.store.SetUserStatus(op.UserID, "evicted", "cold reactivation interrupted by crash")
	coord.store.FailOperation(op.OperationID, "interrupted by crash")
}

// resumeEviction resumes an interrupted eviction operation.
func (coord *Coordinator) resumeEviction(op *Operation, machineStatuses map[string]*machineReality) {
	logger := slog.With("component", "reconciler", "op_id", op.OperationID, "user_id", op.UserID)

	switch op.CurrentStep {
	case "evict-resources-cleaned":
		// Almost done — just update status
		logger.Info("Resources cleaned — completing eviction")
		coord.store.ClearUserBipods(op.UserID)
		coord.store.SetUserStatus(op.UserID, "evicted", "")
		coord.store.SetUserDRBDDisconnected(op.UserID, false)
		_ = coord.store.CompleteOperation(op.OperationID)
	case "evict-backup-verified":
		// Need to clean resources — retry the cleanup
		logger.Info("Backup verified — cleaning resources")
		bipods := coord.store.GetBipods(op.UserID)
		for _, b := range bipods {
			mr := machineStatuses[b.MachineID]
			if mr == nil || !mr.Online {
				continue
			}
			client := NewMachineClient(mr.Address)
			client.DRBDDisconnect(op.UserID)
			client.DRBDDestroy(op.UserID)
			client.DeleteUser(op.UserID)
		}
		coord.store.ClearUserBipods(op.UserID)
		coord.store.SetUserStatus(op.UserID, "evicted", "")
		coord.store.SetUserDRBDDisconnected(op.UserID, false)
		_ = coord.store.CompleteOperation(op.OperationID)
	default:
		// Phase 3b will handle orphan cleanup
		logger.Info("Partial eviction — completing")
		coord.store.SetUserStatus(op.UserID, "suspended", "eviction interrupted by crash")
		coord.store.FailOperation(op.OperationID, "interrupted by crash")
	}
}

// resumeMigration resumes an interrupted live migration operation.
func (coord *Coordinator) resumeMigration(op *Operation, machineStatuses map[string]*machineReality) {
	logger := slog.With("component", "reconciler", "op_id", op.OperationID, "user_id", op.UserID)

	sourceMachine := metaString(op.Metadata, "source_machine")
	targetMachine := metaString(op.Metadata, "target_machine")
	stayerMachine := metaString(op.Metadata, "stayer_machine")
	migrationType := metaString(op.Metadata, "migration_type")
	sourceAddress := metaString(op.Metadata, "source_address")
	targetAddress := metaString(op.Metadata, "target_address")
	stayerAddress := metaString(op.Metadata, "stayer_address")

	logger.Info("Resuming migration",
		"step", op.CurrentStep, "type", migrationType,
		"source", sourceMachine, "target", targetMachine, "stayer", stayerMachine,
	)

	// Helper: cancel migration, revert to 2-node and running
	cancelMigration := func(reason string) {
		logger.Info("Cancelling migration", "reason", reason)

		// Clean up target resources if target is online
		tmr := machineStatuses[targetMachine]
		if tmr != nil && tmr.Online {
			client := NewMachineClient(targetAddress)
			client.DRBDDisconnect(op.UserID)
			client.DRBDDestroy(op.UserID)
			client.DeleteUser(op.UserID)
		}
		coord.store.RemoveBipod(op.UserID, targetMachine)

		// Revert source and stayer to 2-node config if they are online
		sourceMinor := metaInt(op.Metadata, "source_minor")
		stayerMinor := metaInt(op.Metadata, "stayer_minor")
		sourceLoop := metaString(op.Metadata, "source_loop")
		stayerLoop := metaString(op.Metadata, "stayer_loop")
		port := metaInt(op.Metadata, "port")

		bipodNodes := []shared.DRBDNode{
			{Hostname: sourceMachine, Minor: sourceMinor, Disk: sourceLoop, Address: stripPort(sourceAddress)},
			{Hostname: stayerMachine, Minor: stayerMinor, Disk: stayerLoop, Address: stripPort(stayerAddress)},
		}

		// Determine roles: source keeps its original role, stayer is opposite
		var cancelSourceRole, cancelStayerRole string
		if migrationType == "primary" {
			cancelSourceRole = "primary"
			cancelStayerRole = "secondary"
		} else {
			cancelSourceRole = "secondary"
			cancelStayerRole = "primary"
		}

		smr := machineStatuses[sourceMachine]
		if smr != nil && smr.Online {
			client := NewMachineClient(sourceAddress)
			sourceReq := &shared.DRBDReconfigureRequest{Nodes: bipodNodes, Port: port, Force: false, Role: cancelSourceRole}
			if _, err := client.DRBDReconfigure(op.UserID, sourceReq); err != nil {
				sourceReq.Force = true
				client.DRBDReconfigure(op.UserID, sourceReq)
			}
		}

		stmr := machineStatuses[stayerMachine]
		if stmr != nil && stmr.Online {
			client := NewMachineClient(stayerAddress)
			stayerReq := &shared.DRBDReconfigureRequest{Nodes: bipodNodes, Port: port, Force: false, Role: cancelStayerRole}
			if _, err := client.DRBDReconfigure(op.UserID, stayerReq); err != nil {
				stayerReq.Force = true
				client.DRBDReconfigure(op.UserID, stayerReq)
			}
		}

		coord.store.SetUserStatus(op.UserID, "running", "migration cancelled: "+reason)
		coord.store.CancelOperation(op.OperationID)
	}

	// Helper: complete cleanup phase (source removed, back to 2-node)
	completeCleanup := func() {
		// Source may still need cleanup
		smr := machineStatuses[sourceMachine]
		if smr != nil && smr.Online {
			client := NewMachineClient(sourceAddress)
			client.DRBDDisconnect(op.UserID)
			client.DRBDDestroy(op.UserID)
			client.DeleteUser(op.UserID)
		}

		// Reconfigure to 2-node (target + stayer)
		targetMinor := metaInt(op.Metadata, "target_minor")
		stayerMinor := metaInt(op.Metadata, "stayer_minor")
		stayerLoop := metaString(op.Metadata, "stayer_loop")
		port := metaInt(op.Metadata, "port")

		// Get target loop from bipod
		bipods := coord.store.GetBipods(op.UserID)
		var targetLoop string
		for _, b := range bipods {
			if b.MachineID == targetMachine {
				targetLoop = b.LoopDevice
			}
		}

		bipodNodes := []shared.DRBDNode{
			{Hostname: targetMachine, Minor: targetMinor, Disk: targetLoop, Address: stripPort(targetAddress)},
			{Hostname: stayerMachine, Minor: stayerMinor, Disk: stayerLoop, Address: stripPort(stayerAddress)},
		}

		// Determine roles: target takes the migrated role, stayer keeps its role
		var cleanupTargetRole, cleanupStayerRole string
		if migrationType == "primary" {
			cleanupTargetRole = "primary"
			cleanupStayerRole = "secondary"
		} else {
			cleanupTargetRole = "secondary"
			cleanupStayerRole = "primary"
		}

		tmr := machineStatuses[targetMachine]
		if tmr != nil && tmr.Online {
			client := NewMachineClient(targetAddress)
			targetReq := &shared.DRBDReconfigureRequest{Nodes: bipodNodes, Port: port, Force: false, Role: cleanupTargetRole}
			if _, err := client.DRBDReconfigure(op.UserID, targetReq); err != nil {
				targetReq.Force = true
				client.DRBDReconfigure(op.UserID, targetReq)
			}
		}

		stmr := machineStatuses[stayerMachine]
		if stmr != nil && stmr.Online {
			client := NewMachineClient(stayerAddress)
			stayerReq := &shared.DRBDReconfigureRequest{Nodes: bipodNodes, Port: port, Force: false, Role: cleanupStayerRole}
			if _, err := client.DRBDReconfigure(op.UserID, stayerReq); err != nil {
				stayerReq.Force = true
				client.DRBDReconfigure(op.UserID, stayerReq)
			}
		}

		coord.store.RemoveBipod(op.UserID, sourceMachine)
		if migrationType == "primary" {
			coord.store.SetBipodRole(op.UserID, targetMachine, "primary")
		} else {
			coord.store.SetBipodRole(op.UserID, targetMachine, "secondary")
		}

		coord.store.SetUserStatus(op.UserID, "running", "")
		_ = coord.store.CompleteOperation(op.OperationID)
		logger.Info("Migration recovery complete")
	}

	switch op.CurrentStep {
	case "migrate-target-selected":
		// Safe to cancel and restart
		cancelMigration("crashed at target-selected")

	case "migrate-image-created":
		// Image exists, DRBD not configured — cancel
		cancelMigration("crashed at image-created")

	case "migrate-drbd-added":
		// 3-node DRBD configured, may be syncing
		tmr := machineStatuses[targetMachine]
		if tmr == nil || !tmr.Online {
			cancelMigration("target offline during sync")
			return
		}
		// Cancel — let user retry migration manually
		cancelMigration("crashed during sync phase")

	case "migrate-synced":
		if migrationType == "secondary" {
			// Secondary migration — skip to cleanup
			completeCleanup()
		} else {
			// Primary migration — cancel (haven't stopped container yet, safe to revert)
			cancelMigration("crashed at synced")
		}

	case "migrate-container-stopped":
		// CRITICAL: User is DOWN. Must recover ASAP.
		tmr := machineStatuses[targetMachine]
		smr := machineStatuses[sourceMachine]

		if tmr != nil && tmr.Online {
			// Promote target, start container there, cleanup
			client := NewMachineClient(targetAddress)
			if _, err := client.DRBDPromote(op.UserID); err != nil {
				logger.Error("Recovery promote on target failed", "error", err)
				// Try source fallback
				if smr != nil && smr.Online {
					srcClient := NewMachineClient(sourceAddress)
					srcClient.DRBDPromote(op.UserID)
					srcClient.ContainerStart(op.UserID)
					cancelMigration("target promote failed, rolled back to source")
					return
				}
				coord.store.SetUserStatus(op.UserID, "unavailable", "migration recovery failed")
				coord.store.FailOperation(op.OperationID, "target promote failed during recovery")
				return
			}
			if _, err := client.ContainerStart(op.UserID); err != nil {
				logger.Error("Recovery container start on target failed", "error", err)
				coord.store.SetUserStatus(op.UserID, "unavailable", "migration recovery: container start failed")
				coord.store.FailOperation(op.OperationID, "container start failed during recovery")
				return
			}
			coord.store.SetUserPrimary(op.UserID, targetMachine)
			completeCleanup()
		} else if smr != nil && smr.Online {
			// Target offline, rollback to source
			srcClient := NewMachineClient(sourceAddress)
			srcClient.DRBDPromote(op.UserID)
			srcClient.ContainerStart(op.UserID)
			cancelMigration("target offline, rolled back to source")
		} else {
			coord.store.SetUserStatus(op.UserID, "unavailable", "both source and target offline during migration recovery")
			coord.store.FailOperation(op.OperationID, "both machines offline")
		}

	case "migrate-source-demoted":
		// Source demoted, need to promote target and start container
		tmr := machineStatuses[targetMachine]
		if tmr != nil && tmr.Online {
			client := NewMachineClient(targetAddress)
			if _, err := client.DRBDPromote(op.UserID); err != nil {
				logger.Error("Recovery promote on target failed at source-demoted", "error", err)
				coord.store.SetUserStatus(op.UserID, "unavailable", "migration recovery: target promote failed")
				coord.store.FailOperation(op.OperationID, "target promote failed during recovery")
				return
			}
			if _, err := client.ContainerStart(op.UserID); err != nil {
				logger.Error("Recovery container start on target failed at source-demoted", "error", err)
				coord.store.SetUserStatus(op.UserID, "unavailable", "migration recovery: container start failed")
				coord.store.FailOperation(op.OperationID, "container start failed during recovery")
				return
			}
			coord.store.SetUserPrimary(op.UserID, targetMachine)
			completeCleanup()
		} else {
			coord.store.SetUserStatus(op.UserID, "unavailable", "target offline after source demoted")
			coord.store.FailOperation(op.OperationID, "target offline after demote")
		}

	case "migrate-target-promoted":
		// Target promoted, need to start container
		tmr := machineStatuses[targetMachine]
		if tmr != nil && tmr.Online {
			client := NewMachineClient(targetAddress)
			client.ContainerStart(op.UserID)
			coord.store.SetUserPrimary(op.UserID, targetMachine)
			completeCleanup()
		} else {
			coord.store.SetUserStatus(op.UserID, "unavailable", "target offline after promote")
			coord.store.FailOperation(op.OperationID, "target offline after promote")
		}

	case "migrate-container-started":
		// Container running on target. Just finish cleanup.
		coord.store.SetUserPrimary(op.UserID, targetMachine)
		completeCleanup()

	case "migrate-source-cleaned", "migrate-secondary-cleaned":
		// Everything done, just mark complete
		coord.store.SetUserStatus(op.UserID, "running", "")
		_ = coord.store.CompleteOperation(op.OperationID)

	default:
		logger.Warn("Unknown migration step — cancelling", "step", op.CurrentStep)
		cancelMigration("unknown step: " + op.CurrentStep)
	}
}

// reconcilePhase3bCleanOrphans cleans up orphaned resources on machines.
func (coord *Coordinator) reconcilePhase3bCleanOrphans(logger *slog.Logger, orphans []orphanEntry) {
	if len(orphans) == 0 {
		return
	}

	logger.Info("[Phase 3b] Cleaning orphaned resources", "count", len(orphans))

	for _, o := range orphans {
		client := NewMachineClient(o.Address)
		if err := client.DeleteUser(o.UserID); err != nil {
			logger.Warn("[Phase 3b] Orphan cleanup failed", "user_id", o.UserID, "machine_id", o.MachineID, "error", err)
		} else {
			logger.Info("[Phase 3b] Cleaned orphan", "user_id", o.UserID, "machine_id", o.MachineID)
		}
		// Remove stale bipod entry if exists
		coord.store.RemoveBipod(o.UserID, o.MachineID)
	}
}

// reconcilePhase3cCleanStaleTripods cleans up leftover 3rd bipods from interrupted migrations.
// Running users should have exactly 2 non-stale bipods. If a migration was interrupted between
// creating the target bipod and completing cleanup, there may be 3.
func (coord *Coordinator) reconcilePhase3cCleanStaleTripods(logger *slog.Logger, machineStatuses map[string]*machineReality) {
	users := coord.store.AllUsers()
	for _, u := range users {
		if u.Status != "running" {
			continue
		}

		bipods := coord.store.GetBipods(u.UserID)
		var nonStale []*Bipod
		for _, b := range bipods {
			if b.Role != "stale" {
				nonStale = append(nonStale, b)
			}
		}

		if len(nonStale) <= 2 {
			continue
		}

		// Running user has >2 non-stale bipods — find and remove the extra(s)
		logger.Warn("[Phase 3c] Running user has extra bipods, cleaning up",
			"user_id", u.UserID, "bipod_count", len(nonStale))

		// Keep the bipod on the primary machine and one secondary; remove the rest
		kept := 0
		for _, b := range nonStale {
			if b.MachineID == u.PrimaryMachine {
				kept++
				continue // keep the primary
			}
			if kept < 2 {
				kept++
				continue // keep one secondary
			}
			// Extra bipod — clean up
			logger.Info("[Phase 3c] Removing extra bipod",
				"user_id", u.UserID, "machine_id", b.MachineID)
			mr := machineStatuses[b.MachineID]
			if mr != nil && mr.Online {
				client := NewMachineClient(mr.Address)
				client.DRBDDisconnect(u.UserID)
				client.DRBDDestroy(u.UserID)
				client.DeleteUser(u.UserID)
			}
			coord.store.RemoveBipod(u.UserID, b.MachineID)
		}
	}
}

// reconcilePhase4HandleOffline triggers failover for running users on dead machines.
func (coord *Coordinator) reconcilePhase4HandleOffline(logger *slog.Logger) {
	logger.Info("[Phase 4] Handling offline machines")

	machines := coord.store.AllMachines()
	for _, m := range machines {
		if m.Status != "dead" {
			continue
		}

		users := coord.store.GetUsersOnMachine(m.MachineID)
		for _, userID := range users {
			u := coord.store.GetUser(userID)
			if u == nil {
				continue
			}

			if u.Status == "running" || u.Status == "running_degraded" {
				logger.Info("[Phase 4] Triggering failover for user on dead machine",
					"user_id", userID, "machine_id", m.MachineID)
				coord.failoverUser(userID, m.MachineID)
			}
		}
	}
}

// reconcilePhase5EnsureContainers ensures every "running" user has a container on its primary machine.
// This is a safety net for cases where a migration crash left the user in "running" state without a container.
func (coord *Coordinator) reconcilePhase5EnsureContainers(logger *slog.Logger, machineStatuses map[string]*machineReality) {
	users := coord.store.AllUsers()
	for _, u := range users {
		if u.Status != "running" {
			continue
		}
		primary := u.PrimaryMachine
		if primary == "" {
			continue
		}
		mr := machineStatuses[primary]
		if mr == nil || !mr.Online {
			continue
		}

		// Check if the container is running on the primary machine
		userInfo, exists := mr.Users[u.UserID]
		if exists && userInfo.ContainerRunning {
			continue // container is running, nothing to do
		}

		// Container is NOT running on the primary — start it
		logger.Warn("[Phase 5] Running user missing container on primary, starting it",
			"user_id", u.UserID, "primary", primary)
		client := NewMachineClient(mr.Address)
		if _, err := client.ContainerStart(u.UserID); err != nil {
			logger.Error("[Phase 5] Failed to start container", "user_id", u.UserID, "error", err)
		}
	}
}


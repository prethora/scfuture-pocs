package coordinator

import (
	"log/slog"
	"strings"
	"time"

	"scfuture/internal/shared"
)

const (
	ReformerInterval    = 30 * time.Second
	StabilizationPeriod = 30 * time.Second // Wait after status change before reforming
	SyncTimeout         = 300 * time.Second // 5 minutes for initial sync
)

// StartReformer launches a background goroutine that periodically
// scans for degraded users and reforms their bipods.
func StartReformer(store *Store, coord *Coordinator) {
	go func() {
		slog.Info("[REFORMER] Reformer started",
			"interval", ReformerInterval.String(),
			"stabilization_period", StabilizationPeriod.String(),
		)

		ticker := time.NewTicker(ReformerInterval)
		defer ticker.Stop()
		for range ticker.C {
			coord.cleanStaleBipodsOnActiveMachines()
			coord.reformDegradedUsers()
		}
	}()
}

// cleanStaleBipodsOnActiveMachines cleans up stale bipods on machines that have
// returned to active status.
func (coord *Coordinator) cleanStaleBipodsOnActiveMachines() {
	staleBipods := coord.store.GetAllStaleBipodsOnActiveMachines()
	if len(staleBipods) == 0 {
		return
	}

	slog.Info("[REFORMER] Found stale bipods on active machines", "count", len(staleBipods))

	for _, stale := range staleBipods {
		logger := slog.With("component", "reformer", "user_id", stale.UserID, "machine_id", stale.MachineID)
		logger.Info("Cleaning up stale bipod on returned machine")

		staleMachine := coord.store.GetMachine(stale.MachineID)
		if staleMachine != nil {
			client := NewMachineClient(staleMachine.Address)
			if err := client.DRBDDestroy(stale.UserID); err != nil {
				logger.Warn("Stale DRBD destroy failed (non-fatal)", "error", err)
			}
			if err := client.DeleteUser(stale.UserID); err != nil {
				logger.Warn("Stale image delete failed (non-fatal)", "error", err)
			}
		}
		coord.store.RemoveBipod(stale.UserID, stale.MachineID)
		logger.Info("Stale bipod cleaned up")
	}
}

// reformDegradedUsers scans for users needing reformation and processes them.
func (coord *Coordinator) reformDegradedUsers() {
	degraded := coord.store.GetDegradedUsers(StabilizationPeriod)
	if len(degraded) == 0 {
		return
	}

	slog.Info("[REFORMER] Found degraded users", "count", len(degraded))

	for _, user := range degraded {
		coord.reformUser(user.UserID)
	}
}

// reformUser handles bipod reformation for a single degraded user.
func (coord *Coordinator) reformUser(userID string) {
	start := time.Now()
	logger := slog.With("component", "reformer", "user_id", userID)

	// Re-check status (may have changed since scan)
	user := coord.store.GetUser(userID)
	if user == nil || user.Status != "running_degraded" {
		logger.Info("Skipping — user not in running_degraded state")
		return
	}

	// ── Step 0: Clean up stale bipods on active machines ──
	staleBipods := coord.store.GetStaleBipodsOnActiveMachines(userID)
	for _, stale := range staleBipods {
		logger.Info("Cleaning up stale bipod on returned machine", "machine_id", stale.MachineID)
		staleMachine := coord.store.GetMachine(stale.MachineID)
		if staleMachine != nil {
			client := NewMachineClient(staleMachine.Address)
			if err := client.DRBDDestroy(userID); err != nil {
				logger.Warn("Stale DRBD destroy failed (non-fatal)", "error", err)
			}
			if err := client.DeleteUser(userID); err != nil {
				logger.Warn("Stale image delete failed (non-fatal)", "error", err)
			}
		}
		coord.store.RemoveBipod(userID, stale.MachineID)
		logger.Info("Stale bipod cleaned up", "machine_id", stale.MachineID)
	}

	// ── Step 1: Set user status → "reforming" ──
	coord.store.SetUserStatus(userID, "reforming", "")

	// ── Step 2: Select new secondary machine ──
	excludeIDs := []string{user.PrimaryMachine}
	newSecondary, err := coord.store.SelectOneSecondary(excludeIDs)
	if err != nil {
		logger.Warn("No available machine for reformation", "error", err)
		coord.store.SetUserStatus(userID, "running_degraded", "no machine available: "+err.Error())
		return
	}

	newMinor := coord.store.AllocateMinor(newSecondary.MachineID)

	// Get primary machine info
	primaryMachine := coord.store.GetMachine(user.PrimaryMachine)
	if primaryMachine == nil {
		logger.Error("Primary machine not found in store")
		coord.store.SetUserStatus(userID, "running_degraded", "primary machine not found")
		return
	}

	// Get primary bipod
	var primaryBipod *Bipod
	bipods := coord.store.GetBipods(userID)
	for _, b := range bipods {
		if b.MachineID == user.PrimaryMachine && b.Role != "stale" {
			primaryBipod = b
			break
		}
	}
	if primaryBipod == nil {
		logger.Error("Cannot find primary bipod")
		coord.store.SetUserStatus(userID, "running_degraded", "primary bipod not found")
		return
	}

	// Create operation
	opID := generateOpID()
	coord.store.CreateOperation(opID, "reformation", userID, map[string]interface{}{
		"primary_machine":   user.PrimaryMachine,
		"primary_address":   primaryMachine.Address,
		"new_secondary":     newSecondary.MachineID,
		"secondary_address": newSecondary.Address,
		"new_minor":         newMinor,
		"primary_minor":     primaryBipod.DRBDMinor,
		"primary_loop":      primaryBipod.LoopDevice,
		"port":              user.DRBDPort,
	})

	coord.store.CreateBipod(userID, newSecondary.MachineID, "secondary", newMinor)

	logger.Info("Selected new secondary",
		"new_secondary", newSecondary.MachineID,
		"minor", newMinor, "op_id", opID,
	)
	coord.step(opID, "reform-machine-selected")

	primaryClient := NewMachineClient(primaryMachine.Address)
	secondaryClient := NewMachineClient(newSecondary.Address)

	primaryAddr := reformerStripPort(primaryMachine.Address)
	secondaryAddr := reformerStripPort(newSecondary.Address)

	// ── Step 3: Create image on new secondary ──
	imgResp, err := secondaryClient.CreateImage(userID, user.ImageSizeMB)
	if err != nil {
		logger.Error("Create image on new secondary failed", "error", err)
		coord.store.SetUserStatus(userID, "running_degraded", "image create failed: "+err.Error())
		coord.store.FailOperation(opID, "image create: "+err.Error())
		coord.store.RecordReformationEvent(ReformationEvent{
			UserID: userID, OldSecondary: "", NewSecondary: newSecondary.MachineID,
			Success: false, Error: "image create: " + err.Error(),
			DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
		})
		return
	}
	secondaryLoop := imgResp.LoopDevice
	coord.store.SetBipodLoopDevice(userID, newSecondary.MachineID, secondaryLoop)
	logger.Info("Image created on new secondary", "loop", secondaryLoop)
	coord.step(opID, "reform-image-created")

	// Build DRBD config request
	drbdReq := &shared.DRBDCreateRequest{
		ResourceName: "user-" + userID,
		Nodes: []shared.DRBDNode{
			{
				Hostname: primaryMachine.MachineID,
				Minor:    primaryBipod.DRBDMinor,
				Disk:     primaryBipod.LoopDevice,
				Address:  primaryAddr,
			},
			{
				Hostname: newSecondary.MachineID,
				Minor:    newMinor,
				Disk:     secondaryLoop,
				Address:  secondaryAddr,
			},
		},
		Port: user.DRBDPort,
	}

	// ── Step 4: Configure DRBD on new secondary ──
	_, err = secondaryClient.DRBDCreate(userID, drbdReq)
	if err != nil {
		logger.Error("DRBD create on new secondary failed", "error", err)
		secondaryClient.DeleteUser(userID)
		coord.store.RemoveBipod(userID, newSecondary.MachineID)
		coord.store.SetUserStatus(userID, "running_degraded", "drbd create failed: "+err.Error())
		coord.store.FailOperation(opID, "drbd create: "+err.Error())
		coord.store.RecordReformationEvent(ReformationEvent{
			UserID: userID, OldSecondary: "", NewSecondary: newSecondary.MachineID,
			Success: false, Error: "drbd create: " + err.Error(),
			DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
		})
		return
	}
	logger.Info("DRBD configured on new secondary")
	coord.step(opID, "reform-drbd-configured")

	// ── Step 5: Disconnect dead peer on primary ──
	_, err = primaryClient.DRBDDisconnect(userID)
	if err != nil {
		logger.Warn("DRBD disconnect on primary failed (non-fatal)", "error", err)
	}
	logger.Info("Dead peer disconnected on primary")
	coord.step(opID, "reform-old-disconnected")

	// ── Step 6: Reconfigure DRBD on primary ──
	reconfigReq := &shared.DRBDReconfigureRequest{
		Nodes: drbdReq.Nodes,
		Port:  drbdReq.Port,
		Force: false,
		Role:  "primary",
	}

	var reconfigMethod string
	reconfigResp, err := primaryClient.DRBDReconfigure(userID, reconfigReq)
	if err != nil {
		logger.Warn("drbdadm adjust failed, attempting down/up fallback", "error", err)

		if stopErr := primaryClient.ContainerStop(userID); stopErr != nil {
			logger.Error("Container stop failed during fallback", "error", stopErr)
			secondaryClient.DRBDDestroy(userID)
			secondaryClient.DeleteUser(userID)
			coord.store.RemoveBipod(userID, newSecondary.MachineID)
			coord.store.SetUserStatus(userID, "running_degraded", "container stop failed during reconfigure: "+stopErr.Error())
			coord.store.FailOperation(opID, "container stop for fallback: "+stopErr.Error())
			coord.store.RecordReformationEvent(ReformationEvent{
				UserID: userID, OldSecondary: "", NewSecondary: newSecondary.MachineID,
				Success: false, Error: "container stop for fallback: " + stopErr.Error(),
				DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
			})
			return
		}

		reconfigReq.Force = true
		reconfigResp, err = primaryClient.DRBDReconfigure(userID, reconfigReq)
		if err != nil {
			logger.Error("Force reconfigure failed", "error", err)
			primaryClient.ContainerStart(userID)
			secondaryClient.DRBDDestroy(userID)
			secondaryClient.DeleteUser(userID)
			coord.store.RemoveBipod(userID, newSecondary.MachineID)
			coord.store.SetUserStatus(userID, "running_degraded", "drbd reconfigure (force) failed: "+err.Error())
			coord.store.FailOperation(opID, "drbd reconfigure force: "+err.Error())
			coord.store.RecordReformationEvent(ReformationEvent{
				UserID: userID, OldSecondary: "", NewSecondary: newSecondary.MachineID,
				Success: false, Error: "drbd reconfigure force: " + err.Error(),
				DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
			})
			return
		}

		if _, startErr := primaryClient.ContainerStart(userID); startErr != nil {
			logger.Error("Container restart after force reconfigure failed", "error", startErr)
			coord.store.SetUserStatus(userID, "running_degraded", "container restart after reconfigure failed: "+startErr.Error())
			coord.store.FailOperation(opID, "container restart: "+startErr.Error())
			coord.store.RecordReformationEvent(ReformationEvent{
				UserID: userID, OldSecondary: "", NewSecondary: newSecondary.MachineID,
				Success: false, Error: "container restart: " + startErr.Error(), Method: "down_up",
				DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
			})
			return
		}

		reconfigMethod = "down_up"
	} else {
		reconfigMethod = reconfigResp.Method
	}
	logger.Info("DRBD reconfigured on primary", "method", reconfigMethod)
	coord.step(opID, "reform-primary-reconfigured")

	// ── Step 7: Wait for DRBD sync ──
	syncStart := time.Now()
	for {
		if time.Since(syncStart) > SyncTimeout {
			logger.Warn("DRBD sync timeout — leaving bipod in place")
			coord.store.SetUserStatus(userID, "running_degraded", "drbd sync timeout after reformation")
			coord.store.FailOperation(opID, "sync timeout")
			coord.store.RecordReformationEvent(ReformationEvent{
				UserID: userID, OldSecondary: "", NewSecondary: newSecondary.MachineID,
				Success: false, Error: "sync timeout", Method: reconfigMethod,
				DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
			})
			return
		}

		status, err := primaryClient.DRBDStatus(userID)
		if err != nil {
			logger.Warn("DRBD status check failed", "error", err)
			time.Sleep(2 * time.Second)
			continue
		}

		if status.PeerDiskState == "UpToDate" {
			logger.Info("DRBD sync complete")
			break
		}

		progress := "unknown"
		if status.SyncProgress != nil {
			progress = *status.SyncProgress
		}
		logger.Info("DRBD syncing", "peer_disk", status.PeerDiskState, "progress", progress)
		time.Sleep(2 * time.Second)
	}
	coord.step(opID, "reform-synced")

	// ── Step 8: Update state ──
	coord.store.SetUserStatus(userID, "running", "")
	_ = coord.store.CompleteOperation(opID)

	coord.store.RecordReformationEvent(ReformationEvent{
		UserID:       userID,
		OldSecondary: "",
		NewSecondary: newSecondary.MachineID,
		Success:      true,
		Method:       reconfigMethod,
		DurationMS:   time.Since(start).Milliseconds(),
		Timestamp:    time.Now(),
	})

	logger.Info("Reformation complete — user has 2-copy replication again",
		"primary", user.PrimaryMachine,
		"new_secondary", newSecondary.MachineID,
		"method", reconfigMethod,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

func reformerStripPort(address string) string {
	idx := strings.LastIndex(address, ":")
	if idx == -1 {
		return address
	}
	return address[:idx]
}

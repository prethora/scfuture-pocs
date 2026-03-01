package coordinator

import (
	"log/slog"
	"time"
)

const (
	HealthCheckInterval = 10 * time.Second
	SuspectThreshold    = 30 * time.Second // 3 missed heartbeats
	DeadThreshold       = 60 * time.Second // 6 missed heartbeats
)

// StartHealthChecker launches a background goroutine that periodically
// checks machine health and triggers failover when machines die.
func StartHealthChecker(store *Store, client *MachineClient, coord *Coordinator) {
	go func() {
		slog.Info("[HEALTH] Health checker started",
			"interval", HealthCheckInterval.String(),
			"suspect_threshold", SuspectThreshold.String(),
			"dead_threshold", DeadThreshold.String(),
		)

		ticker := time.NewTicker(HealthCheckInterval)
		defer ticker.Stop()
		for range ticker.C {
			newlyDead := store.CheckMachineHealth(SuspectThreshold, DeadThreshold)
			for _, machineID := range newlyDead {
				go coord.failoverMachine(machineID)
			}
		}
	}()
}

// failoverMachine handles all users affected by a machine death.
func (coord *Coordinator) failoverMachine(deadMachineID string) {
	logger := slog.With("component", "failover", "dead_machine", deadMachineID)
	logger.Info("Starting failover for dead machine")

	userIDs := coord.store.GetUsersOnMachine(deadMachineID)
	logger.Info("Users affected", "count", len(userIDs))

	for _, userID := range userIDs {
		coord.failoverUser(userID, deadMachineID)
	}

	logger.Info("Failover complete for machine", "users_processed", len(userIDs))
}

// failoverUser handles failover for a single user when a machine dies.
func (coord *Coordinator) failoverUser(userID, deadMachineID string) {
	start := time.Now()
	logger := slog.With("component", "failover", "user_id", userID, "dead_machine", deadMachineID)

	user := coord.store.GetUser(userID)
	if user == nil {
		logger.Warn("User not found in store")
		return
	}

	// Handle suspended/evicted users — just mark bipod as stale, no failover needed
	if user.Status == "suspended" || user.Status == "evicted" {
		logger.Info("User is suspended/evicted — marking bipod stale only", "status", user.Status)
		coord.store.SetBipodRole(userID, deadMachineID, "stale")
		return
	}

	// Skip if user is not in a state that needs failover
	if user.Status != "running" && user.Status != "running_degraded" {
		logger.Info("Skipping user — not in running state", "status", user.Status)
		return
	}

	survivingBipod := coord.store.GetSurvivingBipod(userID, deadMachineID)
	if survivingBipod == nil {
		// Both machines dead — nothing we can do
		logger.Error("No surviving bipod — both machines dead")
		coord.store.SetUserStatus(userID, "unavailable", "both machines dead")
		coord.store.RecordFailoverEvent(FailoverEvent{
			UserID:      userID,
			FromMachine: deadMachineID,
			ToMachine:   "",
			Type:        "both_dead",
			Success:     false,
			Error:       "no surviving machine",
			DurationMS:  time.Since(start).Milliseconds(),
			Timestamp:   time.Now(),
		})
		return
	}

	survivingMachine := coord.store.GetMachine(survivingBipod.MachineID)
	if survivingMachine == nil {
		logger.Error("Surviving machine not found in store", "machine_id", survivingBipod.MachineID)
		return
	}

	// Case 1: Secondary died — primary is still running, user is degraded but OK
	if user.PrimaryMachine != deadMachineID {
		logger.Info("Secondary died — user continues on primary (degraded)")
		coord.store.SetBipodRole(userID, deadMachineID, "stale")
		coord.store.SetUserStatus(userID, "running_degraded", "secondary machine dead: "+deadMachineID)
		coord.store.RecordFailoverEvent(FailoverEvent{
			UserID:      userID,
			FromMachine: deadMachineID,
			ToMachine:   user.PrimaryMachine,
			Type:        "secondary_failed",
			Success:     true,
			DurationMS:  time.Since(start).Milliseconds(),
			Timestamp:   time.Now(),
		})
		return
	}

	// Case 2: Primary died — need to promote secondary
	logger.Info("Primary died — promoting surviving secondary",
		"surviving_machine", survivingBipod.MachineID,
	)

	opID := generateOpID()
	coord.store.CreateOperation(opID, "failover", userID, map[string]interface{}{
		"dead_machine":      deadMachineID,
		"surviving_machine": survivingBipod.MachineID,
		"surviving_address": survivingMachine.Address,
	})

	coord.store.SetUserStatus(userID, "failing_over", "")
	coord.store.SetBipodRole(userID, deadMachineID, "stale")

	coord.step(opID, "failover-detected")

	client := NewMachineClient(survivingMachine.Address)

	// Step 1: Promote DRBD on surviving machine
	_, err := client.DRBDPromote(userID)
	if err != nil {
		logger.Error("DRBD promote failed", "error", err)
		coord.store.SetUserStatus(userID, "unavailable", "drbd promote failed: "+err.Error())
		coord.store.FailOperation(opID, "drbd promote: "+err.Error())
		coord.store.RecordFailoverEvent(FailoverEvent{
			UserID:      userID,
			FromMachine: deadMachineID,
			ToMachine:   survivingBipod.MachineID,
			Type:        "primary_failed",
			Success:     false,
			Error:       "drbd promote: " + err.Error(),
			DurationMS:  time.Since(start).Milliseconds(),
			Timestamp:   time.Now(),
		})
		return
	}
	logger.Info("DRBD promoted on surviving machine")
	coord.step(opID, "failover-promoted")

	// Step 2: Start container on surviving machine
	_, err = client.ContainerStart(userID)
	if err != nil {
		logger.Error("Container start failed after DRBD promote", "error", err)
		coord.store.SetBipodRole(userID, survivingBipod.MachineID, "primary")
		coord.store.SetUserPrimary(userID, survivingBipod.MachineID)
		coord.store.SetUserStatus(userID, "running_degraded", "container start failed after promote: "+err.Error())
		coord.store.FailOperation(opID, "container start: "+err.Error())
		coord.store.RecordFailoverEvent(FailoverEvent{
			UserID:      userID,
			FromMachine: deadMachineID,
			ToMachine:   survivingBipod.MachineID,
			Type:        "primary_failed",
			Success:     false,
			Error:       "container start: " + err.Error(),
			DurationMS:  time.Since(start).Milliseconds(),
			Timestamp:   time.Now(),
		})
		return
	}
	logger.Info("Container started on surviving machine")
	coord.step(opID, "failover-container-started")

	// Step 3: Update state — user is running but degraded (only 1 copy)
	coord.store.SetBipodRole(userID, survivingBipod.MachineID, "primary")
	coord.store.SetUserPrimary(userID, survivingBipod.MachineID)
	coord.store.SetUserStatus(userID, "running_degraded", "primary failed over from "+deadMachineID)
	coord.store.CompleteOperation(opID)

	coord.store.RecordFailoverEvent(FailoverEvent{
		UserID:      userID,
		FromMachine: deadMachineID,
		ToMachine:   survivingBipod.MachineID,
		Type:        "primary_failed",
		Success:     true,
		DurationMS:  time.Since(start).Milliseconds(),
		Timestamp:   time.Now(),
	})

	logger.Info("Failover complete — user running on new primary",
		"new_primary", survivingBipod.MachineID,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

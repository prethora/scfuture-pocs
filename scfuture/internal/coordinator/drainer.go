package coordinator

import (
	"log/slog"
	"time"
)

// DrainMachine sequentially migrates all running users off a machine.
// Runs as a goroutine, launched by handleDrainMachine or reconciler.
// Exits when: all users migrated, machine status changes from "draining", or no target available.
func (coord *Coordinator) DrainMachine(machineID string) {
	logger := slog.With("component", "drain", "machine_id", machineID)
	logger.Info("Drain goroutine started")
	drainStart := time.Now()

	migratedCount := 0
	skippedCount := 0

	coord.store.RecordDrainEvent(machineID, "started", map[string]interface{}{
		"users_on_machine": len(coord.store.GetUsersOnMachine(machineID)),
	})

	defer func() {
		logger.Info("Drain goroutine exiting",
			"migrated", migratedCount,
			"skipped", skippedCount,
		)
	}()

	for {
		// 1. Check machine is still draining
		machine := coord.store.GetMachine(machineID)
		if machine == nil || machine.Status != "draining" {
			logger.Info("Machine no longer draining, stopping drain")
			coord.store.RecordDrainEvent(machineID, "cancelled", map[string]interface{}{
				"migrated":    migratedCount,
				"skipped":     skippedCount,
				"duration_ms": time.Since(drainStart).Milliseconds(),
			})
			return
		}

		// 2. Find next running user with non-stale bipod on this machine
		userIDs := coord.store.GetUsersOnMachine(machineID)
		var nextUserID string
		var nextUserRole string

		for _, uid := range userIDs {
			u := coord.store.GetUser(uid)
			if u == nil || u.Status != "running" {
				continue
			}
			bipods := coord.store.GetBipods(uid)
			for _, b := range bipods {
				if b.MachineID == machineID && b.Role != "stale" {
					nextUserID = uid
					nextUserRole = b.Role
					break
				}
			}
			if nextUserID != "" {
				break
			}
		}

		// 3. No more migratable users — drain is complete
		if nextUserID == "" {
			logger.Info("All users migrated off machine, drain complete")
			coord.store.RecordDrainEvent(machineID, "completed", map[string]interface{}{
				"migrated":    migratedCount,
				"skipped":     skippedCount,
				"duration_ms": time.Since(drainStart).Milliseconds(),
			})
			return
		}

		// 4. Find target machine
		bipods := coord.store.GetBipods(nextUserID)
		var excludeIDs []string
		for _, b := range bipods {
			if b.Role != "stale" {
				excludeIDs = append(excludeIDs, b.MachineID)
			}
		}

		target, err := coord.store.SelectOneSecondary(excludeIDs)
		if err != nil {
			logger.Warn("No target available for drain migration, waiting 30s",
				"user", nextUserID, "error", err)
			time.Sleep(30 * time.Second)
			continue
		}

		// 5. Execute migration synchronously
		logger.Info("Drain: migrating user",
			"user", nextUserID,
			"role", nextUserRole,
			"target", target.MachineID,
		)

		coord.store.SetUserStatus(nextUserID, "migrating", "")
		coord.MigrateUser(nextUserID, machineID, target.MachineID, "drain")

		// 6. Check result
		u := coord.store.GetUser(nextUserID)
		if u != nil && u.Status == "running" {
			migratedCount++
			logger.Info("Drain: user migrated successfully", "user", nextUserID)
		} else {
			skippedCount++
			status := ""
			if u != nil {
				status = u.Status
			}
			logger.Warn("Drain: user did not return to running after migration",
				"user", nextUserID,
				"status", status,
			)
		}
	}
}

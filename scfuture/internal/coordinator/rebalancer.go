package coordinator

import (
	"log/slog"
	"os"
	"sort"
	"strconv"
	"time"
)

var (
	RebalanceInterval   = 60 * time.Second
	RebalanceThreshold  = 2
	RebalanceStabilizationPeriod = 5 * time.Minute
)

func init() {
	if v := os.Getenv("REBALANCE_INTERVAL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			RebalanceInterval = time.Duration(n) * time.Second
		}
	}
	if v := os.Getenv("REBALANCE_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			RebalanceThreshold = n
		}
	}
	if v := os.Getenv("REBALANCE_STABILIZATION_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			RebalanceStabilizationPeriod = time.Duration(n) * time.Second
		}
	}
}

// StartRebalancer launches a background goroutine that periodically evaluates
// fleet balance and triggers migrations to equalize agent distribution.
func StartRebalancer(store *Store, coord *Coordinator) {
	go func() {
		slog.Info("[REBALANCER] Rebalancer started",
			"interval", RebalanceInterval.String(),
			"threshold", RebalanceThreshold,
			"stabilization_period", RebalanceStabilizationPeriod.String(),
		)

		ticker := time.NewTicker(RebalanceInterval)
		defer ticker.Stop()
		for range ticker.C {
			coord.rebalanceTick()
		}
	}()
}

func (coord *Coordinator) rebalanceTick() {
	logger := slog.With("component", "rebalancer")

	// Precondition 0: Cooldown — wait for heartbeats to propagate after last migration
	if !coord.lastRebalanceMigration.IsZero() && time.Since(coord.lastRebalanceMigration) < RebalanceStabilizationPeriod {
		return // cooling down after last migration
	}

	// Precondition 1: No migration or provisioning in progress
	if coord.store.CountUsersByStatus("migrating") > 0 {
		return // migration in progress, skip this tick
	}
	if coord.store.CountUsersByStatus("provisioning") > 0 {
		return // provisioning in progress, skip this tick
	}

	// Precondition 2: No machine drain in progress
	if len(coord.store.GetDrainingMachines()) > 0 {
		return // drain active, skip this tick
	}

	// Get active non-draining machines
	machines := coord.store.GetActiveNonDrainingMachines()
	if len(machines) < 3 {
		return // need at least 3 machines to meaningfully rebalance
	}

	// Compute average density
	totalAgents := 0
	for _, m := range machines {
		totalAgents += m.ActiveAgents
	}
	avgDensity := float64(totalAgents) / float64(len(machines))

	// Find overloaded machines (sorted by most overloaded first)
	type overloaded struct {
		machine *Machine
		excess  int
	}
	var overloadedMachines []overloaded
	for _, m := range machines {
		excess := m.ActiveAgents - int(avgDensity) - RebalanceThreshold
		if excess > 0 {
			overloadedMachines = append(overloadedMachines, overloaded{m, excess})
		}
	}
	if len(overloadedMachines) == 0 {
		return // fleet is balanced
	}

	sort.Slice(overloadedMachines, func(i, j int) bool {
		return overloadedMachines[i].excess > overloadedMachines[j].excess
	})

	// Try to migrate one user from the most overloaded machine
	for _, ol := range overloadedMachines {
		sourceMachineID := ol.machine.MachineID

		// Get migratable users on this machine
		users := coord.store.GetMigratableUsersOnMachine(sourceMachineID, RebalanceStabilizationPeriod)
		if len(users) == 0 {
			continue // all users on this machine are ineligible
		}

		// Partition: prefer users whose SECONDARY is here (zero-downtime migration)
		var secondaryHere, primaryHere []*User
		for _, u := range users {
			role := coord.store.GetUserBipodRoleOnMachine(u.UserID, sourceMachineID)
			if role == "secondary" {
				secondaryHere = append(secondaryHere, u)
			} else {
				primaryHere = append(primaryHere, u)
			}
		}

		// Sort each list by ImageSizeMB ascending (smallest first = fastest sync)
		sortBySize := func(list []*User) {
			sort.Slice(list, func(i, j int) bool {
				return list[i].ImageSizeMB < list[j].ImageSizeMB
			})
		}
		sortBySize(secondaryHere)
		sortBySize(primaryHere)

		// Try secondary-here first, then primary-here
		candidates := append(secondaryHere, primaryHere...)

		for _, candidate := range candidates {
			// Find target: exclude machines in user's bipod
			bipods := coord.store.GetBipods(candidate.UserID)
			var excludeIDs []string
			for _, b := range bipods {
				if b.Role != "stale" {
					excludeIDs = append(excludeIDs, b.MachineID)
				}
			}

			target, err := coord.store.SelectOneSecondary(excludeIDs)
			if err != nil {
				logger.Warn("[REBALANCER] No target available", "user", candidate.UserID, "error", err)
				continue
			}

			// Anti-thrashing: only migrate if target would have fewer agents than source after move
			if target.ActiveAgents+1 >= ol.machine.ActiveAgents-1 {
				continue // migration would not improve balance
			}

			// Trigger migration
			logger.Info("[REBALANCER] Triggering migration",
				"user", candidate.UserID,
				"source", sourceMachineID,
				"target", target.MachineID,
				"source_agents", ol.machine.ActiveAgents,
				"avg_density", avgDensity,
			)

			coord.store.RecordRebalancerEvent("trigger", map[string]interface{}{
				"user_id":       candidate.UserID,
				"source":        sourceMachineID,
				"target":        target.MachineID,
				"source_agents": ol.machine.ActiveAgents,
				"target_agents": target.ActiveAgents,
				"avg_density":   avgDensity,
			})

			coord.store.SetUserStatus(candidate.UserID, "migrating", "")
			coord.lastRebalanceMigration = time.Now()
			go coord.MigrateUser(candidate.UserID, sourceMachineID, target.MachineID, "rebalancer")
			return // one migration per tick
		}
	}
}

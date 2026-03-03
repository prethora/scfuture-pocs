# Layer 5.2 Build Prompt — Rebalancer & Machine Drain

## What This Is

This is a build prompt for Layer 5.2 of the scfuture distributed agent platform. You are Claude Code. Your job is to:

1. Read all referenced existing files to understand the current codebase
2. Write all new code and modifications described below
3. Report back when code is written
4. When told "yes" / "ready", run the full test lifecycle (infra up → deploy → test → iterate on failures → teardown)
5. When all tests pass, update `SESSION.md` (in the parent directory) with what happened
6. Give a final report

The project lives in `scfuture/` (a subdirectory of the current working directory). All Go code paths are relative to `scfuture/`. All script paths are relative to `scfuture/`. The `SESSION.md` file is in the parent directory (current working directory).

---

## Context: What Exists

Layers 4.1–5.1 are complete and committed. Read these files first to understand the existing codebase:

### Existing code (read-only reference — do NOT rewrite these unless this prompt tells you to):

```
scfuture/go.mod
scfuture/Makefile
scfuture/cmd/machine-agent/main.go
scfuture/cmd/coordinator/main.go
scfuture/internal/shared/types.go
scfuture/internal/machineagent/server.go
scfuture/internal/machineagent/images.go
scfuture/internal/machineagent/drbd.go
scfuture/internal/machineagent/btrfs.go
scfuture/internal/machineagent/containers.go
scfuture/internal/machineagent/state.go
scfuture/internal/machineagent/cleanup.go
scfuture/internal/machineagent/exec.go
scfuture/internal/machineagent/heartbeat.go
scfuture/internal/machineagent/backup.go
scfuture/internal/coordinator/server.go
scfuture/internal/coordinator/store.go
scfuture/internal/coordinator/fleet.go
scfuture/internal/coordinator/provisioner.go
scfuture/internal/coordinator/machineapi.go
scfuture/internal/coordinator/healthcheck.go
scfuture/internal/coordinator/reformer.go
scfuture/internal/coordinator/lifecycle.go
scfuture/internal/coordinator/retention.go
scfuture/internal/coordinator/reconciler.go
scfuture/internal/coordinator/migrator.go
scfuture/container/Dockerfile
scfuture/container/container-init.sh
```

Read ALL of these before writing any code. Pay close attention to:
- How `healthcheck.go` uses `StartHealthChecker` — a goroutine with a `time.NewTicker` that ticks every 10 seconds, calls `store.CheckMachineHealth()`, and launches per-machine failover goroutines. **The rebalancer follows this exact pattern** but with a 60-second tick.
- How `store.go`'s `CheckMachineHealth` computes new status from heartbeat timing and **will overwrite `"draining"` back to `"active"`** if heartbeat is fresh. This must be fixed — `CheckMachineHealth` must preserve `"draining"` status when the machine is alive.
- How `store.go`'s `SelectMachines()` and `SelectOneSecondary()` filter by `Status == "active"`. Since draining machines have `Status = "draining"`, they are **already excluded** from new placements. No change needed to selection logic itself.
- How `migrator.go`'s `MigrateUser(userID, sourceMachineID, targetMachineID)` drives a multi-step migration with `step()` checkpoints and `fail()`/`retry()` helpers. The rebalancer and drain both call this function directly.
- How `server.go`'s `handleMigrateUser` sets `status = "migrating"` and launches `go coord.MigrateUser(...)`. The drain goroutine sets status and calls `MigrateUser` directly (not via goroutine) for sequential execution.
- How `reconciler.go`'s `Reconcile()` runs 6 phases at startup. A new phase is needed to resume drains.
- How `cmd/coordinator/main.go` launches background goroutines after reconciliation: `StartHealthChecker`, `StartReformer`, `StartRetentionEnforcer`. The rebalancer is added here as `StartRebalancer`.
- How `store.go`'s `Machine` struct has `ActiveAgents`, `DiskTotalMB`, `DiskUsedMB` — the metrics the rebalancer uses.
- How `store.go`'s `User` struct has `StatusChangedAt` — used for stabilization period (recently migrated users are skipped by the rebalancer).
- How `store.go`'s `GetUsersOnMachine(machineID)` returns `[]string` (user IDs). A new method is needed that returns full `*User` objects with filtering for migratable users.

### Reference documents (in parent directory):

```
SESSION.md
architecture-v3.md
```

Read the "Layer 5.2 — Rebalancer & Machine Drain" section in `SESSION.md` for the design rationale.

---

## What Layer 5.2 Builds

**What this layer proves:** The platform can automatically rebalance user workloads across the fleet and safely drain machines for maintenance, building on the manual live migration primitive from Layer 5.1.

**In scope:**
- **Rebalancer goroutine** — periodic evaluation of fleet balance, automatic migration of users from overloaded to underloaded machines.
- **Machine drain API** — `POST /api/fleet/{machine_id}/drain` and `POST /api/fleet/{machine_id}/undrain` for planned machine evacuation.
- **Drain orchestration** — sequential migration of all users off a draining machine.
- **Integration with existing systems** — healthcheck preserves draining status, selection logic excludes draining machines, reconciler resumes drains.
- **Full test suite** — rebalancer behavior, drain happy path, drain cancellation, edge cases, crash recovery.

**Explicitly NOT in scope (future layers):**
- HA coordinator (active-passive pair)
- Encryption (LUKS layer)
- Agent process management
- Marketplace

---

## Architecture

### Test Topology

Same as Layer 5.1: 1 coordinator + 3 fleet machines on Hetzner Cloud. Additionally requires:
- Supabase Postgres connection string (same as Layer 5.1)
- B2 credentials (same as Layer 5.1 — not exercised by rebalancer/drain tests, but machine agent binary expects them)

```
macOS (test harness, runs test_suite.sh via SSH / curl / psql)
  │
  ├── l52-coordinator (CX23, coordinator :8080, private 10.0.0.2)
  │     │
  │     ├── l52-fleet-1 (CX23, machine-agent :8080, private 10.0.0.11)
  │     ├── l52-fleet-2 (CX23, machine-agent :8080, private 10.0.0.12)
  │     └── l52-fleet-3 (CX23, machine-agent :8080, private 10.0.0.13)
  │
  Private network: l52-net / 10.0.0.0/24

Supabase Postgres:
  Env var: DATABASE_URL (required, set by user)

Backblaze B2:
  Env vars: B2_KEY_ID, B2_APP_KEY (required)
  Bucket: l52-test-{random} (created/destroyed by run.sh)
```

### Why 3 Fleet Machines Suffice

A user's bipod occupies 2 of 3 machines. The 3rd machine is always the migration target. With 3 machines, we can demonstrate rebalancing by creating artificial imbalance and observing migrations. We can also drain any one machine since 2 machines remain for bipod placement.

### Rebalancer Algorithm

The rebalancer is a periodic goroutine that evaluates fleet balance every 60 seconds (configurable). It detects imbalance using agent density (users per machine) and triggers migrations to equalize load.

**Tick cycle:**

```
rebalanceTick():
  1. Precondition check — skip if:
     - Any user is in "migrating" status (one migration at a time globally)
     - Any machine has status "draining" (drain handles its own migrations)

  2. Get all active, non-draining machines with their ActiveAgents counts

  3. Compute average density: avgDensity = totalAgents / activeMachines
     - If fewer than 3 active non-draining machines, skip (can't rebalance)

  4. Identify overloaded machines: ActiveAgents > avgDensity + threshold
     - Sort by most overloaded first

  5. For the most overloaded machine:
     a. Get migratable users on that machine:
        - Status = "running"
        - StatusChangedAt older than stabilization period
        - Has non-stale bipod on this machine
     b. Partition into two lists:
        - secondary-here: users whose SECONDARY bipod is on this machine (zero-downtime migration)
        - primary-here: users whose PRIMARY bipod is on this machine (~5-15s downtime)
     c. Pick candidate: prefer secondary-here (smallest ImageSizeMB first), then primary-here
     d. Find target: active non-draining machine with lowest ActiveAgents, not in user's bipod, disk < 85%
     e. If candidate and target found: trigger migration
        - Set user status to "migrating"
        - Launch go coord.MigrateUser(userID, sourceMachineID, targetMachineID)
        - Record rebalance trigger in migration event
     f. Return (at most one migration per tick)

  6. No overloaded machine found → no action
```

**Key constants (env-var configurable for testing):**

| Constant | Default | Test Value | Env Var |
|----------|---------|------------|---------|
| `RebalanceInterval` | 60s | 10s | `REBALANCE_INTERVAL_SECONDS` |
| `RebalanceThreshold` | 2 | 1 | `REBALANCE_THRESHOLD` |
| `StabilizationPeriod` | 5m | 15s | `REBALANCE_STABILIZATION_SECONDS` |

**One migration per tick rationale:** After triggering one migration, the rebalancer returns and waits for the next tick. On the next tick, it re-evaluates — the fleet may now be balanced, or a different machine may now be overloaded. This prevents thundering herd and naturally paces migrations to ~1 per minute.

### Machine Drain Protocol

Drain is an imperative command: "evacuate all users from this machine." It's used for planned maintenance — take a machine offline for hardware upgrades, OS updates, etc.

**Drain goroutine:**

```
DrainMachine(machineID):
  logger := slog.With("component", "drain", "machine_id", machineID)
  logger.Info("Starting drain")

  for {
    // 1. Check machine still draining
    machine := store.GetMachine(machineID)
    if machine == nil || machine.Status != "draining" {
      logger.Info("Machine no longer draining, stopping")
      return
    }

    // 2. Find next user to migrate
    userIDs := store.GetUsersOnMachine(machineID)
    var nextUser *User
    for _, uid := range userIDs {
      u := store.GetUser(uid)
      if u != nil && u.Status == "running" {
        // Check user has non-stale bipod on this machine
        bipods := store.GetBipods(uid)
        for _, b := range bipods {
          if b.MachineID == machineID && b.Role != "stale" {
            nextUser = u
            break
          }
        }
        if nextUser != nil { break }
      }
    }

    // 3. No more migratable users — drain complete
    if nextUser == nil {
      logger.Info("No more users to migrate, drain complete")
      // Don't change machine status — leave as "draining" so operator knows
      // it's safe to take offline. Operator can undrain to return to active.
      return
    }

    // 4. Determine source and target
    userID := nextUser.UserID
    bipods := store.GetBipods(userID)
    var sourceBipod *Bipod
    var excludeIDs []string
    for _, b := range bipods {
      if b.MachineID == machineID && b.Role != "stale" {
        sourceBipod = b
      }
      if b.Role != "stale" {
        excludeIDs = append(excludeIDs, b.MachineID)
      }
    }

    target, err := store.SelectOneDrainTarget(excludeIDs)
    if err != nil {
      logger.Warn("No target available for migration, will retry next iteration", "user", userID, "error", err)
      time.Sleep(30 * time.Second)  // Wait and retry — fleet may free up
      continue
    }

    // 5. Execute migration synchronously
    logger.Info("Migrating user", "user", userID, "target", target.MachineID, "role", sourceBipod.Role)
    store.SetUserStatus(userID, "migrating", "")
    coord.MigrateUser(userID, machineID, target.MachineID)

    // 6. Verify user recovered
    u := store.GetUser(userID)
    if u.Status != "running" {
      logger.Warn("User did not return to running after drain migration", "user", userID, "status", u.Status)
      // Continue to next user — don't block drain on one failure
    }
  }
}
```

**`SelectOneDrainTarget` vs `SelectOneSecondary`:** The drain target selection is similar to `SelectOneSecondary` but does NOT increment `ActiveAgents` (the migration itself will handle counter updates). Use a separate method or modify the existing one. Actually, the migration flow handles counter management, so drain target selection should NOT increment. Create `SelectOneDrainTarget(excludeIDs)` that filters and sorts like `SelectOneSecondary` but skips the `ActiveAgents++` step.

Wait — re-examining `MigrateUser`, it does NOT use `SelectOneSecondary` — it receives the target machine ID directly. The `ActiveAgents` counter is not incremented during manual migration either. So `SelectOneDrainTarget` should work the same way: find the best target, return it, let `MigrateUser` handle the rest. Don't increment counters — `MigrateUser` manages bipod creation which is the real tracking.

Simplify: just use `SelectOneSecondary` for drain target selection — the `ActiveAgents++` is a pessimistic reservation that prevents over-placement, which is still useful during drain. This matches the provisioner pattern.

### Interaction with CheckMachineHealth

**Critical fix:** `CheckMachineHealth` currently overwrites any status to `"active"`, `"suspect"`, or `"dead"` based purely on heartbeat timing. This would reset `"draining"` back to `"active"` every 10 seconds.

Fix: When computing the new status, if the machine is currently `"draining"` and the heartbeat is fresh (would normally set `"active"`), preserve `"draining"`. If the heartbeat is stale enough for `"suspect"` or `"dead"`, transition normally — the machine has died during drain.

```go
// In CheckMachineHealth, after computing newStatus:
if m.Status == "draining" && newStatus == "active" {
    newStatus = "draining" // preserve drain status for healthy draining machines
}
```

This is a 2-line change in `store.go`.

### Interaction with Other Operations

| Scenario | Behavior |
|----------|----------|
| Rebalancer tick + user already migrating | Skip entire tick |
| Rebalancer tick + any machine draining | Skip entire tick |
| Rebalancer tick + user in failover/reformation | User not "running" → skipped by candidate selection |
| Rebalancer tick + fewer than 3 active non-draining machines | Skip (can't meaningfully rebalance) |
| Drain + machine dies mid-drain | Healthcheck marks machine dead, triggers failover for affected users. Drain goroutine's next iteration sees machine not "draining" (it's "dead") and exits. |
| Drain + user suspended during drain | Drain goroutine checks user status — suspended user skipped |
| Undrain during active migration | Machine status → "active". Current in-progress migration completes. Drain goroutine sees status != "draining" and exits. |
| Two simultaneous drains | Allowed. Each drain goroutine runs independently. Rebalancer skips when any drain is active. |
| Drain with no users on machine | Drain goroutine finds no migratable users → exits immediately |
| Provision during drain | SelectMachines filters by `Status == "active"` — draining machine excluded from new placements |
| Rebalancer selects user on draining machine | Won't happen — rebalancer skips its tick when any drain is active |

### Reconciliation Updates

The reconciler runs at coordinator startup. Two additions:

**1. Resume drains:** After all other reconciliation phases, check for machines with `status = "draining"` that are online. Re-launch the drain goroutine for each.

```go
// New: Phase 6b — Resume drains for online draining machines
for _, machine := range allMachines {
    if machine.Status == "draining" {
        mr := machineStatuses[machine.MachineID]
        if mr != nil && mr.Online {
            logger.Info("Resuming drain for machine", "machine_id", machine.MachineID)
            go coord.DrainMachine(machine.MachineID)
        } else {
            logger.Warn("Draining machine is offline, transitioning to dead", "machine_id", machine.MachineID)
            coord.store.SetMachineStatus(machine.MachineID, "dead")
        }
    }
}
```

**2. No new operation type needed.** Individual drain migrations use the existing `live_migration` operation type. The drain itself is tracked only via `machine.status = "draining"`.

### No New Crash Checkpoints

The rebalancer and drain don't introduce new checkpoint IDs because:
1. **Rebalancer** is stateless — evaluates on each tick, triggers `MigrateUser` which has its own checkpoints (F34-F42).
2. **Drain** is tracked via `machine.status = "draining"`. Individual migrations use existing checkpoints. The drain goroutine is re-launched from reconciliation.

Crash recovery scenario:
1. Coordinator crashes while drain is active with migration in progress
2. On restart, reconciler sees machine `status = "draining"` → will re-launch drain goroutine
3. Reconciler sees user in `"migrating"` with incomplete `live_migration` operation → `resumeMigration` handles it
4. After reconciliation, drain goroutine starts, scans remaining users, continues

### No Schema Changes

The existing Postgres schema supports everything needed:
- `machines.status` is TEXT — can hold `"draining"` without schema change
- `events.event_type` is TEXT — can hold `"rebalance"` for rebalancer evaluation events
- `operations.type` is TEXT — individual drain migrations use `"live_migration"`
- Migration events already have `method` field — extend with `trigger` field via the JSONB `details` column

### Address Conventions

Same as Layer 5.1:
- Coordinator private: `10.0.0.2:8080`
- Fleet private: `10.0.0.11:8080`, `10.0.0.12:8080`, `10.0.0.13:8080`
- Machine agents register with their private IP as `NODE_ADDRESS`
- Coordinator calls machine agents via their registered address (private IP)
- Test harness calls coordinator and machine agents via public IPs

---

## Modifications to Existing Code

### 1. `internal/coordinator/store.go` — Add drain/rebalancer methods, fix CheckMachineHealth

**1a. Fix `CheckMachineHealth` — preserve draining status:**

In the `CheckMachineHealth` method, after computing `newStatus` from the heartbeat timing, add:

```go
// Preserve "draining" status for machines with healthy heartbeats
if m.Status == "draining" && newStatus == "active" {
    newStatus = "draining"
}
```

This goes inside the `for id, m := range s.machines` loop, after the `switch` block that sets `newStatus` and before the `if newStatus != m.Status` check.

**1b. Add `MigrationEvent.Trigger` field:**

Modify the existing `MigrationEvent` struct to add a trigger field:

```go
type MigrationEvent struct {
	UserID        string    `json:"user_id"`
	SourceMachine string    `json:"source_machine"`
	TargetMachine string    `json:"target_machine"`
	MigrationType string    `json:"migration_type"` // "primary" or "secondary"
	Success       bool      `json:"success"`
	Error         string    `json:"error,omitempty"`
	Method        string    `json:"method,omitempty"` // "adjust" or "down_up"
	Trigger       string    `json:"trigger,omitempty"` // NEW: "manual", "rebalancer", "drain"
	DurationMS    int64     `json:"duration_ms"`
	Timestamp     time.Time `json:"timestamp"`
}
```

Update `RecordMigrationEvent` to include `trigger` in the details map:

```go
func (s *Store) RecordMigrationEvent(event MigrationEvent) {
	details := map[string]interface{}{
		"source_machine": event.SourceMachine,
		"target_machine": event.TargetMachine,
		"migration_type": event.MigrationType,
		"success":        event.Success,
		"error":          event.Error,
		"method":         event.Method,
		"trigger":        event.Trigger, // NEW
		"duration_ms":    event.DurationMS,
	}
	s.RecordEvent("migration", event.SourceMachine, event.UserID, "", details)
}
```

Update `GetMigrationEvents` to parse the `trigger` field from details:

```go
if v, ok := details["trigger"].(string); ok { me.Trigger = v }
```

**1c. Add `GetMigratableUsersOnMachine` — returns users eligible for rebalancer migration:**

```go
func (s *Store) GetMigratableUsersOnMachine(machineID string, stabilizationPeriod time.Duration) []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cutoff := time.Now().Add(-stabilizationPeriod)
	seen := make(map[string]bool)
	var result []*User

	for _, b := range s.bipods {
		if b.MachineID != machineID || b.Role == "stale" || seen[b.UserID] {
			continue
		}
		seen[b.UserID] = true
		u := s.users[b.UserID]
		if u == nil || u.Status != "running" {
			continue
		}
		if u.StatusChangedAt.After(cutoff) {
			continue // recently migrated, skip (stabilization period)
		}
		uCopy := *u
		result = append(result, &uCopy)
	}
	return result
}
```

**1d. Add `GetUserBipodRoleOnMachine` — returns the bipod role a user has on a specific machine:**

```go
func (s *Store) GetUserBipodRoleOnMachine(userID, machineID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := userID + ":" + machineID
	if b, ok := s.bipods[key]; ok && b.Role != "stale" {
		return b.Role
	}
	return ""
}
```

**1e. Add `CountUsersByStatus` — count users in a given status:**

```go
func (s *Store) CountUsersByStatus(status string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for _, u := range s.users {
		if u.Status == status {
			count++
		}
	}
	return count
}
```

**1f. Add `GetDrainingMachines` — returns all machines with status "draining":**

```go
func (s *Store) GetDrainingMachines() []*Machine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*Machine
	for _, m := range s.machines {
		if m.Status == "draining" {
			mCopy := *m
			result = append(result, &mCopy)
		}
	}
	return result
}
```

**1g. Add `GetActiveNonDrainingMachines` — returns active machines not in drain:**

```go
func (s *Store) GetActiveNonDrainingMachines() []*Machine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*Machine
	for _, m := range s.machines {
		if m.Status == "active" {
			mCopy := *m
			result = append(result, &mCopy)
		}
	}
	return result
}
```

### 2. `internal/coordinator/server.go` — Add drain/undrain routes

**Add to `RegisterRoutes`:**

```go
// Machine drain (Layer 5.2)
mux.HandleFunc("POST /api/fleet/{machine_id}/drain", coord.handleDrainMachine)
mux.HandleFunc("POST /api/fleet/{machine_id}/undrain", coord.handleUndrainMachine)
```

**Add handlers:**

```go
func (coord *Coordinator) handleDrainMachine(w http.ResponseWriter, r *http.Request) {
	machineID := r.PathValue("machine_id")

	machine := coord.store.GetMachine(machineID)
	if machine == nil {
		writeError(w, http.StatusNotFound, "machine not found")
		return
	}
	if machine.Status == "draining" {
		writeError(w, http.StatusConflict, "machine is already draining")
		return
	}
	if machine.Status != "active" {
		writeError(w, http.StatusConflict, "machine must be active to drain (current: "+machine.Status+")")
		return
	}

	// Check minimum fleet size — need at least 2 other active machines for bipod placement
	activeMachines := coord.store.GetActiveNonDrainingMachines()
	otherActive := 0
	for _, m := range activeMachines {
		if m.MachineID != machineID {
			otherActive++
		}
	}
	if otherActive < 2 {
		writeError(w, http.StatusConflict, "cannot drain: need at least 2 other active machines for bipod placement")
		return
	}

	// Count users to migrate
	userIDs := coord.store.GetUsersOnMachine(machineID)
	runningCount := 0
	for _, uid := range userIDs {
		u := coord.store.GetUser(uid)
		if u != nil && u.Status == "running" {
			runningCount++
		}
	}

	coord.store.SetMachineStatus(machineID, "draining")
	go coord.DrainMachine(machineID)

	slog.Info("Drain started", "machine_id", machineID, "users_to_migrate", runningCount)
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"machine_id":       machineID,
		"status":           "draining",
		"users_to_migrate": runningCount,
	})
}

func (coord *Coordinator) handleUndrainMachine(w http.ResponseWriter, r *http.Request) {
	machineID := r.PathValue("machine_id")

	machine := coord.store.GetMachine(machineID)
	if machine == nil {
		writeError(w, http.StatusNotFound, "machine not found")
		return
	}
	if machine.Status != "draining" {
		writeError(w, http.StatusConflict, "machine is not draining (current: "+machine.Status+")")
		return
	}

	coord.store.SetMachineStatus(machineID, "active")

	slog.Info("Undrain: machine returned to active", "machine_id", machineID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"machine_id": machineID,
		"status":     "active",
	})
}
```

### 3. `internal/coordinator/reconciler.go` — Resume drains after restart

**At the end of the `Reconcile()` function, before the "Reconciliation complete" log, add a new phase:**

```go
// Phase 6b: Resume drains for online draining machines
coord.reconcilePhase6bResumeDrains(logger, machineStatuses)
```

**Add the new phase function:**

```go
func (coord *Coordinator) reconcilePhase6bResumeDrains(logger *slog.Logger, machineStatuses map[string]*machineReality) {
	drainingMachines := coord.store.GetDrainingMachines()
	for _, machine := range drainingMachines {
		mr := machineStatuses[machine.MachineID]
		if mr != nil && mr.Online {
			logger.Info("[RECONCILE] Resuming drain for online draining machine", "machine_id", machine.MachineID)
			go coord.DrainMachine(machine.MachineID)
		} else {
			logger.Warn("[RECONCILE] Draining machine is offline, marking dead", "machine_id", machine.MachineID)
			coord.store.SetMachineStatus(machine.MachineID, "dead")
		}
	}
}
```

### 4. `internal/coordinator/migrator.go` — Add trigger parameter

The `MigrateUser` function needs to know who triggered the migration so it can record it in the event. Add a `trigger` parameter:

**Change the function signature:**

```go
// Before:
func (coord *Coordinator) MigrateUser(userID, sourceMachineID, targetMachineID string)

// After:
func (coord *Coordinator) MigrateUser(userID, sourceMachineID, targetMachineID, trigger string)
```

**Update the MigrationEvent recording (both success and failure paths) to include the trigger:**

```go
coord.store.RecordMigrationEvent(MigrationEvent{
    // ... existing fields ...
    Trigger: trigger,
})
```

**Update ALL existing callers of MigrateUser:**

1. In `server.go` `handleMigrateUser`: change `go coord.MigrateUser(userID, req.SourceMachine, req.TargetMachine)` to `go coord.MigrateUser(userID, req.SourceMachine, req.TargetMachine, "manual")`
2. In `reconciler.go` `resumeMigration`: any calls to `coord.MigrateUser` should pass `"manual"` (reconciliation resumes the original trigger context, but "manual" is safe as a default)

### 5. `cmd/coordinator/main.go` — Launch rebalancer goroutine

**Add rebalancer env var parsing and launch after the other goroutines:**

```go
// ── Step 4: Start background goroutines ──
ctx, cancel := context.WithCancel(context.Background())
coord.SetCancelFunc(cancel)

coordinator.StartHealthChecker(coord.GetStore(), coordinator.NewMachineClient(""), coord)
coordinator.StartReformer(coord.GetStore(), coord)
coordinator.StartRetentionEnforcer(coord.GetStore(), coord)
coordinator.StartRebalancer(coord.GetStore(), coord) // NEW: Layer 5.2
```

Read rebalancer config from env vars in `StartRebalancer` (see new file section below).

---

## New Files

### 6. `internal/coordinator/rebalancer.go` — Rebalancer goroutine

Create a new file `scfuture/internal/coordinator/rebalancer.go`.

```go
package coordinator

import (
	"log/slog"
	"os"
	"sort"
	"strconv"
	"time"
)

var (
	RebalanceInterval      = 60 * time.Second
	RebalanceThreshold     = 2
	StabilizationPeriod    = 5 * time.Minute
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
			StabilizationPeriod = time.Duration(n) * time.Second
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
			"stabilization_period", StabilizationPeriod.String(),
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

	// Precondition 1: No migration already in progress
	if coord.store.CountUsersByStatus("migrating") > 0 {
		return // migration in progress, skip this tick
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
		users := coord.store.GetMigratableUsersOnMachine(sourceMachineID, StabilizationPeriod)
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

			// Trigger migration
			logger.Info("[REBALANCER] Triggering migration",
				"user", candidate.UserID,
				"source", sourceMachineID,
				"target", target.MachineID,
				"source_agents", ol.machine.ActiveAgents,
				"avg_density", avgDensity,
			)

			coord.store.SetUserStatus(candidate.UserID, "migrating", "")
			go coord.MigrateUser(candidate.UserID, sourceMachineID, target.MachineID, "rebalancer")
			return // one migration per tick
		}
	}
}
```

### 7. `internal/coordinator/drainer.go` — Machine drain orchestration

Create a new file `scfuture/internal/coordinator/drainer.go`.

```go
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

	migratedCount := 0
	skippedCount := 0

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
			logger.Warn("Drain: user did not return to running after migration",
				"user", nextUserID,
				"status", u.Status,
			)
		}
	}
}
```

---

## Test Suite

### Test Harness Scripts

Create `scfuture/scripts/layer-5.2/` with the following files. Base all scripts on the Layer 5.1 equivalents, changing:
- Prefix: `l51-` → `l52-`
- Network name: `l51-net` → `l52-net`
- SSH key name: `l51-key` → `l52-key`
- Layer description in banners

#### `scripts/layer-5.2/run.sh`

Same structure as Layer 5.1 `run.sh`:
1. Verify required env vars (`DATABASE_URL`, `B2_KEY_ID`, `B2_APP_KEY`)
2. Reset database tables
3. Create B2 bucket (named `l52-test-{random}`)
4. Build binaries (`make build`)
5. Create infrastructure (`./infra.sh up`)
6. Deploy (`./deploy.sh`)
7. Run test suite (`./test_suite.sh`)
8. Teardown infrastructure
9. Delete B2 bucket and clean database

#### `scripts/layer-5.2/common.sh`

Copy from Layer 5.1. Change all `l51-` references to `l52-`. In `check_consistency()`:
- No new invariants needed (rebalancer/drain don't introduce new data model invariants)
- Existing invariant 11 already includes `migrating` in transient states

**Add helper function for waiting for machine drain to complete:**

```bash
wait_for_machine_empty() {
    local machine_id=$1
    local timeout=${2:-300}
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local users_on=$(coord_api GET /api/fleet | jq -r --arg m "$machine_id" '.[] | select(.machine_id == $m) | .active_agents')
        # Also check bipods directly
        local bipod_count=$(db_query "SELECT COUNT(*) FROM bipods WHERE machine_id='$machine_id' AND role != 'stale'" 2>/dev/null | tr -d ' ')
        if [ "$bipod_count" = "0" ] 2>/dev/null; then
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    return 1
}

wait_for_machine_status() {
    local machine_id=$1
    local status=$2
    local timeout=${3:-120}
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local current=$(coord_api GET /api/fleet | jq -r --arg m "$machine_id" '.[] | select(.machine_id == $m) | .status')
        if [ "$current" = "$status" ]; then
            return 0
        fi
        sleep 2
        elapsed=$((elapsed + 2))
    done
    return 1
}
```

#### `scripts/layer-5.2/deploy.sh`

Copy from Layer 5.1. Change `l51-` to `l52-` everywhere. **Add rebalancer env vars to coordinator systemd config:**

```bash
# In the coordinator systemd override section, add:
Environment=REBALANCE_INTERVAL_SECONDS=10
Environment=REBALANCE_THRESHOLD=1
Environment=REBALANCE_STABILIZATION_SECONDS=15
```

These aggressive values ensure the test suite doesn't have to wait minutes for the rebalancer to act.

#### `scripts/layer-5.2/infra.sh`

Copy from Layer 5.1. Change `l51-` to `l52-` everywhere.

#### `scripts/layer-5.2/cloud-init/coordinator.yaml`

Identical to Layer 5.1.

#### `scripts/layer-5.2/cloud-init/fleet.yaml`

Identical to Layer 5.1.

### Test Suite Structure — `scripts/layer-5.2/test_suite.sh`

```
═══ Layer 5.2: Rebalancer & Machine Drain — Test Suite ═══

Phase 0: Prerequisites (~9 checks)
  - Coordinator responding
  - 3 fleet machines registered and active
  - Machine agents responding
  - Postgres connected, schema exists, advisory lock held
  - Verify rebalancer env vars are active (check coordinator logs or just proceed)

Phase 1: Baseline — Provision Users (~8 checks)
  - Create and provision 6 users: "rb-user-1" through "rb-user-6" (128MB images for fast sync)
  - Wait for all 6 to reach "running"
  - Record which machines each user is on
  - Verify: users distributed across 3 machines (natural load balancing from SelectMachines)
  - Verify: all 6 users have 2 non-stale bipods each
  - Run consistency check

Phase 2: Create Imbalance & Verify Rebalancer (~12 checks)
  - Manually migrate users to create imbalance:
    - Migrate rb-user-1's primary to fleet-1 (if not already there)
    - Migrate rb-user-2's primary to fleet-1
    - Migrate rb-user-3's primary to fleet-1
    - Migrate rb-user-4's primary to fleet-1
    Goal: fleet-1 has 4+ primaries, fleet-2 and fleet-3 have ~1 each
  - Wait for all manual migrations to complete
  - Record fleet distribution BEFORE rebalancer acts
  - Wait for rebalancer to trigger (up to 90 seconds — rebalancer tick is 10s in test, but
    needs time for migration + stabilization cooldown to pass for next)
  - Verify: at least one migration was triggered by rebalancer
    (check migration events with trigger="rebalancer")
  - Verify: fleet distribution is more balanced than before
  - Verify: all users are running
  - Verify: all user data intact
  - Run consistency check

Phase 3: Rebalancer Stability & Edge Cases (~8 checks)
  - Wait 30 seconds (3 rebalancer ticks with test interval)
  - If fleet is balanced (no machine exceeds threshold): verify no new rebalancer migrations triggered
  - Suspend one user → verify rebalancer doesn't try to migrate suspended user
  - Reactivate the suspended user → verify user returns to running
  - Verify: rebalancer respects stabilization period
    (recently migrated users should NOT be immediately re-migrated)
  - Verify: total migration events count is reasonable (no thundering herd)

Phase 4: Machine Drain — Happy Path (~12 checks)
  - Ensure at least 2 running users have a bipod on fleet-3
    (migrate if needed so fleet-3 has users to drain)
  - Record users on fleet-3 before drain
  - POST /api/fleet/fleet-3/drain → 202
  - Verify: fleet-3 status is "draining"
  - Wait for fleet-3 to be empty (all users migrated off, up to 300s)
  - Verify: no non-stale bipods on fleet-3
  - Verify: all previously-on-fleet-3 users are running
  - Verify: data intact for all drained users
  - Verify: new provision during drain does NOT place on fleet-3
    (create and provision "drain-test-user", verify not placed on fleet-3)
  - Verify: migration events with trigger="drain" recorded
  - Run consistency check
  - Undrain fleet-3: POST /api/fleet/fleet-3/undrain → 200
  - Verify: fleet-3 status is "active"

Phase 5: Drain Cancellation (~8 checks)
  - Ensure fleet-2 has at least 3 running users (provision or migrate if needed)
  - POST /api/fleet/fleet-2/drain → 202
  - Wait for first user to be migrated off fleet-2 (poll until user count decreases by 1)
  - POST /api/fleet/fleet-2/undrain → 200
  - Wait for any in-progress migration to complete (wait_for_operations_settled)
  - Verify: fleet-2 status is "active"
  - Verify: remaining users are still on fleet-2 and running
  - Verify: already-migrated user is running on new machine
  - Run consistency check

Phase 6: Drain & Rebalancer Edge Cases (~8 checks)
  - Drain machine with no users on it:
    - Ensure fleet-3 is empty (or use fleet-3 from Phase 4 which was drained)
    - POST /api/fleet/fleet-3/drain → 202 (or maybe should return immediately since empty)
    - Verify: drain completes immediately
    - Undrain fleet-3
  - Drain non-existent machine → 404
  - Drain already-draining machine → 409
  - Undrain non-draining machine → 409
  - Drain when only 2 other active machines remain (skip if fleet too small to test):
    - If testable: drain fleet-1, verify fleet-2 and fleet-3 can still serve all users
    - Undrain fleet-1
  - Verify: rebalancer skips ticks while drain is active
    (check no rebalancer-triggered migrations during drain — compare event timestamps)

Phase 7: Crash Recovery (~6 checks)
  - Ensure at least 2 users on fleet-1
  - Start drain on fleet-1: POST /api/fleet/fleet-1/drain
  - Wait for first migration to complete (1 user moved off)
  - Crash coordinator (restart systemd, or set FAIL_AT on next migration if users remain)
  - Restart coordinator without FAIL_AT
  - Wait for reconciliation + drain resumption
  - Verify: drain continues — remaining users eventually migrate off fleet-1
  - Verify: all users running
  - Verify: consistency check passes
  - Undrain fleet-1

Phase 8: Post-Test Verification (~4 checks)
  - Provision a new user, verify it works normally
  - Migrate the new user manually, verify it works
  - Verify: all events properly recorded (manual, rebalancer, drain triggers present)
  - Show summary (total users, operations, events by type)

Phase 9: Final Consistency & Cleanup (~2 checks)
  - Final consistency check
  - Verify no stuck operations
```

**Estimated total: ~77 checks.**

### Crash Test Implementation Pattern

The crash test in Phase 7 is simpler than Layer 5.1's checkpoint-by-checkpoint testing because the drain itself has no checkpoints — it's just `machine.status = "draining"` + sequential `MigrateUser` calls. The crash test verifies:

1. Machine stays "draining" across coordinator restart (persisted in Postgres)
2. Reconciler re-launches drain goroutine for online draining machines
3. In-progress migrations are recovered by `resumeMigration`
4. Remaining users are drained after recovery

```bash
# Phase 7: Crash during drain
# Ensure users on fleet-1
# ... (ensure or provision users)

# Start drain
coord_api POST /api/fleet/fleet-1/drain > /dev/null 2>&1

# Wait for at least one user to migrate off
sleep 15  # Give time for first migration

# Verify drain in progress
check "Fleet-1 is draining" '
    status=$(coord_api GET /api/fleet | jq -r ".[] | select(.machine_id==\"fleet-1\") | .status")
    [ "$status" = "draining" ]
'

# Restart coordinator (simulates crash)
ssh_cmd "$COORD_PUB_IP" "systemctl restart coordinator"
wait_for_coordinator 60

# Wait for drain to complete
wait_for_machine_empty "fleet-1" 300

check "All users running after drain crash recovery" '
    stuck=$(db_query "SELECT COUNT(*) FROM users WHERE status NOT IN ('"'"'running'"'"', '"'"'registered'"'"', '"'"'suspended'"'"', '"'"'evicted'"'"')")
    [ "$stuck" = "0" ]
'

check_consistency "After drain crash recovery"

# Undrain
coord_api POST /api/fleet/fleet-1/undrain > /dev/null 2>&1
```

---

## Implementation Notes

### Order of Implementation

1. **Store methods** (`store.go`) — fix `CheckMachineHealth`, add `Trigger` to `MigrationEvent`, add query methods
2. **Migrator update** (`migrator.go`) — add `trigger` parameter, update callers
3. **Rebalancer** (`rebalancer.go`) — new file, rebalancer goroutine
4. **Drainer** (`drainer.go`) — new file, drain orchestration
5. **Server routes** (`server.go`) — add drain/undrain endpoints
6. **Reconciler** (`reconciler.go`) — add Phase 6b drain resumption
7. **Main** (`cmd/coordinator/main.go`) — launch rebalancer goroutine
8. **Test scripts** (`scripts/layer-5.2/`) — full test infrastructure

### Key Risk: Rebalancer Stability

The biggest risk is the rebalancer creating instability — endlessly moving users back and forth. Mitigations:
- **Stabilization period** prevents re-migrating recently moved users (15s in test, 5m in production)
- **One migration per tick** prevents thundering herd
- **Threshold** prevents migration for minor differences (must exceed avg + threshold)
- **Skip during drain** prevents rebalancer and drain from interfering

The test suite validates stability in Phase 3 — after the fleet is balanced, no further migrations should be triggered.

### No Schema Changes

All new state fits the existing schema:
- `machines.status` TEXT — holds `"draining"` without change
- `events.details` JSONB — `trigger` field added to migration event details
- Individual drain migrations use existing `live_migration` operation type

### Backward Compatibility

All existing operations (provision, failover, reformation, suspension, reactivation, eviction, manual migration) continue to work unchanged. The only change to existing code:
- `MigrateUser` gains a 4th parameter (`trigger` string) — all existing callers updated
- `CheckMachineHealth` adds 2 lines to preserve `"draining"` status
- `MigrationEvent` gains `Trigger` field — `omitempty`, backward compatible
- Reconciler gains a new phase at the end — doesn't affect existing phases

# Layer 5.3 Build Prompt — Production Hardening: Concurrency, Convergence & Observability

## What This Is

This is a build prompt for Layer 5.3 of the scfuture distributed agent platform. You are Claude Code. Your job is to:

1. Read all referenced existing files to understand the current codebase
2. Write all new code and modifications described below
3. Report back when code is written
4. When told "yes" / "ready", run the full test lifecycle (infra up → deploy → test → iterate on failures → teardown)
5. When all tests pass, update `SESSION.md` (in the parent directory) with what happened
6. Give a final report

The project lives in `scfuture/` (a subdirectory of the current working directory). All Go code paths are relative to `scfuture/`. All script paths are relative to `scfuture/`. The `SESSION.md` file is in the parent directory (current working directory).

---

## Context: What Exists

Layers 4.1–5.2 are complete and committed. Read these files first to understand the existing codebase:

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
scfuture/internal/coordinator/rebalancer.go
scfuture/internal/coordinator/drainer.go
scfuture/container/Dockerfile
scfuture/container/container-init.sh
```

Read ALL of these before writing any code. Pay close attention to:

- **`server.go` lines 357-373**: `handleSuspendUser`, `handleReactivateUser`, `handleEvictUser` currently launch goroutines via `go coord.suspendUser(...)` and immediately return 202. These must become synchronous with lock acquisition, returning 200/409/500.
- **`server.go` line 517**: `handleMigrateUser` launches `go coord.MigrateUser(...)` without any lock. The lock must be acquired BEFORE the goroutine is launched.
- **`lifecycle.go` line 12**: `suspendUser`, `reactivateUser`, `evictUser` return nothing and silently drop operations on wrong-state users. These must return errors.
- **`migrator.go` line 17**: `MigrateUser` has no lock. The lock is managed by the caller, not inside MigrateUser.
- **`healthcheck.go` lines 43-45**: `failoverUser` is called in a loop with no lock. It checks `user.Status == "migrating"` (line 69) and marks bipod stale for migrating users — this is correct. For running users, it must acquire the lock.
- **`rebalancer.go` line 53**: `rebalanceTick` has no lock checks and uses a threshold formula that doesn't guarantee convergence. The entire tick function must be rewritten with spread-based convergence and lock integration.
- **`drainer.go` line 101-102**: `DrainMachine` calls `MigrateUser` synchronously with no lock. Must acquire lock before each user migration.
- **`provisioner.go` line 14**: `ProvisionUser` has no lock. Must acquire at entry.
- **`reconciler.go`**: All phases that touch individual users must acquire the lock.
- **`server.go` lines 21-30**: `Coordinator` struct has no lock manager, no boot time, no health state.
- **`server.go` lines 792-920**: `handleSystemConsistency` has 9 checks. Two new checks must be added (stuck lock detection, lock-count consistency).

### Reference documents (in parent directory):

```
SESSION.md
architecture-v3.md
```

Read the "Layer 5.3 — Production Hardening" section in `SESSION.md` for the original problem analysis.

---

## What Layer 5.3 Builds

**What this layer proves:** The coordinator is safe under concurrent operations, the rebalancer converges and stabilizes, lifecycle APIs give clear feedback, the system is observable, and the test suite is comprehensive enough to be a credible proof of production readiness.

**In scope:**

1. **Per-user operation lock** — in-memory non-blocking lock preventing concurrent mutations. Every user-mutating operation acquires the lock. Returns 409 on contention. Lock time tracking for stuck detection.
2. **Synchronous lifecycle APIs** — suspend, reactivate, evict return 200/409/500 instead of fire-and-forget 202.
3. **Rebalancer convergence** — spread-based algorithm with hysteresis, convergence proof in tests.
4. **Undrain cooldown** — rebalancer skips recently-undrained machines as targets.
5. **Event query defaults** — default `since` filter to coordinator boot time.
6. **Observability endpoints** — coordinator health, rebalancer state, lock status in user API.
7. **Enhanced consistency checker** — stuck lock detection, lock-count validation.
8. **Comprehensive test suite** — concurrent scenarios, failover during migration, convergence proof, consistency checker validation.

**Already fixed (do NOT re-implement):**
- Fail handler target bipod cleanup — `migrator.go` lines 54-68 already clean up target bipod, DRBD, and image on failure.

**Explicitly NOT in scope (future layers):**
- HA coordinator (active-passive pair) — lock is in-memory, single coordinator
- Encryption (LUKS layer)
- Agent process management
- Marketplace
- Graceful shutdown / context cancellation — the reconciler already handles all crash recovery cases reliably

---

## Architecture

### Test Topology

Same as Layer 5.2: 1 coordinator + 3 fleet machines on Hetzner Cloud.

```
macOS (test harness, runs test_suite.sh via SSH / curl / psql)
  │
  ├── l53-coordinator (CX23, coordinator :8080, private 10.0.0.2)
  │     │
  │     ├── l53-fleet-1 (CX23, machine-agent :8080, private 10.0.0.11)
  │     ├── l53-fleet-2 (CX23, machine-agent :8080, private 10.0.0.12)
  │     └── l53-fleet-3 (CX23, machine-agent :8080, private 10.0.0.13)
  │
  Private network: l53-net / 10.0.0.0/24

Supabase Postgres:
  Env var: DATABASE_URL (required, set by user)

Backblaze B2:
  Env vars: B2_KEY_ID, B2_APP_KEY (required)
  Bucket: l53-test-{random} (created/destroyed by run.sh)
```

### Address Conventions

Same as Layers 5.1/5.2:
- Coordinator private: `10.0.0.2:8080`
- Fleet private: `10.0.0.11:8080`, `10.0.0.12:8080`, `10.0.0.13:8080`
- Machine agents register with their private IP as `NODE_ADDRESS`
- Coordinator calls machine agents via their registered address (private IP)
- Test harness calls coordinator and machine agents via public IPs

### No Schema Changes

All changes are in-memory Go code. No Postgres schema changes needed.

---

## Change 1: Per-User Operation Lock (CRITICAL)

### New File: `internal/coordinator/userlock.go`

```go
package coordinator

import (
	"sync"
	"time"
)

// UserLockManager provides per-user non-blocking mutual exclusion.
// Prevents concurrent operations (migrate, suspend, failover, etc.) on the same user.
type UserLockManager struct {
	locks     sync.Map // map[string]*sync.Mutex
	lockTimes sync.Map // map[string]time.Time — when the lock was acquired
}

func NewUserLockManager() *UserLockManager {
	return &UserLockManager{}
}

// TryLock attempts to acquire the lock for a user. Returns true if acquired.
// Non-blocking: returns false immediately if the lock is held by another operation.
func (m *UserLockManager) TryLock(userID string) bool {
	mu, _ := m.locks.LoadOrStore(userID, &sync.Mutex{})
	acquired := mu.(*sync.Mutex).TryLock()
	if acquired {
		m.lockTimes.Store(userID, time.Now())
	}
	return acquired
}

// Unlock releases the lock for a user. Must only be called after a successful TryLock.
func (m *UserLockManager) Unlock(userID string) {
	m.lockTimes.Delete(userID)
	mu, ok := m.locks.Load(userID)
	if ok {
		mu.(*sync.Mutex).Unlock()
	}
}

// IsLocked returns true if the user is currently locked by an operation.
func (m *UserLockManager) IsLocked(userID string) bool {
	mu, ok := m.locks.Load(userID)
	if !ok {
		return false
	}
	if mu.(*sync.Mutex).TryLock() {
		mu.(*sync.Mutex).Unlock()
		return false
	}
	return true
}

// LockedSince returns the time the lock was acquired, or zero if not locked.
func (m *UserLockManager) LockedSince(userID string) time.Time {
	if t, ok := m.lockTimes.Load(userID); ok {
		return t.(time.Time)
	}
	return time.Time{}
}

// ActiveLocks returns the count and details of currently held locks.
func (m *UserLockManager) ActiveLocks() (int, []LockedUser) {
	count := 0
	var locked []LockedUser
	m.lockTimes.Range(func(key, value interface{}) bool {
		count++
		locked = append(locked, LockedUser{
			UserID:   key.(string),
			LockedAt: value.(time.Time),
		})
		return true
	})
	return count, locked
}

type LockedUser struct {
	UserID   string    `json:"user_id"`
	LockedAt time.Time `json:"locked_at"`
}
```

### Lock Integration — Every Caller

The lock pattern is always non-blocking: acquire or back off. The lock is NEVER acquired inside `MigrateUser`, `suspendUser`, `reactivateUser`, or `evictUser` themselves — it is always the **caller's** responsibility.

**`handleMigrateUser` (server.go)** — Acquire before launching goroutine:
```go
if !coord.userLocks.TryLock(userID) {
    writeError(w, http.StatusConflict, "operation already in progress for this user")
    return
}
coord.store.SetUserStatus(userID, "migrating", "")
go func() {
    defer coord.userLocks.Unlock(userID)
    coord.MigrateUser(userID, req.SourceMachine, req.TargetMachine, "manual")
}()
writeJSON(w, http.StatusAccepted, ...)
```

**`handleSuspendUser` (server.go)** — Synchronous with lock:
```go
if !coord.userLocks.TryLock(userID) {
    writeError(w, http.StatusConflict, "operation already in progress for this user")
    return
}
defer coord.userLocks.Unlock(userID)
if err := coord.suspendUser(userID); err != nil {
    writeError(w, http.StatusConflict, err.Error())
    return
}
writeJSON(w, http.StatusOK, map[string]string{"status": "suspended", "user_id": userID})
```

Same pattern for `handleReactivateUser` (returns `"running"`) and `handleEvictUser` (returns `"evicted"`).

**`handleProvisionUser` (server.go)** — Acquire before launching goroutine:
```go
if !coord.userLocks.TryLock(userID) {
    writeError(w, http.StatusConflict, "operation already in progress for this user")
    return
}
coord.store.SetUserStatus(userID, "provisioning", "")
go func() {
    defer coord.userLocks.Unlock(userID)
    coord.ProvisionUser(userID)
}()
```

**Rebalancer (rebalancer.go)** — TryLock before triggering migration:
```go
if !coord.userLocks.TryLock(candidate.UserID) {
    continue // user is locked, try next candidate
}
coord.store.SetUserStatus(candidate.UserID, "migrating", "")
coord.lastRebalanceMigration = time.Now()
go func() {
    defer coord.userLocks.Unlock(candidate.UserID)
    coord.MigrateUser(candidate.UserID, sourceMachineID, target.MachineID, "rebalancer")
}()
return
```

**Drainer (drainer.go)** — TryLock before each user migration (synchronous):
```go
if !coord.userLocks.TryLock(nextUserID) {
    logger.Info("User locked, will retry", "user", nextUserID)
    time.Sleep(5 * time.Second)
    continue
}
coord.store.SetUserStatus(nextUserID, "migrating", "")
coord.MigrateUser(nextUserID, machineID, target.MachineID, "drain")
coord.userLocks.Unlock(nextUserID)
```

**Failover (healthcheck.go)** — TryLock in `failoverUser`, with special handling:
```go
func (coord *Coordinator) failoverUser(userID, deadMachineID string) {
    // ...
    // For migrating users, just mark bipod stale (existing behavior, no lock needed)
    if user.Status == "migrating" {
        coord.store.SetBipodRole(userID, deadMachineID, "stale")
        return
    }

    // For running/degraded users, acquire lock
    if user.Status == "running" || user.Status == "running_degraded" {
        if !coord.userLocks.TryLock(userID) {
            logger.Warn("User locked, will retry on next healthcheck tick", "user_id", userID)
            return
        }
        defer coord.userLocks.Unlock(userID)
        // ... proceed with failover ...
    }
    // ...
}
```

**Reconciler (reconciler.go)** — TryLock in per-user phases:
```go
// In Phase 3 (resume operations), Phase 3c (clean tripods), Phase 4 (offline), Phase 5 (ensure containers):
if !coord.userLocks.TryLock(userID) {
    logger.Info("[RECONCILE] User locked, skipping", "user_id", userID)
    continue
}
// ... process user ...
coord.userLocks.Unlock(userID)
```

Note: The reconciler runs at startup before background goroutines start, so lock contention during reconciliation is rare. But the lock is still needed because Phase 6b launches drain goroutines that may process users concurrently with later reconciler phases.

---

## Change 2: Synchronous Lifecycle APIs

### Changes to `lifecycle.go`

Change all three function signatures to return error:

```go
func (coord *Coordinator) suspendUser(userID string) error
func (coord *Coordinator) reactivateUser(userID string) error
func (coord *Coordinator) evictUser(userID string) error
```

Replace every silent return with an error return:
```go
// Before:
if user.Status != "running" && user.Status != "running_degraded" {
    logger.Warn("Cannot suspend — invalid status", "status", user.Status)
    return
}

// After:
if user.Status != "running" && user.Status != "running_degraded" {
    return fmt.Errorf("cannot suspend: user is in %q state (must be running or running_degraded)", user.Status)
}
```

Apply to ALL early-return paths. The happy path returns `nil`.

**Do NOT add lock acquisition inside these functions** — the caller (HTTP handler) holds the lock.

### Changes to `server.go` — Handlers

Replace `handleSuspendUser`, `handleReactivateUser`, `handleEvictUser` with the synchronous + lock pattern described in Change 1.

Also add user-not-found checks before attempting the lock:
```go
user := coord.store.GetUser(userID)
if user == nil {
    writeError(w, http.StatusNotFound, "user not found")
    return
}
```

---

## Change 3: Rebalancer Convergence

### Replace `rebalanceTick` with Spread-Based Algorithm

The current algorithm's problems:
1. `excess = ActiveAgents - int(avgDensity) - RebalanceThreshold` uses integer truncation
2. No guarantee that a migration improves the global balance
3. `lastRebalanceMigration` compares against `StabilizationPeriod` (too long for inter-migration pacing)

**New algorithm: spread-based with convergence guarantee.**

A migration is only triggered if:
- `spread = maxAgents - minAgents > RebalanceThreshold + 1` (hysteresis band)
- The migration would **strictly reduce** the global spread (verified by computing the post-migration distribution)

Add a separate `RebalanceCooldown` constant (shorter than `StabilizationPeriod`) for pacing:

```go
var (
    RebalanceInterval            = 60 * time.Second
    RebalanceThreshold           = 2
    RebalanceStabilizationPeriod = 5 * time.Minute  // per-user cooldown after migration
    RebalanceCooldown            = 30 * time.Second  // global cooldown between any two rebalancer migrations
    RebalanceUndrainCooldown     = 60 * time.Second  // skip recently-undrained machines as targets
)
```

All configurable via env vars (add `REBALANCE_COOLDOWN_SECONDS`, `REBALANCE_UNDRAIN_COOLDOWN_SECONDS` in `init()`).

**New `rebalanceTick` pseudocode:**
```
1. Cooldown check: skip if last migration was < RebalanceCooldown ago
2. Preconditions: skip if any user migrating/provisioning, skip if any machine draining
3. Get active non-draining machines (need ≥ 3)
4. Compute spread = maxAgents - minAgents
5. Hysteresis: skip if spread ≤ RebalanceThreshold + 1
6. Get migratable users on the most-loaded machine (respecting StabilizationPeriod)
7. For each candidate (prefer secondary-here):
   a. TryLock(candidate) — skip if locked
   b. Find target (exclude bipod machines, exclude recently-undrained machines)
   c. Verify migration strictly reduces spread: compute newSpread, reject if newSpread >= spread
   d. Trigger migration
   e. Return (one per tick)
```

The convergence guarantee: every triggered migration strictly reduces the max-min spread. The spread is bounded below by 0, so the algorithm must terminate.

### Undrain Cooldown

Add `StatusChangedAt time.Time` to the in-memory `Machine` struct. Update `SetMachineStatus` to set it.

In `rebalanceTick`, build a set of recently-undrained machine IDs and add them to the exclude list when selecting targets.

---

## Change 4: Observability Endpoints

### 4a. `GET /api/health`

Returns coordinator health information:

```go
func (coord *Coordinator) handleHealth(w http.ResponseWriter, r *http.Request) {
    lockCount, _ := coord.userLocks.ActiveLocks()

    writeJSON(w, http.StatusOK, map[string]interface{}{
        "status":     "ok",
        "boot_time":  coord.bootTime.Format(time.RFC3339),
        "uptime_sec": int(time.Since(coord.bootTime).Seconds()),
        "locks":      lockCount,
    })
}
```

Register as `GET /api/health` in `RegisterRoutes`.

### 4b. `GET /api/rebalancer/status`

Returns current rebalancer state:

```go
func (coord *Coordinator) handleRebalancerStatus(w http.ResponseWriter, r *http.Request) {
    machines := coord.store.GetActiveNonDrainingMachines()

    maxAgents, minAgents := 0, int(^uint(0)>>1)
    var distribution []map[string]interface{}
    for _, m := range machines {
        distribution = append(distribution, map[string]interface{}{
            "machine_id":    m.MachineID,
            "active_agents": m.ActiveAgents,
        })
        if m.ActiveAgents > maxAgents { maxAgents = m.ActiveAgents }
        if m.ActiveAgents < minAgents { minAgents = m.ActiveAgents }
    }
    if len(machines) == 0 { minAgents = 0 }

    spread := maxAgents - minAgents
    balanced := spread <= RebalanceThreshold+1

    cooldownRemaining := time.Duration(0)
    if !coord.lastRebalanceMigration.IsZero() {
        elapsed := time.Since(coord.lastRebalanceMigration)
        if elapsed < RebalanceCooldown {
            cooldownRemaining = RebalanceCooldown - elapsed
        }
    }

    writeJSON(w, http.StatusOK, map[string]interface{}{
        "spread":              spread,
        "balanced":            balanced,
        "threshold":           RebalanceThreshold,
        "distribution":        distribution,
        "cooldown_remaining_sec": int(cooldownRemaining.Seconds()),
        "last_migration":      coord.lastRebalanceMigration.Format(time.RFC3339),
    })
}
```

Register as `GET /api/rebalancer/status`.

### 4c. Lock Status in User API

In `handleGetUser` (server.go line 281), add a `locked` field to the response:

```go
writeJSON(w, http.StatusOK, map[string]interface{}{
    // ... existing fields from UserDetailResponse ...
    "locked": coord.userLocks.IsLocked(userID),
})
```

This may require changing the response type or adding the field to `UserDetailResponse`. The simplest approach is to build a map[string]interface{} response (like the consistency endpoint does) rather than using the struct.

### 4d. Event Query Default `since`

In `handleQueryEvents` (server.go line 636), if `since` is not provided, default to the coordinator's boot time:

```go
if v := q.Get("since"); v != "" {
    query += fmt.Sprintf(` AND timestamp > $%d`, argN)
    args = append(args, v)
    argN++
} else {
    // Default to coordinator boot time
    query += fmt.Sprintf(` AND timestamp > $%d`, argN)
    args = append(args, coord.bootTime.Format(time.RFC3339))
    argN++
}
```

Same for `handleCountEvents`.

---

## Change 5: Enhanced Consistency Checker

### Two New Checks

Add to `handleSystemConsistency` (server.go):

**Check 10: No locks held for excessive duration (>10 minutes)**
```go
_, lockedUsers := coord.userLocks.ActiveLocks()
var stuckLocks []string
for _, lu := range lockedUsers {
    if time.Since(lu.LockedAt) > 10*time.Minute {
        stuckLocks = append(stuckLocks, fmt.Sprintf("%s(%ds)", lu.UserID, int(time.Since(lu.LockedAt).Seconds())))
    }
}
addCheck("no_stuck_locks", len(stuckLocks) == 0, fmt.Sprintf("stuck_locks: %v", stuckLocks))
```

**Check 11: Lock count roughly matches transient user count**
```go
lockCount, _ := coord.userLocks.ActiveLocks()
transientCount := 0
for _, s := range []string{"provisioning", "failing_over", "reforming", "suspending", "reactivating", "evicting", "migrating"} {
    transientCount += coord.store.CountUsersByStatus(s)
}
// Allow lockCount >= transientCount (lock acquired before status change) or lockCount <= transientCount+1
mismatch := lockCount > transientCount+2 || lockCount < transientCount-1
addCheck("lock_count_consistent", !mismatch,
    fmt.Sprintf("locks=%d transient_users=%d", lockCount, transientCount))
```

---

## Change 6: Coordinator Struct Updates

Add fields to `Coordinator`:

```go
type Coordinator struct {
    store                  *Store
    B2BucketName           string
    failAt                 string
    chaosMode              bool
    chaosProbability       float64
    cancelFunc             context.CancelFunc
    drainDone              sync.Map
    lastRebalanceMigration time.Time
    userLocks              *UserLockManager  // NEW
    bootTime               time.Time         // NEW
}
```

Initialize in `NewCoordinator`:
```go
return &Coordinator{
    // ...existing...
    userLocks: NewUserLockManager(),
    bootTime:  time.Now(),
}, nil
```

---

## Modifications Summary

| File | Changes | Magnitude |
|------|---------|-----------|
| `userlock.go` | NEW FILE — lock manager | New |
| `server.go` | Add `userLocks`/`bootTime` fields, synchronous handlers, lock on migrate/provision, health + rebalancer-status + event-default endpoints, enhanced consistency checks, locked field in user API | Large |
| `lifecycle.go` | Return errors instead of void, replace silent returns | Medium |
| `rebalancer.go` | Rewrite `rebalanceTick` with spread-based algorithm, add cooldown/undrain constants | Large |
| `drainer.go` | Add lock acquisition per user | Small |
| `healthcheck.go` | Add lock in `failoverUser` for running/degraded users | Small |
| `provisioner.go` | Lock in handleProvisionUser (server.go), not in ProvisionUser itself | Small (in server.go) |
| `reconciler.go` | Add lock in per-user phases | Medium |
| `store.go` | Add `StatusChangedAt` to Machine, update `SetMachineStatus` | Small |
| `cmd/coordinator/main.go` | No changes — NewCoordinator handles init | None |

### Order of Implementation

1. `userlock.go` — new file
2. `server.go` — add struct fields, `NewCoordinator` init
3. `lifecycle.go` — change signatures to return error
4. `server.go` handlers — synchronous lifecycle, lock on migrate/provision
5. `healthcheck.go` — lock in failoverUser
6. `rebalancer.go` — spread-based algorithm + lock + undrain cooldown
7. `drainer.go` — lock per user
8. `reconciler.go` — lock in per-user phases
9. `store.go` — StatusChangedAt
10. `server.go` endpoints — health, rebalancer-status, event default since, enhanced consistency
11. Test scripts — `scripts/layer-5.3/`

---

## Test Suite

### Test Harness Scripts

Create `scfuture/scripts/layer-5.3/` with the following files. Base all scripts on the Layer 5.2 equivalents, changing:
- Prefix: `l52-` → `l53-`
- Network name: `l52-net` → `l53-net`
- SSH key name: `l52-key` → `l53-key`
- Layer description in banners

#### `scripts/layer-5.3/run.sh`

Same structure as Layer 5.2 `run.sh`. B2 bucket: `l53-test-{random}`.

#### `scripts/layer-5.3/common.sh`

Copy from Layer 5.2. Change all `l52-` references to `l53-`.

#### `scripts/layer-5.3/deploy.sh`

Copy from Layer 5.2. Change `l52-` to `l53-`. Coordinator env vars:

```bash
Environment=REBALANCE_INTERVAL_SECONDS=10
Environment=REBALANCE_THRESHOLD=1
Environment=REBALANCE_STABILIZATION_SECONDS=15
Environment=REBALANCE_COOLDOWN_SECONDS=5
Environment=REBALANCE_UNDRAIN_COOLDOWN_SECONDS=30
```

#### `scripts/layer-5.3/infra.sh`

Copy from Layer 5.2. Change `l52-` to `l53-`.

#### `scripts/layer-5.3/cloud-init/coordinator.yaml` and `fleet.yaml`

Identical to Layer 5.2.

### Test Suite — `scripts/layer-5.3/test_suite.sh`

**Design philosophy:** The test suite is designed to be a credible indicator of production readiness, not just a feature checklist. Every phase proves a specific property of the system. The key principles:

1. **Event-log verification first.** Following the Layer 5.2 pattern, operations are verified by waiting for events via `wait_for_event` and `count_events`, NOT by polling user status. Events are persistent, queryable, and race-free. The event log is the source of truth for what actually happened.
2. **Always use `mark_time` + `since` timestamps.** Every phase marks a timestamp BEFORE starting and filters all event queries by it. This prevents stale events from previous phases bleeding through.
3. **Test both sides of every contract.** The lock must be tested for exclusion (same user) AND parallelism (different users). The rebalancer must be tested for convergence AND stability. The consistency checker must be tested for pass AND fail.
4. **Verify state durability, not just status codes.** Every synchronous API test follows up with an immediate GET to verify the state was durably changed.
5. **Verify lock lifecycle across all boundaries.** Lock release must be proven after: success, failure (fail handler), and crash (coordinator restart). Three different release paths, all must work.
6. **Prove negatives with two samples.** One observation of "nothing happened" could be coincidence. Two observations — one during the expected quiet period, one after it should end — proves the mechanism.
7. **Consistency check after every phase.** Non-negotiable.
8. **Use background curl + wait for true concurrency.** Sequential curls with small sleeps don't prove parallelism.

**Helper functions** (inherited from Layer 5.2 common.sh, plus additions):
- `mark_time` — returns UTC ISO8601 timestamp for use as `since` filter
- `wait_for_event "type=migration&trigger=manual&since=$TS" [timeout]` — polls `count_events` every 3s until event appears or timeout (default 120s)
- `count_events "type=provision&success=true&since=$TS"` — returns integer count of matching events
- `query_events "type=migration&since=$TS"` — returns full event objects as JSON
- `coord_api GET|POST "/api/..."` — call coordinator API, return JSON body

```
═══ Layer 5.3: Production Hardening — Test Suite ═══

Phase 0: Prerequisites & Observability Baseline (~11 checks)
  Purpose: Infrastructure is up, new endpoints work, event default since works.

  - Coordinator responding (GET /api/health → 200)
  - /api/health has boot_time, uptime_sec, locks=0
  - Record BOOT_TIME from /api/health for later use
  - 3 fleet machines registered and active
  - Machine agents responding (curl each fleet agent :8080/status)
  - Postgres connected, schema exists
  - GET /api/rebalancer/status → 200, has spread, balanced, distribution array
  - Event query default since works:
    - GET /api/events/query (no since param) → 200, returns array
    - All returned events (if any) have timestamps AFTER BOOT_TIME
    (This verifies Change 4d: default since = coordinator boot time)
  - Consistency checker returns all-pass on empty system (all 11 checks)
  - Consistency check

Phase 1: Setup & Smoke (~10 checks)
  Purpose: Establish test state and verify basic operations work with locks.
  This is NOT a regression suite — it provisions the users we need for later
  phases and does minimal smoke testing.

  - PHASE1_TS=$(mark_time)
  - Provision 6 users: "l53-user-1" through "l53-user-6" (128MB images)
  - Wait for 6 provision events: wait_for_event "type=provision&success=true&since=$PHASE1_TS" (poll count until 6)
  - Verify: distributed across 3 machines (each machine has ≥1 primary)
  - Verify: each user has exactly 2 non-stale bipods
  - MIG_TS=$(mark_time)
  - Manual migrate l53-user-1 → wait_for_event "type=migration&trigger=manual&since=$MIG_TS"
  - Suspend l53-user-2 → expect 200 (proves synchronous API + lock works)
  - Reactivate l53-user-2 → expect 200
  - Verify: count_events "type=lifecycle&since=$PHASE1_TS" ≥ 2 (suspend + reactivate)
  - Consistency check

Phase 2: Lock Correctness (~14 checks)
  Purpose: Prove mutual exclusion on the same user AND parallelism across users.

  Phase 2a: Same-user mutual exclusion (~8 checks)
    - LOCK_TS=$(mark_time)
    - Start migration of l53-user-1 (async, returns 202)
    - Wait for migration to be in flight:
      wait_for_event "type=migration&user_id=l53-user-1&since=$LOCK_TS" with short
      timeout, OR poll GET /api/users/l53-user-1 until "locked": true
      (We need the lock to be held, and migration events may only fire at completion.
       For this specific test, polling the locked field is the correct approach since
       we're testing the lock itself.)
    - GET /api/users/l53-user-1 → verify "locked": true
    - POST /api/users/l53-user-1/suspend → expect 409
    - POST /api/users/l53-user-1/migrate (different target) → expect 409
    - Wait for migration to complete: wait_for_event "type=migration&trigger=manual&user_id=l53-user-1&since=$LOCK_TS"
    - GET /api/users/l53-user-1 → verify "locked": false
    - POST /api/users/l53-user-1/suspend → expect 200 (lock was released)
    - POST /api/users/l53-user-1/reactivate → expect 200

  Phase 2b: Cross-user parallelism (~6 checks)
    - PARA_TS=$(mark_time)
    - Identify l53-user-3 and l53-user-4 on different machines
    - Launch BOTH migrations simultaneously using background curl:
        curl ... /api/users/l53-user-3/migrate {...} &
        PID1=$!
        curl ... /api/users/l53-user-4/migrate {...} &
        PID2=$!
        wait $PID1 $PID2
    - Both return 202 (NOT 409 — proves lock is per-user, not global)
    - During those migrations, POST /api/users/l53-user-5/suspend → expect 200
      (unrelated user is not blocked)
    - Reactivate l53-user-5 → expect 200
    - Wait for both migrations via events:
      Poll count_events "type=migration&trigger=manual&since=$PARA_TS" until ≥ 2
    - Consistency check

Phase 3: Synchronous API Correctness (~10 checks)
  Purpose: Prove lifecycle APIs are truly synchronous — state is durable when
  the response arrives — and return correct error codes.

  - SYNC_TS=$(mark_time)
  - Suspend l53-user-2 → HTTP 200, body has "status":"suspended"
  - Immediately GET /api/users/l53-user-2 → status MUST be "suspended" (no sleep)
    This proves the API waited for completion, not fire-and-forget.
  - Suspend l53-user-2 again → HTTP 409 (wrong state: already suspended)
  - Reactivate l53-user-2 → HTTP 200, body has "status":"running"
  - Immediately GET /api/users/l53-user-2 → status MUST be "running"
  - Reactivate l53-user-2 again → HTTP 409 (already running)
  - Evict l53-user-6 → HTTP 200, body has "status":"evicted"
  - Immediately GET /api/users/l53-user-6 → status MUST be "evicted"
  - Suspend nonexistent user → 404
  - Verify: count_events "type=lifecycle&since=$SYNC_TS" ≥ 3
    (suspend, reactivate, evict all recorded)
  - Consistency check

Phase 4: Rebalancer Convergence & Hysteresis (~11 checks)
  Purpose: Prove the rebalancer reduces spread, stops at the hysteresis boundary,
  and doesn't oscillate.

  Test config: threshold=1, so hysteresis boundary = threshold+1 = 2.
  Spread must be > 2 (i.e., ≥ 3) to trigger rebalancer.

  - REBAL_TS=$(mark_time)
  - Re-provision l53-user-6 (was evicted in Phase 3):
    wait_for_event "type=provision&success=true&since=$REBAL_TS"
  - Now have 6 active users across 3 machines
  - Create deliberate imbalance:
    - Manually migrate users until fleet-1 has 4 active agents, others have 1 each
    - Record IMBALANCE_TS=$(mark_time) and distribution
    - Verify: spread ≥ 3 via /api/rebalancer/status (imbalance confirmed)
  - Wait for rebalancer to act:
    wait_for_event "type=migration&trigger=rebalancer&since=$IMBALANCE_TS" 120
  - Verify: count_events "type=migration&trigger=rebalancer&since=$IMBALANCE_TS" ≥ 1
  - Poll /api/rebalancer/status until spread ≤ 2 (converged)
  - Hysteresis proof:
    - Record STABLE_TS=$(mark_time) when spread first reaches ≤ 2
    - Wait 3 rebalancer ticks (30s in test config)
    - Verify: count_events "type=migration&trigger=rebalancer&since=$STABLE_TS" = 0
      (rebalancer stopped at hysteresis boundary)
    - Verify: spread is STILL ≤ 2 (didn't oscillate)
  - GET /api/rebalancer/status → "balanced": true
  - Verify: all active users running (GET each user)
  - Consistency check

Phase 5: Failover During Migration + Lock Release (~10 checks)
  Purpose: Prove the system recovers from target failure mid-migration AND
  that the lock is properly released by the fail handler.

  - FAIL_TS=$(mark_time)
  - Pick a user on fleet-1 (primary) + fleet-2 (secondary)
  - Start migration to fleet-3 (returns 202)
  - Poll until user shows "locked": true in GET /api/users/{id}
    (confirms migration is in flight and lock is held)
  - Stop fleet-3 machine-agent: ssh fleet-3 "systemctl stop machine-agent"
  - Wait for migration failure event:
    wait_for_event "type=migration&user_id={id}&since=$FAIL_TS" 90
    (migration event is recorded on completion, including failures)
  - Verify: user status is "running" (GET /api/users/{id})
  - Verify: user has exactly 2 non-stale bipods (original machines)
  - Verify: no phantom bipod on fleet-3 (role != "stale")
  - Lock release proof: POST /api/users/{id}/suspend → expect 200
    If this returns 409 with "operation already in progress", the lock leaked.
  - Reactivate the user → expect 200
  - Restart fleet-3 machine-agent: ssh fleet-3 "systemctl start machine-agent"
  - Wait for fleet-3 heartbeat: poll GET /api/fleet until fleet-3 shows active
  - Consistency check

Phase 6: Drain + Lock Integration + Undrain Cooldown (~12 checks)
  Purpose: Prove drain acquires locks correctly, and undrain cooldown prevents
  the rebalancer from immediately refilling a recently-undrained machine.

  - Ensure fleet-3 has at least 2 users (migrate if needed)
  - DRAIN_TS=$(mark_time)
  - Start drain: POST /api/fleet/fleet-3/drain → 202
  - Verify: wait_for_event "type=drain_started&machine_id=fleet-3&since=$DRAIN_TS"
  - During drain, poll for a user with "locked": true on fleet-3:
    - POST /api/users/{locked-user}/suspend → expect 409 (locked by drain)
  - Wait for drain to complete:
    wait_for_event "type=drain_completed&machine_id=fleet-3&since=$DRAIN_TS" 300
  - Verify: count_events "type=migration&trigger=drain&since=$DRAIN_TS" ≥ 2
  - Verify: all users running (GET each user)
  - Undrain: POST /api/fleet/fleet-3/undrain → 200
  - UNDRAIN_TS=$(mark_time)
  - Create reason for rebalancer to target fleet-3:
    - Manually migrate a user TO the most-loaded machine (increase spread)
    - Verify spread > threshold+1 via /api/rebalancer/status
  - Cooldown negative test: wait 10s
    - Count rebalancer migrations since UNDRAIN_TS:
      count_events "type=migration&trigger=rebalancer&since=$UNDRAIN_TS" → expect 0
      (undrain cooldown is 30s, so no rebalancer migration should target fleet-3 yet)
  - Cooldown expiry test: wait 25 more seconds (now past 30s undrain cooldown)
    - If wait_for_event "type=migration&trigger=rebalancer&since=$UNDRAIN_TS" with
      short timeout finds an event → proves cooldown expired and rebalancer acted
    - If not, verify via /api/rebalancer/status that spread no longer requires it
      (Either outcome is valid; the negative test at 10s is the critical one)
  - Consistency check

Phase 7: Consistency Checker Validation (~8 checks)
  Purpose: Prove the consistency checker catches injected problems AND that
  the new checks (10, 11) work correctly during live operations.

  Part A: Injection test (existing checks)
    - Insert phantom bipod via SQL:
      INSERT INTO bipods (user_id, machine_id, role, minor)
      VALUES ('l53-user-1', 'fleet-3', 'secondary', 99)
    - GET /api/system/consistency → expect "pass": false
    - Verify: "running_users_have_2_bipods" check specifically reports failure
    - DELETE the phantom row
    - GET /api/system/consistency → expect "pass": true

  Part B: Live operation validation (new checks 10 + 11)
    - LIVE_TS=$(mark_time)
    - Start migration of l53-user-1 (async, returns 202)
    - Poll GET /api/users/l53-user-1 until "locked": true
      (migration is in flight, lock is held)
    - GET /api/system/consistency → expect "pass": true
      (lock IS held but is NOT stuck — proves check 10 has a threshold)
    - Verify check "no_stuck_locks" detail mentions the held lock but passes
    - Verify check "lock_count_consistent" shows locks=1, transient_users=1
    - Wait for migration to complete:
      wait_for_event "type=migration&user_id=l53-user-1&since=$LIVE_TS"
    - GET /api/system/consistency → locks=0, still passes

Phase 8: Crash Recovery + Lock State (~8 checks)
  Purpose: Prove coordinator restart clears all locks and the reconciler
  correctly resumes in-progress operations.

  - Ensure at least 2 users on fleet-1
  - CRASH_TS=$(mark_time)
  - Start drain: POST /api/fleet/fleet-1/drain → 202
  - Wait for drain to start moving users:
    wait_for_event "type=drain_started&machine_id=fleet-1&since=$CRASH_TS"
  - Poll until a user shows "locked": true (drain migration in flight)
  - Record DRAIN_USER (the locked user)
  - Restart coordinator: ssh coordinator "systemctl restart coordinator"
  - Wait for coordinator to come back: poll GET /api/health until 200
  - GET /api/health → verify:
    - locks = 0 (all in-memory locks cleared on restart)
    - boot_time is recent (within last 30s)
  - RECOVERY_TS=$(mark_time)
  - Wait for drain to resume and complete:
    wait_for_event "type=drain_completed&machine_id=fleet-1&since=$RECOVERY_TS" 300
    (reconciler resumes the drain after restart)
  - Verify: all users running (GET each user)
  - Lock release proof: POST /api/users/$DRAIN_USER/suspend → 200
    (proves no phantom lock survived the restart)
  - Reactivate that user → 200
  - Consistency check (all 11 checks pass)
  - Undrain fleet-1

Phase 9: Final Verification (~5 checks)
  Purpose: Final proof of system health after all stress testing.

  - FINAL_TS=$(mark_time)
  - Provision new user "l53-user-7":
    wait_for_event "type=provision&success=true&since=$FINAL_TS"
  - GET /api/health → verify stable (locks=0, uptime reasonable)
  - Verify event summary — all three migration triggers present:
    count_events "type=migration&trigger=manual" ≥ 1
    count_events "type=migration&trigger=rebalancer" ≥ 1
    count_events "type=migration&trigger=drain" ≥ 1
  - Verify: no stuck locks (consistency check 10)
  - Final consistency check (all 11 checks pass)

═══════════════════════════════════════════════════
Estimated total: ~99 checks
═══════════════════════════════════════════════════
```

### Key Test Implementation Patterns

**Event-based waiting (primary verification method — inherited from Layer 5.2):**
```bash
# These helpers are defined in common.sh (copied from Layer 5.2):

mark_time() {
    date -u +"%Y-%m-%dT%H:%M:%SZ"
}

count_events() {
    local resp
    resp=$(coord_api GET "/api/events/count?$1" 2>/dev/null) || echo "0"
    echo "$resp" | jq -r '.count // 0'
}

query_events() {
    coord_api GET "/api/events/query?$1"
}

wait_for_event() {
    local query="$1"
    local timeout="${2:-120}"
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local c
        c=$(count_events "$query")
        if [ "${c:-0}" -gt 0 ]; then
            return 0
        fi
        sleep 3
        elapsed=$((elapsed + 3))
    done
    return 1
}

# Usage: mark time BEFORE operation, then wait for event with since filter
MIG_TS=$(mark_time)
coord_api POST "/api/users/l53-user-1/migrate" '{"source_machine":"...","target_machine":"..."}'
wait_for_event "type=migration&trigger=manual&user_id=l53-user-1&since=$MIG_TS" 120
check "Migration completed" '[ $? -eq 0 ]'
```

**Waiting for multiple provision events (Phase 1):**
```bash
PHASE1_TS=$(mark_time)

# Provision 6 users
for i in 1 2 3 4 5 6; do
    coord_api POST "/api/users" '{"user_id":"l53-user-'$i'","image_size_mb":128}'
done

# Wait for all 6 to complete via event log
ELAPSED=0
while [ $ELAPSED -lt 300 ]; do
    PROV_COUNT=$(count_events "type=provision&success=true&since=$PHASE1_TS")
    if [ "${PROV_COUNT:-0}" -ge 6 ]; then break; fi
    sleep 5
    ELAPSED=$((ELAPSED + 5))
done
check "All 6 users provisioned" '[ "${PROV_COUNT:-0}" -ge 6 ]'
```

**True concurrent requests with background curl (Phase 2b):**
```bash
PARA_TS=$(mark_time)

# Launch both migrations simultaneously
curl -s -o /tmp/migrate_resp_1 -w "%{http_code}" \
    -X POST "http://$COORD_PUB_IP:8080/api/users/l53-user-3/migrate" \
    -d '{"source_machine":"'$SRC3'","target_machine":"'$TGT3'"}' > /tmp/migrate_code_1 &
PID1=$!
curl -s -o /tmp/migrate_resp_2 -w "%{http_code}" \
    -X POST "http://$COORD_PUB_IP:8080/api/users/l53-user-4/migrate" \
    -d '{"source_machine":"'$SRC4'","target_machine":"'$TGT4'"}' > /tmp/migrate_code_2 &
PID2=$!
wait $PID1 $PID2

CODE1=$(cat /tmp/migrate_code_1)
CODE2=$(cat /tmp/migrate_code_2)
check "Parallel migration user-3 accepted" '[ "$CODE1" = "202" ]'
check "Parallel migration user-4 accepted" '[ "$CODE2" = "202" ]'

# Meanwhile, suspend an unrelated user (should not be blocked)
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    -X POST "http://$COORD_PUB_IP:8080/api/users/l53-user-5/suspend")
check "Unrelated user suspend succeeds during parallel migrations" '[ "$HTTP_CODE" = "200" ]'

# Wait for both migrations via event log
ELAPSED=0
while [ $ELAPSED -lt 120 ]; do
    MIG_COUNT=$(count_events "type=migration&trigger=manual&since=$PARA_TS")
    if [ "${MIG_COUNT:-0}" -ge 2 ]; then break; fi
    sleep 3
    ELAPSED=$((ELAPSED + 3))
done
check "Both parallel migrations completed" '[ "${MIG_COUNT:-0}" -ge 2 ]'
```

**Verifying synchronous state durability (Phase 3):**
```bash
# Suspend — response says suspended
HTTP_CODE=$(curl -s -o /tmp/suspend_resp -w "%{http_code}" \
    -X POST "http://$COORD_PUB_IP:8080/api/users/l53-user-2/suspend")
check "Suspend returns 200" '[ "$HTTP_CODE" = "200" ]'
RESP_STATUS=$(jq -r '.status' /tmp/suspend_resp)
check "Suspend response body says suspended" '[ "$RESP_STATUS" = "suspended" ]'

# Immediately verify — NO SLEEP — state is durable
ACTUAL_STATUS=$(coord_api GET "/api/users/l53-user-2" | jq -r '.status')
check "User status is suspended immediately after response" '[ "$ACTUAL_STATUS" = "suspended" ]'
```

**Polling locked field for lock-specific tests (Phases 2a, 5, 7B):**
```bash
# For tests that specifically need to observe the lock being HELD (not just
# operation completion), we poll the locked field. This is the one case where
# status polling is appropriate — we're testing the lock itself, not the operation.

wait_for_locked() {
    local USER_ID=$1 TIMEOUT=${2:-30}
    local ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
        LOCKED=$(coord_api GET "/api/users/$USER_ID" | jq -r '.locked')
        if [ "$LOCKED" = "true" ]; then return 0; fi
        sleep 1
        ELAPSED=$((ELAPSED + 1))
    done
    return 1
}

# Start migration
coord_api POST "/api/users/$USER_ID/migrate" '{"source_machine":"...","target_machine":"..."}'

# Wait for lock to be held (we need to observe the in-flight state)
wait_for_locked "$USER_ID" 30
check "User locked during migration" '[ $? -eq 0 ]'
```

**Failover during migration with event-based recovery detection (Phase 5):**
```bash
FAIL_TS=$(mark_time)

# Start migration to fleet-3
coord_api POST "/api/users/$USER_ID/migrate" \
    '{"source_machine":"'$FLEET1_ID'","target_machine":"'$FLEET3_ID'"}'

# Wait for lock to be held (migration in flight)
wait_for_locked "$USER_ID" 30

# NOW kill fleet-3 agent (we know migration is in flight)
ssh_cmd "$FLEET3_PUB_IP" "systemctl stop machine-agent"

# Wait for migration event (recorded on completion, including failures)
wait_for_event "type=migration&user_id=$USER_ID&since=$FAIL_TS" 90

# Verify recovery
STATUS=$(coord_api GET "/api/users/$USER_ID" | jq -r '.status')
check "User recovered to running after target failure" '[ "$STATUS" = "running" ]'

# Verify no phantom bipods
BIPOD_COUNT=$(coord_api GET "/api/users/$USER_ID/bipod" \
    | jq '[.[] | select(.role != "stale")] | length')
check "Exactly 2 non-stale bipods after failed migration" '[ "$BIPOD_COUNT" = "2" ]'

FLEET3_BIPOD=$(coord_api GET "/api/users/$USER_ID/bipod" \
    | jq '[.[] | select(.machine_id == "'$FLEET3_ID'" and .role != "stale")] | length')
check "No phantom bipod on fleet-3" '[ "$FLEET3_BIPOD" = "0" ]'

# CRITICAL: Verify lock was released by fail handler
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    -X POST "http://$COORD_PUB_IP:8080/api/users/$USER_ID/suspend")
check "Lock released after failed migration (suspend succeeds)" '[ "$HTTP_CODE" = "200" ]'

# Clean up
coord_api POST "/api/users/$USER_ID/reactivate"

# Restart fleet-3 and wait for it to re-register
ssh_cmd "$FLEET3_PUB_IP" "systemctl start machine-agent"
FLEET3_TS=$(mark_time)
wait_for_event "type=heartbeat&machine_id=$FLEET3_ID&since=$FLEET3_TS" 30 || sleep 15
```

**Drain with event-based completion detection (Phase 6):**
```bash
DRAIN_TS=$(mark_time)

# Start drain
coord_api POST "/api/fleet/fleet-3/drain"

# Verify drain started event
check "Drain started event recorded" '
    wait_for_event "type=drain_started&machine_id=fleet-3&since=$DRAIN_TS" 30
'

# During drain, find a locked user for contention test
ELAPSED=0
LOCKED_USER=""
while [ $ELAPSED -lt 60 ]; do
    for U in $FLEET3_USERS; do
        LOCKED=$(coord_api GET "/api/users/$U" | jq -r '.locked')
        if [ "$LOCKED" = "true" ]; then LOCKED_USER=$U; break 2; fi
    done
    sleep 2
    ELAPSED=$((ELAPSED + 2))
done

if [ -n "$LOCKED_USER" ]; then
    HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
        -X POST "http://$COORD_PUB_IP:8080/api/users/$LOCKED_USER/suspend")
    check "Suspend locked-by-drain user returns 409" '[ "$HTTP_CODE" = "409" ]'
fi

# Wait for drain to complete via event log
wait_for_event "type=drain_completed&machine_id=fleet-3&since=$DRAIN_TS" 300
check "Drain completed" '[ $? -eq 0 ]'

# Verify drain migrations recorded
DRAIN_MIGS=$(count_events "type=migration&trigger=drain&since=$DRAIN_TS")
check "Drain migration events recorded" '[ "${DRAIN_MIGS:-0}" -ge 2 ]'
```

**Crash recovery with event-based drain resumption (Phase 8):**
```bash
CRASH_TS=$(mark_time)

# Start drain
coord_api POST "/api/fleet/$FLEET1_ID/drain"

# Wait for drain to start
wait_for_event "type=drain_started&machine_id=$FLEET1_ID&since=$CRASH_TS"

# Wait for a user to be mid-migration (lock held)
DRAIN_USER=""
ELAPSED=0
while [ $ELAPSED -lt 60 ]; do
    for U in $FLEET1_USERS; do
        LOCKED=$(coord_api GET "/api/users/$U" | jq -r '.locked')
        if [ "$LOCKED" = "true" ]; then DRAIN_USER=$U; break 2; fi
    done
    sleep 2
    ELAPSED=$((ELAPSED + 2))
done

# Restart coordinator mid-drain
ssh_cmd "$COORD_PUB_IP" "systemctl restart coordinator"

# Wait for coordinator to come back (poll /api/health)
ELAPSED=0
while [ $ELAPSED -lt 30 ]; do
    if coord_api GET "/api/health" >/dev/null 2>&1; then break; fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
done

# Verify locks cleared on restart
HEALTH=$(coord_api GET "/api/health")
LOCKS=$(echo "$HEALTH" | jq '.locks')
check "Zero locks after coordinator restart" '[ "$LOCKS" = "0" ]'

BOOT_TIME=$(echo "$HEALTH" | jq -r '.boot_time')
check "Boot time is recent" 'python3 -c "
from datetime import datetime, timezone, timedelta
bt = datetime.fromisoformat(\"$BOOT_TIME\".replace(\"Z\",\"+00:00\"))
assert (datetime.now(timezone.utc) - bt) < timedelta(seconds=30)
"'

# Wait for reconciler + drain to resume and complete via event log
RECOVERY_TS=$(mark_time)
wait_for_event "type=drain_completed&machine_id=$FLEET1_ID&since=$RECOVERY_TS" 300

# Verify all users running
for U in l53-user-1 l53-user-2 l53-user-3 l53-user-4 l53-user-5 l53-user-6; do
    STATUS=$(coord_api GET "/api/users/$U" | jq -r '.status')
    [ "$STATUS" = "running" ] || [ "$STATUS" = "suspended" ] || \
        echo "WARNING: $U in unexpected status $STATUS"
done

# Prove no phantom lock on the user that was mid-migration
if [ -n "$DRAIN_USER" ]; then
    HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
        -X POST "http://$COORD_PUB_IP:8080/api/users/$DRAIN_USER/suspend")
    check "No phantom lock after restart (suspend succeeds)" '[ "$HTTP_CODE" = "200" ]'
    coord_api POST "/api/users/$DRAIN_USER/reactivate"
fi
```

**Hysteresis boundary test with event verification (Phase 4):**
```bash
# After creating imbalance (fleet-1 has 4 agents, others have 1)
IMBALANCE_TS=$(mark_time)

# Wait for rebalancer to act via event log
wait_for_event "type=migration&trigger=rebalancer&since=$IMBALANCE_TS" 120
check "Rebalancer triggered" '[ $? -eq 0 ]'

# Verify count
REBAL_COUNT=$(count_events "type=migration&trigger=rebalancer&since=$IMBALANCE_TS")
check "At least 1 rebalancer migration" '[ "$REBAL_COUNT" -ge 1 ]'

# Poll /api/rebalancer/status until spread ≤ 2 (converged)
ELAPSED=0
while [ $ELAPSED -lt 120 ]; do
    SPREAD=$(coord_api GET "/api/rebalancer/status" | jq '.spread')
    if [ "${SPREAD:-99}" -le 2 ]; then break; fi
    sleep 5
    ELAPSED=$((ELAPSED + 5))
done
check "Rebalancer converged (spread ≤ 2)" '[ "${SPREAD:-99}" -le 2 ]'

# Now verify the hysteresis boundary: spread ≤ 2 should NOT trigger more migrations
STABLE_TS=$(mark_time)

# Wait 3 full rebalancer ticks (30s with 10s interval)
sleep 30

# Verify NO new migrations via event log
NEW_MIGRATIONS=$(count_events "type=migration&trigger=rebalancer&since=$STABLE_TS")
check "No rebalancer migrations at hysteresis boundary (no thrashing)" '[ "$NEW_MIGRATIONS" = "0" ]'

# Verify spread hasn't changed (stable)
SPREAD_AFTER=$(coord_api GET "/api/rebalancer/status" | jq '.spread')
check "Spread stable after hysteresis window" '[ "${SPREAD_AFTER:-99}" -le 2 ]'

# Verify balanced flag
BALANCED=$(coord_api GET "/api/rebalancer/status" | jq '.balanced')
check "Rebalancer reports balanced" '[ "$BALANCED" = "true" ]'
```

**Live consistency checker during migration (Phase 7 Part B):**
```bash
LIVE_TS=$(mark_time)

# Start migration (lock will be held)
coord_api POST "/api/users/l53-user-1/migrate" \
    '{"source_machine":"'$SRC'","target_machine":"'$TGT'"}'

# Wait for lock to be held
wait_for_locked "l53-user-1" 30

# Run consistency check DURING migration
RESULT=$(coord_api GET "/api/system/consistency")
PASS=$(echo "$RESULT" | jq '.pass')
check "Consistency passes during active migration" '[ "$PASS" = "true" ]'

# Check 10: lock exists but is not stuck (held for seconds, not 10+ min)
STUCK_CHECK=$(echo "$RESULT" | jq '.checks[] | select(.name == "no_stuck_locks") | .pass')
check "Stuck-lock check passes (lock held but not stuck)" '[ "$STUCK_CHECK" = "true" ]'

# Check 11: lock count matches transient users
LOCK_CHECK=$(echo "$RESULT" | jq '.checks[] | select(.name == "lock_count_consistent")')
LOCK_PASS=$(echo "$LOCK_CHECK" | jq '.pass')
LOCK_DETAIL=$(echo "$LOCK_CHECK" | jq -r '.detail')
check "Lock-count check passes during migration" '[ "$LOCK_PASS" = "true" ]'
echo "  Lock count detail: $LOCK_DETAIL"

# Wait for migration to complete via event log
wait_for_event "type=migration&user_id=l53-user-1&since=$LIVE_TS" 120

# Verify locks cleared
RESULT=$(coord_api GET "/api/system/consistency")
LOCK_DETAIL=$(echo "$RESULT" | jq -r '.checks[] | select(.name == "lock_count_consistent") | .detail')
check "Zero locks after migration completes" 'echo "$LOCK_DETAIL" | grep -q "locks=0"'
```

---

## Implementation Notes

### Why No Graceful Shutdown

Every background goroutine (healthchecker, rebalancer, drainer, reformer) runs `for range ticker.C` with no context cancellation. On `systemctl stop coordinator`, these goroutines are killed mid-step. This is safe because:

1. The reconciler handles every crash point in every operation (proven across Layers 4.6, 5.1, 5.2)
2. The coordinator restarts in <5 seconds
3. Adding context cancellation to all goroutines and their callees (including HTTP clients, DRBD wait loops) is a significant refactor with marginal benefit

The trade-off: a user whose migration is at `migrate-container-stopped` (user is DOWN) stays down until the coordinator restarts (~5 seconds) and the reconciler runs (~2-3 seconds). Total: ~8 seconds of additional downtime. This is acceptable for a PoC and even for early production.

### Why No Operation Watchdog

Operations are bounded by existing timeouts:
- HTTP client: 30 seconds per call
- DRBD sync: 300 seconds (`MigrationSyncTimeout`)
- Total worst-case migration: ~600 seconds

Instead of a complex watchdog, the consistency checker has Check 10 (stuck locks >10 minutes) which catches wedged operations and makes them visible. An operator can then investigate and manually intervene.

### Lock Safety Properties

- **No deadlocks:** Every lock acquire is non-blocking (`TryLock`). No operation ever waits for a lock. No cross-user locking.
- **No lock leaks:** Every `TryLock` is paired with a `defer Unlock` in the same scope (handler or goroutine function).
- **No starvation:** The healthchecker, rebalancer, and drainer all skip locked users and retry on the next tick/iteration.
- **Clean restart:** All locks are in-memory. Coordinator restart releases all locks. The reconciler re-processes any in-progress operations.

### Backward Compatibility

- `MigrateUser` signature unchanged (lock managed by caller)
- Lifecycle functions gain return values (only the HTTP handlers call them, and those are updated)
- New API endpoints are additive (no existing endpoints changed except lifecycle status codes: 202 → 200)
- Event query gains default `since` but explicit `since` still overrides
- Consistency checker gains 2 new checks (checks 10-11) alongside existing 9

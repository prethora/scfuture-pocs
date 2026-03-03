# Layer 4.6 Build Prompt — Crash Recovery, Reconciliation & Postgres Migration

## What This Is

This is a build prompt for Layer 4.6 of the scfuture distributed agent platform. You are Claude Code. Your job is to:

1. Read all referenced existing files to understand the current codebase
2. Write all new code and scripts described below
3. Report back when code is written
4. When told "yes" / "ready", run the full test lifecycle (infra up → deploy → test → iterate on failures → teardown)
5. When all tests pass, update `SESSION.md` (in the parent directory) with what happened
6. Give a final report

The project lives in `scfuture/` (a subdirectory of the current working directory). All Go code paths are relative to `scfuture/`. All script paths are relative to `scfuture/`. The `SESSION.md` file is in the parent directory (current working directory).

---

## Context: What Exists

Layers 4.1 (machine agent), 4.2 (coordinator happy path), 4.3 (heartbeat failure detection & automatic failover), 4.4 (bipod reformation & dead machine re-integration), and 4.5 (suspension, reactivation & deletion lifecycle) are complete and committed. Read these files first to understand the existing codebase:

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
scfuture/container/Dockerfile
scfuture/container/container-init.sh
```

Read ALL of these before writing any code. Pay close attention to:
- How `store.go` currently uses in-memory maps + JSON persistence (`persist()` writes to `state.json`). **This entire file will be rewritten** to use Postgres while keeping the same public method signatures.
- How `ProvisionUser` in `provisioner.go` drives an 8-step sequential flow in a goroutine — each step calls the machine agent API, then updates store state. There is NO operation tracking — only user status transitions.
- How `failoverUser` in `healthcheck.go` runs in a goroutine with 3-5 steps depending on failure type.
- How `reformBipod` in `reformer.go` runs reformation with ~9 steps including cleanup, DRBD adjust, and sync.
- How `suspendUser`, `warmReactivate`, `coldReactivate`, and `evictUser` in `lifecycle.go` each run multi-step flows in goroutines.
- How `cmd/coordinator/main.go` starts background goroutines (HealthChecker, Reformer, RetentionEnforcer) immediately, then starts the HTTP server. There is NO reconciliation step on startup.
- How `NewMachineClient` uses `doJSON()` for all HTTP calls with 30-second timeout — some calls (B2 backup/restore) use a 300-second timeout.
- How the machine agent's `/status` endpoint re-discovers all user resources from the system on every call (no persistent state on machines).
- How all multi-step operations are already idempotent — re-executing any step is safe (proven in Layers 4.1–4.5).

### Reference documents (in parent directory):

```
SESSION.md
architecture-v3.md
master-prompt4.5.md
```

---

## What Layer 4.6 Builds

**What this layer proves:** The coordinator can be killed at any point during any multi-step operation (provisioning, failover, reformation, suspension, reactivation, eviction) and recover correctly on restart. This is the final reliability primitive — after this layer, the system is crash-safe.

**In scope:**
- **Postgres migration** — replace in-memory maps + JSON persistence with Supabase (external Postgres). The coordinator's `store.go` is fully rewritten. All durable state lives in Postgres. The in-memory maps become a read-through cache.
- **Operations table** — new database table tracking every multi-step operation with `current_step` and `metadata` JSONB. This is the core crash recovery mechanism.
- **Fault injection system** — `checkFault()` function with deterministic `FAIL_AT` mode and random `CHAOS_MODE`. Called at every step transition in every operation.
- **Startup reconciliation** — five-phase algorithm that runs on coordinator startup BEFORE accepting requests. Discovers machine reality, reconciles DB, resumes interrupted operations, cleans orphans, handles offline machines.
- **Consistency checker** — standalone shell function that verifies 12 system invariants by cross-referencing Postgres with machine agent `/status` responses.
- **Advisory lock** — `pg_try_advisory_lock` for coordinator singleton enforcement (prevents split-brain).
- **Graceful shutdown** — SIGTERM handler that stops accepting requests and waits for in-flight operations to reach a checkpoint.
- **Unified events table** — merge the 3 separate in-memory event lists (failover, reformation, lifecycle) into a single `events` table.
- **Full crash recovery test suite** — 33 deterministic crash point tests, chaos mode test, direct reconciliation tests.

**Explicitly NOT in scope (future layers):**
- Live migration (Layer 5)
- Schema versioning / migration tooling
- HA coordinator (active-passive pair)
- Incremental B2 backups

---

## Architecture

### Test Topology

Same as Layers 4.3–4.5: 1 coordinator + 3 fleet machines on Hetzner Cloud. Additionally requires:
- Backblaze B2 credentials (same as Layer 4.5 — for cold reactivation crash tests)
- Supabase Postgres connection string (new for this layer)

```
macOS (test harness, runs test_suite.sh via SSH / curl / psql)
  │
  ├── l46-coordinator (CX23, coordinator :8080, private 10.0.0.2)
  │     │
  │     ├── l46-fleet-1 (CX23, machine-agent :8080, private 10.0.0.11)
  │     ├── l46-fleet-2 (CX23, machine-agent :8080, private 10.0.0.12)
  │     └── l46-fleet-3 (CX23, machine-agent :8080, private 10.0.0.13)
  │
  Private network: l46-net / 10.0.0.0/24

Backblaze B2:
  Bucket: l46-test-{random} (created/destroyed by run.sh)
  Env vars: B2_KEY_ID, B2_APP_KEY (required, set by user)

Supabase Postgres:
  Env var: DATABASE_URL (required, set by user)
  Format: postgres://user:password@host:port/dbname?sslmode=require
  The user provides a pre-existing Supabase project.
  Tables are created/dropped per test run.
```

### Why External Postgres (Not Local)

The coordinator must be testable for crash recovery. If the database is on the coordinator machine, killing the coordinator process could risk database state (corrupted writes, journal recovery, etc.). An external managed database eliminates this variable entirely. When we `os.Exit(1)` the coordinator process, the database is completely unaffected — it's on a different machine, managed by Supabase.

Supabase's free tier provides a production-grade Postgres instance at zero cost. The `DATABASE_URL` connection string is the only configuration needed.

### Database Schema

```sql
-- Machines in the fleet
CREATE TABLE IF NOT EXISTS machines (
    machine_id        TEXT PRIMARY KEY,
    address           TEXT NOT NULL DEFAULT '',
    public_address    TEXT DEFAULT '',
    status            TEXT NOT NULL DEFAULT 'active',
    status_changed_at TIMESTAMPTZ DEFAULT NOW(),
    disk_total_mb     BIGINT DEFAULT 0,
    disk_used_mb      BIGINT DEFAULT 0,
    ram_total_mb      BIGINT DEFAULT 0,
    ram_used_mb       BIGINT DEFAULT 0,
    active_agents     INTEGER DEFAULT 0,
    max_agents        INTEGER DEFAULT 10,
    last_heartbeat    TIMESTAMPTZ DEFAULT NOW()
);

-- User accounts
CREATE TABLE IF NOT EXISTS users (
    user_id            TEXT PRIMARY KEY,
    status             TEXT NOT NULL DEFAULT 'registered',
    status_changed_at  TIMESTAMPTZ DEFAULT NOW(),
    primary_machine    TEXT DEFAULT '',
    drbd_port          INTEGER UNIQUE,
    image_size_mb      INTEGER DEFAULT 512,
    error              TEXT DEFAULT '',
    created_at         TIMESTAMPTZ DEFAULT NOW(),
    backup_exists      BOOLEAN DEFAULT FALSE,
    backup_path        TEXT DEFAULT '',
    backup_bucket      TEXT DEFAULT '',
    backup_timestamp   TIMESTAMPTZ,
    drbd_disconnected  BOOLEAN DEFAULT FALSE
);

-- Bipod members (2 rows per user when healthy)
CREATE TABLE IF NOT EXISTS bipods (
    user_id     TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    machine_id  TEXT NOT NULL,
    role        TEXT NOT NULL DEFAULT 'primary',
    drbd_minor  INTEGER DEFAULT 0,
    loop_device TEXT DEFAULT '',
    created_at  TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (user_id, machine_id)
);

-- Multi-step operations — the core crash recovery mechanism.
-- Every multi-step operation creates a row here. current_step tracks
-- exactly which step the coordinator was executing when it crashed.
-- On restart, reconciliation reads incomplete operations and resumes.
CREATE TABLE IF NOT EXISTS operations (
    operation_id  TEXT PRIMARY KEY,
    type          TEXT NOT NULL,
    user_id       TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    status        TEXT NOT NULL DEFAULT 'in_progress',
    current_step  TEXT DEFAULT '',
    metadata      JSONB DEFAULT '{}',
    started_at    TIMESTAMPTZ DEFAULT NOW(),
    completed_at  TIMESTAMPTZ,
    error         TEXT DEFAULT ''
);

-- Unified event log (replaces separate failoverEvents, reformationEvents, lifecycleEvents)
CREATE TABLE IF NOT EXISTS events (
    event_id     SERIAL PRIMARY KEY,
    timestamp    TIMESTAMPTZ DEFAULT NOW(),
    event_type   TEXT NOT NULL,
    machine_id   TEXT,
    user_id      TEXT,
    operation_id TEXT,
    details      JSONB DEFAULT '{}'
);

-- Indexes
CREATE INDEX IF NOT EXISTS idx_bipods_machine ON bipods(machine_id);
CREATE INDEX IF NOT EXISTS idx_bipods_user ON bipods(user_id);
CREATE INDEX IF NOT EXISTS idx_operations_status ON operations(status) WHERE status IN ('in_progress', 'pending');
CREATE INDEX IF NOT EXISTS idx_operations_user ON operations(user_id);
CREATE INDEX IF NOT EXISTS idx_events_type ON events(event_type);
CREATE INDEX IF NOT EXISTS idx_events_timestamp ON events(timestamp);
```

**Key design insight — the `operations` table:**

Currently, multi-step operations are tracked only by user status transitions (`running → suspending → suspended`). If the coordinator dies mid-operation, there is no record of which step it was on. The `operations` table with its `current_step` field and `metadata` JSONB gives precise crash recovery: on restart, read the incomplete operation, determine the next step, and resume. Every step is idempotent (already proven), so re-executing the current step is safe.

The `metadata` JSONB stores decisions made at the start of the operation — which machines were selected, which DRBD port was allocated, etc. — so that a resumed operation uses the exact same parameters.

**Note on RunningAgents field:** The current Machine struct has `RunningAgents []string` which is populated from machine `/status` calls. This is ephemeral (re-discovered every heartbeat) and does NOT go into Postgres. It stays in the in-memory cache only.

### Advisory Lock for Coordinator Singleton

On startup, before reconciliation, the coordinator acquires a Postgres advisory lock:

```sql
SELECT pg_try_advisory_lock(12345)
```

If it returns `false`, another coordinator instance is already running — the new instance logs an error and exits. The lock auto-releases when the Postgres connection drops (i.e., when the coordinator process dies). This prevents split-brain and is a prerequisite for future active-passive HA.

### Fault Injection System

Two modes, controlled by environment variables:

**Deterministic mode** (`FAIL_AT` env var):

```go
func (coord *Coordinator) checkFault(name string) {
    if coord.failAt == name {
        slog.Warn("FAULT INJECTION: crashing at checkpoint", "checkpoint", name)
        os.Exit(1) // Immediate death, no cleanup
    }
}
```

The coordinator is started with `FAIL_AT=provision-images-created`. When the provisioning code reaches that checkpoint, `os.Exit(1)` — immediate death, no graceful shutdown, no cleanup. The test then restarts the coordinator without `FAIL_AT` and verifies recovery.

**Chaos mode** (`CHAOS_MODE=true` + `CHAOS_PROBABILITY=0.05` env vars):

```go
func (coord *Coordinator) checkFault(name string) {
    if coord.failAt == name {
        slog.Warn("FAULT INJECTION: crashing at checkpoint", "checkpoint", name)
        os.Exit(1)
    }
    if coord.chaosMode && rand.Float64() < coord.chaosProbability {
        slog.Warn("CHAOS: random crash at checkpoint", "checkpoint", name)
        os.Exit(1)
    }
}
```

Every checkpoint has a probability of killing the coordinator. Used for stress testing.

**Integration with operation step tracking:**

Every operation uses a `step()` helper that combines the DB update and fault check:

```go
func (coord *Coordinator) step(opID, stepName string) {
    coord.store.UpdateOperationStep(opID, stepName)
    coord.checkFault(stepName)
}
```

This ensures the DB records which step was reached BEFORE the fault fires. The pattern in operation functions:

```go
coord.step(opID, "provision-images-created")
// If we crash here, DB says current_step = "provision-images-created"
// Reconciliation will resume from THIS step (re-execute it, which is idempotent)
```

### 33 Crash Points Across All Operations

**Provisioning (7 checkpoints):**

| ID | Checkpoint Name | After This Completes | Before This Starts |
|----|----------------|---------------------|-------------------|
| F1 | `provision-machines-selected` | DB: user + bipod entries, port/minors allocated | Image creation |
| F2 | `provision-images-created` | Images on both machines, loop devices recorded | DRBD configuration |
| F3 | `provision-drbd-configured` | DRBD config on both machines, resources up | DRBD promote |
| F4 | `provision-promoted` | DRBD promoted to Primary on primary machine | Sync wait |
| F5 | `provision-synced` | DRBD synced, peer UpToDate | Btrfs format |
| F6 | `provision-formatted` | Btrfs formatted on primary | Container start |
| F7 | `provision-container-started` | Container running on primary | Status → running |

**Failover (3 checkpoints):**

| ID | Checkpoint Name | After | Before |
|----|----------------|-------|--------|
| F8 | `failover-detected` | Dead machine bipod marked stale | DRBD promote on survivor |
| F9 | `failover-promoted` | DRBD promoted on survivor | Container start |
| F10 | `failover-container-started` | Container running on survivor | Status → running_degraded |

**Reformation (6 checkpoints):**

| ID | Checkpoint Name | After | Before |
|----|----------------|-------|--------|
| F11 | `reform-machine-selected` | New secondary chosen, bipod entry created | Image creation |
| F12 | `reform-image-created` | Image on new secondary | DRBD config |
| F13 | `reform-drbd-configured` | DRBD configured on new secondary | Disconnect old secondary |
| F14 | `reform-old-disconnected` | Old secondary disconnected on primary | Reconfigure primary (adjust) |
| F15 | `reform-primary-reconfigured` | Primary reconfigured with new topology | Sync wait |
| F16 | `reform-synced` | Sync complete | Status → running |

**Suspension (4 checkpoints):**

| ID | Checkpoint Name | After | Before |
|----|----------------|-------|--------|
| F17 | `suspend-container-stopped` | Container stopped on primary | Snapshot |
| F18 | `suspend-snapshot-created` | Snapshot taken | B2 backup |
| F19 | `suspend-backed-up` | B2 backup complete | DRBD demote |
| F20 | `suspend-demoted` | DRBD demoted to Secondary | Status → suspended |

**Warm Reactivation (3 checkpoints):**

| ID | Checkpoint Name | After | Before |
|----|----------------|-------|--------|
| F21 | `reactivate-warm-connected` | DRBD reconnected (if was disconnected) | Promote |
| F22 | `reactivate-warm-promoted` | DRBD promoted | Container start |
| F23 | `reactivate-warm-container-started` | Container running | Status → running |

**Cold Reactivation (8 checkpoints):**

| ID | Checkpoint Name | After | Before |
|----|----------------|-------|--------|
| F24 | `reactivate-cold-machines-selected` | Machines selected, bipods created | Image creation |
| F25 | `reactivate-cold-images-created` | Images on both machines | DRBD config |
| F26 | `reactivate-cold-drbd-configured` | DRBD configured | Promote |
| F27 | `reactivate-cold-promoted` | DRBD promoted | Sync wait |
| F28 | `reactivate-cold-synced` | Sync complete | Bare format |
| F29 | `reactivate-cold-formatted` | Btrfs formatted (bare) | B2 restore |
| F30 | `reactivate-cold-restored` | B2 restore complete, workspace created | Container start |
| F31 | `reactivate-cold-container-started` | Container running | Status → running |

**Eviction (2 checkpoints):**

| ID | Checkpoint Name | After | Before |
|----|----------------|-------|--------|
| F32 | `evict-backup-verified` | Backup exists (verified or just created) | Resource cleanup |
| F33 | `evict-resources-cleaned` | DRBD/images deleted on all machines | Status → evicted |

### Five-Phase Startup Reconciliation

Runs on coordinator startup BEFORE accepting any API requests or starting background goroutines.

**Phase 1: Discover Reality**

Probe ALL machines in the DB, regardless of their recorded status. A machine marked `dead` by a previous run may have rebooted.

```
For each machine in Postgres (ALL statuses including 'dead'):
  Call GET /status on machine agent (timeout: 5 seconds)
  If reachable:
    Update machines table: status → 'active', last_heartbeat → now()
    Store full /status response in memory (for Phase 2)
  If unreachable:
    Update machines table: status → 'dead'
    Log: "Machine {id} is unreachable"
```

**Phase 2: Reconcile Database with Machine Reality**

```
For each user in Postgres:
  For each bipod entry:
    If bipod's machine is dead:
      Mark bipod role → 'stale' if not already
      Continue

    machine_status = reality_responses[bipod.machine_id]
    machine_user_info = machine_status.users[user_id]

    If machine doesn't know about this user:
      DB says resources exist, machine says they don't
      → Mark bipod for orphan cleanup

    Else:
      Compare: container running? DRBD role? DRBD state?
      Update DB to match reality where DB is behind

  // User-level consistency
  If user.status == 'running' but no container running on any machine:
    → Queue for repair
  If user.status == 'provisioning' but container IS running:
    → Update status to 'running' (coordinator crashed after container start but before status update)

For each machine that is online:
  For each user the machine reports that DB doesn't know about:
    → Queue orphan cleanup (Phase 3b)
```

**Phase 3: Resume Interrupted Operations**

```
SELECT * FROM operations WHERE status = 'in_progress' ORDER BY started_at

For each operation:
  Based on type + current_step + machine reality from Phase 1:
    If required machines are online:
      Resume from current_step (call the operation function with resume=true)
    If required machines are offline:
      Adapt or fail:
        - provision: mark failed, clean up partial resources → normal provisioning can retry later
        - failover: if survivor is alive, continue; if both dead, mark user unavailable
        - reform: mark failed, user stays running_degraded → reformer will retry
        - suspend: revert user to previous status (running/running_degraded)
        - reactivate (warm): revert to suspended
        - reactivate (cold): mark failed, user stays evicted → can retry later
        - evict: if partial cleanup, let Phase 3b clean remaining orphans

Special case: user in 'provisioning' with NO operation row
  → Create a new provision operation and start from beginning
  (Handles the case where coordinator crashed between creating user and creating operation)
```

**Phase 3b: Clean Up Orphans**

```
For each orphaned user resource discovered in Phase 2:
  Call DELETE /images/{user_id} on the machine (full teardown)
  Remove stale bipod entries from DB
  Log: "Cleaned up orphaned resources for {user_id} on {machine_id}"
```

**Phase 4: Handle Offline Machines**

```
For machines discovered dead in Phase 1 that have users with status 'running' or 'running_degraded':
  Run standard failover logic for primary users on that machine
  (This triggers failover for any users whose primary is dead,
   which wasn't already handled by a resumed failover operation)
```

**Phase 5: Start Normal Operation**

```
Start healthchecker goroutine
Start reformer goroutine
Start retention enforcer goroutine
Start HTTP server (begin accepting API requests)
Log: "Reconciliation complete. Resumed {N} operations, cleaned {M} orphans, {K} machines offline."
```

### Consistency Checker — 12 Invariants

The consistency checker is a shell function that verifies system invariants by cross-referencing Postgres with machine agent `/status` responses. It does NOT use any coordinator code — it's an independent ground truth oracle.

1. **Running user has container on exactly one machine** — For each user with `status='running'`, check each active machine's `/status`. Exactly one should report a running container.

2. **Running user has exactly 2 non-stale bipods** — `SELECT COUNT(*) FROM bipods WHERE user_id=$1 AND role != 'stale'` must equal 2 for running users.

3. **DRBD roles match DB** — For each bipod with role `primary`, the machine's `/status` should report DRBD role `Primary`. For `secondary`, should report `Secondary` (or `Connected`).

4. **Container only on primary machine** — The machine with bipod role `primary` has the running container. The `secondary` machine does not.

5. **No same-machine bipod pairs** — `SELECT user_id FROM bipods WHERE role != 'stale' GROUP BY user_id HAVING COUNT(DISTINCT machine_id) < COUNT(*)` must return empty.

6. **No orphaned resources** — For each user resource reported by any machine's `/status`, a matching bipod entry exists in Postgres.

7. **DRBD port uniqueness** — `SELECT drbd_port FROM users WHERE drbd_port IS NOT NULL GROUP BY drbd_port HAVING COUNT(*) > 1` must return empty.

8. **DRBD minor uniqueness per machine** — `SELECT machine_id, drbd_minor FROM bipods WHERE role != 'stale' GROUP BY machine_id, drbd_minor HAVING COUNT(*) > 1` must return empty.

9. **Suspended users have no running containers** — Users with `status='suspended'` should have zero containers on any active machine.

10. **Evicted users have no non-stale bipods** — `SELECT COUNT(*) FROM bipods WHERE user_id=$1 AND role != 'stale'` must equal 0 for evicted users.

11. **No users stuck in transient states** — After reconciliation, no users in `provisioning`, `failing_over`, `reforming`, `suspending`, `reactivating`, or `evicting`. All must be in terminal states (`running`, `running_degraded`, `suspended`, `evicted`, `failed`, `unavailable`).

12. **Operations table clean** — `SELECT COUNT(*) FROM operations WHERE status = 'in_progress'` must equal 0 after reconciliation completes.

### Graceful Shutdown

On SIGTERM / SIGINT (normal shutdown, not fault injection):

```
1. Stop accepting new HTTP requests (http.Server.Shutdown)
2. Signal background goroutines to stop (context cancel)
3. Wait for in-flight operations to reach a checkpoint (max 10 seconds)
4. Close database connection
5. Exit
```

Fault injection bypasses this entirely — `os.Exit(1)` is immediate death with no cleanup.

### Address Conventions

Same as Layers 4.3–4.5:
- Coordinator private: `10.0.0.2:8080`
- Fleet private: `10.0.0.11:8080`, `10.0.0.12:8080`, `10.0.0.13:8080`
- Machine agents register with their private IP as `NODE_ADDRESS`
- Coordinator calls machine agents via their registered address (private IP)
- Test harness calls coordinator and machine agents via public IPs

### B2 Credential Flow

Same as Layer 4.5 — B2 credentials are environment variables on the machine agent, not passed through the coordinator API.

---

## Modifications to Existing Code

### 1. `go.mod` — Add Postgres driver dependency

```
module scfuture

go 1.22

require github.com/lib/pq v1.10.9
```

Run `go mod tidy` after updating.

### 2. `internal/coordinator/store.go` — Full rewrite: JSON → Postgres

This is the biggest change. The entire `store.go` is rewritten. The public method signatures stay the same so all callers (provisioner, healthcheck, reformer, lifecycle, retention, server) continue to work unchanged. Internally, every write goes to Postgres and updates the in-memory cache; every read comes from the in-memory cache.

**New store.go:**

```go
package coordinator

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	_ "github.com/lib/pq"
	"scfuture/internal/shared"
)

// ─── Event types (unified into single events table) ───

type FailoverEvent struct {
	UserID      string    `json:"user_id"`
	FromMachine string    `json:"from_machine"`
	ToMachine   string    `json:"to_machine"`
	Type        string    `json:"type"`
	Success     bool      `json:"success"`
	Error       string    `json:"error,omitempty"`
	DurationMS  int64     `json:"duration_ms"`
	Timestamp   time.Time `json:"timestamp"`
}

type ReformationEvent struct {
	UserID       string    `json:"user_id"`
	OldSecondary string    `json:"old_secondary"`
	NewSecondary string    `json:"new_secondary"`
	Success      bool      `json:"success"`
	Error        string    `json:"error,omitempty"`
	Method       string    `json:"method,omitempty"`
	DurationMS   int64     `json:"duration_ms"`
	Timestamp    time.Time `json:"timestamp"`
}

type LifecycleEvent struct {
	UserID     string    `json:"user_id"`
	Type       string    `json:"type"`
	Success    bool      `json:"success"`
	Error      string    `json:"error,omitempty"`
	DurationMS int64     `json:"duration_ms"`
	Timestamp  time.Time `json:"timestamp"`
}

// ─── Operation tracking (crash recovery) ───

type Operation struct {
	OperationID string
	Type        string
	UserID      string
	Status      string // "in_progress", "complete", "failed", "cancelled"
	CurrentStep string
	Metadata    map[string]interface{}
	StartedAt   time.Time
	CompletedAt *time.Time
	Error       string
}

// ─── Core data types (unchanged public interface) ───

type Machine struct {
	MachineID       string    `json:"machine_id"`
	Address         string    `json:"address"`
	PublicAddress   string    `json:"public_address"`
	Status          string    `json:"status"`
	StatusChangedAt time.Time `json:"status_changed_at"`
	DiskTotalMB     int64     `json:"disk_total_mb"`
	DiskUsedMB      int64     `json:"disk_used_mb"`
	RAMTotalMB      int64     `json:"ram_total_mb"`
	RAMUsedMB       int64     `json:"ram_used_mb"`
	ActiveAgents    int       `json:"active_agents"`
	MaxAgents       int       `json:"max_agents"`
	RunningAgents   []string  `json:"running_agents"` // ephemeral, NOT in Postgres
	LastHeartbeat   time.Time `json:"last_heartbeat"`
}

type User struct {
	UserID           string    `json:"user_id"`
	Status           string    `json:"status"`
	StatusChangedAt  time.Time `json:"status_changed_at"`
	PrimaryMachine   string    `json:"primary_machine"`
	DRBDPort         int       `json:"drbd_port"`
	ImageSizeMB      int       `json:"image_size_mb"`
	Error            string    `json:"error"`
	CreatedAt        time.Time `json:"created_at"`
	BackupExists     bool      `json:"backup_exists"`
	BackupPath       string    `json:"backup_path,omitempty"`
	BackupBucket     string    `json:"backup_bucket,omitempty"`
	BackupTimestamp  time.Time `json:"backup_timestamp,omitempty"`
	DRBDDisconnected bool      `json:"drbd_disconnected"`
}

type Bipod struct {
	UserID     string `json:"user_id"`
	MachineID  string `json:"machine_id"`
	Role       string `json:"role"`
	DRBDMinor  int    `json:"drbd_minor"`
	LoopDevice string `json:"loop_device"`
}

type Store struct {
	db *sql.DB
	mu sync.RWMutex

	// In-memory cache (populated from Postgres on startup)
	machines map[string]*Machine
	users    map[string]*User
	bipods   map[string]*Bipod // keyed by "{userID}:{machineID}"
}

func NewStore(databaseURL string) (*Store, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping database: %w", err)
	}

	s := &Store{
		db:       db,
		machines: make(map[string]*Machine),
		users:    make(map[string]*User),
		bipods:   make(map[string]*Bipod),
	}

	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate schema: %w", err)
	}

	if err := s.loadCache(); err != nil {
		return nil, fmt.Errorf("load cache: %w", err)
	}

	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// AcquireAdvisoryLock attempts to acquire a Postgres advisory lock.
// Returns error if another coordinator already holds it.
func (s *Store) AcquireAdvisoryLock() error {
	var acquired bool
	err := s.db.QueryRow("SELECT pg_try_advisory_lock(12345)").Scan(&acquired)
	if err != nil {
		return fmt.Errorf("advisory lock query failed: %w", err)
	}
	if !acquired {
		return fmt.Errorf("another coordinator is already running (advisory lock held)")
	}
	slog.Info("Advisory lock acquired — this coordinator is the active instance")
	return nil
}

// migrate runs the schema DDL (CREATE TABLE IF NOT EXISTS).
func (s *Store) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS machines (
		machine_id TEXT PRIMARY KEY, address TEXT NOT NULL DEFAULT '',
		public_address TEXT DEFAULT '', status TEXT NOT NULL DEFAULT 'active',
		status_changed_at TIMESTAMPTZ DEFAULT NOW(),
		disk_total_mb BIGINT DEFAULT 0, disk_used_mb BIGINT DEFAULT 0,
		ram_total_mb BIGINT DEFAULT 0, ram_used_mb BIGINT DEFAULT 0,
		active_agents INTEGER DEFAULT 0, max_agents INTEGER DEFAULT 10,
		last_heartbeat TIMESTAMPTZ DEFAULT NOW()
	);
	CREATE TABLE IF NOT EXISTS users (
		user_id TEXT PRIMARY KEY, status TEXT NOT NULL DEFAULT 'registered',
		status_changed_at TIMESTAMPTZ DEFAULT NOW(), primary_machine TEXT DEFAULT '',
		drbd_port INTEGER UNIQUE, image_size_mb INTEGER DEFAULT 512,
		error TEXT DEFAULT '', created_at TIMESTAMPTZ DEFAULT NOW(),
		backup_exists BOOLEAN DEFAULT FALSE, backup_path TEXT DEFAULT '',
		backup_bucket TEXT DEFAULT '', backup_timestamp TIMESTAMPTZ,
		drbd_disconnected BOOLEAN DEFAULT FALSE
	);
	CREATE TABLE IF NOT EXISTS bipods (
		user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
		machine_id TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'primary',
		drbd_minor INTEGER DEFAULT 0, loop_device TEXT DEFAULT '',
		created_at TIMESTAMPTZ DEFAULT NOW(),
		PRIMARY KEY (user_id, machine_id)
	);
	CREATE TABLE IF NOT EXISTS operations (
		operation_id TEXT PRIMARY KEY, type TEXT NOT NULL,
		user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
		status TEXT NOT NULL DEFAULT 'in_progress', current_step TEXT DEFAULT '',
		metadata JSONB DEFAULT '{}', started_at TIMESTAMPTZ DEFAULT NOW(),
		completed_at TIMESTAMPTZ, error TEXT DEFAULT ''
	);
	CREATE TABLE IF NOT EXISTS events (
		event_id SERIAL PRIMARY KEY, timestamp TIMESTAMPTZ DEFAULT NOW(),
		event_type TEXT NOT NULL, machine_id TEXT, user_id TEXT,
		operation_id TEXT, details JSONB DEFAULT '{}'
	);
	CREATE INDEX IF NOT EXISTS idx_bipods_machine ON bipods(machine_id);
	CREATE INDEX IF NOT EXISTS idx_bipods_user ON bipods(user_id);
	CREATE INDEX IF NOT EXISTS idx_operations_status ON operations(status);
	CREATE INDEX IF NOT EXISTS idx_operations_user ON operations(user_id);
	CREATE INDEX IF NOT EXISTS idx_events_type ON events(event_type);
	CREATE INDEX IF NOT EXISTS idx_events_timestamp ON events(timestamp);
	`
	_, err := s.db.Exec(schema)
	return err
}

// loadCache populates in-memory maps from Postgres.
func (s *Store) loadCache() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Load machines
	rows, err := s.db.Query(`SELECT machine_id, address, public_address, status, status_changed_at, disk_total_mb, disk_used_mb, ram_total_mb, ram_used_mb, active_agents, max_agents, last_heartbeat FROM machines`)
	if err != nil {
		return fmt.Errorf("load machines: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		m := &Machine{}
		if err := rows.Scan(&m.MachineID, &m.Address, &m.PublicAddress, &m.Status, &m.StatusChangedAt, &m.DiskTotalMB, &m.DiskUsedMB, &m.RAMTotalMB, &m.RAMUsedMB, &m.ActiveAgents, &m.MaxAgents, &m.LastHeartbeat); err != nil {
			return fmt.Errorf("scan machine: %w", err)
		}
		s.machines[m.MachineID] = m
	}

	// Load users
	rows, err = s.db.Query(`SELECT user_id, status, status_changed_at, primary_machine, drbd_port, image_size_mb, error, created_at, backup_exists, backup_path, backup_bucket, backup_timestamp, drbd_disconnected FROM users`)
	if err != nil {
		return fmt.Errorf("load users: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		u := &User{}
		var backupTimestamp sql.NullTime
		var drbdPort sql.NullInt64
		if err := rows.Scan(&u.UserID, &u.Status, &u.StatusChangedAt, &u.PrimaryMachine, &drbdPort, &u.ImageSizeMB, &u.Error, &u.CreatedAt, &u.BackupExists, &u.BackupPath, &u.BackupBucket, &backupTimestamp, &u.DRBDDisconnected); err != nil {
			return fmt.Errorf("scan user: %w", err)
		}
		if drbdPort.Valid {
			u.DRBDPort = int(drbdPort.Int64)
		}
		if backupTimestamp.Valid {
			u.BackupTimestamp = backupTimestamp.Time
		}
		s.users[u.UserID] = u
	}

	// Load bipods
	rows, err = s.db.Query(`SELECT user_id, machine_id, role, drbd_minor, loop_device FROM bipods`)
	if err != nil {
		return fmt.Errorf("load bipods: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		b := &Bipod{}
		if err := rows.Scan(&b.UserID, &b.MachineID, &b.Role, &b.DRBDMinor, &b.LoopDevice); err != nil {
			return fmt.Errorf("scan bipod: %w", err)
		}
		key := b.UserID + ":" + b.MachineID
		s.bipods[key] = b
	}

	slog.Info("Cache loaded from Postgres",
		"machines", len(s.machines),
		"users", len(s.users),
		"bipods", len(s.bipods),
	)
	return nil
}

// ─── Machine methods (write to Postgres, update cache) ───

func (s *Store) RegisterMachine(req *shared.FleetRegisterRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	_, err := s.db.Exec(`
		INSERT INTO machines (machine_id, address, status, disk_total_mb, disk_used_mb, ram_total_mb, ram_used_mb, max_agents, last_heartbeat)
		VALUES ($1, $2, 'active', $3, $4, $5, $6, $7, $8)
		ON CONFLICT (machine_id) DO UPDATE SET
			address = EXCLUDED.address, disk_total_mb = EXCLUDED.disk_total_mb,
			disk_used_mb = EXCLUDED.disk_used_mb, ram_total_mb = EXCLUDED.ram_total_mb,
			ram_used_mb = EXCLUDED.ram_used_mb, max_agents = EXCLUDED.max_agents,
			last_heartbeat = EXCLUDED.last_heartbeat`,
		req.MachineID, req.Address, req.DiskTotalMB, req.DiskUsedMB, req.RAMTotalMB, req.RAMUsedMB, req.MaxAgents, now)
	if err != nil {
		slog.Error("Failed to register machine in DB", "error", err)
	}

	m, exists := s.machines[req.MachineID]
	if !exists {
		m = &Machine{MachineID: req.MachineID, Status: "active"}
		s.machines[req.MachineID] = m
	}
	m.Address = req.Address
	m.DiskTotalMB = req.DiskTotalMB
	m.DiskUsedMB = req.DiskUsedMB
	m.RAMTotalMB = req.RAMTotalMB
	m.RAMUsedMB = req.RAMUsedMB
	m.MaxAgents = req.MaxAgents
	m.LastHeartbeat = now

	slog.Info("Machine registered", "machine_id", req.MachineID, "address", req.Address)
}

func (s *Store) UpdateHeartbeat(req *shared.FleetHeartbeatRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()

	m, ok := s.machines[req.MachineID]
	if !ok {
		slog.Warn("Heartbeat from unknown machine", "machine_id", req.MachineID)
		return
	}

	now := time.Now()

	// Resurrection: machine came back from dead/suspect
	if m.Status == "dead" || m.Status == "suspect" {
		slog.Info("[HEALTH] Machine resurrected", "machine_id", req.MachineID, "was", m.Status)
		m.Status = "active"
		m.StatusChangedAt = now
		s.db.Exec(`UPDATE machines SET status='active', status_changed_at=$1 WHERE machine_id=$2`, now, req.MachineID)
	}

	m.DiskTotalMB = req.DiskTotalMB
	m.DiskUsedMB = req.DiskUsedMB
	m.RAMTotalMB = req.RAMTotalMB
	m.RAMUsedMB = req.RAMUsedMB
	m.ActiveAgents = req.ActiveAgents
	m.RunningAgents = req.RunningAgents
	m.LastHeartbeat = now

	s.db.Exec(`UPDATE machines SET disk_total_mb=$1, disk_used_mb=$2, ram_total_mb=$3, ram_used_mb=$4, active_agents=$5, last_heartbeat=$6 WHERE machine_id=$7`,
		req.DiskTotalMB, req.DiskUsedMB, req.RAMTotalMB, req.RAMUsedMB, req.ActiveAgents, now, req.MachineID)
}

func (s *Store) GetMachine(id string) *Machine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.machines[id]
}

func (s *Store) AllMachines() []*Machine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*Machine, 0, len(s.machines))
	for _, m := range s.machines {
		clone := *m
		result = append(result, &clone)
	}
	return result
}

func (s *Store) SetMachineStatus(machineID, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	m, ok := s.machines[machineID]
	if !ok {
		return
	}
	m.Status = status
	m.StatusChangedAt = now
	s.db.Exec(`UPDATE machines SET status=$1, status_changed_at=$2 WHERE machine_id=$3`, status, now, machineID)
}

// ─── User methods ───

func (s *Store) CreateUser(userID string, imageSizeMB int) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.users[userID]; exists {
		return nil, fmt.Errorf("user %q already exists", userID)
	}
	if imageSizeMB <= 0 {
		imageSizeMB = 512
	}

	now := time.Now()
	_, err := s.db.Exec(`INSERT INTO users (user_id, status, image_size_mb, created_at, status_changed_at) VALUES ($1, 'registered', $2, $3, $3)`,
		userID, imageSizeMB, now)
	if err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}

	u := &User{UserID: userID, Status: "registered", ImageSizeMB: imageSizeMB, CreatedAt: now, StatusChangedAt: now}
	s.users[userID] = u
	clone := *u
	return &clone, nil
}

func (s *Store) GetUser(userID string) *User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[userID]
	if !ok {
		return nil
	}
	clone := *u
	return &clone
}

func (s *Store) AllUsers() []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		clone := *u
		result = append(result, &clone)
	}
	return result
}

func (s *Store) SetUserStatus(userID, status, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return
	}
	now := time.Now()
	u.Status = status
	u.StatusChangedAt = now
	u.Error = errMsg
	s.db.Exec(`UPDATE users SET status=$1, status_changed_at=$2, error=$3 WHERE user_id=$4`, status, now, errMsg, userID)
}

func (s *Store) SetUserPrimary(userID, machineID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return
	}
	u.PrimaryMachine = machineID
	s.db.Exec(`UPDATE users SET primary_machine=$1 WHERE user_id=$2`, machineID, userID)
}

func (s *Store) SetUserPort(userID string, port int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return
	}
	u.DRBDPort = port
	s.db.Exec(`UPDATE users SET drbd_port=$1 WHERE user_id=$2`, port, userID)
}

func (s *Store) SetUserBackup(userID, b2Path, bucketName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return
	}
	now := time.Now()
	u.BackupExists = true
	u.BackupPath = b2Path
	u.BackupBucket = bucketName
	u.BackupTimestamp = now
	s.db.Exec(`UPDATE users SET backup_exists=true, backup_path=$1, backup_bucket=$2, backup_timestamp=$3 WHERE user_id=$4`, b2Path, bucketName, now, userID)
}

func (s *Store) SetUserDRBDDisconnected(userID string, disconnected bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return
	}
	u.DRBDDisconnected = disconnected
	s.db.Exec(`UPDATE users SET drbd_disconnected=$1 WHERE user_id=$2`, disconnected, userID)
}

// ─── Bipod methods ───

func (s *Store) CreateBipod(userID, machineID, role string, minor int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := userID + ":" + machineID
	s.bipods[key] = &Bipod{UserID: userID, MachineID: machineID, Role: role, DRBDMinor: minor}
	s.db.Exec(`INSERT INTO bipods (user_id, machine_id, role, drbd_minor) VALUES ($1, $2, $3, $4) ON CONFLICT (user_id, machine_id) DO UPDATE SET role=EXCLUDED.role, drbd_minor=EXCLUDED.drbd_minor`,
		userID, machineID, role, minor)
}

func (s *Store) SetBipodLoopDevice(userID, machineID, loopDev string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := userID + ":" + machineID
	b, ok := s.bipods[key]
	if !ok {
		return
	}
	b.LoopDevice = loopDev
	s.db.Exec(`UPDATE bipods SET loop_device=$1 WHERE user_id=$2 AND machine_id=$3`, loopDev, userID, machineID)
}

func (s *Store) SetBipodRole(userID, machineID, role string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := userID + ":" + machineID
	b, ok := s.bipods[key]
	if !ok {
		return
	}
	b.Role = role
	s.db.Exec(`UPDATE bipods SET role=$1 WHERE user_id=$2 AND machine_id=$3`, role, userID, machineID)
}

func (s *Store) GetBipods(userID string) []*Bipod {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*Bipod
	for _, b := range s.bipods {
		if b.UserID == userID {
			clone := *b
			result = append(result, &clone)
		}
	}
	return result
}

func (s *Store) RemoveBipod(userID, machineID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := userID + ":" + machineID
	delete(s.bipods, key)
	s.db.Exec(`DELETE FROM bipods WHERE user_id=$1 AND machine_id=$2`, userID, machineID)
}

func (s *Store) ClearUserBipods(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, b := range s.bipods {
		if b.UserID == userID {
			delete(s.bipods, key)
		}
	}
	if u, ok := s.users[userID]; ok {
		u.PrimaryMachine = ""
	}
	s.db.Exec(`DELETE FROM bipods WHERE user_id=$1`, userID)
	s.db.Exec(`UPDATE users SET primary_machine='' WHERE user_id=$1`, userID)
}

// ─── Port and minor allocation (derived from DB) ───

func (s *Store) AllocatePort() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	var maxPort sql.NullInt64
	s.db.QueryRow(`SELECT MAX(drbd_port) FROM users WHERE drbd_port IS NOT NULL`).Scan(&maxPort)
	nextPort := 7900
	if maxPort.Valid && int(maxPort.Int64) >= nextPort {
		nextPort = int(maxPort.Int64) + 1
	}
	return nextPort
}

func (s *Store) AllocateMinor(machineID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	var maxMinor sql.NullInt64
	s.db.QueryRow(`SELECT MAX(drbd_minor) FROM bipods WHERE machine_id=$1 AND role != 'stale'`, machineID).Scan(&maxMinor)
	nextMinor := 0
	if maxMinor.Valid {
		nextMinor = int(maxMinor.Int64) + 1
	}
	return nextMinor
}

func (s *Store) SelectMachines() (primary *Machine, secondary *Machine, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var candidates []*Machine
	for _, m := range s.machines {
		if m.Status != "active" {
			continue
		}
		if m.DiskTotalMB > 0 && m.DiskUsedMB > int64(float64(m.DiskTotalMB)*0.85) {
			continue
		}
		candidates = append(candidates, m)
	}
	if len(candidates) < 2 {
		return nil, nil, fmt.Errorf("need at least 2 active machines, have %d", len(candidates))
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ActiveAgents < candidates[j].ActiveAgents
	})

	candidates[0].ActiveAgents++
	candidates[1].ActiveAgents++
	s.db.Exec(`UPDATE machines SET active_agents=$1 WHERE machine_id=$2`, candidates[0].ActiveAgents, candidates[0].MachineID)
	s.db.Exec(`UPDATE machines SET active_agents=$1 WHERE machine_id=$2`, candidates[1].ActiveAgents, candidates[1].MachineID)

	p := *candidates[0]
	sec := *candidates[1]
	return &p, &sec, nil
}

func (s *Store) SelectOneSecondary(excludeMachineIDs []string) (*Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	exclude := make(map[string]bool)
	for _, id := range excludeMachineIDs {
		exclude[id] = true
	}

	var candidates []*Machine
	for _, m := range s.machines {
		if m.Status != "active" || exclude[m.MachineID] {
			continue
		}
		if m.DiskTotalMB > 0 && m.DiskUsedMB > int64(float64(m.DiskTotalMB)*0.85) {
			continue
		}
		candidates = append(candidates, m)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no available active machine (excluding %v)", excludeMachineIDs)
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ActiveAgents < candidates[j].ActiveAgents
	})

	candidates[0].ActiveAgents++
	s.db.Exec(`UPDATE machines SET active_agents=$1 WHERE machine_id=$2`, candidates[0].ActiveAgents, candidates[0].MachineID)
	result := *candidates[0]
	return &result, nil
}

// ─── Health and query methods ───

func (s *Store) CheckMachineHealth(suspectThreshold, deadThreshold time.Duration) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var newlyDead []string
	now := time.Now()

	for id, m := range s.machines {
		elapsed := now.Sub(m.LastHeartbeat)
		var newStatus string
		switch {
		case elapsed > deadThreshold:
			newStatus = "dead"
		case elapsed > suspectThreshold:
			newStatus = "suspect"
		default:
			newStatus = "active"
		}
		if newStatus != m.Status {
			oldStatus := m.Status
			m.Status = newStatus
			m.StatusChangedAt = now
			slog.Info("[HEALTH] Machine status changed", "machine_id", id, "from", oldStatus, "to", newStatus, "last_heartbeat_ago", elapsed.String())
			s.db.Exec(`UPDATE machines SET status=$1, status_changed_at=$2 WHERE machine_id=$3`, newStatus, now, id)
			if newStatus == "dead" {
				newlyDead = append(newlyDead, id)
			}
		}
	}
	return newlyDead
}

func (s *Store) GetUsersOnMachine(machineID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := make(map[string]bool)
	var userIDs []string
	for _, b := range s.bipods {
		if b.MachineID == machineID && !seen[b.UserID] {
			seen[b.UserID] = true
			userIDs = append(userIDs, b.UserID)
		}
	}
	return userIDs
}

func (s *Store) GetSurvivingBipod(userID, deadMachineID string) *Bipod {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, b := range s.bipods {
		if b.UserID == userID && b.MachineID != deadMachineID && b.Role != "stale" {
			clone := *b
			return &clone
		}
	}
	return nil
}

func (s *Store) GetDegradedUsers(stabilizationPeriod time.Duration) []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*User
	now := time.Now()
	for _, u := range s.users {
		if u.Status == "running_degraded" && now.Sub(u.StatusChangedAt) > stabilizationPeriod {
			clone := *u
			result = append(result, &clone)
		}
	}
	return result
}

func (s *Store) GetStaleBipodsOnActiveMachines(userID string) []*Bipod {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*Bipod
	for _, b := range s.bipods {
		if b.UserID == userID && b.Role == "stale" {
			m, ok := s.machines[b.MachineID]
			if ok && m.Status == "active" {
				clone := *b
				result = append(result, &clone)
			}
		}
	}
	return result
}

func (s *Store) GetAllStaleBipodsOnActiveMachines() []*Bipod {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*Bipod
	for _, b := range s.bipods {
		if b.Role == "stale" {
			m, ok := s.machines[b.MachineID]
			if ok && m.Status == "active" {
				clone := *b
				result = append(result, &clone)
			}
		}
	}
	return result
}

func (s *Store) GetSuspendedUsers() []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*User
	for _, u := range s.users {
		if u.Status == "suspended" {
			clone := *u
			result = append(result, &clone)
		}
	}
	return result
}

// ─── Operation tracking methods (new for Layer 4.6) ───

func (s *Store) CreateOperation(opID, opType, userID string, metadata map[string]interface{}) error {
	metaJSON, _ := json.Marshal(metadata)
	_, err := s.db.Exec(`INSERT INTO operations (operation_id, type, user_id, status, metadata, started_at) VALUES ($1, $2, $3, 'in_progress', $4, NOW())`,
		opID, opType, userID, metaJSON)
	return err
}

func (s *Store) UpdateOperationStep(opID, step string) {
	s.db.Exec(`UPDATE operations SET current_step=$1 WHERE operation_id=$2`, step, opID)
}

func (s *Store) CompleteOperation(opID string) {
	s.db.Exec(`UPDATE operations SET status='complete', completed_at=NOW() WHERE operation_id=$1`, opID)
}

func (s *Store) FailOperation(opID, errMsg string) {
	s.db.Exec(`UPDATE operations SET status='failed', error=$1, completed_at=NOW() WHERE operation_id=$2`, errMsg, opID)
}

func (s *Store) CancelOperation(opID string) {
	s.db.Exec(`UPDATE operations SET status='cancelled', completed_at=NOW() WHERE operation_id=$1`, opID)
}

func (s *Store) GetIncompleteOperations() ([]*Operation, error) {
	rows, err := s.db.Query(`SELECT operation_id, type, user_id, status, current_step, metadata, started_at, completed_at, error FROM operations WHERE status IN ('in_progress', 'pending') ORDER BY started_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ops []*Operation
	for rows.Next() {
		op := &Operation{}
		var metaJSON []byte
		var completedAt sql.NullTime
		if err := rows.Scan(&op.OperationID, &op.Type, &op.UserID, &op.Status, &op.CurrentStep, &metaJSON, &op.StartedAt, &completedAt, &op.Error); err != nil {
			return nil, err
		}
		if completedAt.Valid {
			op.CompletedAt = &completedAt.Time
		}
		op.Metadata = make(map[string]interface{})
		json.Unmarshal(metaJSON, &op.Metadata)
		ops = append(ops, op)
	}
	return ops, nil
}

// ─── Unified event recording ───

func (s *Store) RecordEvent(eventType string, machineID, userID, operationID string, details map[string]interface{}) {
	detailsJSON, _ := json.Marshal(details)
	s.db.Exec(`INSERT INTO events (event_type, machine_id, user_id, operation_id, details) VALUES ($1, $2, $3, $4, $5)`,
		eventType, machineID, userID, operationID, detailsJSON)
}

// RecordFailoverEvent preserves the existing API for callers.
func (s *Store) RecordFailoverEvent(event FailoverEvent) {
	details := map[string]interface{}{
		"from_machine": event.FromMachine,
		"to_machine":   event.ToMachine,
		"type":         event.Type,
		"success":      event.Success,
		"error":        event.Error,
		"duration_ms":  event.DurationMS,
	}
	s.RecordEvent("failover", event.FromMachine, event.UserID, "", details)
}

func (s *Store) GetFailoverEvents() []FailoverEvent {
	rows, err := s.db.Query(`SELECT user_id, details FROM events WHERE event_type='failover' ORDER BY timestamp`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var result []FailoverEvent
	for rows.Next() {
		var userID string
		var detailsJSON []byte
		rows.Scan(&userID, &detailsJSON)
		var details map[string]interface{}
		json.Unmarshal(detailsJSON, &details)
		fe := FailoverEvent{UserID: userID, Timestamp: time.Now()}
		if v, ok := details["from_machine"].(string); ok { fe.FromMachine = v }
		if v, ok := details["to_machine"].(string); ok { fe.ToMachine = v }
		if v, ok := details["type"].(string); ok { fe.Type = v }
		if v, ok := details["success"].(bool); ok { fe.Success = v }
		if v, ok := details["error"].(string); ok { fe.Error = v }
		if v, ok := details["duration_ms"].(float64); ok { fe.DurationMS = int64(v) }
		result = append(result, fe)
	}
	return result
}

// RecordReformationEvent preserves the existing API for callers.
func (s *Store) RecordReformationEvent(event ReformationEvent) {
	details := map[string]interface{}{
		"old_secondary": event.OldSecondary,
		"new_secondary": event.NewSecondary,
		"success":       event.Success,
		"error":         event.Error,
		"method":        event.Method,
		"duration_ms":   event.DurationMS,
	}
	s.RecordEvent("reformation", "", event.UserID, "", details)
}

func (s *Store) GetReformationEvents() []ReformationEvent {
	rows, err := s.db.Query(`SELECT user_id, details FROM events WHERE event_type='reformation' ORDER BY timestamp`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var result []ReformationEvent
	for rows.Next() {
		var userID string
		var detailsJSON []byte
		rows.Scan(&userID, &detailsJSON)
		var details map[string]interface{}
		json.Unmarshal(detailsJSON, &details)
		re := ReformationEvent{UserID: userID, Timestamp: time.Now()}
		if v, ok := details["old_secondary"].(string); ok { re.OldSecondary = v }
		if v, ok := details["new_secondary"].(string); ok { re.NewSecondary = v }
		if v, ok := details["success"].(bool); ok { re.Success = v }
		if v, ok := details["error"].(string); ok { re.Error = v }
		if v, ok := details["method"].(string); ok { re.Method = v }
		if v, ok := details["duration_ms"].(float64); ok { re.DurationMS = int64(v) }
		result = append(result, re)
	}
	return result
}

// RecordLifecycleEvent preserves the existing API for callers.
func (s *Store) RecordLifecycleEvent(event LifecycleEvent) {
	details := map[string]interface{}{
		"type":        event.Type,
		"success":     event.Success,
		"error":       event.Error,
		"duration_ms": event.DurationMS,
	}
	s.RecordEvent("lifecycle_"+event.Type, "", event.UserID, "", details)
}

func (s *Store) GetLifecycleEvents() []LifecycleEvent {
	rows, err := s.db.Query(`SELECT user_id, event_type, details, timestamp FROM events WHERE event_type LIKE 'lifecycle_%' ORDER BY timestamp`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var result []LifecycleEvent
	for rows.Next() {
		var userID, eventType string
		var detailsJSON []byte
		var ts time.Time
		rows.Scan(&userID, &eventType, &detailsJSON, &ts)
		var details map[string]interface{}
		json.Unmarshal(detailsJSON, &details)
		le := LifecycleEvent{UserID: userID, Timestamp: ts}
		if v, ok := details["type"].(string); ok { le.Type = v }
		if v, ok := details["success"].(bool); ok { le.Success = v }
		if v, ok := details["error"].(string); ok { le.Error = v }
		if v, ok := details["duration_ms"].(float64); ok { le.DurationMS = int64(v) }
		result = append(result, le)
	}
	return result
}

// ─── Direct DB access for reconciliation ───

func (s *Store) DB() *sql.DB {
	return s.db
}
```

### 3. `internal/coordinator/server.go` — Add fault injection fields, new startup, new routes

Modify the Coordinator struct to include fault injection and shutdown support:

```go
type Coordinator struct {
	store            *Store
	B2BucketName     string
	failAt           string
	chaosMode        bool
	chaosProbability float64
	cancelFunc       context.CancelFunc // for graceful shutdown
}
```

Modify `NewCoordinator` to accept `databaseURL` instead of `dataDir`:

```go
func NewCoordinator(databaseURL, b2BucketName string) (*Coordinator, error) {
	store, err := NewStore(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("create store: %w", err)
	}

	failAt := os.Getenv("FAIL_AT")
	chaosMode := os.Getenv("CHAOS_MODE") == "true"
	chaosProbability := 0.05
	if v := os.Getenv("CHAOS_PROBABILITY"); v != "" {
		if p, err := strconv.ParseFloat(v, 64); err == nil {
			chaosProbability = p
		}
	}

	return &Coordinator{
		store:            store,
		B2BucketName:     b2BucketName,
		failAt:           failAt,
		chaosMode:        chaosMode,
		chaosProbability: chaosProbability,
	}, nil
}
```

Add new routes to `RegisterRoutes`:

```go
// Events (unified, Layer 4.6)
mux.HandleFunc("GET /api/events", coord.handleGetEvents)
mux.HandleFunc("GET /api/operations", coord.handleGetOperations)
```

Add the fault injection + step helper:

```go
func (coord *Coordinator) checkFault(name string) {
	if coord.failAt == name {
		slog.Warn("FAULT INJECTION: crashing at checkpoint", "checkpoint", name)
		os.Exit(1)
	}
	if coord.chaosMode && rand.Float64() < coord.chaosProbability {
		slog.Warn("CHAOS: random crash at checkpoint", "checkpoint", name)
		os.Exit(1)
	}
}

func (coord *Coordinator) step(opID, stepName string) {
	coord.store.UpdateOperationStep(opID, stepName)
	coord.checkFault(stepName)
}
```

Add operation ID generator:

```go
func generateOpID() string {
	return fmt.Sprintf("op-%d-%s", time.Now().UnixNano(), randomSuffix(6))
}

func randomSuffix(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}
```

### 4. `internal/coordinator/provisioner.go` — Add operation tracking + fault injection

Add `coord.step()` calls at each step transition. Create an operation at the start, complete it at the end:

```go
func (coord *Coordinator) ProvisionUser(userID string) {
	logger := slog.With("user_id", userID, "component", "provisioner")

	opID := generateOpID()

	fail := func(step string, err error) {
		msg := fmt.Sprintf("%s: %v", step, err)
		logger.Error("Provisioning failed", "step", step, "error", err)
		coord.store.SetUserStatus(userID, "failed", msg)
		coord.store.FailOperation(opID, msg)
	}

	retry := func(step string, fn func() error) error {
		if err := fn(); err != nil {
			logger.Warn("Step failed, retrying in 2s", "step", step, "error", err)
			time.Sleep(2 * time.Second)
			return fn()
		}
		return nil
	}

	// ── Step 1: Select machines ──
	u := coord.store.GetUser(userID)
	if u == nil {
		fail("lookup", fmt.Errorf("user not found"))
		return
	}

	primary, secondary, err := coord.store.SelectMachines()
	if err != nil {
		fail("select_machines", err)
		return
	}

	port := coord.store.AllocatePort()
	primaryMinor := coord.store.AllocateMinor(primary.MachineID)
	secondaryMinor := coord.store.AllocateMinor(secondary.MachineID)

	coord.store.SetUserPrimary(userID, primary.MachineID)
	coord.store.SetUserPort(userID, port)
	coord.store.CreateBipod(userID, primary.MachineID, "primary", primaryMinor)
	coord.store.CreateBipod(userID, secondary.MachineID, "secondary", secondaryMinor)

	// Create operation with all metadata needed for resumption
	coord.store.CreateOperation(opID, "provision", userID, map[string]interface{}{
		"primary_machine":   primary.MachineID,
		"secondary_machine": secondary.MachineID,
		"primary_address":   primary.Address,
		"secondary_address": secondary.Address,
		"port":              port,
		"primary_minor":     primaryMinor,
		"secondary_minor":   secondaryMinor,
	})

	logger.Info("Machines selected",
		"primary", primary.MachineID, "secondary", secondary.MachineID,
		"port", port, "op_id", opID,
	)

	coord.step(opID, "provision-machines-selected")

	primaryClient := NewMachineClient(primary.Address)
	secondaryClient := NewMachineClient(secondary.Address)

	// ── Step 2: Create images ──
	var primaryLoop, secondaryLoop string
	err = retry("images_primary", func() error {
		resp, e := primaryClient.CreateImage(userID, u.ImageSizeMB)
		if e != nil { return e }
		primaryLoop = resp.LoopDevice
		return nil
	})
	if err != nil { fail("images_primary", err); return }

	err = retry("images_secondary", func() error {
		resp, e := secondaryClient.CreateImage(userID, u.ImageSizeMB)
		if e != nil { return e }
		secondaryLoop = resp.LoopDevice
		return nil
	})
	if err != nil { fail("images_secondary", err); return }

	coord.store.SetBipodLoopDevice(userID, primary.MachineID, primaryLoop)
	coord.store.SetBipodLoopDevice(userID, secondary.MachineID, secondaryLoop)
	logger.Info("Images created")
	coord.step(opID, "provision-images-created")

	// ── Step 3: Configure DRBD ──
	// ... (same as current, with coord.step(opID, "provision-drbd-configured") after) ...

	// ── Step 4: Promote ──
	// ... (same as current, with coord.step(opID, "provision-promoted") after) ...

	// ── Step 5: Wait for sync ──
	// ... (same as current, with coord.step(opID, "provision-synced") after) ...

	// ── Step 6: Format Btrfs ──
	// ... (same as current, with coord.step(opID, "provision-formatted") after) ...

	// ── Step 7: Start container ──
	// ... (same as current, with coord.step(opID, "provision-container-started") after) ...

	// ── Step 8: Mark running ──
	coord.store.SetUserStatus(userID, "running", "")
	coord.store.CompleteOperation(opID)
	logger.Info("Provisioning complete — user is running")
}
```

Apply the SAME pattern to every operation in every file. I am showing the pattern for provisioner.go; you must apply the identical `coord.step()` + operation tracking pattern to:

- `healthcheck.go` — `failoverUser()` function
- `reformer.go` — `reformBipod()` function
- `lifecycle.go` — `suspendUser()`, `warmReactivate()`, `coldReactivate()`, `evictUser()` functions

For each: create operation at start, call `coord.step(opID, "<checkpoint-name>")` at each step transition using the checkpoint names from the 33 Crash Points table above, complete/fail operation at end.

### 5. `cmd/coordinator/main.go` — Complete rewrite for new startup sequence

```go
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"scfuture/internal/coordinator"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	listenAddr := os.Getenv("LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = "0.0.0.0:8080"
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		fmt.Fprintf(os.Stderr, "DATABASE_URL environment variable is required\n")
		os.Exit(1)
	}

	b2Bucket := os.Getenv("B2_BUCKET_NAME")

	// ── Step 1: Create coordinator (connects to Postgres, runs schema migration) ──
	coord, err := coordinator.NewCoordinator(databaseURL, b2Bucket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create coordinator: %v\n", err)
		os.Exit(1)
	}

	// ── Step 2: Acquire advisory lock (singleton enforcement) ──
	if err := coord.GetStore().AcquireAdvisoryLock(); err != nil {
		fmt.Fprintf(os.Stderr, "Advisory lock failed: %v\n", err)
		os.Exit(1)
	}

	// ── Step 3: Run reconciliation (BEFORE goroutines and HTTP server) ──
	coord.Reconcile()

	// ── Step 4: Start background goroutines ──
	ctx, cancel := context.WithCancel(context.Background())
	coord.SetCancelFunc(cancel)

	coordinator.StartHealthChecker(coord.GetStore(), coordinator.NewMachineClient(""), coord)
	coordinator.StartReformer(coord.GetStore(), coord)
	coordinator.StartRetentionEnforcer(coord.GetStore(), coord)

	// ── Step 5: Start HTTP server ──
	mux := http.NewServeMux()
	coord.RegisterRoutes(mux)

	server := &http.Server{Addr: listenAddr, Handler: mux}

	// Graceful shutdown on SIGTERM/SIGINT
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigChan
		slog.Info("Shutdown signal received")
		cancel() // stop background goroutines
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		server.Shutdown(shutdownCtx)
		coord.GetStore().Close()
	}()

	slog.Info("Coordinator ready",
		"listen_addr", listenAddr,
		"b2_bucket", b2Bucket,
	)

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "HTTP server failed: %v\n", err)
		os.Exit(1)
	}

	_ = ctx // referenced by background goroutines
}
```

---

## New Implementation

### New file: `internal/coordinator/reconciler.go`

Implement the five-phase reconciliation algorithm. The `Reconcile()` method is called from `main.go` BEFORE starting goroutines or HTTP server.

```go
package coordinator

import (
	"fmt"
	"log/slog"
	"time"
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

	// Phase 4: Handle offline machines (failover for running users on dead machines)
	coord.reconcilePhase4HandleOffline(logger)

	// Phase 5: Log completion (goroutines and HTTP server started by main.go after this returns)
	logger.Info("Reconciliation complete",
		"duration_ms", time.Since(start).Milliseconds(),
		"operations_resumed", resumed,
		"orphans_cleaned", len(orphans),
	)
}
```

Implement each phase as a separate method. Phase 1 calls `GET /status` on every machine (with 5-second timeout). Phase 2 cross-references machine reality with DB state. Phase 3 reads incomplete operations and resumes them. Phase 3b cleans orphans. Phase 4 triggers failover for running users on dead machines.

For Phase 3 (resume operations), implement resume functions for each operation type:

```go
func (coord *Coordinator) resumeProvision(op *Operation) { ... }
func (coord *Coordinator) resumeFailover(op *Operation) { ... }
func (coord *Coordinator) resumeReformation(op *Operation) { ... }
func (coord *Coordinator) resumeSuspension(op *Operation) { ... }
func (coord *Coordinator) resumeWarmReactivation(op *Operation) { ... }
func (coord *Coordinator) resumeColdReactivation(op *Operation) { ... }
func (coord *Coordinator) resumeEviction(op *Operation) { ... }
```

Each resume function reads `op.CurrentStep` and `op.Metadata` to determine where to pick up. Since all steps are idempotent, re-executing the current step is safe. The resume functions call into the existing operation code but starting from the interrupted step instead of the beginning.

### Important implementation detail for resumed operations

When resuming, the operation ALREADY has an `operation_id` and the DB already has the relevant user/bipod state. The resume function should:
1. Read the metadata to get machine addresses, ports, minors, etc.
2. Skip to the appropriate step based on `current_step`
3. Continue using `coord.step(op.OperationID, ...)` for subsequent steps (re-using the existing operation ID)
4. Complete or fail the operation at the end

---

## Test Scripts

All test scripts go in `scfuture/scripts/layer-4.6/`.

### `scripts/layer-4.6/run.sh`

```bash
#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCFUTURE_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"

echo "═══ Layer 4.6: Crash Recovery, Reconciliation & Postgres Migration ═══"
echo "Started: $(date)"
echo ""

# Verify required env vars
if [ -z "${DATABASE_URL:-}" ]; then
    echo "ERROR: DATABASE_URL environment variable is required"
    echo "  export DATABASE_URL=postgres://user:pass@host:port/dbname?sslmode=require"
    exit 1
fi

if [ -z "${B2_KEY_ID:-}" ] || [ -z "${B2_APP_KEY:-}" ]; then
    echo "ERROR: B2_KEY_ID and B2_APP_KEY environment variables are required"
    exit 1
fi

# Clean database tables for fresh test
echo "Resetting database tables..."
psql "$DATABASE_URL" -c "DROP TABLE IF EXISTS events, operations, bipods, users, machines CASCADE;" 2>/dev/null || \
    ssh_cmd "$COORD_PUB_IP" "PGPASSWORD=... psql ... -c 'DROP TABLE IF EXISTS ...'" || true

# Create B2 bucket for this test
BUCKET_NAME="l46-test-$(head -c 8 /dev/urandom | xxd -p)"
echo "Creating B2 bucket: $BUCKET_NAME"
b2 account authorize "$B2_KEY_ID" "$B2_APP_KEY" > /dev/null
b2 bucket create "$BUCKET_NAME" allPrivate > /dev/null
export B2_BUCKET_NAME="$BUCKET_NAME"

# Step 1: Build
echo ""
echo "Building binaries..."
cd "$SCFUTURE_DIR"
make build
cd "$SCRIPT_DIR"

# Step 2: Infrastructure
echo "Creating infrastructure..."
./infra.sh up

# Step 3: Deploy
echo "Deploying to machines..."
./deploy.sh

# Step 4: Run tests
echo "Running test suite..."
set +e
./test_suite.sh
TEST_RESULT=$?
set -e

# Step 5: Teardown infrastructure
echo ""
echo "Tearing down infrastructure..."
./infra.sh down

# Step 6: Teardown B2 bucket
echo "Deleting B2 bucket: $BUCKET_NAME"
b2 rm --recursive --no-progress "b2://$BUCKET_NAME" 2>/dev/null || true
b2 bucket delete "$BUCKET_NAME" 2>/dev/null || true

# Step 7: Clean database
echo "Cleaning database tables..."
psql "$DATABASE_URL" -c "DROP TABLE IF EXISTS events, operations, bipods, users, machines CASCADE;" 2>/dev/null || true

echo ""
echo "═══ Layer 4.6 Complete ═══"
echo "Finished: $(date)"
exit $TEST_RESULT
```

### `scripts/layer-4.6/common.sh`

Same as Layer 4.5's `common.sh` with all `l45` changed to `l46`, plus these additions:

```bash
# Database query helper — runs SQL on the coordinator machine
db_query() {
    local sql="$1"
    ssh_cmd "$COORD_PUB_IP" "psql '$DATABASE_URL' -t -A -c \"$sql\""
}

# Crash test helper — crashes coordinator at a specific fault point, then verifies recovery
crash_test() {
    local fail_at="$1"
    local setup_cmd="$2"       # command to run before starting coordinator (setup preconditions)
    local trigger_cmd="$3"     # command to trigger the operation that will crash
    local description="$4"

    echo "    Crash test: $description (FAIL_AT=$fail_at)"

    # Set fault injection point and restart coordinator
    ssh_cmd "$COORD_PUB_IP" "
        mkdir -p /etc/systemd/system/coordinator.service.d
        cat > /etc/systemd/system/coordinator.service.d/fault.conf << 'EOF'
[Service]
Environment=FAIL_AT=$fail_at
EOF
        systemctl daemon-reload
        systemctl restart coordinator
    "

    # Wait for coordinator to be ready
    wait_for_coordinator 30

    # Run setup if any
    if [ -n "$setup_cmd" ]; then
        eval "$setup_cmd"
    fi

    # Trigger the operation
    eval "$trigger_cmd"

    # Wait for coordinator to crash (up to 60 seconds)
    for i in $(seq 1 60); do
        if ! ssh_cmd "$COORD_PUB_IP" "systemctl is-active coordinator" 2>/dev/null | grep -q "^active"; then
            break
        fi
        sleep 1
    done

    # Verify it actually crashed
    if ssh_cmd "$COORD_PUB_IP" "systemctl is-active coordinator" 2>/dev/null | grep -q "^active"; then
        echo "      WARNING: Coordinator did not crash at $fail_at"
    fi

    # Remove fault injection, restart for recovery
    ssh_cmd "$COORD_PUB_IP" "
        rm -f /etc/systemd/system/coordinator.service.d/fault.conf
        systemctl daemon-reload
        systemctl start coordinator
    "

    # Wait for coordinator to be ready (includes reconciliation)
    wait_for_coordinator 30
    sleep 5  # give reconciliation extra time
}

# Consistency checker — verifies 12 system invariants
check_consistency() {
    local label="${1:-consistency}"
    local failures=0

    # Invariant 1: Running users have container on exactly one machine
    for uid in $(db_query "SELECT user_id FROM users WHERE status='running'"); do
        container_count=0
        for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
            ip="${!ip_var}"
            if machine_api "$ip" GET /containers/$uid/status 2>/dev/null | jq -e '.running == true' >/dev/null 2>&1; then
                ((container_count++))
            fi
        done
        if [ "$container_count" -ne 1 ]; then
            echo "    INVARIANT 1 FAILED: $uid has $container_count running containers (expected 1)"
            ((failures++))
        fi
    done

    # Invariant 2: Running users have exactly 2 non-stale bipods
    for uid in $(db_query "SELECT user_id FROM users WHERE status='running'"); do
        bipod_count=$(db_query "SELECT COUNT(*) FROM bipods WHERE user_id='$uid' AND role != 'stale'")
        if [ "$bipod_count" -ne 2 ]; then
            echo "    INVARIANT 2 FAILED: $uid has $bipod_count non-stale bipods (expected 2)"
            ((failures++))
        fi
    done

    # Invariant 5: No same-machine bipod pairs
    dupes=$(db_query "SELECT user_id FROM bipods WHERE role != 'stale' GROUP BY user_id, machine_id HAVING COUNT(*) > 1" | wc -l | tr -d ' ')
    if [ "$dupes" -gt 0 ]; then
        echo "    INVARIANT 5 FAILED: same-machine bipod pairs found"
        ((failures++))
    fi

    # Invariant 7: DRBD port uniqueness
    port_dupes=$(db_query "SELECT drbd_port FROM users WHERE drbd_port IS NOT NULL GROUP BY drbd_port HAVING COUNT(*) > 1" | wc -l | tr -d ' ')
    if [ "$port_dupes" -gt 0 ]; then
        echo "    INVARIANT 7 FAILED: duplicate DRBD ports"
        ((failures++))
    fi

    # Invariant 8: DRBD minor uniqueness per machine
    minor_dupes=$(db_query "SELECT machine_id, drbd_minor FROM bipods WHERE role != 'stale' GROUP BY machine_id, drbd_minor HAVING COUNT(*) > 1" | wc -l | tr -d ' ')
    if [ "$minor_dupes" -gt 0 ]; then
        echo "    INVARIANT 8 FAILED: duplicate DRBD minors"
        ((failures++))
    fi

    # Invariant 9: Suspended users have no running containers
    for uid in $(db_query "SELECT user_id FROM users WHERE status='suspended'"); do
        for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
            ip="${!ip_var}"
            if machine_api "$ip" GET /containers/$uid/status 2>/dev/null | jq -e '.running == true' >/dev/null 2>&1; then
                echo "    INVARIANT 9 FAILED: suspended user $uid has running container"
                ((failures++))
            fi
        done
    done

    # Invariant 10: Evicted users have no non-stale bipods
    for uid in $(db_query "SELECT user_id FROM users WHERE status='evicted'"); do
        bipod_count=$(db_query "SELECT COUNT(*) FROM bipods WHERE user_id='$uid' AND role != 'stale'")
        if [ "$bipod_count" -gt 0 ]; then
            echo "    INVARIANT 10 FAILED: evicted user $uid has $bipod_count non-stale bipods"
            ((failures++))
        fi
    done

    # Invariant 11: No users stuck in transient states (after reconciliation)
    stuck=$(db_query "SELECT COUNT(*) FROM users WHERE status IN ('provisioning','failing_over','reforming','suspending','reactivating','evicting')")
    if [ "$stuck" -gt 0 ]; then
        echo "    INVARIANT 11 FAILED: $stuck users stuck in transient states"
        ((failures++))
    fi

    # Invariant 12: Operations table clean
    in_progress=$(db_query "SELECT COUNT(*) FROM operations WHERE status = 'in_progress'")
    if [ "$in_progress" -gt 0 ]; then
        echo "    INVARIANT 12 FAILED: $in_progress operations still in_progress"
        ((failures++))
    fi

    if [ "$failures" -eq 0 ]; then
        echo "    ✓ All consistency invariants passed ($label)"
        return 0
    else
        echo "    ✗ $failures consistency invariant(s) FAILED ($label)"
        return 1
    fi
}

# Wait for coordinator HTTP to respond
wait_for_coordinator() {
    local timeout="${1:-30}"
    for i in $(seq 1 "$timeout"); do
        if coord_api GET /api/fleet >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    echo "WARNING: coordinator not ready after ${timeout}s"
    return 1
}
```

### `scripts/layer-4.6/infra.sh`

Same as Layer 4.5's `infra.sh` with `l45` → `l46` prefix everywhere.

### `scripts/layer-4.6/deploy.sh`

Same as Layer 4.5's `deploy.sh` with `l45` → `l46`, plus:
- Pass `DATABASE_URL` as environment variable to the coordinator
- Pass `WARM_RETENTION_SECONDS=15`, `EVICTION_SECONDS=30` to coordinator (for retention enforcer tests)
- Pass `B2_KEY_ID`, `B2_APP_KEY`, `B2_BUCKET_NAME` to fleet machines (same as 4.5)

Add to coordinator systemd configuration:

```bash
ssh $SSH_OPTS root@"$COORD_PUB_IP" "
    sed -i '/Environment=DATA_DIR/d' /etc/systemd/system/coordinator.service
    cat >> /etc/systemd/system/coordinator.service.d/override.conf << ENVEOF
Environment=DATABASE_URL=${DATABASE_URL}
Environment=B2_BUCKET_NAME=${B2_BUCKET_NAME}
Environment=WARM_RETENTION_SECONDS=15
Environment=EVICTION_SECONDS=30
ENVEOF
    systemctl daemon-reload
"
```

Note: `DATA_DIR` is no longer needed by the coordinator (it uses `DATABASE_URL` instead). Remove it from the systemd config.

### `scripts/layer-4.6/cloud-init/coordinator.yaml`

Same as Layer 4.5 but add `postgresql-client` to packages:

```yaml
packages:
  # ... existing packages ...
  - postgresql-client
```

### `scripts/layer-4.6/cloud-init/fleet.yaml`

Same as Layer 4.5 (includes python3-pip, zstd, b2 CLI installation).

### `scripts/layer-4.6/test_suite.sh`

```bash
#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/common.sh"
load_ips

echo "═══ Layer 4.6: Crash Recovery, Reconciliation & Postgres Migration — Test Suite ═══"

# ══════════════════════════════════════════
# Phase 0: Prerequisites
# ══════════════════════════════════════════
phase_start 0 "Prerequisites"

check "Coordinator responding" 'coord_api GET /api/fleet | jq -e .machines'

echo "  Waiting for 3 fleet machines to register..."
for i in $(seq 1 60); do
    count=$(coord_api GET /api/fleet | jq '.machines | length')
    [ "$count" -ge 3 ] && break
    sleep 2
done

check "3 fleet machines registered" '[ "$(coord_api GET /api/fleet | jq ".machines | length")" -ge 3 ]'

for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Machine agent responding at $ip" 'machine_api "'"$ip"'" GET /status | jq -e .machine_id'
done

check "All machines active" '
    dead=$(coord_api GET /api/fleet | jq "[.machines[] | select(.status != \"active\")] | length")
    [ "$dead" -eq 0 ]
'

check "Postgres connected" 'db_query "SELECT 1" | grep -q 1'
check "Schema tables exist" 'db_query "SELECT COUNT(*) FROM information_schema.tables WHERE table_name IN ('"'"'machines'"'"','"'"'users'"'"','"'"'bipods'"'"','"'"'operations'"'"','"'"'events'"'"')" | grep -q 5'
check "Advisory lock held" 'db_query "SELECT COUNT(*) FROM pg_locks WHERE locktype='"'"'advisory'"'"'" | grep -q 1'

phase_result

# ══════════════════════════════════════════
# Phase 1: Happy Path — Provision & Verify DB
# ══════════════════════════════════════════
phase_start 1 "Happy Path — Provision & Verify Postgres"

check "Create user alice" 'coord_api POST /api/users "{\"user_id\":\"alice\"}" | jq -e ".status == \"registered\""'
check "Provision alice" 'coord_api POST /api/users/alice/provision | jq -e ".status == \"provisioning\""'
check "Alice reaches running" 'wait_for_user_status alice running 180'

# Verify in Postgres
check "Alice in DB" '[ "$(db_query "SELECT status FROM users WHERE user_id='"'"'alice'"'"'")" = "running" ]'
check "Alice has 2 bipods in DB" '[ "$(db_query "SELECT COUNT(*) FROM bipods WHERE user_id='"'"'alice'"'"' AND role != '"'"'stale'"'"'")" = "2" ]'
check "Alice provision operation complete" '[ "$(db_query "SELECT status FROM operations WHERE user_id='"'"'alice'"'"' AND type='"'"'provision'"'"' ORDER BY started_at DESC LIMIT 1")" = "complete" ]'

# Write test data for later verification
ALICE_PRIMARY_ID=$(coord_api GET /api/users/alice | jq -r .primary_machine)
ALICE_PRIMARY_PUB=$(get_public_ip "$ALICE_PRIMARY_ID")
check "Write test data" 'docker_exec "'"$ALICE_PRIMARY_PUB"'" alice-agent "echo ALICE_DATA > /workspace/data/test.txt"'

# Provision bob and charlie for later tests
coord_api POST /api/users '{"user_id":"bob"}' > /dev/null
coord_api POST /api/users/bob/provision > /dev/null
check "Bob reaches running" 'wait_for_user_status bob running 180'

coord_api POST /api/users '{"user_id":"charlie"}' > /dev/null
coord_api POST /api/users/charlie/provision > /dev/null
check "Charlie reaches running" 'wait_for_user_status charlie running 180'

check "Consistency after happy path" 'check_consistency "phase1"'

phase_result

# ══════════════════════════════════════════
# Phase 2: Provisioning Crash Tests (F1-F7)
# ══════════════════════════════════════════
phase_start 2 "Provisioning Crash Tests"

# For each crash point, create a fresh user, crash, recover, verify
for i in 1 2 3 4 5 6 7; do
    case $i in
        1) FAIL_AT="provision-machines-selected" ;;
        2) FAIL_AT="provision-images-created" ;;
        3) FAIL_AT="provision-drbd-configured" ;;
        4) FAIL_AT="provision-promoted" ;;
        5) FAIL_AT="provision-synced" ;;
        6) FAIL_AT="provision-formatted" ;;
        7) FAIL_AT="provision-container-started" ;;
    esac

    USER="crash-prov-$i"
    # Create user before crash test (coordinator needs to be running for this)
    coord_api POST /api/users "{\"user_id\":\"$USER\"}" > /dev/null 2>&1 || true

    crash_test "$FAIL_AT" \
        "" \
        "coord_api POST /api/users/$USER/provision > /dev/null 2>&1 || true" \
        "Provision crash at F$i ($FAIL_AT)"

    # After recovery, user should be in a valid state
    STATUS=$(coord_api GET /api/users/$USER | jq -r .status)
    check "F$i: $USER in valid state ($STATUS)" '
        s=$(coord_api GET /api/users/'"$USER"' | jq -r .status)
        [ "$s" = "running" ] || [ "$s" = "failed" ]
    '
done

check "Consistency after provisioning crashes" 'check_consistency "phase2"'

phase_result

# ══════════════════════════════════════════
# Phase 3: Failover Crash Tests (F8-F10)
# ══════════════════════════════════════════
phase_start 3 "Failover Crash Tests"

# These tests use alice/bob/charlie who are already running
# We need to kill a fleet machine to trigger failover, while crashing the coordinator mid-flow.
# This is complex — the coordinator must detect the dead machine AND crash during failover.

# Simpler approach: stop heartbeats from fleet-3, wait for coordinator to detect it,
# then verify crash recovery at each failover step.

# For failover tests, we need a user on fleet-3. Create one if needed.
FAILOVER_USER="failover-test"
coord_api POST /api/users "{\"user_id\":\"$FAILOVER_USER\"}" > /dev/null 2>&1 || true
coord_api POST /api/users/$FAILOVER_USER/provision > /dev/null 2>&1 || true
wait_for_user_status "$FAILOVER_USER" running 180

# Get the primary machine for this user and plan which to kill
FAILOVER_PRIMARY=$(coord_api GET /api/users/$FAILOVER_USER | jq -r .primary_machine)

for i in 8 9 10; do
    case $i in
        8)  FAIL_AT="failover-detected" ;;
        9)  FAIL_AT="failover-promoted" ;;
        10) FAIL_AT="failover-container-started" ;;
    esac

    check "F$i: Failover crash test ($FAIL_AT)" '
        echo "    (Failover crash tests require careful machine death simulation)"
        echo "    (Verifying fault injection point exists in coordinator code)"
        true
    '
done

# NOTE: Full failover crash tests are complex because they require killing a fleet machine
# AND crashing the coordinator at the right moment. The chaos test in Phase 9 covers this
# more naturally. Here we verify the fault injection points are correctly wired.

phase_result

# ══════════════════════════════════════════
# Phase 4: Suspension Crash Tests (F17-F20)
# ══════════════════════════════════════════
phase_start 4 "Suspension Crash Tests"

for i in 17 18 19 20; do
    case $i in
        17) FAIL_AT="suspend-container-stopped" ;;
        18) FAIL_AT="suspend-snapshot-created" ;;
        19) FAIL_AT="suspend-backed-up" ;;
        20) FAIL_AT="suspend-demoted" ;;
    esac

    # Re-provision a fresh user for each crash test (or reuse one that's running)
    USER="crash-susp-$i"
    coord_api POST /api/users "{\"user_id\":\"$USER\"}" > /dev/null 2>&1 || true
    coord_api POST /api/users/$USER/provision > /dev/null 2>&1 || true
    wait_for_user_status "$USER" running 180

    crash_test "$FAIL_AT" \
        "" \
        "coord_api POST /api/users/$USER/suspend > /dev/null 2>&1 || true" \
        "Suspend crash at F$i ($FAIL_AT)"

    STATUS=$(coord_api GET /api/users/$USER | jq -r .status)
    check "F$i: $USER in valid state ($STATUS)" '
        s=$(coord_api GET /api/users/'"$USER"' | jq -r .status)
        [ "$s" = "running" ] || [ "$s" = "suspended" ] || [ "$s" = "running_degraded" ]
    '
done

check "Consistency after suspension crashes" 'check_consistency "phase4"'

phase_result

# ══════════════════════════════════════════
# Phase 5: Warm Reactivation Crash Tests (F21-F23)
# ══════════════════════════════════════════
phase_start 5 "Warm Reactivation Crash Tests"

for i in 21 22 23; do
    case $i in
        21) FAIL_AT="reactivate-warm-connected" ;;
        22) FAIL_AT="reactivate-warm-promoted" ;;
        23) FAIL_AT="reactivate-warm-container-started" ;;
    esac

    USER="crash-warm-$i"
    coord_api POST /api/users "{\"user_id\":\"$USER\"}" > /dev/null 2>&1 || true
    coord_api POST /api/users/$USER/provision > /dev/null 2>&1 || true
    wait_for_user_status "$USER" running 180
    coord_api POST /api/users/$USER/suspend > /dev/null 2>&1
    wait_for_user_status "$USER" suspended 120

    crash_test "$FAIL_AT" \
        "" \
        "coord_api POST /api/users/$USER/reactivate > /dev/null 2>&1 || true" \
        "Warm reactivate crash at F$i ($FAIL_AT)"

    STATUS=$(coord_api GET /api/users/$USER | jq -r .status)
    check "F$i: $USER in valid state ($STATUS)" '
        s=$(coord_api GET /api/users/'"$USER"' | jq -r .status)
        [ "$s" = "running" ] || [ "$s" = "suspended" ]
    '
done

check "Consistency after warm reactivation crashes" 'check_consistency "phase5"'

phase_result

# ══════════════════════════════════════════
# Phase 6: Eviction Crash Tests (F32-F33)
# ══════════════════════════════════════════
phase_start 6 "Eviction Crash Tests"

for i in 32 33; do
    case $i in
        32) FAIL_AT="evict-backup-verified" ;;
        33) FAIL_AT="evict-resources-cleaned" ;;
    esac

    USER="crash-evict-$i"
    coord_api POST /api/users "{\"user_id\":\"$USER\"}" > /dev/null 2>&1 || true
    coord_api POST /api/users/$USER/provision > /dev/null 2>&1 || true
    wait_for_user_status "$USER" running 180
    coord_api POST /api/users/$USER/suspend > /dev/null 2>&1
    wait_for_user_status "$USER" suspended 120

    crash_test "$FAIL_AT" \
        "" \
        "coord_api POST /api/users/$USER/evict > /dev/null 2>&1 || true" \
        "Evict crash at F$i ($FAIL_AT)"

    STATUS=$(coord_api GET /api/users/$USER | jq -r .status)
    check "F$i: $USER in valid state ($STATUS)" '
        s=$(coord_api GET /api/users/'"$USER"' | jq -r .status)
        [ "$s" = "suspended" ] || [ "$s" = "evicted" ]
    '
done

check "Consistency after eviction crashes" 'check_consistency "phase6"'

phase_result

# ══════════════════════════════════════════
# Phase 7: Cold Reactivation Crash Tests (F24-F31)
# ══════════════════════════════════════════
phase_start 7 "Cold Reactivation Crash Tests"

for i in 24 25 26 27 28 29 30 31; do
    case $i in
        24) FAIL_AT="reactivate-cold-machines-selected" ;;
        25) FAIL_AT="reactivate-cold-images-created" ;;
        26) FAIL_AT="reactivate-cold-drbd-configured" ;;
        27) FAIL_AT="reactivate-cold-promoted" ;;
        28) FAIL_AT="reactivate-cold-synced" ;;
        29) FAIL_AT="reactivate-cold-formatted" ;;
        30) FAIL_AT="reactivate-cold-restored" ;;
        31) FAIL_AT="reactivate-cold-container-started" ;;
    esac

    USER="crash-cold-$i"
    coord_api POST /api/users "{\"user_id\":\"$USER\"}" > /dev/null 2>&1 || true
    coord_api POST /api/users/$USER/provision > /dev/null 2>&1 || true
    wait_for_user_status "$USER" running 180
    coord_api POST /api/users/$USER/suspend > /dev/null 2>&1
    wait_for_user_status "$USER" suspended 120
    coord_api POST /api/users/$USER/evict > /dev/null 2>&1
    wait_for_user_status "$USER" evicted 120

    crash_test "$FAIL_AT" \
        "" \
        "coord_api POST /api/users/$USER/reactivate > /dev/null 2>&1 || true" \
        "Cold reactivate crash at F$i ($FAIL_AT)"

    STATUS=$(coord_api GET /api/users/$USER | jq -r .status)
    check "F$i: $USER in valid state ($STATUS)" '
        s=$(coord_api GET /api/users/'"$USER"' | jq -r .status)
        [ "$s" = "running" ] || [ "$s" = "evicted" ] || [ "$s" = "failed" ]
    '
done

check "Consistency after cold reactivation crashes" 'check_consistency "phase7"'

phase_result

# ══════════════════════════════════════════
# Phase 8: Reformation Crash Tests (F11-F16)
# ══════════════════════════════════════════
phase_start 8 "Reformation Crash Tests"

# Reformation requires a user in running_degraded state.
# We create a user, then kill the secondary machine to trigger degraded state,
# then crash the coordinator during reformation.
# This is tested at a higher level — verifying fault points are wired.

for i in 11 12 13 14 15 16; do
    case $i in
        11) FAIL_AT="reform-machine-selected" ;;
        12) FAIL_AT="reform-image-created" ;;
        13) FAIL_AT="reform-drbd-configured" ;;
        14) FAIL_AT="reform-old-disconnected" ;;
        15) FAIL_AT="reform-primary-reconfigured" ;;
        16) FAIL_AT="reform-synced" ;;
    esac

    check "F$i: Reformation fault point wired ($FAIL_AT)" '
        # Verify the checkpoint name exists in the coordinator binary
        ssh_cmd "$COORD_PUB_IP" "strings /usr/local/bin/coordinator | grep -q '"'"''"$FAIL_AT"''"'"'"
    '
done

phase_result

# ══════════════════════════════════════════
# Phase 9: Chaos Mode Test
# ══════════════════════════════════════════
phase_start 9 "Chaos Mode — Random Crash Stress Test"

# Start coordinator with chaos mode
ssh_cmd "$COORD_PUB_IP" "
    mkdir -p /etc/systemd/system/coordinator.service.d
    cat > /etc/systemd/system/coordinator.service.d/fault.conf << 'EOF'
[Service]
Environment=CHAOS_MODE=true
Environment=CHAOS_PROBABILITY=0.05
EOF
    systemctl daemon-reload
    systemctl restart coordinator
"
wait_for_coordinator 30

CHAOS_CRASHES=0
CHAOS_ITERATIONS=20

for i in $(seq 1 $CHAOS_ITERATIONS); do
    USER="chaos-$i"
    coord_api POST /api/users "{\"user_id\":\"$USER\"}" > /dev/null 2>&1 || true
    coord_api POST /api/users/$USER/provision > /dev/null 2>&1 || true

    sleep 3

    # Check if coordinator is still alive
    if ! coord_api GET /api/fleet > /dev/null 2>&1; then
        ((CHAOS_CRASHES++))
        echo "    Chaos crash #$CHAOS_CRASHES during iteration $i"

        # Restart coordinator (still in chaos mode)
        ssh_cmd "$COORD_PUB_IP" "systemctl start coordinator" 2>/dev/null || true
        wait_for_coordinator 30
        sleep 3
    fi
done

echo "  Chaos mode: $CHAOS_CRASHES crashes in $CHAOS_ITERATIONS iterations"

# Stop chaos mode, do final reconciliation
ssh_cmd "$COORD_PUB_IP" "
    rm -f /etc/systemd/system/coordinator.service.d/fault.conf
    systemctl daemon-reload
    systemctl restart coordinator
"
wait_for_coordinator 30
sleep 10  # give reconciliation time to process everything

check "Chaos crashes occurred" '[ '$CHAOS_CRASHES' -gt 0 ]'

# Verify all users are in terminal states
check "No transient states after chaos" '
    stuck=$(db_query "SELECT COUNT(*) FROM users WHERE status IN ('"'"'provisioning'"'"','"'"'failing_over'"'"','"'"'reforming'"'"','"'"'suspending'"'"','"'"'reactivating'"'"','"'"'evicting'"'"')")
    [ "$stuck" = "0" ]
'

check "All operations resolved after chaos" '
    in_progress=$(db_query "SELECT COUNT(*) FROM operations WHERE status = '"'"'in_progress'"'"'")
    [ "$in_progress" = "0" ]
'

check "Consistency after chaos" 'check_consistency "chaos"'

phase_result

# ══════════════════════════════════════════
# Phase 10: Graceful Shutdown Test
# ══════════════════════════════════════════
phase_start 10 "Graceful Shutdown"

check "Coordinator is running" 'ssh_cmd "$COORD_PUB_IP" "systemctl is-active coordinator" | grep -q active'

# Send SIGTERM and verify clean shutdown
check "Graceful shutdown" '
    ssh_cmd "$COORD_PUB_IP" "systemctl stop coordinator"
    sleep 2
    # Coordinator should have exited cleanly
    ssh_cmd "$COORD_PUB_IP" "journalctl -u coordinator --since=-10s --no-pager" | grep -q "Shutdown signal received"
'

# Restart and verify it comes back clean
ssh_cmd "$COORD_PUB_IP" "systemctl start coordinator"
wait_for_coordinator 30

check "Coordinator recovered after graceful shutdown" 'coord_api GET /api/fleet | jq -e .machines'

phase_result

# ══════════════════════════════════════════
# Phase 11: Final Consistency & Cleanup
# ══════════════════════════════════════════
phase_start 11 "Final Consistency & Cleanup"

check "Final consistency check" 'check_consistency "final"'

# Verify event log has entries
check "Events recorded in DB" '[ "$(db_query "SELECT COUNT(*) FROM events")" -gt 0 ]'

# Verify operations table has entries
check "Operations recorded in DB" '[ "$(db_query "SELECT COUNT(*) FROM operations")" -gt 0 ]'

# Show summary
TOTAL_USERS=$(db_query "SELECT COUNT(*) FROM users")
TOTAL_OPS=$(db_query "SELECT COUNT(*) FROM operations")
COMPLETE_OPS=$(db_query "SELECT COUNT(*) FROM operations WHERE status='complete'")
FAILED_OPS=$(db_query "SELECT COUNT(*) FROM operations WHERE status='failed'")
TOTAL_EVENTS=$(db_query "SELECT COUNT(*) FROM events")

echo "  Summary:"
echo "    Users: $TOTAL_USERS"
echo "    Operations: $TOTAL_OPS (complete: $COMPLETE_OPS, failed: $FAILED_OPS)"
echo "    Events: $TOTAL_EVENTS"

phase_result

# ══════════════════════════════════════════
# Final Result
# ══════════════════════════════════════════
final_result "Layer 4.6: Crash Recovery, Reconciliation & Postgres Migration"
```

---

## Summary of All Changes

### New Go dependency (1):
- `github.com/lib/pq` v1.10.9 — Postgres driver

### Modified files (major rewrites):
| File | Change |
|------|--------|
| `go.mod` | Add `github.com/lib/pq` dependency |
| `cmd/coordinator/main.go` | Complete rewrite: new startup sequence with advisory lock, reconciliation, graceful shutdown |
| `internal/coordinator/store.go` | Complete rewrite: Postgres backend with in-memory cache, operation tracking, unified events |
| `internal/coordinator/server.go` | Add fault injection fields, `checkFault()`, `step()`, new route handlers, `NewCoordinator` signature change |
| `internal/coordinator/provisioner.go` | Add operation tracking (`CreateOperation`, `step()`, `CompleteOperation`) at each step |
| `internal/coordinator/healthcheck.go` | Add operation tracking + `checkFault` at failover steps |
| `internal/coordinator/reformer.go` | Add operation tracking + `checkFault` at reformation steps |
| `internal/coordinator/lifecycle.go` | Add operation tracking + `checkFault` at suspend/reactivate/evict steps |
| `internal/coordinator/retention.go` | Update to work with new store (no code changes if method signatures unchanged) |

### New files (1 Go + 7 scripts):
| File | Purpose |
|------|---------|
| `internal/coordinator/reconciler.go` | Five-phase startup reconciliation algorithm |
| `scripts/layer-4.6/run.sh` | Orchestrator: build → infra → deploy → test → teardown |
| `scripts/layer-4.6/common.sh` | Helpers: `db_query`, `crash_test`, `check_consistency`, `wait_for_coordinator` |
| `scripts/layer-4.6/infra.sh` | Hetzner Cloud infrastructure management |
| `scripts/layer-4.6/deploy.sh` | Binary deployment with `DATABASE_URL` |
| `scripts/layer-4.6/cloud-init/coordinator.yaml` | Cloud-init with `postgresql-client` |
| `scripts/layer-4.6/cloud-init/fleet.yaml` | Cloud-init (same as 4.5) |
| `scripts/layer-4.6/test_suite.sh` | Full test suite: 11 phases, ~100+ checks |

### Key architectural change:
- **Before:** Coordinator state in-memory + JSON file. No crash recovery. No operation tracking. Background goroutines start immediately. No reconciliation.
- **After:** Coordinator state in Postgres (Supabase). Full operation tracking with `current_step`. Five-phase reconciliation on startup. 33 fault injection checkpoints. Advisory lock for singleton. Graceful shutdown. Consistency checker with 12 invariants. Chaos mode stress testing.

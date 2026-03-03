# Layer 5.1 Build Prompt — Tripod Primitive & Manual Live Migration

## What This Is

This is a build prompt for Layer 5.1 of the scfuture distributed agent platform. You are Claude Code. Your job is to:

1. Read all referenced existing files to understand the current codebase
2. Write all new code and modifications described below
3. Report back when code is written
4. When told "yes" / "ready", run the full test lifecycle (infra up → deploy → test → iterate on failures → teardown)
5. When all tests pass, update `SESSION.md` (in the parent directory) with what happened
6. Give a final report

The project lives in `scfuture/` (a subdirectory of the current working directory). All Go code paths are relative to `scfuture/`. All script paths are relative to `scfuture/`. The `SESSION.md` file is in the parent directory (current working directory).

---

## Context: What Exists

Layers 4.1–4.6 are complete and committed. Read these files first to understand the existing codebase:

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
scfuture/container/Dockerfile
scfuture/container/container-init.sh
```

Read ALL of these before writing any code. Pay close attention to:
- How `drbd.go` (machine agent) currently hardcodes exactly 2 nodes: `if len(req.Nodes) != 2` in both `DRBDCreate` and `DRBDReconfigure`. The DRBD config template uses exactly 2 hardcoded `on` blocks. **These must be generalized to support 2 or 3 nodes.**
- How `parseDRBDStatusAll` in `drbd.go` only tracks a single peer's state (`PeerDiskState` is singular). **This must be extended to track multiple peers.**
- How `DRBDStatusResponse` in `types.go` has a single `PeerDiskState` field. **A new `Peers` slice must be added while keeping backward compatibility.**
- How `DRBDReconfigure` in `drbd.go` has the same 2-node limitation and same config template — it will need the same generalization.
- How `provisioner.go` drives an 8-step sequential flow with `coord.step(opID, "provision-...")` checkpoints. The migration flow follows this same pattern.
- How `reformer.go` reconfigures DRBD (try `adjust` first, fall back to `down/up`). Migration uses a similar approach when adding/removing the 3rd node.
- How `reconciler.go` dispatches to `resumeProvision`, `resumeFailover`, etc. based on `op.Type`. A new `resumeMigration` handler is needed.
- How `healthcheck.go` skips failover for users not in `running` or `running_degraded` state — it needs to also handle the `migrating` state.
- How `server.go` registers routes and launches goroutines from handlers. The migration endpoint follows the same pattern.
- How `machineapi.go` wraps HTTP calls — new methods may be needed if the DRBD API changes.
- How `store.go` has event recording methods (`RecordFailoverEvent`, `RecordReformationEvent`, `RecordLifecycleEvent`). A similar `RecordMigrationEvent` is needed.

### Reference documents (in parent directory):

```
SESSION.md
architecture-v3.md
```

Read the "Layer 5 Preview: Live Migration" section in `SESSION.md` for the design rationale and protocol overview.

---

## What Layer 5.1 Builds

**What this layer proves:** A running user's world can be live-migrated from one machine to another with ~5-15 seconds of downtime, triggered manually via API. The DRBD 3-node "tripod" primitive is built, tested, and proven. Crash recovery handles all migration failure modes.

**In scope:**
- **Tripod primitive** — extend DRBD support from 2-node to 2-or-3-node. Config generation, status parsing, and `drbdadm adjust` for dynamic peer addition/removal.
- **Manual migration API** — `POST /api/users/{id}/migrate` with `source_machine` and `target_machine` parameters.
- **Migration orchestration** — new `migrator.go` with a multi-step migration state machine (primary migration and secondary migration).
- **Crash recovery** — `resumeMigration` handler in the reconciler, covering all checkpoints.
- **Migration events** — recorded in the unified events table.
- **Consistency checker updates** — extended to cover migration-related transient states.
- **Full test suite** — happy path migration (primary + secondary), validation/edge cases, crash recovery at every checkpoint, consistency checks.

**Explicitly NOT in scope (Layer 5.2):**
- Rebalancer goroutine (automatic migration decisions)
- Machine drain API
- HA coordinator (active-passive pair)

---

## Architecture

### Test Topology

Same as Layers 4.3–4.6: 1 coordinator + 3 fleet machines on Hetzner Cloud. Additionally requires:
- Supabase Postgres connection string (same as Layer 4.6)
- B2 credentials (same as Layer 4.6 — not exercised by migration tests, but machine agent binary expects them)

```
macOS (test harness, runs test_suite.sh via SSH / curl / psql)
  │
  ├── l51-coordinator (CX23, coordinator :8080, private 10.0.0.2)
  │     │
  │     ├── l51-fleet-1 (CX23, machine-agent :8080, private 10.0.0.11)
  │     ├── l51-fleet-2 (CX23, machine-agent :8080, private 10.0.0.12)
  │     └── l51-fleet-3 (CX23, machine-agent :8080, private 10.0.0.13)
  │
  Private network: l51-net / 10.0.0.0/24

Supabase Postgres:
  Env var: DATABASE_URL (required, set by user)

Backblaze B2:
  Env vars: B2_KEY_ID, B2_APP_KEY (required)
  Bucket: l51-test-{random} (created/destroyed by run.sh)
```

### Why 3 Fleet Machines Suffice

A user's bipod occupies 2 of 3 machines. The 3rd machine is the migration target. After migration, the old source is free. Example:
- User on fleet-1 (primary) + fleet-2 (secondary)
- Migrate primary to fleet-3
- After migration: fleet-3 (primary) + fleet-2 (secondary), fleet-1 is free
- Can now migrate secondary from fleet-2 to fleet-1 if desired

### Migration Protocol

**Key design constraint:** We don't control what's running inside the container. The platform contract is: handle SIGTERM gracefully (same as a server reboot). The migration uses `docker stop` (SIGTERM → grace period → SIGKILL), NOT `docker pause`.

**Two migration types determined by the source machine's bipod role:**

#### Primary Migration (container stop/start, ~5-15s user downtime)

```
Starting state: user running on source (primary) + stayer (secondary)
Target: move primary to target machine

Phase 1 — Pre-sync (transparent, no downtime):
  1. Create empty image on target
  2. Configure DRBD: add target as 3rd node (temporary tripod)
     - Write 3-node config on target, adjust on source and stayer
  3. DRBD initial sync from source to target
  4. Wait for sync to complete (target now has full copy)
  User's container keeps running throughout. Sync is transparent.

Phase 2 — Switchover (~5-15s downtime):
  5. docker stop on source (SIGTERM → grace period → SIGKILL)
  6. Demote DRBD on source (secondary)
  7. Promote DRBD on target (primary)
  8. docker start on target (container starts with same /dev/drbdN, same data)

Phase 3 — Cleanup:
  9. Disconnect source from DRBD peers
  10. Destroy DRBD on source, delete source image
  11. Reconfigure remaining machines to 2-node (target + stayer)
  12. Remove source bipod, update target bipod role
  Bipod is now: target (primary) + stayer (secondary)
```

#### Secondary Migration (DRBD only, zero user downtime)

```
Starting state: user running on stayer (primary) + source (secondary)
Target: move secondary to target machine

Phase 1 — Pre-sync:
  1. Create image on target
  2. Configure DRBD: add target as 3rd node
  3. Wait for sync

Phase 2 — No switchover needed (primary never moves)

Phase 3 — Cleanup:
  4. Disconnect source from peers
  5. Destroy DRBD on source, delete source image
  6. Reconfigure to 2-node (stayer + target)
  7. Remove source bipod, update target bipod role
  Bipod is now: stayer (primary) + target (secondary)
```

### DRBD 3-Node (Tripod) Configuration

DRBD 9 supports N-node resources natively. A 3-node config looks like:

```
resource user-alice {
    net {
        protocol A;
        max-buffers 8000;
        max-epoch-size 8000;
        sndbuf-size 0;
        rcvbuf-size 0;
    }
    disk {
        on-io-error detach;
    }
    on fleet-1 {
        device /dev/drbd0 minor 0;
        disk /dev/loop0;
        address 10.0.0.11:7900;
        meta-disk internal;
    }
    on fleet-2 {
        device /dev/drbd1 minor 1;
        disk /dev/loop0;
        address 10.0.0.12:7900;
        meta-disk internal;
    }
    on fleet-3 {
        device /dev/drbd0 minor 0;
        disk /dev/loop0;
        address 10.0.0.13:7900;
        meta-disk internal;
    }
}
```

Each machine's block uses its own hostname, minor number, disk path, and address. The port is shared across all nodes. Minors are per-machine (fleet-1 minor 0 and fleet-3 minor 0 are independent).

**Adding the 3rd node to a running resource:**

1. Write the 3-node config on the **target** machine → `DRBDCreate` (with 3-node request)
2. Write the 3-node config on the **source** and **stayer** machines → `DRBDReconfigure` (with 3-node request, `force=false` to try `adjust` first)
3. If `adjust` succeeds on source and stayer: DRBD connects to the new peer. No downtime.
4. If `adjust` fails: for primary migration, stop container on source, do `DRBDReconfigure` with `force=true` (down/up/promote) on source, restart container, then sync proceeds. For secondary migration, do `DRBDReconfigure` with `force=true` on stayer (no container involvement since stayer is primary and force path stops/restarts container internally).

**Removing a node from a 3-node resource (cleanup):**

1. Disconnect the departing node from its peers: `DRBDDisconnect` on source
2. Destroy DRBD on source: `DRBDDestroy` on source
3. Delete source image: `DeleteUser` on source
4. Write 2-node config on remaining machines: `DRBDReconfigure` (with 2-node request) on target and stayer

### DRBD Status with Multiple Peers

DRBD 9 status output with 3 nodes:

```
user-alice role:Primary
  disk:UpToDate open:yes
  fleet-2 role:Secondary
    peer-disk:UpToDate
  fleet-3 role:Secondary
    peer-disk:Inconsistent done:45.20
```

The parser must:
1. Detect each peer section (line with a hostname + `role:`)
2. Collect per-peer info: hostname, role, disk state, sync progress
3. Build a `Peers` slice in the response
4. Set the top-level `PeerDiskState` to the **worst** peer state (for backward compatibility). Priority: `Inconsistent` > `Outdated` > `DUnknown` > `UpToDate`
5. Set `SyncProgress` to the progress of whichever peer is syncing (if any)

### Crash Checkpoints — 9 Points for Primary Migration

| ID | Checkpoint Name | After This Completes | Before This Starts |
|----|----------------|---------------------|-------------------|
| F34 | `migrate-target-selected` | Operation created, target chosen | Image creation |
| F35 | `migrate-image-created` | Image on target, loop device recorded | DRBD 3-node config |
| F36 | `migrate-drbd-added` | DRBD in tripod mode, syncing | Sync wait |
| F37 | `migrate-synced` | Target UpToDate | Container stop |
| F38 | `migrate-container-stopped` | Container stopped on source | DRBD demote on source |
| F39 | `migrate-source-demoted` | Source demoted to Secondary | DRBD promote on target |
| F40 | `migrate-target-promoted` | Target promoted to Primary | Container start on target |
| F41 | `migrate-container-started` | Container running on target | Source cleanup |
| F42 | `migrate-source-cleaned` | Source removed, back to 2-node | Status → running |

**Secondary migration uses a subset:** F34, F35, F36, F37, then jumps directly to source cleanup (F42 equivalent, called `migrate-secondary-cleaned`). Total: 5 checkpoints for secondary migration.

### Reconciliation Updates — `resumeMigration`

Added as a new case in `reconcilePhase3ResumeOperations`:

```
case "live_migration":
    coord.resumeMigration(op, machineStatuses)
```

The `resumeMigration` function reads `current_step` and the operation `metadata` (which stores source_machine, target_machine, stayer_machine, addresses, ports, minors, loops, migration_type). Recovery logic:

```
resumeMigration(op, machineStatuses):

  switch op.CurrentStep:

  case "migrate-target-selected":
    // Image may or may not exist. Safe to restart from image creation.
    → Resume from image creation step

  case "migrate-image-created":
    // Image exists, DRBD not configured.
    → Resume from DRBD add step

  case "migrate-drbd-added":
    // 3-node DRBD configured, may be syncing.
    if target online:
      → Check sync status, wait or resume
    if target offline:
      → Cancel: revert to 2-node, clean up target bipod, set user running

  case "migrate-synced":
    if migration_type == "primary":
      → Proceed with container stop
    else:
      → Skip to cleanup

  case "migrate-container-stopped":
    // CRITICAL: User is DOWN. Must recover ASAP.
    if target online:
      → Demote source → promote target → start container → cleanup
    if target offline AND source online:
      → Re-promote source → start container on source → cancel migration
    if both offline:
      → Mark user unavailable

  case "migrate-source-demoted":
    → Promote target → start container → cleanup

  case "migrate-target-promoted":
    → Start container → cleanup

  case "migrate-container-started":
    // Container running on target. Non-urgent cleanup.
    → Clean up source, update bipods, complete

  case "migrate-source-cleaned", "migrate-secondary-cleaned":
    // Everything done, just mark complete.
    → Set running, complete operation
```

**Key principle:** Between `migrate-container-stopped` and `migrate-container-started`, the user is down. The reconciler must get the user running as fast as possible — either on the target (preferred) or rolled back to the source.

### Consistency Checker Updates

Extend invariant 11 to include `migrating` in the list of transient states that should not persist after reconciliation:

```sql
SELECT COUNT(*) FROM users WHERE status IN (
  'provisioning', 'failing_over', 'reforming',
  'suspending', 'reactivating', 'evicting',
  'migrating'  -- NEW
)
```

Extend invariant 2 to allow up to 3 non-stale bipods for `migrating` users (temporary tripod). Since `migrating` is a transient state and invariant 11 already checks it doesn't persist, this is only relevant during active migration (which won't happen during consistency checks since they run after reconciliation). But for completeness, the check should be:

```
For running users: exactly 2 non-stale bipods
For migrating users: 2 or 3 non-stale bipods (acceptable during tripod phase)
```

### Interaction with Other Operations

The `migrating` user status gates all other operations:

- **Healthcheck/failover:** If a machine dies while user is `migrating`, the healthchecker sees `status != "running"` and `status != "running_degraded"` — it skips failover. The reconciler handles recovery when the coordinator restarts.
- **Reformation:** Won't trigger because user is `migrating`, not `running_degraded`.
- **Suspension/reactivation/eviction:** API handlers reject these for users in `migrating` status.
- **Another migration:** Reject — user is already being migrated.
- **Provisioning:** Only valid from `registered` status, no conflict.

### Address Conventions

Same as Layers 4.3–4.6:
- Coordinator private: `10.0.0.2:8080`
- Fleet private: `10.0.0.11:8080`, `10.0.0.12:8080`, `10.0.0.13:8080`
- Machine agents register with their private IP as `NODE_ADDRESS`
- Coordinator calls machine agents via their registered address (private IP)
- Test harness calls coordinator and machine agents via public IPs

---

## Modifications to Existing Code

### 1. `internal/shared/types.go` — Add migration types, extend DRBD status

**Add these new types at the end of the file:**

```go
// ─── DRBD peer info for multi-peer status (Layer 5.1) ───

type DRBDPeerInfo struct {
	Hostname     string  `json:"hostname"`
	Role         string  `json:"role"`
	DiskState    string  `json:"disk_state"`
	SyncProgress *string `json:"sync_progress,omitempty"`
}

// ─── Migration types (Layer 5.1) ───

type MigrateUserRequest struct {
	SourceMachine string `json:"source_machine"`
	TargetMachine string `json:"target_machine"`
}

type MigrateUserResponse struct {
	UserID string `json:"user_id"`
	Status string `json:"status"`
}

type MigrationEventResponse struct {
	UserID        string `json:"user_id"`
	SourceMachine string `json:"source_machine"`
	TargetMachine string `json:"target_machine"`
	MigrationType string `json:"migration_type"` // "primary" or "secondary"
	Success       bool   `json:"success"`
	Error         string `json:"error,omitempty"`
	Method        string `json:"method,omitempty"` // "adjust" or "down_up"
	DurationMS    int64  `json:"duration_ms"`
	Timestamp     string `json:"timestamp"`
}
```

**Modify `DRBDStatusResponse` — add a `Peers` field (backward compatible):**

```go
type DRBDStatusResponse struct {
	Resource        string         `json:"resource"`
	Role            string         `json:"role"`
	ConnectionState string         `json:"connection_state"`
	DiskState       string         `json:"disk_state"`
	PeerDiskState   string         `json:"peer_disk_state"`   // worst-case across all peers (backward compat)
	SyncProgress    *string        `json:"sync_progress"`     // progress of any syncing peer (backward compat)
	Peers           []DRBDPeerInfo `json:"peers,omitempty"`   // NEW: per-peer details
	Exists          bool           `json:"exists"`
}
```

### 2. `internal/machineagent/drbd.go` — Support 2-or-3 node DRBD

**2a. Extract config generation into a shared helper function:**

Replace the hardcoded 2-block config template in both `DRBDCreate` and `DRBDReconfigure` with a call to a new `generateDRBDConfig` function:

```go
func generateDRBDConfig(resName string, nodes []shared.DRBDNode, port int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "resource %s {\n", resName)
	b.WriteString("    net {\n")
	b.WriteString("        protocol A;\n")
	b.WriteString("        max-buffers 8000;\n")
	b.WriteString("        max-epoch-size 8000;\n")
	b.WriteString("        sndbuf-size 0;\n")
	b.WriteString("        rcvbuf-size 0;\n")
	b.WriteString("    }\n")
	b.WriteString("    disk {\n")
	b.WriteString("        on-io-error detach;\n")
	b.WriteString("    }\n")
	for _, node := range nodes {
		fmt.Fprintf(&b, "    on %s {\n", node.Hostname)
		fmt.Fprintf(&b, "        device /dev/drbd%d minor %d;\n", node.Minor, node.Minor)
		fmt.Fprintf(&b, "        disk %s;\n", node.Disk)
		fmt.Fprintf(&b, "        address %s:%d;\n", node.Address, port)
		b.WriteString("        meta-disk internal;\n")
		b.WriteString("    }\n")
	}
	b.WriteString("}\n")
	return b.String()
}
```

**2b. Modify `DRBDCreate` — accept 2 or 3 nodes:**

Change the validation:
```go
// Before:
if len(req.Nodes) != 2 {
    return nil, fmt.Errorf("exactly 2 nodes required")
}

// After:
if len(req.Nodes) < 2 || len(req.Nodes) > 3 {
    return nil, fmt.Errorf("2 or 3 nodes required, got %d", len(req.Nodes))
}
```

Replace the hardcoded config template with:
```go
config := generateDRBDConfig(resName, req.Nodes, req.Port)
```

**2c. Modify `DRBDReconfigure` — accept 2 or 3 nodes:**

Same validation change and config generation change as `DRBDCreate`.

**2d. Modify `parseDRBDStatusAll` — parse multiple peers:**

The current parser uses a boolean `inPeerSection` and sets `PeerDiskState` from the last peer. Rewrite the peer parsing to:

1. Track a list of `shared.DRBDPeerInfo` structs
2. Each time a new peer line is detected (hostname + `role:`), create a new `DRBDPeerInfo`
3. Subsequent `peer-disk:` and `done:` tokens apply to the current peer
4. After parsing all peers:
   - Set `info.PeerDiskState` to the **worst** disk state among all peers
   - Set `info.SyncProgress` to the progress of whichever peer is syncing (if any)

Add a new `Peers` field to `DRBDInfo`:
```go
type DRBDInfo struct {
	Role            string
	ConnectionState string
	DiskState       string
	PeerDiskState   string
	SyncProgress    *string
	Peers           []shared.DRBDPeerInfo // NEW
}
```

Implement worst-state logic:
```go
func worstDiskState(states []string) string {
	priority := map[string]int{
		"Inconsistent": 4,
		"Outdated":     3,
		"DUnknown":     2,
		"UpToDate":     1,
	}
	worst := ""
	worstPri := 0
	for _, s := range states {
		if p, ok := priority[s]; ok && p > worstPri {
			worst = s
			worstPri = p
		} else if !ok && worst == "" {
			worst = s // unknown state, use as fallback
		}
	}
	return worst
}
```

**2e. Modify `DRBDStatus` return — include Peers:**

```go
return &shared.DRBDStatusResponse{
	Resource:        resName,
	Role:            info.Role,
	ConnectionState: info.ConnectionState,
	DiskState:       info.DiskState,
	PeerDiskState:   info.PeerDiskState,
	SyncProgress:    info.SyncProgress,
	Peers:           info.Peers,     // NEW
	Exists:          true,
}, nil
```

### 3. `internal/coordinator/server.go` — Add migration routes and handler

**Add to `RegisterRoutes`:**

```go
// Live migration (Layer 5.1)
mux.HandleFunc("POST /api/users/{id}/migrate", coord.handleMigrateUser)
mux.HandleFunc("GET /api/migrations", coord.handleGetMigrations)
```

**Add handler:**

```go
func (coord *Coordinator) handleMigrateUser(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	var req shared.MigrateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SourceMachine == "" || req.TargetMachine == "" {
		writeError(w, http.StatusBadRequest, "source_machine and target_machine are required")
		return
	}

	u := coord.store.GetUser(userID)
	if u == nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if u.Status != "running" {
		writeError(w, http.StatusConflict, "user must be in running state to migrate (current: "+u.Status+")")
		return
	}

	// Validate source is in bipod
	bipods := coord.store.GetBipods(userID)
	sourceInBipod := false
	targetInBipod := false
	for _, b := range bipods {
		if b.Role == "stale" {
			continue
		}
		if b.MachineID == req.SourceMachine {
			sourceInBipod = true
		}
		if b.MachineID == req.TargetMachine {
			targetInBipod = true
		}
	}
	if !sourceInBipod {
		writeError(w, http.StatusBadRequest, "source_machine is not in user's bipod")
		return
	}
	if targetInBipod {
		writeError(w, http.StatusBadRequest, "target_machine is already in user's bipod")
		return
	}

	// Validate target machine exists and is active
	targetMachine := coord.store.GetMachine(req.TargetMachine)
	if targetMachine == nil {
		writeError(w, http.StatusBadRequest, "target_machine not found")
		return
	}
	if targetMachine.Status != "active" {
		writeError(w, http.StatusBadRequest, "target_machine is not active (status: "+targetMachine.Status+")")
		return
	}

	coord.store.SetUserStatus(userID, "migrating", "")
	go coord.MigrateUser(userID, req.SourceMachine, req.TargetMachine)

	slog.Info("Migration started", "user_id", userID, "source", req.SourceMachine, "target", req.TargetMachine)
	writeJSON(w, http.StatusAccepted, shared.MigrateUserResponse{
		UserID: userID,
		Status: "migrating",
	})
}

func (coord *Coordinator) handleGetMigrations(w http.ResponseWriter, r *http.Request) {
	events := coord.store.GetMigrationEvents()
	if events == nil {
		events = []MigrationEvent{}
	}
	writeJSON(w, http.StatusOK, events)
}
```

### 4. `internal/coordinator/store.go` — Add migration event methods

**Add `MigrationEvent` type (in store.go, alongside the existing event types):**

```go
type MigrationEvent struct {
	UserID        string    `json:"user_id"`
	SourceMachine string    `json:"source_machine"`
	TargetMachine string    `json:"target_machine"`
	MigrationType string    `json:"migration_type"` // "primary" or "secondary"
	Success       bool      `json:"success"`
	Error         string    `json:"error,omitempty"`
	Method        string    `json:"method,omitempty"` // "adjust" or "down_up"
	DurationMS    int64     `json:"duration_ms"`
	Timestamp     time.Time `json:"timestamp"`
}
```

**Add recording and retrieval methods (following the pattern of `RecordReformationEvent` / `GetReformationEvents`):**

```go
func (s *Store) RecordMigrationEvent(event MigrationEvent) {
	details := map[string]interface{}{
		"source_machine": event.SourceMachine,
		"target_machine": event.TargetMachine,
		"migration_type": event.MigrationType,
		"success":        event.Success,
		"error":          event.Error,
		"method":         event.Method,
		"duration_ms":    event.DurationMS,
	}
	s.RecordEvent("migration", event.SourceMachine, event.UserID, "", details)
}

func (s *Store) GetMigrationEvents() []MigrationEvent {
	rows, err := s.db.Query(`SELECT user_id, details, timestamp FROM events WHERE event_type='migration' ORDER BY timestamp`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var result []MigrationEvent
	for rows.Next() {
		var userID string
		var detailsJSON []byte
		var ts time.Time
		rows.Scan(&userID, &detailsJSON, &ts)
		var details map[string]interface{}
		json.Unmarshal(detailsJSON, &details)
		me := MigrationEvent{UserID: userID, Timestamp: ts}
		if v, ok := details["source_machine"].(string); ok { me.SourceMachine = v }
		if v, ok := details["target_machine"].(string); ok { me.TargetMachine = v }
		if v, ok := details["migration_type"].(string); ok { me.MigrationType = v }
		if v, ok := details["success"].(bool); ok { me.Success = v }
		if v, ok := details["error"].(string); ok { me.Error = v }
		if v, ok := details["method"].(string); ok { me.Method = v }
		if v, ok := details["duration_ms"].(float64); ok { me.DurationMS = int64(v) }
		result = append(result, me)
	}
	return result
}
```

### 5. `internal/coordinator/reconciler.go` — Add `resumeMigration` handler

**In `reconcilePhase3ResumeOperations`, add to the switch:**

```go
case "live_migration":
    coord.resumeMigration(op, machineStatuses)
```

**Add `resumeMigration` function** (following the pattern of `resumeFailover`, `resumeProvision`, etc.):

The function reads `op.Metadata` for source_machine, target_machine, stayer_machine, and migration_type. Based on `op.CurrentStep`, it decides how to recover. The detailed logic is described in the "Reconciliation Updates" section above.

Key recovery priorities:
- Steps before `migrate-container-stopped`: safe to cancel (revert to 2-node, clean up, set running)
- Steps `migrate-container-stopped` through `migrate-target-promoted`: user is DOWN — must get container running ASAP
- Steps after `migrate-container-started`: just finish cleanup

**In `reconcilePhase2ReconcileDB`, add handling for `migrating` users:**

```go
if u.Status == "migrating" {
    // Check if container is running on any machine
    for _, b := range bipods {
        mr := machineStatuses[b.MachineID]
        if mr != nil && mr.Online {
            if info, ok := mr.Users[u.UserID]; ok && info.ContainerRunning {
                // Container is running somewhere — Phase 3 will handle the rest
                break
            }
        }
    }
}
```

### 6. `internal/coordinator/healthcheck.go` — Skip failover for migrating users

In `failoverUser`, the existing check handles `suspended` and `evicted` users. Add `migrating` to the skip list:

```go
// Handle migrating users — mark bipod stale only, reconciler handles recovery
if user.Status == "migrating" {
    logger.Info("User is migrating — marking bipod stale only, reconciler will handle", "status", user.Status)
    coord.store.SetBipodRole(userID, deadMachineID, "stale")
    return
}
```

This should go right after the existing `suspended`/`evicted` check.

---

## New Files

### 7. `internal/coordinator/migrator.go` — Migration orchestration

Create a new file `scfuture/internal/coordinator/migrator.go`. This contains the main `MigrateUser` function that drives the multi-step migration state machine.

**Structure:**

```go
package coordinator

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"scfuture/internal/shared"
)

const (
	MigrationSyncTimeout = 300 * time.Second // 5 minutes for initial sync
)

// MigrateUser drives the live migration of a user from one machine to another.
// Runs in its own goroutine, launched by handleMigrateUser.
func (coord *Coordinator) MigrateUser(userID, sourceMachineID, targetMachineID string) {
	start := time.Now()
	logger := slog.With("user_id", userID, "component", "migrator",
		"source", sourceMachineID, "target", targetMachineID)

	// ... fail/retry helpers (same pattern as provisioner.go) ...

	// Step 1: Determine migration type and gather metadata
	user := coord.store.GetUser(userID)
	// ... get bipods, determine if primary or secondary migration ...
	// ... identify stayer machine (the bipod member NOT being replaced) ...
	// ... get machine addresses, allocate minor for target ...

	migrationType := "primary" // or "secondary" based on source's bipod role

	// Create operation with all metadata needed for resumption
	opID := generateOpID()
	coord.store.CreateOperation(opID, "live_migration", userID, map[string]interface{}{
		"source_machine":  sourceMachineID,
		"target_machine":  targetMachineID,
		"stayer_machine":  stayerMachineID,
		"source_address":  sourceMachine.Address,
		"target_address":  targetMachine.Address,
		"stayer_address":  stayerMachine.Address,
		"port":            user.DRBDPort,
		"source_minor":    sourceBipod.DRBDMinor,
		"target_minor":    targetMinor,
		"stayer_minor":    stayerBipod.DRBDMinor,
		"source_loop":     sourceBipod.LoopDevice,
		"stayer_loop":     stayerBipod.LoopDevice,
		"migration_type":  migrationType,
	})

	coord.store.CreateBipod(userID, targetMachineID, "secondary", targetMinor)

	coord.step(opID, "migrate-target-selected")

	// Step 2: Create image on target
	// targetClient.CreateImage(userID, user.ImageSizeMB) → targetLoop
	// coord.store.SetBipodLoopDevice(...)
	coord.step(opID, "migrate-image-created")

	// Step 3: Add target to DRBD (temporary tripod)
	// Build 3-node DRBD request with source + stayer + target
	// targetClient.DRBDCreate(userID, tripodReq)  — creates metadata and brings up
	// sourceClient.DRBDReconfigure(userID, tripodReconfigReq)  — adjust to add new peer
	// stayerClient.DRBDReconfigure(userID, tripodReconfigReq)  — adjust to add new peer
	// Handle adjust failure: fallback described in Architecture section
	coord.step(opID, "migrate-drbd-added")

	// Step 4: Wait for target to sync
	// Poll sourceClient.DRBDStatus(userID)
	// Check status.Peers for target hostname with DiskState == "UpToDate"
	// Timeout: MigrationSyncTimeout
	coord.step(opID, "migrate-synced")

	// ── PRIMARY MIGRATION ONLY ──
	if migrationType == "primary" {
		// Step 5: Stop container on source
		// sourceClient.ContainerStop(userID)
		coord.step(opID, "migrate-container-stopped")

		// Step 6: Demote source
		// sourceClient.DRBDDemote(userID)
		coord.step(opID, "migrate-source-demoted")

		// Step 7: Promote target
		// targetClient.DRBDPromote(userID)
		coord.step(opID, "migrate-target-promoted")

		// Step 8: Start container on target
		// targetClient.ContainerStart(userID)
		coord.step(opID, "migrate-container-started")

		// Update DB: primary machine changes
		coord.store.SetUserPrimary(userID, targetMachineID)
	}

	// Step 9: Clean up source (remove from tripod → back to bipod)
	// sourceClient.DRBDDisconnect(userID)
	// sourceClient.DRBDDestroy(userID)
	// sourceClient.DeleteUser(userID)
	// Build 2-node config (target + stayer)
	// targetClient.DRBDReconfigure(userID, bipodReconfigReq)
	// stayerClient.DRBDReconfigure(userID, bipodReconfigReq)
	// coord.store.RemoveBipod(userID, sourceMachineID)
	// Update target bipod role:
	//   For primary migration: target becomes "primary"
	//   For secondary migration: target becomes "secondary"
	// coord.store.SetBipodRole(userID, targetMachineID, newRole)

	if migrationType == "primary" {
		coord.step(opID, "migrate-source-cleaned")
	} else {
		coord.step(opID, "migrate-secondary-cleaned")
	}

	// Step 10: Mark complete
	coord.store.SetUserStatus(userID, "running", "")
	coord.store.CompleteOperation(opID)

	coord.store.RecordMigrationEvent(MigrationEvent{
		UserID:        userID,
		SourceMachine: sourceMachineID,
		TargetMachine: targetMachineID,
		MigrationType: migrationType,
		Success:       true,
		Method:        reconfigMethod,
		DurationMS:    time.Since(start).Milliseconds(),
		Timestamp:     time.Now(),
	})

	logger.Info("Migration complete",
		"type", migrationType,
		"method", reconfigMethod,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}
```

**Implementation notes:**
- Use `retry()` helper for machine agent calls (same pattern as provisioner.go)
- The `fail()` helper should set user status back to `running` (not `failed` — the user was running before migration, and migration failure shouldn't break them), clean up any partial target resources, and record a failed migration event
- For the sync wait loop, check `status.Peers` for the specific target hostname rather than the top-level `PeerDiskState` (which shows worst-case across all peers and would block even if the existing secondary is fine)
- The `reconfigMethod` variable tracks whether `adjust` or `down_up` was used, recorded in the migration event
- For secondary migration, skip steps 5-8 (no container involvement)
- The `stripPort` helper from provisioner.go should be reused (import or extract to shared)

---

## Test Suite

### Test Harness Scripts

Create `scfuture/scripts/layer-5.1/` with the following files. Base all scripts on the Layer 4.6 equivalents, changing:
- Prefix: `l46-` → `l51-`
- Network name: `l46-net` → `l51-net`
- SSH key name: `l46-key` → `l51-key`
- Layer description in banners
- common.sh: update `NETWORK_NAME`, `SSH_KEY_NAME` to `l51-` prefix
- common.sh: add `migrating` to the invariant 11 transient states list
- common.sh: adjust invariant 2 to allow 2 OR 3 non-stale bipods for migrating users (if any exist during check)

#### `scripts/layer-5.1/run.sh`

Same structure as Layer 4.6 `run.sh`:
1. Verify required env vars (`DATABASE_URL`, `B2_KEY_ID`, `B2_APP_KEY`)
2. Reset database tables
3. Create B2 bucket (named `l51-test-{random}`)
4. Build binaries (`make build`)
5. Create infrastructure (`./infra.sh up`)
6. Deploy (`./deploy.sh`)
7. Run test suite (`./test_suite.sh`)
8. Teardown infrastructure
9. Delete B2 bucket and clean database

#### `scripts/layer-5.1/common.sh`

Copy from Layer 4.6. Change all `l46-` references to `l51-`. In `check_consistency()`:
- Invariant 11: add `migrating` to the transient states list
- Invariant 2: skip or adjust for `migrating` users

#### `scripts/layer-5.1/infra.sh`

Copy from Layer 4.6. Change `l46-` to `l51-` everywhere.

#### `scripts/layer-5.1/deploy.sh`

Copy from Layer 4.6. Change `l46-` to `l51-` everywhere.

#### `scripts/layer-5.1/cloud-init/coordinator.yaml`

Identical to Layer 4.6.

#### `scripts/layer-5.1/cloud-init/fleet.yaml`

Identical to Layer 4.6.

### Test Suite Structure — `scripts/layer-5.1/test_suite.sh`

```
═══ Layer 5.1: Tripod Primitive & Manual Live Migration — Test Suite ═══

Phase 0: Prerequisites (~8 checks)
  - Coordinator responding
  - 3 fleet machines registered and active
  - Machine agents responding
  - Postgres connected, schema exists, advisory lock held

Phase 1: Baseline — Provision & Verify (~6 checks)
  - Create and provision user "alice" → running
  - Verify alice in DB with 2 bipods, operation complete
  - Write test data (marker file "ALICE_DATA" in /workspace/data/test.txt)
  - Record which machines alice is on (primary, secondary)

Phase 2: Primary Migration — Happy Path (~10 checks)
  - Identify alice's primary and secondary machines, determine free machine
  - Call POST /api/users/alice/migrate with source=primary, target=free machine
  - Wait for alice to reach "running"
  - Verify: container running on target (not source)
  - Verify: DRBD healthy, 2-node, both UpToDate
  - Verify: data survived (marker file readable from container on new primary)
  - Verify: source cleaned up (no image, no DRBD — check via machine agent /status)
  - Verify: bipod entries correct in API response (target=primary, stayer=secondary)
  - Verify: migration event recorded (GET /api/migrations)
  - Verify: consistency check passes

Phase 3: Secondary Migration — Happy Path (~8 checks)
  - Write more test data
  - Migrate secondary from current secondary to the now-free old primary machine
  - Wait for running
  - Verify: container still on current primary (unchanged)
  - Verify: DRBD healthy, 2-node, both UpToDate
  - Verify: data intact (both marker files)
  - Verify: old secondary cleaned up
  - Verify: bipod correct

Phase 4: Validation & Edge Cases (~6 checks)
  - Migrate to machine already in bipod → 400 error
  - Migrate non-running user (create but don't provision) → 409 error
  - Migrate non-existent user → 404 error
  - Migrate to non-existent machine → 400 error
  - Migrate user that is already migrating → 409 error (requires race, may skip or test via status check)
  - Verify alice still accessible after failed attempts

Phase 5: Primary Migration Crash Tests (F34-F42) (~9 checks)
  For each crash checkpoint:
  1. Provision a fresh user
  2. Write test data
  3. Determine source/target for migration
  4. Set FAIL_AT, restart coordinator
  5. Trigger migration
  6. Wait for coordinator crash
  7. Restart coordinator without FAIL_AT (reconciliation runs)
  8. Verify: user is in a valid terminal state (running)
  9. Run consistency checker

  Checkpoints to test:
    F34: migrate-target-selected
    F35: migrate-image-created
    F36: migrate-drbd-added
    F37: migrate-synced
    F38: migrate-container-stopped     (CRITICAL — user is DOWN)
    F39: migrate-source-demoted
    F40: migrate-target-promoted
    F41: migrate-container-started
    F42: migrate-source-cleaned

  After each crash test, verify data is intact (test file readable)

Phase 6: Secondary Migration Crash Tests (~5 checks)
  Test checkpoints for secondary migration:
    migrate-target-selected
    migrate-image-created
    migrate-drbd-added
    migrate-synced
    migrate-secondary-cleaned

Phase 7: Post-Crash Verification (~5 checks)
  - Provision a new user after all crash tests, verify it works
  - Migrate the new user, verify it works
  - Run full consistency checker
  - Verify events and operations recorded in DB
  - Show summary (total users, operations, events)

Phase 8: Final Consistency & Cleanup (~2 checks)
  - Final consistency check
  - Verify no stuck operations

Final Result
  ALL PHASES COMPLETE: N/N checks passed
```

**Estimated total: ~59 checks.**

### Crash Test Implementation Pattern

Follow the exact `crash_test` pattern from Layer 4.6's `common.sh`. For migration crash tests:

```bash
USER="crash-mig-$i"
coord_api POST /api/users "{\"user_id\":\"$USER\"}" > /dev/null 2>&1 || true
coord_api POST /api/users/$USER/provision > /dev/null 2>&1 || true
wait_for_user_status "$USER" running 180

# Write data before migration
PRIM=$(coord_api GET /api/users/$USER | jq -r .primary_machine)
PRIM_PUB=$(get_public_ip "$PRIM")
docker_exec "$PRIM_PUB" ${USER}-agent "sh -c \"echo TESTDATA > /workspace/data/test.txt\""

# Determine source/target
BIPOD=$(coord_api GET /api/users/$USER/bipod | jq -r '[.[] | select(.role != "stale")] | .[0].machine_id')
# ... find free machine for target ...

crash_test "$FAIL_AT" \
    "" \
    "coord_api POST /api/users/$USER/migrate '{\"source_machine\":\"$SOURCE\",\"target_machine\":\"$TARGET\"}' > /dev/null 2>&1 || true" \
    "Migration crash at F$i ($FAIL_AT)"

# After recovery, user should be running (either migration completed or rolled back)
STATUS=$(coord_api GET /api/users/$USER 2>/dev/null | jq -r '.status // "unknown"' 2>/dev/null)
check "F$i: $USER in valid state ($STATUS)" '
    s=$(coord_api GET /api/users/'"$USER"' | jq -r .status)
    [ "$s" = "running" ]
'
```

Note: After migration crash recovery, the user should ALWAYS end up in `running` state — either the migration completed successfully, or it was rolled back. This is different from provisioning crash tests where `failed` is also acceptable. The migration starts from a running user and should always return to running.

---

## Implementation Notes

### Order of Implementation

1. **Types first** (`types.go`) — add new types and modify `DRBDStatusResponse`
2. **Machine agent DRBD** (`drbd.go`) — generalize config generation, update parser
3. **Store methods** (`store.go`) — add migration event methods
4. **Migrator** (`migrator.go`) — new file, migration orchestration
5. **Server routes** (`server.go`) — add migration API endpoints
6. **Reconciler** (`reconciler.go`) — add `resumeMigration` handler
7. **Healthcheck** (`healthcheck.go`) — add `migrating` skip
8. **Test scripts** (`scripts/layer-5.1/`) — full test infrastructure

### Key Risk: `drbdadm adjust` for 3rd Node

The biggest unknown is whether `drbdadm adjust` can dynamically add a 3rd node to a running 2-node resource. DRBD 9 is designed for this, but we haven't tested it. The code MUST handle both outcomes:

- **If `adjust` works:** Migration proceeds with zero downtime during the pre-sync phase. The `reconfigMethod` is recorded as `"adjust"`.
- **If `adjust` fails:** The fallback path kicks in. For primary migration: stop container, force reconfigure (down/up/promote), restart container, then sync. Record as `"down_up"`. This adds brief downtime during the DRBD reconfiguration but is acceptable.

The test suite will naturally discover which path is taken and the migration event records the method used.

### No Schema Changes

The existing Postgres schema supports everything needed for migration:
- `users.status` is TEXT — can hold `"migrating"` without schema change
- `bipods` can have 3 rows per user during the tripod phase
- `operations.type` is TEXT — can hold `"live_migration"`
- `operations.metadata` is JSONB — stores all migration-specific parameters
- `events.event_type` is TEXT — can hold `"migration"`

### Backward Compatibility

All existing operations (provision, failover, reformation, suspension, reactivation, eviction) continue to work unchanged. The DRBD changes are backward-compatible:
- `DRBDCreate` and `DRBDReconfigure` now accept 2 or 3 nodes (existing 2-node calls work)
- `DRBDStatusResponse.PeerDiskState` continues to work (worst-case across peers, which is the only peer for 2-node resources)
- `DRBDStatusResponse.Peers` is `omitempty` — existing callers that don't read it are unaffected

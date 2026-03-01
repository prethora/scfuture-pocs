# Layer 4.3 Build Prompt — Heartbeat Failure Detection & Automatic Failover

## What This Is

This is a build prompt for Layer 4.3 of the scfuture distributed agent platform. You are Claude Code. Your job is to:

1. Read all referenced existing files to understand the current codebase
2. Write all new code and scripts described below
3. Report back when code is written
4. When told "yes" / "ready", run the full test lifecycle (infra up → deploy → test → iterate on failures → teardown)
5. When all tests pass, update `SESSION.md` (in the parent directory) with what happened
6. Give a final report

The project lives in `scfuture/` (a subdirectory of the current working directory). All Go code paths are relative to `scfuture/`. All script paths are relative to `scfuture/`. The `SESSION.md` file is in the parent directory (current working directory).

---

## Context: What Exists

Layer 4.1 (machine agent) and Layer 4.2 (coordinator happy path) are complete and committed. Read these files first to understand the existing codebase:

### Existing code (read-only reference — do NOT rewrite these):

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
scfuture/internal/coordinator/server.go
scfuture/internal/coordinator/store.go
scfuture/internal/coordinator/fleet.go
scfuture/internal/coordinator/provisioner.go
scfuture/internal/coordinator/machineapi.go
scfuture/container/Dockerfile
scfuture/container/container-init.sh
```

Read ALL of these before writing any code. Pay close attention to:
- How API types are defined in `internal/shared/types.go`
- How the coordinator's `Store` struct manages in-memory state (machines, users, bipods)
- How `Machine.Status` is currently set to `"active"` on registration and never changed
- How `Machine.LastHeartbeat` is updated on every heartbeat
- How `Coordinator` struct is defined in `server.go` (holds `store *Store`)
- How the provisioner calls machine agent endpoints via `MachineClient`
- How `SelectMachines()` in `store.go` filters by `m.Status != "active"`
- How the machine agent's `DRBDPromote` uses `--force` flag (allows promotion without connected peer)
- How the machine agent's `ContainerStart` handles the full device-mount workflow

### Reference documents (in parent directory):

```
SESSION.md
architecture-v3.md (if present)
master-prompt4.2.md
```

---

## What Layer 4.3 Builds

**In scope:**
- Heartbeat timeout detection — coordinator detects when machine heartbeats stop arriving
- Machine health status transitions: `active` → `suspect` (30s) → `dead` (60s)
- Automatic failover when a machine dies: promote DRBD on surviving secondary, start container on new primary
- User status transitions during failover: `running` → `failing_over` → `running` (or `running_degraded` or `unavailable`)
- Failover event recording for observability
- New API endpoint: `GET /api/failovers` — returns recorded failover events
- Machine resurrection handling: if a dead machine's heartbeats resume, mark it active again (but do NOT re-integrate bipods)
- Full integration test: provision users → kill a fleet machine → verify automatic detection + failover + data integrity

**Explicitly NOT in scope (future layers):**
- Bipod reformation after failure — getting back to 2 copies (Layer 4.4)
- Dead machine return + re-integration into bipods (Layer 4.4)
- Suspension / reactivation / deletion lifecycle (Layer 4.5)
- Crash recovery / reconciliation (Layer 4.6)
- Live migration (Layer 5)

---

## Architecture

### Test Topology

Same as Layer 4.2: 1 coordinator + 3 fleet machines on Hetzner Cloud.

```
macOS (test harness, runs test_suite.sh via SSH / curl)
  │
  ├── l43-coordinator (CX23, coordinator :8080, private 10.0.0.2)
  │     │
  │     ├── l43-fleet-1 (CX23, machine-agent :8080, private 10.0.0.11)
  │     ├── l43-fleet-2 (CX23, machine-agent :8080, private 10.0.0.12)
  │     └── l43-fleet-3 (CX23, machine-agent :8080, private 10.0.0.13)
  │
  Private network: l43-net / 10.0.0.0/24
```

### How Failure Detection Works

The coordinator runs a **health checker goroutine** that ticks every 10 seconds:

1. For each registered machine, compare `time.Since(machine.LastHeartbeat)` against two thresholds
2. **Suspect threshold (30s / 3 missed heartbeats):** Mark machine `"suspect"`. Log warning. No action.
3. **Dead threshold (60s / 6 missed heartbeats):** Mark machine `"dead"`. Trigger failover for all affected users.
4. Only **newly-dead** machines trigger failover (transition from suspect/active → dead). Already-dead machines are skipped.

### Failover Sequence (per affected user)

When a machine transitions to `"dead"`, for each user with a bipod on that machine:

```
1. Identify surviving machine (the other bipod partner)
2. Check: was dead machine this user's primary?
   ├── YES (primary died): user needs failover
   │   a. Set user status → "failing_over"
   │   b. POST /images/{user_id}/drbd/promote on surviving machine
   │   c. POST /containers/{user_id}/start on surviving machine
   │   d. Update bipod roles: surviving → "primary", dead → "stale"
   │   e. Update user's PrimaryMachine to surviving machine
   │   f. Set user status → "running"
   │   g. Record failover event (success)
   │
   └── NO (secondary died): user keeps running on primary
       a. Mark dead machine's bipod role → "stale"
       b. Set user status → "running_degraded"
       c. Record failover event (degraded, no action needed)
```

**Error handling during failover:**
- If DRBD promote fails → set user status `"unavailable"`, record event
- If container start fails after successful promote → set user status `"running_degraded"` with error note, record event
- Each user's failover is independent — one failure does not block others
- Failover for a machine runs in its own goroutine to not block the health checker

### State Transitions

**Machine statuses:**
```
active  ──(30s no heartbeat)──▶  suspect  ──(60s no heartbeat)──▶  dead
  ▲                                                                  │
  └──────────────────(heartbeat resumes)─────────────────────────────┘
```

**User statuses (new):**
```
running  ──(primary dies)──▶  failing_over  ──▶  running (on new primary)
                                             ──▶  running_degraded (promote OK, container failed)
                                             ──▶  unavailable (promote failed)

running  ──(secondary dies)──▶  running_degraded (still on primary, no replication)
```

### Address Conventions

Same as Layer 4.2:
- Coordinator private: `10.0.0.2:8080`
- Fleet private: `10.0.0.11:8080`, `10.0.0.12:8080`, `10.0.0.13:8080`
- Machine agents register with their private IP as `NODE_ADDRESS`
- Coordinator calls machine agents via their registered address (private IP)
- Test harness calls coordinator and machine agents via public IPs

---

## Modifications to Existing Code

### 1. `internal/coordinator/store.go` — Add health check and failover support

Add `StatusChangedAt` field to the `Machine` struct:

```go
type Machine struct {
    // ... existing fields unchanged ...
    StatusChangedAt time.Time `json:"status_changed_at"`
}
```

Add a `FailoverEvent` struct and slice to the `Store`:

```go
type FailoverEvent struct {
    UserID      string    `json:"user_id"`
    FromMachine string    `json:"from_machine"`
    ToMachine   string    `json:"to_machine"`
    Type        string    `json:"type"`        // "primary_failed" or "secondary_failed"
    Success     bool      `json:"success"`
    Error       string    `json:"error,omitempty"`
    DurationMS  int64     `json:"duration_ms"`
    Timestamp   time.Time `json:"timestamp"`
}
```

Add to `Store` struct:

```go
type Store struct {
    // ... existing fields ...
    failoverEvents []FailoverEvent
}
```

Add to `persistState`:

```go
type persistState struct {
    // ... existing fields ...
    FailoverEvents []FailoverEvent `json:"failover_events"`
}
```

Update `persist()` and `NewStore()` to include `failoverEvents`.

Add new methods:

```go
// CheckMachineHealth scans all machines, updates statuses based on heartbeat age.
// Returns the list of machine IDs that just transitioned to "dead".
func (s *Store) CheckMachineHealth(suspectThreshold, deadThreshold time.Duration) []string

// GetUsersOnMachine returns all users that have a bipod on the given machine.
func (s *Store) GetUsersOnMachine(machineID string) []string

// GetSurvivingBipod returns the bipod NOT on the dead machine for a given user.
// Returns nil if no surviving bipod exists (both machines dead).
func (s *Store) GetSurvivingBipod(userID, deadMachineID string) *Bipod

// SetBipodRole updates a bipod's role.
func (s *Store) SetBipodRole(userID, machineID, role string)

// RecordFailoverEvent appends a failover event.
func (s *Store) RecordFailoverEvent(event FailoverEvent)

// GetFailoverEvents returns all recorded failover events.
func (s *Store) GetFailoverEvents() []FailoverEvent
```

**`CheckMachineHealth` implementation logic:**

```go
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
            slog.Info("[HEALTH] Machine status changed",
                "machine_id", id,
                "from", oldStatus,
                "to", newStatus,
                "last_heartbeat_ago", elapsed.String(),
            )
            if newStatus == "dead" {
                newlyDead = append(newlyDead, id)
            }
        }
    }

    s.persist()
    return newlyDead
}
```

**Machine resurrection in `UpdateHeartbeat`:** When a heartbeat arrives from a machine whose status is `"dead"` or `"suspect"`, reset status to `"active"` and log prominently:

```go
func (s *Store) UpdateHeartbeat(req *shared.FleetHeartbeatRequest) {
    s.mu.Lock()
    defer s.mu.Unlock()

    m, ok := s.machines[req.MachineID]
    if !ok {
        slog.Warn("Heartbeat from unknown machine", "machine_id", req.MachineID)
        return
    }

    // Resurrection: machine came back from dead/suspect
    if m.Status == "dead" || m.Status == "suspect" {
        slog.Info("[HEALTH] Machine resurrected",
            "machine_id", req.MachineID,
            "was", m.Status,
        )
        m.Status = "active"
        m.StatusChangedAt = time.Now()
    }

    m.DiskTotalMB = req.DiskTotalMB
    m.DiskUsedMB = req.DiskUsedMB
    m.RAMTotalMB = req.RAMTotalMB
    m.RAMUsedMB = req.RAMUsedMB
    m.ActiveAgents = req.ActiveAgents
    m.RunningAgents = req.RunningAgents
    m.LastHeartbeat = time.Now()

    s.persist()
}
```

### 2. `internal/coordinator/server.go` — Add failover events endpoint and expose store

Add the `GET /api/failovers` route to `RegisterRoutes`:

```go
mux.HandleFunc("GET /api/failovers", coord.handleGetFailovers)
```

Add handler:

```go
func (coord *Coordinator) handleGetFailovers(w http.ResponseWriter, r *http.Request) {
    events := coord.store.GetFailoverEvents()
    if events == nil {
        events = []FailoverEvent{}
    }
    writeJSON(w, http.StatusOK, events)
}
```

Add a `GetStore()` accessor on `Coordinator` so that `main.go` can pass it to the health checker:

```go
func (coord *Coordinator) GetStore() *Store {
    return coord.store
}
```

### 3. `internal/shared/types.go` — Add failover event type for API responses

Add:

```go
// ─── Failover types (from coordinator healthcheck) ───

type FailoverEventResponse struct {
    UserID      string `json:"user_id"`
    FromMachine string `json:"from_machine"`
    ToMachine   string `json:"to_machine"`
    Type        string `json:"type"`
    Success     bool   `json:"success"`
    Error       string `json:"error,omitempty"`
    DurationMS  int64  `json:"duration_ms"`
    Timestamp   string `json:"timestamp"`
}
```

Note: The coordinator `FailoverEvent` struct in `store.go` uses `time.Time` for `Timestamp`, but the API response uses string. The handler should convert when responding. Alternatively, since the coordinator's `handleGetFailovers` returns the internal type directly and `json.Marshal` handles `time.Time` as RFC3339, this works without conversion. Choose whichever is simpler — returning the internal type directly is fine for the PoC.

### 4. `cmd/coordinator/main.go` — Start health checker goroutine

After creating the coordinator and before starting the HTTP server:

```go
coord := coordinator.NewCoordinator(dataDir)

// Start health checker
coordinator.StartHealthChecker(coord.GetStore(), coordinator.NewMachineClient(""), coord)

mux := http.NewServeMux()
coord.RegisterRoutes(mux)
```

The health checker needs access to the `Store` (for `CheckMachineHealth`), a way to create `MachineClient` instances per-machine, and the `Coordinator` (for the failover logic that uses `MachineClient`). See the new file spec below for the exact interface.

---

## New Implementation

### New file: `internal/coordinator/healthcheck.go`

```go
package coordinator

import (
    "log/slog"
    "time"
)

const (
    HealthCheckInterval = 10 * time.Second
    SuspectThreshold    = 30 * time.Second  // 3 missed heartbeats
    DeadThreshold       = 60 * time.Second  // 6 missed heartbeats
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
```

Add to `Coordinator` (can go in `healthcheck.go` or a separate file — keep in `healthcheck.go`):

```go
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

    coord.store.SetUserStatus(userID, "failing_over", "")
    coord.store.SetBipodRole(userID, deadMachineID, "stale")

    client := NewMachineClient(survivingMachine.Address)

    // Step 1: Promote DRBD on surviving machine
    _, err := client.DRBDPromote(userID)
    if err != nil {
        logger.Error("DRBD promote failed", "error", err)
        coord.store.SetUserStatus(userID, "unavailable", "drbd promote failed: "+err.Error())
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

    // Step 2: Start container on surviving machine
    _, err = client.ContainerStart(userID)
    if err != nil {
        logger.Error("Container start failed after DRBD promote", "error", err)
        coord.store.SetBipodRole(userID, survivingBipod.MachineID, "primary")
        coord.store.SetUserPrimary(userID, survivingBipod.MachineID)
        coord.store.SetUserStatus(userID, "running_degraded", "container start failed after promote: "+err.Error())
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

    // Step 3: Update state
    coord.store.SetBipodRole(userID, survivingBipod.MachineID, "primary")
    coord.store.SetUserPrimary(userID, survivingBipod.MachineID)
    coord.store.SetUserStatus(userID, "running", "")

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
```

### Coordinator struct modification

The `Coordinator` struct in `server.go` currently holds only `store *Store`. It does NOT need additional fields — `StartHealthChecker` receives the store and coordinator as arguments. No changes to `Coordinator` struct needed.

---

## Test Scripts

All test scripts go in `scfuture/scripts/layer-4.3/`.

### `scripts/layer-4.3/run.sh`

Same orchestration pattern as Layer 4.2:

```bash
#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCFUTURE_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"

echo "═══ Layer 4.3: Heartbeat Failure Detection & Automatic Failover ═══"
echo "Started: $(date)"
echo ""

# Step 1: Build
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
./test_suite.sh
TEST_RESULT=$?

# Step 5: Teardown
echo ""
echo "Tearing down infrastructure..."
./infra.sh down

echo ""
echo "═══ Layer 4.3 Complete ═══"
echo "Finished: $(date)"

exit $TEST_RESULT
```

### `scripts/layer-4.3/common.sh`

Same as Layer 4.2's `common.sh` with prefix changes and an additional polling helper:

- Change `NETWORK_NAME="l43-net"`
- Change `SSH_KEY_NAME="l43-key"`
- Change all `l42-` server name references to `l43-`
- Copy all helpers: `save_ips`, `load_ips`, `get_public_ip`, `ssh_cmd`, `docker_exec`, `coord_api`, `machine_api`, `check`, `phase_start`, `phase_result`, `final_result`, `wait_for_user_status`
- Add new helper:

```bash
# Wait for a machine to reach a specific status in the coordinator
wait_for_machine_status() {
    local machine_id="$1" target_status="$2" timeout="${3:-90}"
    local elapsed=0
    local status=""
    while [ "$elapsed" -lt "$timeout" ]; do
        status=$(coord_api GET /api/fleet | jq -r ".machines[] | select(.machine_id == \"$machine_id\") | .status // empty")
        if [ "$status" = "$target_status" ]; then
            return 0
        fi
        sleep 2
        elapsed=$((elapsed + 2))
    done
    echo "  ✗ Timeout waiting for machine $machine_id to reach $target_status (stuck at $status)"
    return 1
}
```

### `scripts/layer-4.3/infra.sh`

Same as Layer 4.2's `infra.sh` with `l42` → `l43` prefix everywhere:
- `COORD="l43-coordinator"`
- `FLEET_MACHINES=("l43-fleet-1" "l43-fleet-2" "l43-fleet-3")`
- Rest of the structure identical.

### `scripts/layer-4.3/deploy.sh`

Same as Layer 4.2's `deploy.sh` with `l42` → `l43` header. No structural changes — deploys coordinator + 3 fleet machines identically.

### `scripts/layer-4.3/cloud-init/coordinator.yaml`

Identical to Layer 4.2.

### `scripts/layer-4.3/cloud-init/fleet.yaml`

Identical to Layer 4.2.

### `scripts/layer-4.3/test_suite.sh`

This is the core of Layer 4.3. The test proves automatic failure detection and failover.

```bash
#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/common.sh"
load_ips

echo "═══ Layer 4.3: Heartbeat Failure Detection & Automatic Failover — Test Suite ═══"

# ══════════════════════════════════════════
# Phase 0: Prerequisites
# ══════════════════════════════════════════
phase_start 0 "Prerequisites"

# Coordinator responding
check "Coordinator responding" 'coord_api GET /api/fleet | jq -e .machines'

# Wait for fleet machines to register
echo "  Waiting for 3 fleet machines to register..."
for i in $(seq 1 60); do
    count=$(coord_api GET /api/fleet | jq '.machines | length')
    [ "$count" -ge 3 ] && break
    sleep 2
done

check "3 fleet machines registered" '[ "$(coord_api GET /api/fleet | jq ".machines | length")" -ge 3 ]'

# Check each fleet machine is active
for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Machine agent responding at $ip" 'machine_api "'"$ip"'" GET /status | jq -e .machine_id'
done

# All machines active
check "All machines active" '
    dead=$(coord_api GET /api/fleet | jq "[.machines[] | select(.status != \"active\")] | length")
    [ "$dead" -eq 0 ]
'

# DRBD module on each fleet machine
for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "DRBD module loaded on $ip" 'ssh_cmd "'"$ip"'" "lsmod | grep -q drbd"'
done

# Container image on each fleet machine
for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Container image on $ip" 'ssh_cmd "'"$ip"'" "docker images platform/app-container -q" | grep -q .'
done

# Failover events should be empty initially
check "No failover events initially" '[ "$(coord_api GET /api/failovers | jq ". | length")" -eq 0 ]'

phase_result

# ══════════════════════════════════════════
# Phase 1: Provision Users (baseline)
# ══════════════════════════════════════════
phase_start 1 "Provision Users (baseline)"

# Provision 3 users so we have bipods spread across machines
for user in alice bob charlie; do
    coord_api POST /api/users "{\"user_id\":\"$user\"}" > /dev/null
    coord_api POST /api/users/$user/provision > /dev/null
done

for user in alice bob charlie; do
    check "$user reaches running" 'wait_for_user_status '"$user"' running 180'
done

# Verify all running
for user in alice bob charlie; do
    check "$user is running" '[ "$(coord_api GET /api/users/'"$user"' | jq -r .status)" = "running" ]'
done

# Record which machine is primary for each user
echo ""
echo "  User placements:"
for user in alice bob charlie; do
    primary=$(coord_api GET /api/users/$user | jq -r .primary_machine)
    echo "    $user → primary: $primary"
done

# Write test data into each user's container
for user in alice bob charlie; do
    PRIMARY_ID=$(coord_api GET /api/users/$user | jq -r .primary_machine)
    PRIMARY_PUB=$(get_public_ip "$PRIMARY_ID")
    docker_exec "$PRIMARY_PUB" ${user}-agent "sh -c 'echo ${user}-data-before > /workspace/data/test.txt'"
done

for user in alice bob charlie; do
    PRIMARY_ID=$(coord_api GET /api/users/$user | jq -r .primary_machine)
    PRIMARY_PUB=$(get_public_ip "$PRIMARY_ID")
    check "$user data written" '
        result=$(docker_exec "'"$PRIMARY_PUB"'" '"$user"'-agent "cat /workspace/data/test.txt")
        [ "$result" = "'"$user"'-data-before" ]
    '
done

# Verify DRBD replication is healthy for all users
for user in alice bob charlie; do
    PRIMARY_ID=$(coord_api GET /api/users/$user | jq -r .primary_machine)
    PRIMARY_PUB=$(get_public_ip "$PRIMARY_ID")
    check "$user DRBD healthy (UpToDate)" '
        machine_api "'"$PRIMARY_PUB"'" GET /images/'"$user"'/drbd/status | jq -e ".peer_disk_state == \"UpToDate\""
    '
done

phase_result

# ══════════════════════════════════════════
# Phase 2: Kill a Fleet Machine
# ══════════════════════════════════════════
phase_start 2 "Kill a Fleet Machine"

# Determine which machine to kill — pick fleet-1 (has known IP)
KILL_MACHINE_ID="fleet-1"
KILL_PUB_IP="$FLEET1_PUB_IP"

echo "  Target: $KILL_MACHINE_ID ($KILL_PUB_IP)"
echo "  Users on this machine (as primary or secondary):"
coord_api GET /api/users | jq -r '.[] | select(.bipod[].machine_id == "fleet-1") | "    \(.user_id) (primary: \(.primary_machine))"'

# Record which users have their primary on fleet-1 (these need failover)
FAILOVER_USERS=$(coord_api GET /api/users | jq -r '.[] | select(.primary_machine == "fleet-1") | .user_id')
DEGRADED_USERS=$(coord_api GET /api/users | jq -r '.[] | select(.primary_machine != "fleet-1") | select(.bipod[].machine_id == "fleet-1") | .user_id')

echo "  Users needing failover (primary on fleet-1): $FAILOVER_USERS"
echo "  Users becoming degraded (secondary on fleet-1): $DEGRADED_USERS"

# Shutdown the machine via hcloud (simulates hardware failure)
check "Shutdown fleet-1" 'hcloud server shutdown l43-fleet-1'

# Verify machine is actually down (SSH should fail)
sleep 5
check "fleet-1 unreachable via SSH" '! ssh_cmd "$KILL_PUB_IP" "true" 2>/dev/null'

phase_result

# ══════════════════════════════════════════
# Phase 3: Failure Detection
# ══════════════════════════════════════════
phase_start 3 "Failure Detection"

echo "  Waiting for coordinator to detect fleet-1 as dead (up to 90s)..."
check "fleet-1 detected as dead" 'wait_for_machine_status "fleet-1" "dead" 90'

# Other machines should still be active
check "fleet-2 still active" '
    status=$(coord_api GET /api/fleet | jq -r ".machines[] | select(.machine_id == \"fleet-2\") | .status")
    [ "$status" = "active" ]
'
check "fleet-3 still active" '
    status=$(coord_api GET /api/fleet | jq -r ".machines[] | select(.machine_id == \"fleet-3\") | .status")
    [ "$status" = "active" ]
'

phase_result

# ══════════════════════════════════════════
# Phase 4: Automatic Failover Verification
# ══════════════════════════════════════════
phase_start 4 "Automatic Failover Verification"

# Wait for failover to complete — users should reach running or running_degraded
echo "  Waiting for failover to complete..."
sleep 10  # Give the failover goroutine time to work

# Check all users' final status
for user in alice bob charlie; do
    USER_STATUS=$(coord_api GET /api/users/$user | jq -r .status)
    check "$user status is running or running_degraded (got: $USER_STATUS)" '
        status=$(coord_api GET /api/users/'"$user"' | jq -r .status)
        [ "$status" = "running" ] || [ "$status" = "running_degraded" ]
    '
done

# Verify that users whose primary was on fleet-1 have been failed over
if [ -n "$FAILOVER_USERS" ]; then
    for user in $FAILOVER_USERS; do
        NEW_PRIMARY=$(coord_api GET /api/users/$user | jq -r .primary_machine)
        check "$user primary moved from fleet-1 to $NEW_PRIMARY" '
            primary=$(coord_api GET /api/users/'"$user"' | jq -r .primary_machine)
            [ "$primary" != "fleet-1" ]
        '

        # Verify DRBD is Primary on the new machine
        NEW_PUB=$(get_public_ip "$NEW_PRIMARY")
        check "$user DRBD is Primary on new machine ($NEW_PRIMARY)" '
            machine_api "'"$NEW_PUB"'" GET /images/'"$user"'/drbd/status | jq -e ".role == \"Primary\""
        '

        # Verify container is running on new machine
        check "$user container running on $NEW_PRIMARY" '
            machine_api "'"$NEW_PUB"'" GET /containers/'"$user"'/status | jq -e .running
        '
    done
fi

# Verify failover events were recorded
check "Failover events recorded" '[ "$(coord_api GET /api/failovers | jq ". | length")" -gt 0 ]'

# Verify each failover event has the right structure
check "Failover events have correct structure" '
    coord_api GET /api/failovers | jq -e ".[0].user_id" &&
    coord_api GET /api/failovers | jq -e ".[0].from_machine" &&
    coord_api GET /api/failovers | jq -e ".[0].type"
'

phase_result

# ══════════════════════════════════════════
# Phase 5: Data Integrity After Failover
# ══════════════════════════════════════════
phase_start 5 "Data Integrity After Failover"

# For each user: verify data written BEFORE failover survived
for user in alice bob charlie; do
    USER_STATUS=$(coord_api GET /api/users/$user | jq -r .status)
    if [ "$USER_STATUS" = "running" ] || [ "$USER_STATUS" = "running_degraded" ]; then
        PRIMARY_ID=$(coord_api GET /api/users/$user | jq -r .primary_machine)
        PRIMARY_PUB=$(get_public_ip "$PRIMARY_ID")

        # Check: can we read test data?
        check "$user pre-failover data survived" '
            result=$(docker_exec "'"$PRIMARY_PUB"'" '"$user"'-agent "cat /workspace/data/test.txt")
            [ "$result" = "'"$user"'-data-before" ]
        '

        # Check: can we write NEW data?
        docker_exec "$PRIMARY_PUB" ${user}-agent "sh -c 'echo ${user}-data-after > /workspace/data/test2.txt'" 2>/dev/null || true
        check "$user can write new data after failover" '
            result=$(docker_exec "'"$PRIMARY_PUB"'" '"$user"'-agent "cat /workspace/data/test2.txt")
            [ "$result" = "'"$user"'-data-after" ]
        '

        # Check: config.json from initial provisioning still present
        check "$user config.json survived failover" '
            docker_exec "'"$PRIMARY_PUB"'" '"$user"'-agent "cat /workspace/data/config.json" | jq -e .user
        '
    fi
done

phase_result

# ══════════════════════════════════════════
# Phase 6: Unaffected Users & Degraded State
# ══════════════════════════════════════════
phase_start 6 "Unaffected Users & Degraded State"

# Users whose primary was NOT on fleet-1 should still be "running" or "running_degraded"
if [ -n "$DEGRADED_USERS" ]; then
    for user in $DEGRADED_USERS; do
        check "$user is running_degraded (secondary lost)" '
            status=$(coord_api GET /api/users/'"$user"' | jq -r .status)
            [ "$status" = "running_degraded" ] || [ "$status" = "running" ]
        '

        # Verify primary hasn't changed
        check "$user primary unchanged" '
            primary=$(coord_api GET /api/users/'"$user"' | jq -r .primary_machine)
            [ "$primary" != "fleet-1" ]
        '

        # Verify container still running on original primary
        PRIMARY_ID=$(coord_api GET /api/users/$user | jq -r .primary_machine)
        PRIMARY_PUB=$(get_public_ip "$PRIMARY_ID")
        check "$user container still running on $PRIMARY_ID" '
            machine_api "'"$PRIMARY_PUB"'" GET /containers/'"$user"'/status | jq -e .running
        '
    done
fi

# Verify bipod roles are consistent
for user in alice bob charlie; do
    check "$user has a 'stale' bipod on fleet-1" '
        coord_api GET /api/users/'"$user"'/bipod | jq -e ".[] | select(.machine_id == \"fleet-1\") | .role == \"stale\""
    ' 2>/dev/null || true  # Some users might not have bipods on fleet-1
done

phase_result

# ══════════════════════════════════════════
# Phase 7: Coordinator State Consistency
# ══════════════════════════════════════════
phase_start 7 "Coordinator State Consistency"

# Fleet status: fleet-1 should be dead, others active
check "Fleet shows fleet-1 as dead" '
    coord_api GET /api/fleet | jq -e ".machines[] | select(.machine_id == \"fleet-1\") | .status == \"dead\""
'
check "Fleet shows fleet-2 as active" '
    coord_api GET /api/fleet | jq -e ".machines[] | select(.machine_id == \"fleet-2\") | .status == \"active\""
'
check "Fleet shows fleet-3 as active" '
    coord_api GET /api/fleet | jq -e ".machines[] | select(.machine_id == \"fleet-3\") | .status == \"active\""
'

# No user should claim fleet-1 as primary
check "No user has fleet-1 as primary" '
    count=$(coord_api GET /api/users | jq "[.[] | select(.primary_machine == \"fleet-1\")] | length")
    [ "$count" -eq 0 ]
'

# All users should be in a valid state
check "All users in valid state" '
    coord_api GET /api/users | jq -e ".[] | .status" | while read status; do
        case $status in
            \"running\"|\"running_degraded\"|\"unavailable\") true ;;
            *) exit 1 ;;
        esac
    done
'

# state.json should reflect the correct state
check "Coordinator state.json persisted" '
    ssh_cmd "$COORD_PUB_IP" "test -f /data/state.json" &&
    ssh_cmd "$COORD_PUB_IP" "cat /data/state.json | jq -e .machines"
'

phase_result

# ══════════════════════════════════════════
# Phase 8: Cleanup
# ══════════════════════════════════════════
phase_start 8 "Cleanup"

# Clean up surviving machines only (fleet-1 is down)
for ip_var in FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Cleanup $ip" 'machine_api "'"$ip"'" POST /cleanup'
done

# Verify clean state on surviving machines
for ip_var in FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Machine $ip clean" '
        users=$(machine_api "'"$ip"'" GET /status | jq ".users | length")
        [ "$users" -eq 0 ]
    '
done

phase_result

# ══════════════════════════════════════════
final_result
```

---

## Critical Constraints

These are hard-won rules from SESSION.md. Violating any of them will cause test failures or data corruption.

**Carried forward from Layers 4.1 and 4.2:**

1. **DRBD config hostnames must match `hostname` output** — the DRBD resource file's `on <hostname>` blocks must match the system hostname. Deploy scripts set hostname to node ID (e.g., `fleet-1`).

2. **DRBD needs promote-before-sync** — the secondary won't sync until one side is Primary. Always promote the intended primary BEFORE waiting for sync.

3. **DRBD `--force` is required for initial promotion** — both sides start as Secondary. The first `drbdadm primary --force` is needed because there's no data to agree on yet. This same `--force` flag also works for failover promotion when the peer is disconnected.

4. **Container device-mount pattern** — containers get the DRBD device via `--device`, not bind mounts. The init script inside the container mounts the Btrfs subvolume. The host does NOT keep Btrfs mounted after provisioning.

5. **Host must NOT have Btrfs mounted** when container starts — `FormatBtrfs` unmounts after formatting. If the host still has the Btrfs filesystem mounted, the container's `mount` inside will fail with "device already mounted".

6. **Protocol A (async) replication** — DRBD is configured with `protocol A` for performance. Writes are acknowledged locally before reaching the peer. During failover, the last few seconds of writes may be lost. This is a known, accepted trade-off.

7. **Sparse files for disk efficiency** — images are created with `truncate`, not `dd`. Actual disk usage is proportional to data written, not apparent size.

8. **`vfs` storage driver for inner Docker** — not `overlay2`. DinD (or bare metal with kernel DRBD) uses the `vfs` storage driver for the nested Docker daemon.

9. **Private IPs for inter-machine communication** — coordinator talks to machine agents via 10.0.0.x addresses. Public IPs are only for SSH and the test harness.

**New constraints for Layer 4.3:**

10. **Failover must be idempotent** — if `failoverUser` is called twice for the same user+machine, the second call must be a no-op. Check user status before acting (skip if not `"running"` or `"running_degraded"`). The machine agent's `DRBDPromote` already returns `already_existed: true` if already Primary.

11. **Never promote a secondary that's already Primary** — the machine agent handles this gracefully (returns success with `already_existed`), but the coordinator should still check.

12. **Failover must not block the health checker** — each `failoverMachine` runs in its own goroutine so one slow failover doesn't delay detection of other failures.

13. **Dead machine resurrection must not auto-integrate** — if a dead machine's heartbeats resume, set status back to `"active"` but do NOT touch bipods or users. Bipod reformation is Layer 4.4.

14. **Container start requires DRBD Primary** — never attempt to start a container on a machine where DRBD is secondary. The mount will fail because the device is read-only.

15. **`hcloud server shutdown` for realistic testing** — use `hcloud server shutdown` (not `hcloud server poweroff`) to stop the target machine. This simulates a graceful system halt, which is close enough to hardware failure for the heartbeat timeout test. The key behavior: the machine agent process stops, heartbeats cease, DRBD connections drop.

---

## Directory Structure After Layer 4.3

```
scfuture/
├── go.mod                                           # unchanged
├── Makefile                                         # unchanged
├── schema.sql                                       # unchanged
├── .gitignore                                       # unchanged
├── architecture.md                                  # unchanged
│
├── bin/
│   ├── machine-agent                                # build output
│   └── coordinator                                  # build output
│
├── cmd/
│   ├── machine-agent/
│   │   └── main.go                                  # unchanged
│   └── coordinator/
│       └── main.go                                  # MODIFIED: start health checker
│
├── internal/
│   ├── shared/
│   │   └── types.go                                 # MODIFIED: add FailoverEventResponse
│   ├── machineagent/
│   │   ├── server.go                                # unchanged
│   │   ├── state.go                                 # unchanged
│   │   ├── images.go                                # unchanged
│   │   ├── drbd.go                                  # unchanged
│   │   ├── btrfs.go                                 # unchanged
│   │   ├── containers.go                            # unchanged
│   │   ├── cleanup.go                               # unchanged
│   │   ├── exec.go                                  # unchanged
│   │   └── heartbeat.go                             # unchanged
│   └── coordinator/
│       ├── server.go                                # MODIFIED: add /api/failovers, GetStore()
│       ├── store.go                                 # MODIFIED: add health check methods, failover events
│       ├── fleet.go                                 # MODIFIED: resurrection handling in UpdateHeartbeat
│       ├── provisioner.go                           # unchanged
│       ├── machineapi.go                            # unchanged
│       └── healthcheck.go                           # NEW: health checker + failover logic
│
├── container/
│   ├── Dockerfile                                   # unchanged
│   └── container-init.sh                            # unchanged
│
└── scripts/
    ├── layer-4.1/                                   # unchanged
    │   ├── run.sh
    │   ├── common.sh
    │   ├── infra.sh
    │   ├── deploy.sh
    │   ├── test_suite.sh
    │   └── cloud-init/fleet.yaml
    │
    ├── layer-4.2/                                   # unchanged
    │   ├── run.sh
    │   ├── common.sh
    │   ├── infra.sh
    │   ├── deploy.sh
    │   ├── test_suite.sh
    │   └── cloud-init/
    │       ├── coordinator.yaml
    │       └── fleet.yaml
    │
    └── layer-4.3/                                   # NEW
        ├── run.sh
        ├── common.sh
        ├── infra.sh
        ├── deploy.sh
        ├── test_suite.sh
        └── cloud-init/
            ├── coordinator.yaml
            └── fleet.yaml
```

---

## Execution Instructions

### Phase 1: Write Code

Read all existing files listed in "Context: What Exists" above. Then:

1. Modify `internal/coordinator/store.go` — add `StatusChangedAt` to Machine, add `FailoverEvent` struct, add `failoverEvents` to Store, add `CheckMachineHealth`, `GetUsersOnMachine`, `GetSurvivingBipod`, `SetBipodRole`, `RecordFailoverEvent`, `GetFailoverEvents`. Update `UpdateHeartbeat` for resurrection handling. Update `persist()` and `persistState`.
2. Modify `internal/coordinator/server.go` — add `GET /api/failovers` route and handler, add `GetStore()` accessor.
3. Modify `internal/shared/types.go` — add `FailoverEventResponse` type.
4. Modify `cmd/coordinator/main.go` — start health checker goroutine after creating coordinator.
5. Create `internal/coordinator/healthcheck.go` — health checker + failover logic.
6. Create all test scripts in `scripts/layer-4.3/`.

Make all scripts executable (`chmod +x`).

Report back when code is written. Do NOT run tests yet.

### Phase 2: Run Tests

When told "yes" / "ready":

```bash
cd scfuture/scripts/layer-4.3
./run.sh
```

This builds, creates infra, deploys, and runs the test suite.

**If tests fail:** Do NOT tear down infrastructure. Fix the issue (in Go code or test scripts), rebuild (`cd scfuture && make build`), redeploy if needed (`cd scripts/layer-4.3 && ./deploy.sh`), and re-run only the test suite (`./test_suite.sh`). Iterate until all checks pass.

**When all tests pass:** Tear down infrastructure (`./infra.sh down`).

### Phase 3: Update SESSION.md

Append a new section to the parent directory's `SESSION.md` documenting:
- What Layer 4.3 built
- Test results (checks passed/failed)
- Issues encountered and fixes (continue numbering from Layer 4.2)
- Any drift from this prompt
- Updated PoC progression plan

### Phase 4: Final Report

Provide a summary including:
- Total checks: passed / total
- Number of issues encountered
- Key technical observations
- What Layer 4.4 will need to address

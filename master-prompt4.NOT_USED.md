# Layer 4 Build Prompt: Coordinator & Machine Agent PoC

## Overview

Build a Go-based orchestration system that manages multiple users' replicated environments (bipods) across a fleet of machines. This is Layer 4 in the proof-of-concept progression for a distributed agent platform. It proves that a coordinator service can autonomously provision, monitor, failover, and recover multi-user bipod deployments — including full crash recovery from any partial state.

### What This PoC Proves

1. **Automated multi-user provisioning** — API call creates a user, coordinator assigns two machines, provisions the full block device stack (image → loop → DRBD → Btrfs → container) without human intervention
2. **Failure detection and automatic failover** — machine dies, coordinator detects via heartbeat timeout, promotes the surviving secondary, agent is back online
3. **Bipod reformation** — after failover, coordinator picks a new machine and rebuilds the second copy
4. **Crash recovery from any partial state** — coordinator can be killed at any point during any operation and recover correctly on restart
5. **Multi-user density** — 6 users across 3 machines, balanced placement, no resource collisions
6. **Deterministic fault injection** — 18 specific crash points tested, each verified for correct recovery
7. **Chaos mode** — random crashes across hundreds of iterations with consistency verification

### What This PoC Does NOT Cover

- Live migration (Layer 5)
- Backblaze B2 backups
- Snapshots / tweak mode
- Credential proxy / cost tracking
- Telegram gateway
- Rebalancing
- Fleet scale up/down
- Real agent containers (uses dummy alpine containers)

### Prior Art

This PoC builds on three completed PoCs:

- **PoC 1 (poc-btrfs)**: Proved Btrfs world isolation with device-mount pattern
- **PoC 2 (poc-drbd)**: Proved DRBD bipod replication between two Hetzner servers, failover, data survival (47/47 checks)
- **PoC 3 (poc-backblaze)**: Proved Backblaze B2 backup and cold restore with DRBD (65/65 checks)

The shell commands used in PoCs 2 and 3 for DRBD, Btrfs, and Docker operations are the reference for the machine agent's Go implementation. This PoC replaces manual orchestration with software orchestration.

---

## Infrastructure

### Machines

4 Hetzner Cloud CX23 instances (2 vCPU, 4GB RAM, 40GB disk), Ubuntu 24.04, private network 10.0.0.0/24.

| Machine | Role | Private IP | Public IP |
|---------|------|------------|-----------|
| coordinator | Coordinator service | 10.0.0.1 | (assigned by Hetzner) |
| fleet-1 | Machine agent | 10.0.0.2 | (assigned by Hetzner) |
| fleet-2 | Machine agent | 10.0.0.3 | (assigned by Hetzner) |
| fleet-3 | Machine agent | 10.0.0.4 | (assigned by Hetzner) |

All machines are on the same Hetzner private network. The coordinator communicates with machine agents over private IPs. DRBD replication uses private IPs. The test harness on macOS connects via public IPs (SSH and HTTP).

### Database

Supabase Postgres (free tier). The connection string is provided as an environment variable (`DATABASE_URL`). Both the coordinator and the test harness use this same connection string. The database is external to all machines — it survives coordinator restarts and machine failures.

### Why Supabase

The coordinator must be testable for crash recovery. If the database is on the coordinator machine, killing the coordinator process risks database state. An external managed database eliminates this variable entirely. Supabase's free tier provides a production-grade Postgres instance at zero cost.

---

## Network Topology

```
macOS (test harness)
  │
  ├── Public Internet ──→ coordinator (public IP, port 8080) — HTTP API
  ├── Public Internet ──→ fleet-1 (public IP, port 22) — SSH for ground truth
  ├── Public Internet ──→ fleet-2 (public IP, port 22) — SSH for ground truth
  └── Public Internet ──→ fleet-3 (public IP, port 22) — SSH for ground truth

Hetzner private network (10.0.0.0/24):
  coordinator (10.0.0.1) ──HTTP──→ fleet-1 (10.0.0.2:8080)
                          ──HTTP──→ fleet-2 (10.0.0.3:8080)
                          ──HTTP──→ fleet-3 (10.0.0.4:8080)

  fleet-1 ←──DRBD──→ fleet-2  (per-user ports, 7900+)
  fleet-1 ←──DRBD──→ fleet-3
  fleet-2 ←──DRBD──→ fleet-3
```

---

## Database Schema

Create these tables in Supabase Postgres. The test harness seeds the `machines` table before starting the coordinator.

```sql
-- Machines in the fleet
CREATE TABLE machines (
    machine_id      TEXT PRIMARY KEY,
    address         TEXT NOT NULL,              -- private IP:port of machine agent
    status          TEXT NOT NULL DEFAULT 'active',  -- active | offline
    disk_used_mb    INTEGER DEFAULT 0,
    ram_used_mb     INTEGER DEFAULT 0,
    active_agents   INTEGER DEFAULT 0,
    max_agents      INTEGER DEFAULT 10,        -- low for PoC
    last_heartbeat  TIMESTAMPTZ
);

-- User accounts
CREATE TABLE users (
    user_id         TEXT PRIMARY KEY,
    status          TEXT NOT NULL DEFAULT 'provisioning',
                    -- provisioning | running | suspended | failed
    primary_machine TEXT REFERENCES machines(machine_id),
    drbd_port       INTEGER UNIQUE,            -- globally unique DRBD port
    created_at      TIMESTAMPTZ DEFAULT NOW(),
    suspended_at    TIMESTAMPTZ
);

-- Bipod members (2 rows per user when healthy)
CREATE TABLE bipods (
    user_id         TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    machine_id      TEXT NOT NULL REFERENCES machines(machine_id),
    role            TEXT NOT NULL,              -- primary | secondary
    drbd_minor      INTEGER,                   -- local to this machine
    state           TEXT DEFAULT 'pending',
                    -- Provisioning states (in order):
                    --   pending → image_created → drbd_configured → drbd_synced
                    --   → promoted → formatted → mounted → containers_running → ready
                    -- Failure states:
                    --   failed | torn_down
                    -- Suspension:
                    --   suspended
    last_verified   TIMESTAMPTZ,
    created_at      TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (user_id, machine_id)
);

-- Multi-step operations (for crash recovery)
CREATE TABLE operations (
    operation_id    TEXT PRIMARY KEY,
    type            TEXT NOT NULL,              -- provision | failover | reform_bipod | suspend | reactivate | cleanup
    user_id         TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    status          TEXT NOT NULL DEFAULT 'pending',
                    -- pending | in_progress | complete | failed | cancelled
    current_step    TEXT,                       -- tracks progress through multi-step operation
    metadata        JSONB DEFAULT '{}',         -- operation-specific data (e.g., target machines)
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    error           TEXT
);

-- Event log
CREATE TABLE events (
    event_id        SERIAL PRIMARY KEY,
    timestamp       TIMESTAMPTZ DEFAULT NOW(),
    event_type      TEXT NOT NULL,
    machine_id      TEXT,
    user_id         TEXT,
    operation_id    TEXT,
    details         JSONB
);

-- Indexes
CREATE INDEX idx_bipods_machine ON bipods(machine_id);
CREATE INDEX idx_bipods_state ON bipods(state);
CREATE INDEX idx_operations_status ON operations(status);
CREATE INDEX idx_operations_user ON operations(user_id);
CREATE INDEX idx_events_type ON events(event_type);
CREATE INDEX idx_events_timestamp ON events(timestamp);
```

---

## Go Project Structure

Single Go module, two binaries:

```
poc-coordinator/
├── go.mod                           (module: poc-coordinator)
├── go.sum
├── Makefile
├── cmd/
│   ├── coordinator/
│   │   └── main.go                  # Coordinator entry point
│   └── machine-agent/
│       └── main.go                  # Machine agent entry point
├── internal/
│   ├── coordinator/
│   │   ├── server.go                # HTTP server, route registration
│   │   ├── provisioner.go           # Provisioning state machine (11 steps)
│   │   ├── failover.go              # Failure detection + failover logic
│   │   ├── reformer.go              # Bipod reformation after failover
│   │   ├── suspender.go             # Suspend + reactivate logic
│   │   ├── reconciler.go            # Startup reconciliation (5 phases)
│   │   ├── heartbeat.go             # Heartbeat monitor loop
│   │   ├── bipod_health.go          # Background bipod health checker
│   │   ├── placement.go             # Machine selection algorithm
│   │   ├── fault_injection.go       # Deterministic + chaos mode checkpoints
│   │   ├── db.go                    # All database queries and updates
│   │   └── cleanup.go              # Orphan detection + resource cleanup
│   ├── machineagent/
│   │   ├── server.go                # HTTP server, route registration
│   │   ├── images.go                # Image create/delete, loop device mgmt
│   │   ├── drbd.go                  # DRBD lifecycle (config, promote, demote, status, destroy)
│   │   ├── mount.go                 # Btrfs mount/unmount
│   │   ├── containers.go            # Docker container start/stop/status
│   │   ├── heartbeat.go             # Heartbeat sender loop
│   │   ├── state.go                 # Local state discovery on startup
│   │   └── cleanup.go              # Full cleanup of all user resources
│   └── shared/
│       ├── types.go                 # Shared request/response types
│       ├── client.go                # Typed HTTP client wrappers
│       └── logging.go              # Structured logging (JSON)
├── scripts/
│   ├── run.sh                       # Full PoC lifecycle
│   ├── common.sh                    # Shared functions (IP discovery, helpers)
│   ├── infra.sh                     # Hetzner machine lifecycle (up/down/status)
│   ├── deploy.sh                    # Cross-compile + SCP binaries
│   ├── seed_db.sh                   # Create schema + seed machines table
│   ├── reset.sh                     # Reset DB + clean all machines
│   ├── test_suite.sh                # All test phases
│   ├── consistency_checker.sh       # Ground truth verification
│   └── cloud-init/
│       ├── coordinator.yaml
│       └── fleet.yaml
└── BUILD_PROMPT.md                  # This file
```

### Makefile

```makefile
.PHONY: build build-coordinator build-agent deploy test clean

build: build-coordinator build-agent

build-coordinator:
	GOOS=linux GOARCH=amd64 go build -o bin/coordinator ./cmd/coordinator

build-agent:
	GOOS=linux GOARCH=amd64 go build -o bin/machine-agent ./cmd/machine-agent

deploy: build
	./scripts/deploy.sh

test:
	./scripts/test_suite.sh

clean:
	rm -rf bin/
```

---

## Component 1: Machine Agent

The machine agent is a Go HTTP server that runs on each fleet machine. It translates coordinator requests into local system operations (losetup, drbdadm, mount, docker). All endpoints are **idempotent** — safe to retry after coordinator crash.

### Startup Sequence

```
1. Read configuration from environment variables:
     NODE_ID (required — e.g. "fleet-1")
     NODE_ADDRESS (required — e.g. "10.0.0.2:8080")
     COORDINATOR_URL (required — e.g. "http://10.0.0.1:8080")
     DATA_DIR (default: "/data")
     MAX_AGENTS (default: "10")
     DRBD_MINOR_START (required — e.g. "0", "100", "200")
2. Discover existing local state:
   - Scan losetup -a for active loop devices matching /data/images/*.img
   - Scan drbdadm status all for DRBD resources
   - Scan mount output for /mnt/users/* mounts
   - Scan docker ps for *-agent containers
   - Build in-memory state map: user_id → { loop_device, drbd_resource, mounted, container_running }
3. Start HTTP server on 0.0.0.0:8080
4. Start heartbeat sender loop (every 5 seconds, POST to coordinator)
5. Log: "Machine agent ready: {node_id} at {address}, {N} existing users discovered"
```

### API Endpoints

#### GET /status

Returns machine health and per-user resource state. This is the coordinator's primary source of truth during reconciliation.

Response:
```json
{
    "machine_id": "fleet-1",
    "disk_total_mb": 40000,
    "disk_used_mb": 3500,
    "ram_total_mb": 4096,
    "ram_used_mb": 1200,
    "cpu_load": 0.8,
    "users": {
        "alice": {
            "image_exists": true,
            "loop_device": "/dev/loop0",
            "drbd_resource": "user-alice",
            "drbd_role": "Primary",
            "drbd_connection": "Connected",
            "drbd_peer_state": "UpToDate",
            "btrfs_mounted": true,
            "mount_point": "/mnt/users/alice",
            "container_running": true,
            "container_name": "alice-agent"
        }
    }
}
```

Implementation: calls `losetup -a`, `drbdadm status all`, `mount`, `docker ps --format json` and aggregates.

#### POST /images/{user_id}/create

Creates an empty sparse image file and attaches a loop device. The image is NOT formatted — Btrfs formatting happens later via `format-btrfs` on the DRBD device (DRBD-first rule from PoC 3).

Request body:
```json
{
    "image_size_mb": 512
}
```

Implementation:
```
1. Check if /data/images/{user_id}.img already exists
   → If yes AND loop device attached: return 200 with {"already_existed": true, "loop_device": "..."}
   → If yes but no loop device: attach loop device, return 200
2. truncate -s {image_size_mb}M /data/images/{user_id}.img
3. losetup -f /data/images/{user_id}.img → capture loop device path
4. Store mapping: user_id → loop_device in memory
5. Return 200 with {"loop_device": "/dev/loop0"}
```

#### DELETE /images/{user_id}

Tears down ALL resources for a user in reverse order. Each step checks if the resource exists before acting.

Implementation:
```
1. If container {user_id}-agent running → docker stop + docker rm
2. If /mnt/users/{user_id} mounted → umount
3. If DRBD resource user-{user_id} exists → drbdadm down user-{user_id}
4. If /etc/drbd.d/user-{user_id}.res exists → rm
5. If loop device attached to {user_id}.img → losetup -d
6. If /data/images/{user_id}.img exists → rm
7. If /mnt/users/{user_id} dir exists → rmdir
8. Remove from in-memory state
9. Return 200
```

#### POST /images/{user_id}/drbd/create

Configures DRBD resource for this user. Both machines in the bipod receive the SAME request body and write the SAME config file. DRBD determines which `on` block is local by matching the system hostname.

**Prerequisite:** Images must be created on BOTH machines first (coordinator needs both loop device paths).

Request body:
```json
{
    "resource_name": "user-alice",
    "nodes": [
        {
            "hostname": "fleet-1",
            "minor": 0,
            "disk": "/dev/loop0",
            "address": "10.0.0.2"
        },
        {
            "hostname": "fleet-2",
            "minor": 100,
            "disk": "/dev/loop1",
            "address": "10.0.0.3"
        }
    ],
    "port": 7900
}
```

Implementation:
```
1. Check if DRBD resource user-{user_id} already exists (drbdadm status) → return 200 if so
2. Write /etc/drbd.d/user-{user_id}.res:

   resource user-{user_id} {
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
       on {nodes[0].hostname} {
           device /dev/drbd{nodes[0].minor} minor {nodes[0].minor};
           disk {nodes[0].disk};
           address {nodes[0].address}:{port};
           meta-disk internal;
       }
       on {nodes[1].hostname} {
           device /dev/drbd{nodes[1].minor} minor {nodes[1].minor};
           disk {nodes[1].disk};
           address {nodes[1].address}:{port};
           meta-disk internal;
       }
   }

3. drbdadm create-md --force user-{user_id}
4. drbdadm up user-{user_id}
5. Return 200
```

#### POST /images/{user_id}/drbd/promote

Promotes DRBD resource to primary.

Implementation:
```
1. Check current role via drbdadm status → if already Primary, return 200
2. drbdadm primary user-{user_id}
3. Return 200
```

#### POST /images/{user_id}/drbd/demote

Demotes DRBD resource to secondary. Must unmount first if mounted.

Implementation:
```
1. Check current role → if already Secondary, return 200
2. If /mnt/users/{user_id} is mounted → umount first
3. drbdadm secondary user-{user_id}
4. Return 200
```

#### GET /images/{user_id}/drbd/status

Returns DRBD resource status.

Response:
```json
{
    "resource": "user-alice",
    "role": "Primary",
    "connection_state": "Connected",
    "disk_state": "UpToDate",
    "peer_disk_state": "UpToDate",
    "sync_progress": null,
    "exists": true
}
```

If the resource doesn't exist, return `{"exists": false}`.

Implementation: Parse output of `drbdadm status user-{user_id}`. DRBD 9 status output format:

```
user-alice role:Primary
  disk:UpToDate
  fleet-2 role:Secondary
    peer-disk:UpToDate
```

Or during sync:
```
user-alice role:Primary
  disk:UpToDate
  fleet-2 role:Secondary
    replication:SyncSource peer-disk:Inconsistent done:45.20
```

Or when disconnected:
```
user-alice role:Primary
  disk:UpToDate
```

The parser must handle all these variants robustly.

#### DELETE /images/{user_id}/drbd

Tears down DRBD resource.

Implementation:
```
1. drbdadm down user-{user_id} (ignore error if already down)
2. rm /etc/drbd.d/user-{user_id}.res
3. Return 200
```

#### POST /images/{user_id}/mount

Mounts Btrfs from DRBD device.

Implementation:
```
1. Check if /mnt/users/{user_id} is already mounted → return 200
2. mkdir -p /mnt/users/{user_id}
3. Determine DRBD device: /dev/drbd{minor} (from in-memory state or from drbd status)
4. mount /dev/drbd{minor} /mnt/users/{user_id}
5. Verify workspace subvolume exists: test -d /mnt/users/{user_id}/workspace
6. Return 200
```

#### POST /images/{user_id}/unmount

Unmounts Btrfs.

Implementation:
```
1. Check if mounted → if not, return 200
2. umount /mnt/users/{user_id}
3. Return 200
```

#### POST /images/{user_id}/format-btrfs

Formats the DRBD device with Btrfs and creates the workspace subvolume structure. Only called on the primary, after DRBD promotion. This is the DRBD-first pattern: DRBD metadata is written on the empty device first, then the filesystem is created on top of `/dev/drbdN`.

Implementation:
```
1. Determine DRBD device: /dev/drbd{minor} (from in-memory state)
2. Check if already formatted (try mount, check for workspace subvolume)
   → If workspace exists: unmount, return 200 with {"already_formatted": true}
3. mkfs.btrfs -f /dev/drbd{minor}
4. mkdir -p /mnt/users/{user_id}
5. mount /dev/drbd{minor} /mnt/users/{user_id}
6. btrfs subvolume create /mnt/users/{user_id}/workspace
7. mkdir -p /mnt/users/{user_id}/workspace/{memory,apps,data}
8. mkdir -p /mnt/users/{user_id}/snapshots
9. echo '{"created":"$(date -Iseconds)"}' > /mnt/users/{user_id}/workspace/data/config.json
10. btrfs subvolume snapshot -r /mnt/users/{user_id}/workspace /mnt/users/{user_id}/snapshots/layer-000
11. umount /mnt/users/{user_id}
12. Return 200
```

Note: unmounts after formatting. The separate `mount` endpoint is used when the provisioning sequence is ready to mount for container startup.

#### POST /containers/{user_id}/start

Starts a dummy agent container.

Implementation:
```
1. Check if container {user_id}-agent already running → return 200
2. docker run -d --name {user_id}-agent \
     -v /mnt/users/{user_id}/workspace:/workspace:rw \
     --network none \
     --memory 64m \
     alpine:latest \
     sh -c "echo 'Agent started for {user_id}' > /workspace/data/agent.log; while true; do sleep 60; done"
3. Verify container is running: docker inspect {user_id}-agent
4. Return 200 with {"container_id": "..."}
```

Note: uses a simple alpine container with a sleep loop. This is a dummy — we're proving orchestration, not agent functionality. `--network none` for isolation.

#### POST /containers/{user_id}/stop

Stops and removes container.

Implementation:
```
1. docker stop {user_id}-agent (timeout 10s)
2. docker rm {user_id}-agent
3. If container doesn't exist, return 200 (idempotent)
4. Return 200
```

#### GET /containers/{user_id}/status

Returns container status.

Response:
```json
{
    "exists": true,
    "running": true,
    "container_name": "alice-agent",
    "started_at": "2026-02-28T12:00:00Z"
}
```

#### POST /cleanup

Tears down ALL user resources on this machine. Used by the test harness between tests.

Implementation:
```
1. docker stop $(docker ps -q) 2>/dev/null; docker rm $(docker ps -aq) 2>/dev/null
2. umount /mnt/users/* 2>/dev/null
3. for each DRBD resource: drbdadm down {resource}
4. rm /etc/drbd.d/user-*.res 2>/dev/null
5. for each loop device attached to /data/images: losetup -d
6. rm /data/images/*.img 2>/dev/null
7. Clear in-memory state
8. Return 200
```

### Heartbeat Sender

Background goroutine, every 5 seconds:

```go
func (a *Agent) heartbeatLoop() {
    ticker := time.NewTicker(5 * time.Second)
    for range ticker.C {
        status := a.collectStatus() // same data as GET /status
        resp, err := http.Post(a.coordinatorURL+"/api/heartbeat", "application/json", marshal(status))
        if err != nil {
            log.Printf("Heartbeat failed: %v", err)
            // Continue — coordinator might be restarting
        }
    }
}
```

### Error Handling

Every `exec.Command()` call must:
1. Capture stdout and stderr
2. Check the exit code
3. Return a structured error to the HTTP handler
4. The HTTP handler returns appropriate status codes:
   - 200: success (including idempotent "already done")
   - 400: bad request (missing parameters)
   - 500: operation failed (with error details in response body)

Response format for errors:
```json
{
    "error": "drbdadm create-md failed",
    "details": "stderr: Device size too small",
    "exit_code": 1
}
```

### Per-User Locking

The machine agent holds a `sync.Mutex` per user_id. Every endpoint that operates on a user's resources acquires this lock first. This prevents races when the coordinator sends rapid sequential requests for the same user.

```go
type Agent struct {
    locks sync.Map // map[string]*sync.Mutex — user_id → lock
}

func (a *Agent) getUserLock(userID string) *sync.Mutex {
    val, _ := a.locks.LoadOrStore(userID, &sync.Mutex{})
    return val.(*sync.Mutex)
}
```

---

## Component 2: Coordinator

The coordinator is a Go HTTP server that manages the fleet. It's the brain — it makes all placement, failover, and recovery decisions. Machine agents are the hands — they execute.

### Startup Sequence

```
1. Read configuration from environment variables:
     LISTEN_ADDR (default: "0.0.0.0:8080")
     DATABASE_URL (required — Supabase Postgres connection string)
     FAIL_AT (optional — deterministic fault injection checkpoint name)
     CHAOS_MODE (optional — "true" to enable random crashes)
     CHAOS_PROBABILITY (optional — e.g. "0.05" for 5% crash chance per checkpoint)
2. Connect to Postgres (DATABASE_URL)
3. Acquire Postgres advisory lock (prevents two coordinators running):
     SELECT pg_try_advisory_lock(1)
   If fails: log "Another coordinator is running", exit
4. Run reconciliation (see Reconciliation section below)
5. Start background goroutines:
   - Heartbeat monitor (continuous)
   - Bipod health checker (every 15 seconds)
6. Start HTTP server
7. Log: "Coordinator ready, reconciliation complete, serving on {addr}"
```

### Fault Injection

The coordinator has a `checkFault(name string)` function called at each checkpoint in every multi-step operation:

```go
func (c *Coordinator) checkFault(name string) {
    // Deterministic mode
    if c.failAt == name {
        log.Printf("FAULT INJECTION: crashing at checkpoint '%s'", name)
        os.Exit(1)  // Immediate, no cleanup
    }
    // Chaos mode
    if c.chaosMode && rand.Float64() < c.chaosProbability {
        log.Printf("CHAOS: random crash at checkpoint '%s'", name)
        os.Exit(1)
    }
}
```

This is called at every step transition in provisioning, failover, and reformation. The checkpoints are listed in each operation's section below.

### API Endpoints

#### External API (called by test harness)

```
POST   /api/users                    → Create user, trigger provisioning
GET    /api/users/{id}               → Get user status + bipod info
POST   /api/users/{id}/suspend       → Suspend user
POST   /api/users/{id}/reactivate    → Reactivate suspended user
DELETE /api/users/{id}               → Delete user, tear down everything
GET    /api/fleet                    → Fleet overview (machines, load, status)
GET    /api/fleet/{machine_id}       → Single machine details
GET    /api/users                    → List all users with status
GET    /api/operations               → List active operations
GET    /api/events                   → Recent events
```

#### Internal API (called by machine agents)

```
POST   /api/heartbeat                → Machine agent heartbeat
```

### POST /api/users — Create User

Request body:
```json
{
    "user_id": "alice"
}
```

Implementation:
```
1. Validate user_id (alphanumeric + hyphens, 3-32 chars)
2. Check if user already exists → 409 Conflict
3. Allocate DRBD port: SELECT COALESCE(MAX(drbd_port), 7899) + 1 FROM users
4. Select two machines via placement algorithm (see Placement section)
5. Allocate DRBD minors: for each machine, next unused minor in that machine's range
6. Begin transaction:
   - INSERT into users (user_id, status='provisioning', drbd_port)
   - INSERT into bipods (user_id, machine_A, role='primary', minor, state='pending')
   - INSERT into bipods (user_id, machine_B, role='secondary', minor, state='pending')
   - INSERT into operations (type='provision', user_id, status='in_progress',
     current_step='user_created', metadata={machine_a, machine_b, minors, port})
   - INSERT event
   Commit
7. Start provisioning goroutine (async — don't block the API response)
8. Return 201 with user info + assigned machines
```

### POST /api/heartbeat — Machine Heartbeat

Request body: same as machine agent's GET /status response.

Implementation:
```
1. Update machines SET last_heartbeat=NOW(), disk_used_mb=..., ram_used_mb=..., active_agents=...
2. Compare reported user states against bipods table:
   - For each user reported by the machine:
     - If bipod entry exists: compare states, log discrepancies (don't auto-fix during normal operation — only during reconciliation)
     - If no bipod entry: log orphan warning
   - For each bipod entry on this machine not reported by the machine:
     - Log missing resource warning
3. Return 200
```

### Heartbeat Monitor

Background goroutine, runs continuously:

```go
func (c *Coordinator) heartbeatMonitor() {
    ticker := time.NewTicker(5 * time.Second)
    for range ticker.C {
        machines := c.db.GetActiveMachines()
        for _, m := range machines {
            if time.Since(m.LastHeartbeat) > 15*time.Second {
                if m.Status == "active" {
                    log.Printf("Machine %s offline (no heartbeat for %v)", m.ID, time.Since(m.LastHeartbeat))
                    c.db.UpdateMachineStatus(m.ID, "offline")
                    c.db.InsertEvent("machine_offline", m.ID, "", nil)
                    go c.handleMachineFailure(m.ID)
                }
            }
        }
    }
}
```

### Bipod Health Checker

Background goroutine, every 15 seconds:

```go
func (c *Coordinator) bipodHealthCheck() {
    ticker := time.NewTicker(15 * time.Second)
    for range ticker.C {
        users := c.db.GetRunningUsers()
        for _, u := range users {
            bipods := c.db.GetBipodMembers(u.ID)
            healthyCount := 0
            for _, b := range bipods {
                machine := c.db.GetMachine(b.MachineID)
                if machine.Status == "active" && b.State == "ready" {
                    healthyCount++
                }
            }
            if healthyCount < 2 {
                // Check if a reform operation is already in progress
                if !c.db.HasActiveOperation(u.ID, "reform_bipod") {
                    log.Printf("User %s has only %d healthy bipod members, queueing reformation", u.ID, healthyCount)
                    c.queueBipodReformation(u.ID)
                }
            }
        }
    }
}
```

---

## Provisioning State Machine

The provisioning sequence is the coordinator's most complex operation. It has 11 steps with a database checkpoint after each. The coordinator can crash and restart at any point and resume correctly.

### Step Sequence

```
Step 1: USER_CREATED
  DB state: user exists, bipod entries exist with state='pending', operation created
  Fault checkpoint: "provision-user-created"

Step 2: PRIMARY_IMAGE_CREATED
  Action: POST fleet-A /images/{user_id}/create
  Capture: loop_device from response
  DB update: bipods SET state='image_created' WHERE machine=A
             operations SET current_step='primary_image_created'
             operations metadata += primary_loop_device
  Fault checkpoint: "provision-primary-image-created"

Step 3: SECONDARY_IMAGE_CREATED
  Action: POST fleet-B /images/{user_id}/create
  Capture: loop_device from response
  DB update: bipods SET state='image_created' WHERE machine=B
             operations SET current_step='secondary_image_created'
             operations metadata += secondary_loop_device
  Fault checkpoint: "provision-secondary-image-created"

Step 4: PRIMARY_DRBD_CONFIGURED
  Action: POST fleet-A /images/{user_id}/drbd/create
    Body: full config with both nodes (using captured loop devices from steps 2-3)
  DB update: bipods SET state='drbd_configured' WHERE machine=A
             operations SET current_step='primary_drbd_configured'
  Fault checkpoint: "provision-primary-drbd-configured"

Step 5: SECONDARY_DRBD_CONFIGURED
  Action: POST fleet-B /images/{user_id}/drbd/create
    Body: same full config as step 4
  DB update: bipods SET state='drbd_configured' WHERE machine=B
             operations SET current_step='secondary_drbd_configured'
  Fault checkpoint: "provision-secondary-drbd-configured"

Step 6: DRBD_SYNCED
  Action: Poll GET fleet-A /images/{user_id}/drbd/status every 2 seconds
    Until: peer_disk_state == "UpToDate" (or timeout after 60 seconds)
  DB update: bipods SET state='drbd_synced' on both machines
             operations SET current_step='drbd_synced'
  Fault checkpoint: "provision-drbd-synced"

Step 7: PRIMARY_PROMOTED
  Action: POST fleet-A /images/{user_id}/drbd/promote
  DB update: bipods SET state='promoted' WHERE machine=A
             operations SET current_step='primary_promoted'
  Fault checkpoint: "provision-primary-promoted"

Step 8: BTRFS_FORMATTED
  Action: POST fleet-A /images/{user_id}/format-btrfs
  DB update: bipods SET state='formatted' WHERE machine=A
             operations SET current_step='btrfs_formatted'
  Fault checkpoint: "provision-btrfs-formatted"
  Note: This creates workspace subvolume, seed dirs, layer-000 snapshot, then unmounts.

Step 9: BTRFS_MOUNTED
  Action: POST fleet-A /images/{user_id}/mount
  DB update: bipods SET state='mounted' WHERE machine=A
             operations SET current_step='btrfs_mounted'
  Fault checkpoint: "provision-btrfs-mounted"

Step 10: CONTAINERS_STARTED
  Action: POST fleet-A /containers/{user_id}/start
  DB update: bipods SET state='containers_running' WHERE machine=A
             operations SET current_step='containers_started'
  Fault checkpoint: "provision-containers-started"

Step 11: FINALIZED
  DB update (single transaction):
    users SET status='running', primary_machine=A
    bipods SET state='ready' on both
    operations SET status='complete', completed_at=NOW()
    INSERT event: 'user_provisioned'
  Fault checkpoint: "provision-finalized" (after this, operation is complete)
```

### Error Handling During Provisioning

If any step's HTTP call to a machine agent fails:
1. If the machine is offline (heartbeat timeout): the operation pauses. When the machine comes back or during reconciliation, the operation adapts or retries.
2. If the machine agent returns 500: retry up to 3 times with 2-second backoff. If still failing, mark the operation as failed, log the error.
3. If timeout: same as 500 — retry then fail.

When an operation is marked failed, the user status is set to 'failed'. The test harness or a future admin can investigate and retry or clean up.

---

## Failover Sequence

Triggered by `handleMachineFailure(machineID)` when a machine goes offline.

```
Step 1: IDENTIFY AFFECTED USERS
  Query: SELECT * FROM bipods WHERE machine_id = {dead_machine} AND state NOT IN ('torn_down', 'failed')
  Separate into:
    - primary_users: users where this machine was primary (need failover)
    - secondary_users: users where this machine was secondary (need reformation only)

Step 2: FAILOVER EACH PRIMARY USER (sequentially)
  For each user where the dead machine was primary:

  2a. Create operation: type='failover', status='in_progress'
      Find surviving bipod member machine
      Fault checkpoint: "failover-detected"

  2b. Promote surviving machine:
      POST surviving /images/{user_id}/drbd/promote
      DB: bipods SET role='primary', state='promoted' WHERE machine=surviving
      operations SET current_step='promoted'
      Fault checkpoint: "failover-promoted"

  2c. Mount Btrfs:
      POST surviving /images/{user_id}/mount
      DB: bipods SET state='mounted' WHERE machine=surviving
      operations SET current_step='mounted'
      Fault checkpoint: "failover-mounted"

  2d. Start containers:
      POST surviving /containers/{user_id}/start
      DB: bipods SET state='containers_running' WHERE machine=surviving
      operations SET current_step='containers_started'
      Fault checkpoint: "failover-containers-started"

  2e. Finalize:
      DB: users SET primary_machine=surviving
          bipods SET state='ready' WHERE machine=surviving
          bipods SET state='failed' WHERE machine=dead
          operations SET status='complete'
          INSERT event: 'failover_complete'

Step 3: MARK SECONDARY LOSSES
  For each user where the dead machine was secondary:
    DB: bipods SET state='failed' WHERE machine=dead
    INSERT event: 'bipod_member_lost'
    (Bipod health checker will queue reformation)
```

---

## Bipod Reformation Sequence

Triggered by the bipod health checker when a user has fewer than 2 healthy bipod members.

```
Step 1: PICK NEW MACHINE
  Exclude: current primary machine, any machine with status='offline'
  Use placement algorithm to pick least-loaded remaining machine
  Create operation: type='reform_bipod', metadata={new_machine}
  Fault checkpoint: "reform-machine-picked"

Step 2: CREATE IMAGE ON NEW MACHINE
  POST new-machine /images/{user_id}/create {image_size_mb: 512}
  DB: INSERT bipods (user_id, new_machine, role='secondary', state='image_created')
  operations SET current_step='image_created'
  Fault checkpoint: "reform-image-created"

Step 3: CONFIGURE DRBD ON NEW MACHINE
  This is the most complex step. The primary's DRBD config references the OLD (dead)
  secondary. We must reconfigure DRBD to point at the new secondary. For the PoC, this
  requires briefly stopping the agent (production should use drbdadm adjust or DRBD 9
  multi-peer features).

  Sequence (10 sub-steps, brief disruption to running agent):
    a. Query primary machine agent: GET /status → get primary's loop device path
    b. Stop containers on primary: POST primary /containers/{user_id}/stop
    c. Unmount Btrfs on primary: POST primary /images/{user_id}/unmount
    d. DRBD down on primary: drbdadm down (via a new endpoint or rolled into drbd/create)
    e. Write new DRBD config on primary (with new secondary as peer)
    f. Write DRBD config on new secondary (same config)
    g. drbdadm create-md --force on new secondary only
    h. drbdadm up on both machines
    i. drbdadm primary on primary (re-promote)
    j. DRBD initial sync begins (primary → new secondary)

  For the coordinator, this means:
    POST primary /images/{user_id}/drbd (DELETE existing DRBD resource)
    POST new-secondary /images/{user_id}/drbd/create {full config with both nodes}
    POST primary /images/{user_id}/drbd/create {same full config}
    POST primary /images/{user_id}/drbd/promote

  Then wait for sync, mount, restart containers.

  DB: INSERT bipods (user_id, new_machine, role='secondary', state='drbd_configured')
  operations SET current_step='drbd_configured'
  Fault checkpoint: "reform-drbd-configured"

Step 4: WAIT FOR SYNC
  Poll DRBD status until UpToDate/UpToDate
  DB: bipods SET state='drbd_synced' for new secondary
  operations SET current_step='synced'
  Fault checkpoint: "reform-synced"

Step 5: REMOUNT AND RESTART
  Step 3 stopped containers and unmounted Btrfs for DRBD reconfiguration.
  Now restore the agent to running state:
    POST primary /images/{user_id}/mount
    POST primary /containers/{user_id}/start
  DB: bipods SET state='ready' for primary (back to running)
  operations SET current_step='restarted'

Step 6: FINALIZE
  DB: bipods SET state='ready' for new secondary
      DELETE bipods WHERE state='failed' for this user (old dead member)
      operations SET status='complete'
      INSERT event: 'bipod_reformed'
```

---

## Suspension and Reactivation

### Suspend (POST /api/users/{id}/suspend)

```
1. Verify user status is 'running'
2. Create operation: type='suspend'
3. Stop containers: POST primary /containers/{user_id}/stop
4. Unmount: POST primary /images/{user_id}/unmount
5. DB: users SET status='suspended', suspended_at=NOW()
       bipods SET state='suspended' for both members
       operations SET status='complete'
       INSERT event: 'user_suspended'
```

DRBD stays connected. Images stay on both machines. Quick reactivation.

### Reactivate (POST /api/users/{id}/reactivate)

```
1. Verify user status is 'suspended'
2. Create operation: type='reactivate'
3. Check DRBD status on primary — should still be connected
   If disconnected (machine was recycled): more complex recovery needed, skip for PoC
4. Mount: POST primary /images/{user_id}/mount
5. Start containers: POST primary /containers/{user_id}/start
6. DB: users SET status='running', suspended_at=NULL
       bipods SET state='ready' for both
       operations SET status='complete'
       INSERT event: 'user_reactivated'
```

---

## Placement Algorithm

```go
func (c *Coordinator) selectBipodMachines(excludeUserID string) (primary, secondary string, err error) {
    machines := c.db.GetActiveMachines()
    if len(machines) < 2 {
        return "", "", errors.New("not enough active machines for bipod placement")
    }

    // Count bipod members per machine
    type machineLoad struct {
        ID    string
        Count int
    }
    var loads []machineLoad
    for _, m := range machines {
        count := c.db.CountBipodMembers(m.ID)  // count where state != 'torn_down' and state != 'failed'
        loads = append(loads, machineLoad{m.ID, count})
    }

    // Sort by count ascending, then by machine_id for determinism
    sort.Slice(loads, func(i, j int) bool {
        if loads[i].Count == loads[j].Count {
            return loads[i].ID < loads[j].ID
        }
        return loads[i].Count < loads[j].Count
    })

    // Pick first two (least loaded)
    return loads[0].ID, loads[1].ID, nil
}
```

### DRBD Minor Allocation

Each machine has a minor number range:

| Machine | Minor range |
|---------|------------|
| fleet-1 | 0-99 |
| fleet-2 | 100-199 |
| fleet-3 | 200-299 |

The coordinator allocates the next unused minor in the machine's range:

```go
func (c *Coordinator) allocateMinor(machineID string) (int, error) {
    minorStart, minorEnd := c.getMinorRange(machineID)
    usedMinors := c.db.GetUsedMinors(machineID)  // SELECT drbd_minor FROM bipods WHERE machine_id=...
    for m := minorStart; m <= minorEnd; m++ {
        if !contains(usedMinors, m) {
            return m, nil
        }
    }
    return 0, errors.New("no free DRBD minors on " + machineID)
}
```

### DRBD Port Allocation

Ports are globally unique, starting from 7900:

```go
func (c *Coordinator) allocatePort() (int, error) {
    var maxPort int
    err := c.db.QueryRow("SELECT COALESCE(MAX(drbd_port), 7899) FROM users").Scan(&maxPort)
    return maxPort + 1, err
}
```

---

## Reconciliation

The most critical piece. Runs on coordinator startup BEFORE accepting any API requests or starting background loops.

### Phase 1: Discover Reality

Probe ALL machines in the DB, regardless of their recorded status. This is critical — a machine marked 'offline' by a previous coordinator run may have rebooted and be fully healthy. Only by probing can we know the actual current state.

```
For each machine in DB (ALL statuses, including 'offline'):
  Try: GET machine /status (timeout: 5 seconds)
  If reachable:
    Store full status response in memory
    Update last_heartbeat
    Set status to 'active' (even if it was 'offline' — it's back)
  If unreachable:
    Mark as 'offline' in DB
    Record: this machine is down
```

### Phase 2: Reconcile Database with Machine Reality

```
For each user in DB:
  For each bipod member in DB:
    machine_status = reality[bipod.machine_id]
    
    If machine is offline:
      If bipod.state not in ('failed', 'torn_down'):
        Mark bipod as 'failed' (will handle in Phase 4)
      Continue
    
    machine_user_info = machine_status.users[user_id]
    
    If machine_user_info is nil (machine doesn't know about this user):
      If bipod.state in ('pending'):
        Fine — nothing was created yet
      Else:
        DB says resources exist, machine says they don't
        Reset bipod.state to 'pending' (resources were lost)
    
    Else (machine has resources for this user):
      Reconcile each layer:
        image_exists → at least 'image_created'
        drbd exists and connected → at least 'drbd_synced'
        drbd exists, primary → at least 'promoted'
        mounted → at least 'mounted'
        container running → at least 'containers_running'
      
      Update bipod.state to match reality if DB is behind
      
  Check user-level consistency:
    If user.status == 'running' but no container is running on any machine:
      User needs repair
    If user.status == 'provisioning' but containers are running:
      User is actually running — update status

For each machine that is online:
  For each user the machine reports that DB doesn't know about:
    This is an orphan — queue cleanup (see Phase 3b)
```

### Phase 3: Resume Interrupted Operations

```
operations = SELECT * FROM operations WHERE status IN ('in_progress', 'pending') ORDER BY started_at

For each operation:
  Acquire user lock
  
  Switch on operation.type:
    
    'provision':
      Check if required machines are online
      If not: try to adapt (swap offline machine for a new one)
               or mark failed and re-queue with fresh machines
      Read current_step from operation
      Determine next step based on current_step and actual bipod states
      Resume provisioning from that step
      (All steps are idempotent, so re-executing current step is safe)
    
    'failover':
      Check if the surviving machine is online
      Read current_step
      Resume failover from next step
    
    'reform_bipod':
      Check if target machine is online
      Read current_step
      Resume reformation from next step
    
    'suspend':
      Read current_step
      Resume suspension from next step
    
    'reactivate':
      Read current_step
      Resume reactivation from next step

Special case: user in 'provisioning' with NO active operation
  → Create a new provision operation and start from beginning
  → This handles the case where coordinator crashed between creating the user
    and creating the operation
```

### Phase 3b: Clean Up Orphans

```
For each orphaned user resource discovered in Phase 2:
  POST machine /images/{user_id} DELETE (full teardown)
  Log: "Cleaned up orphaned resources for {user_id} on {machine_id}"
```

### Phase 4: Handle Offline Machines

```
For machines discovered offline in Phase 1:
  Run standard handleMachineFailure(machineID) logic
  This triggers failovers for primary users on that machine
  And queues reformations for all affected users
```

### Phase 5: Start Normal Operation

```
Start heartbeat monitor goroutine
Start bipod health checker goroutine
Start HTTP server (begin accepting API requests)
Log: "Reconciliation complete. Processed {N} operations, {M} orphans, {K} offline machines."
```

---

## Machine Failure Handler

```go
func (c *Coordinator) handleMachineFailure(machineID string) {
    c.mu.Lock()  // global lock during failure handling to prevent races
    defer c.mu.Unlock()

    // Get all bipod members on this machine
    bipods := c.db.GetBipodsByMachine(machineID)

    // Separate by role
    var primaryUsers, secondaryUsers []BipodMember
    for _, b := range bipods {
        if b.State == "failed" || b.State == "torn_down" {
            continue
        }
        if b.Role == "primary" {
            primaryUsers = append(primaryUsers, b)
        } else {
            secondaryUsers = append(secondaryUsers, b)
        }
    }

    // Failover primary users first (restore service)
    for _, b := range primaryUsers {
        surviving := c.db.GetSurvivingBipodMember(b.UserID, machineID)
        if surviving == nil {
            // Both members down — cannot failover
            c.db.UpdateUserStatus(b.UserID, "failed")
            c.db.InsertEvent("double_failure", machineID, b.UserID, nil)
            continue
        }
        c.executeFailover(b.UserID, surviving.MachineID)
    }

    // Mark all bipod members on dead machine as failed
    for _, b := range append(primaryUsers, secondaryUsers...) {
        c.db.UpdateBipodState(b.UserID, machineID, "failed")
    }

    // Bipod health checker will handle reformation
}
```

---

## Dead Machine Recovery

When a previously offline machine starts heartbeating again:

```
In heartbeat monitor:
  If machine.Status == 'offline' AND heartbeat received:
    Log: "Machine {id} back online"
    Update machine status to 'active'
    
    // Check for orphaned resources
    machine_status = GET machine /status
    for each user the machine reports:
      bipod = DB lookup (user_id, machine_id)
      if bipod is nil OR bipod.state == 'failed' OR bipod.state == 'torn_down':
        // This machine has stale data — bipod was reformed without it
        Queue cleanup: POST machine /images/{user_id} DELETE
        Log: "Cleaning orphaned resources for {user_id} on returning machine {id}"
```

---

## Test Harness

### scripts/run.sh — Main Entry Point

```bash
#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

source "$SCRIPT_DIR/common.sh"  # shared functions

echo "=== LAYER 4: COORDINATOR & MACHINE AGENT PoC ==="
echo "Started: $(date)"

# ── Phase 0: Infrastructure ──
echo ""
echo "Phase 0: Infrastructure Setup"

# Create machines
"$SCRIPT_DIR/infra.sh" up

# Wait for cloud-init to complete on all machines
wait_for_cloud_init

# Get IPs
load_ips  # sets COORD_IP, COORD_PRIV, FLEET1_IP, FLEET1_PRIV, etc.

# Build and deploy
cd "$PROJECT_DIR"
make build
"$SCRIPT_DIR/deploy.sh"

# Set up database
"$SCRIPT_DIR/seed_db.sh"

# Start machine agents
for ip in $FLEET1_IP $FLEET2_IP $FLEET3_IP; do
    ssh root@$ip "systemctl start machine-agent"
done

# Start coordinator
ssh root@$COORD_IP "systemctl start coordinator"

# Wait for heartbeats
sleep 10
verify_all_heartbeating

phase_checks 0 8
echo ""

# ── Run test suite ──
"$SCRIPT_DIR/test_suite.sh"

# ── Teardown ──
echo ""
echo "Tearing down infrastructure..."
"$SCRIPT_DIR/infra.sh" down

echo ""
echo "=== PoC COMPLETE ==="
echo "Finished: $(date)"
```

### scripts/infra.sh — Hetzner Lifecycle

Same pattern as PoCs 2/3. Creates 4 CX23 instances in a private network.

```bash
#!/bin/bash
set -euo pipefail

NETWORK_NAME="poc-coordinator-net"
NETWORK_SUBNET="10.0.0.0/24"
SSH_KEY_NAME="poc-coordinator"
LOCATION="nbg1"
SERVER_TYPE="cx23"
IMAGE="ubuntu-24.04"

MACHINES=(
    "poc4-coordinator:10.0.0.1:coordinator"
    "poc4-fleet-1:10.0.0.2:fleet"
    "poc4-fleet-2:10.0.0.3:fleet"
    "poc4-fleet-3:10.0.0.4:fleet"
)

case "${1:-}" in
    up)
        # Create SSH key if needed
        if ! hcloud ssh-key describe "$SSH_KEY_NAME" &>/dev/null; then
            hcloud ssh-key create --name "$SSH_KEY_NAME" --public-key-from-file ~/.ssh/id_ed25519.pub
        fi

        # Create network
        hcloud network create --name "$NETWORK_NAME" --ip-range "$NETWORK_SUBNET"
        hcloud network add-subnet "$NETWORK_NAME" --ip-range "$NETWORK_SUBNET" --type cloud --network-zone eu-central

        # Create machines
        for entry in "${MACHINES[@]}"; do
            IFS=: read -r name priv_ip role <<< "$entry"
            cloud_init="scripts/cloud-init/${role}.yaml"
            
            hcloud server create \
                --name "$name" \
                --type "$SERVER_TYPE" \
                --image "$IMAGE" \
                --location "$LOCATION" \
                --ssh-key "$SSH_KEY_NAME" \
                --network "$NETWORK_NAME" \
                --user-data-from-file "$cloud_init"

            # Assign specific private IP
            # NOTE: Hetzner assigns IPs automatically in the subnet.
            # To get specific IPs, we may need to use the API directly or
            # accept auto-assigned IPs and discover them.
            # For the PoC: discover assigned IPs after creation and use those.
        done

        echo "Waiting for servers to be ready..."
        sleep 30

        # Discover and save IPs
        save_ips
        ;;

    down)
        for entry in "${MACHINES[@]}"; do
            IFS=: read -r name _ _ <<< "$entry"
            hcloud server delete "$name" 2>/dev/null || true
        done
        hcloud network delete "$NETWORK_NAME" 2>/dev/null || true
        hcloud ssh-key delete "$SSH_KEY_NAME" 2>/dev/null || true
        ;;

    status)
        for entry in "${MACHINES[@]}"; do
            IFS=: read -r name _ _ <<< "$entry"
            hcloud server describe "$name" -o format='{{.Name}}: {{.Status}} ({{.PublicNet.IPv4.IP}})' 2>/dev/null || echo "$name: not found"
        done
        ;;
esac
```

### scripts/cloud-init/fleet.yaml

```yaml
#cloud-config
package_update: true
packages:
  - btrfs-progs
  - docker.io
  - xfsprogs
  - curl
  - jq
  - zstd
  - kmod
  - software-properties-common

runcmd:
  # DRBD 9 (same as PoCs 2/3)
  - add-apt-repository -y ppa:linbit/linbit-drbd9-stack
  - apt install -y drbd-dkms linux-headers-$(uname -r)
  - |
    echo "postfix postfix/main_mailer_type select No configuration" | debconf-set-selections
    echo "postfix postfix/mailname string localhost" | debconf-set-selections
  - DEBIAN_FRONTEND=noninteractive apt install -y drbd-utils
  - dkms autoinstall
  - modprobe drbd

  # Docker
  - systemctl enable docker
  - systemctl start docker

  # Storage setup (no template — DRBD-first: filesystem created after DRBD setup)
  - mkdir -p /data/images /mnt/users

  # Machine agent systemd service (binary deployed separately)
  - |
    cat > /etc/systemd/system/machine-agent.service << 'EOF'
    [Unit]
    Description=Platform Machine Agent
    After=network.target docker.service
    Requires=docker.service
    [Service]
    Type=simple
    ExecStart=/usr/local/bin/machine-agent
    Environment=NODE_ID=PLACEHOLDER
    Environment=NODE_ADDRESS=PLACEHOLDER
    Environment=COORDINATOR_URL=PLACEHOLDER
    Environment=DATA_DIR=/data
    Environment=MAX_AGENTS=10
    Environment=DRBD_MINOR_START=PLACEHOLDER
    Restart=on-failure
    RestartSec=5
    [Install]
    WantedBy=multi-user.target
    EOF
  - systemctl daemon-reload

write_files:
  - path: /etc/sysctl.d/99-drbd.conf
    content: |
      net.ipv4.tcp_keepalive_time = 60
      net.ipv4.tcp_keepalive_intvl = 10
      net.ipv4.tcp_keepalive_probes = 5
```

NOTE: The `deploy.sh` script will SSH in and replace the PLACEHOLDER values in the systemd unit file with actual values based on the machine's role and IP assignments.

### scripts/cloud-init/coordinator.yaml

```yaml
#cloud-config
package_update: true
packages:
  - curl
  - jq
  - postgresql-client

runcmd:
  - |
    cat > /etc/systemd/system/coordinator.service << 'EOF'
    [Unit]
    Description=Platform Coordinator
    After=network.target
    [Service]
    Type=simple
    ExecStart=/usr/local/bin/coordinator
    Environment=DATABASE_URL=PLACEHOLDER
    Environment=LISTEN_ADDR=0.0.0.0:8080
    Restart=on-failure
    RestartSec=5
    [Install]
    WantedBy=multi-user.target
    EOF
  - systemctl daemon-reload
```

### scripts/deploy.sh

```bash
#!/bin/bash
set -euo pipefail

source "$(dirname "$0")/common.sh"
load_ips

echo "Deploying coordinator binary..."
scp -o StrictHostKeyChecking=no bin/coordinator root@$COORD_IP:/usr/local/bin/

# Configure coordinator systemd with actual DATABASE_URL
ssh root@$COORD_IP "sed -i 's|PLACEHOLDER|$DATABASE_URL|' /etc/systemd/system/coordinator.service"
ssh root@$COORD_IP "systemctl daemon-reload"

echo "Deploying machine agent binaries..."
FLEET_CONFIGS=(
    "$FLEET1_IP:fleet-1:$FLEET1_PRIV:0"
    "$FLEET2_IP:fleet-2:$FLEET2_PRIV:100"
    "$FLEET3_IP:fleet-3:$FLEET3_PRIV:200"
)

for config in "${FLEET_CONFIGS[@]}"; do
    IFS=: read -r pub_ip node_id priv_ip minor_start <<< "$config"
    
    scp -o StrictHostKeyChecking=no bin/machine-agent root@$pub_ip:/usr/local/bin/
    
    # Configure systemd with actual values
    ssh root@$pub_ip "
        sed -i 's/NODE_ID=PLACEHOLDER/NODE_ID=$node_id/' /etc/systemd/system/machine-agent.service
        sed -i 's/NODE_ADDRESS=PLACEHOLDER/NODE_ADDRESS=$priv_ip:8080/' /etc/systemd/system/machine-agent.service
        sed -i 's|COORDINATOR_URL=PLACEHOLDER|COORDINATOR_URL=http://${COORD_PRIV}:8080|' /etc/systemd/system/machine-agent.service
        sed -i 's/DRBD_MINOR_START=PLACEHOLDER/DRBD_MINOR_START=$minor_start/' /etc/systemd/system/machine-agent.service
        hostnamectl set-hostname $node_id
        systemctl daemon-reload
    "
done

echo "Deploy complete."
```

### scripts/common.sh — Shared Functions

```bash
#!/bin/bash
# common.sh — shared functions sourced by all scripts

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
IP_FILE="$SCRIPT_DIR/.ips"

# DATABASE_URL must be set in environment (Supabase connection string)
: "${DATABASE_URL:?DATABASE_URL environment variable must be set}"

save_ips() {
    # Discover and save all machine IPs after Hetzner creation
    echo "Discovering machine IPs..."
    cat > "$IP_FILE" << EOF
COORD_IP=$(hcloud server ip poc4-coordinator)
COORD_PRIV=$(hcloud server describe poc4-coordinator -o json | jq -r '.private_net[0].ip')
FLEET1_IP=$(hcloud server ip poc4-fleet-1)
FLEET1_PRIV=$(hcloud server describe poc4-fleet-1 -o json | jq -r '.private_net[0].ip')
FLEET2_IP=$(hcloud server ip poc4-fleet-2)
FLEET2_PRIV=$(hcloud server describe poc4-fleet-2 -o json | jq -r '.private_net[0].ip')
FLEET3_IP=$(hcloud server ip poc4-fleet-3)
FLEET3_PRIV=$(hcloud server describe poc4-fleet-3 -o json | jq -r '.private_net[0].ip')
EOF
    echo "IPs saved to $IP_FILE"
}

load_ips() {
    if [ ! -f "$IP_FILE" ]; then
        echo "ERROR: IP file not found. Run infra.sh up first."
        exit 1
    fi
    source "$IP_FILE"
    export COORD_IP COORD_PRIV FLEET1_IP FLEET1_PRIV FLEET2_IP FLEET2_PRIV FLEET3_IP FLEET3_PRIV
}

get_pub_ip() {
    # Given a machine_id (fleet-1, fleet-2, fleet-3), return its public IP
    local machine_id=$1
    case "$machine_id" in
        fleet-1) echo "$FLEET1_IP" ;;
        fleet-2) echo "$FLEET2_IP" ;;
        fleet-3) echo "$FLEET3_IP" ;;
        *) echo "ERROR: unknown machine $machine_id" >&2; return 1 ;;
    esac
}

get_priv_ip() {
    local machine_id=$1
    case "$machine_id" in
        fleet-1) echo "$FLEET1_PRIV" ;;
        fleet-2) echo "$FLEET2_PRIV" ;;
        fleet-3) echo "$FLEET3_PRIV" ;;
        *) echo "ERROR: unknown machine $machine_id" >&2; return 1 ;;
    esac
}

wait_for_cloud_init() {
    echo "Waiting for cloud-init to complete on all machines..."
    for ip in $COORD_IP $FLEET1_IP $FLEET2_IP $FLEET3_IP; do
        for attempt in $(seq 1 60); do
            if ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 root@$ip                 "cloud-init status --wait" 2>/dev/null | grep -q "done"; then
                echo "  $ip: cloud-init complete"
                break
            fi
            sleep 5
        done
    done
}

wait_for_ssh() {
    local ip=$1
    for attempt in $(seq 1 30); do
        if ssh -o StrictHostKeyChecking=no -o ConnectTimeout=3 root@$ip "true" 2>/dev/null; then
            return 0
        fi
        sleep 2
    done
    return 1
}

verify_all_heartbeating() {
    echo "Verifying all machine agents are heartbeating..."
    for machine in fleet-1 fleet-2 fleet-3; do
        last_hb=$(psql "$DATABASE_URL" -t -A -c             "SELECT EXTRACT(EPOCH FROM (NOW() - last_heartbeat)) FROM machines WHERE machine_id='$machine'")
        if [ -n "$last_hb" ] && [ "$(echo "$last_hb < 30" | bc)" -eq 1 ]; then
            echo "  $machine: heartbeating (${last_hb}s ago)"
        else
            echo "  ERROR: $machine not heartbeating (last: ${last_hb:-never})"
            exit 1
        fi
    done
}

phase_checks() {
    local phase=$1 expected=$2
    echo "  [Phase $phase infrastructure checks: $expected/$expected]"
}
```

### scripts/seed_db.sh

```bash
#!/bin/bash
set -euo pipefail

source "$(dirname "$0")/common.sh"
load_ips

echo "Setting up database schema..."
psql "$DATABASE_URL" -f "$PROJECT_DIR/schema.sql"

echo "Seeding machines table..."
psql "$DATABASE_URL" -c "
INSERT INTO machines (machine_id, address, status, max_agents) VALUES
    ('fleet-1', '${FLEET1_PRIV}:8080', 'active', 10),
    ('fleet-2', '${FLEET2_PRIV}:8080', 'active', 10),
    ('fleet-3', '${FLEET3_PRIV}:8080', 'active', 10)
ON CONFLICT (machine_id) DO UPDATE SET address = EXCLUDED.address;
"

echo "Database seeded."
```

### scripts/reset.sh

```bash
#!/bin/bash
set -euo pipefail

source "$(dirname "$0")/common.sh"
load_ips

echo "Resetting database..."
psql "$DATABASE_URL" -c "TRUNCATE events, operations, bipods, users CASCADE;"

echo "Cleaning fleet machines..."
for ip in $FLEET1_IP $FLEET2_IP $FLEET3_IP; do
    echo "  Cleaning $ip..."
    ssh root@$ip "curl -s -X POST http://localhost:8080/cleanup" || echo "  (agent not running, manual cleanup)"
    # Fallback: direct cleanup if agent isn't running
    ssh root@$ip "
        docker stop \$(docker ps -q) 2>/dev/null; docker rm \$(docker ps -aq) 2>/dev/null
        umount /mnt/users/* 2>/dev/null
        for res in \$(ls /etc/drbd.d/user-*.res 2>/dev/null | xargs -I{} basename {} .res); do
            drbdadm down \$res 2>/dev/null
        done
        rm -f /etc/drbd.d/user-*.res
        for dev in \$(losetup -a | grep /data/images/ | cut -d: -f1); do
            losetup -d \$dev 2>/dev/null
        done
        rm -f /data/images/*.img
        rm -rf /mnt/users/*
    " 2>/dev/null || true
done

echo "Reset complete."
```

### scripts/consistency_checker.sh

This is the ground truth oracle. It compares the coordinator's database view against SSH-verified machine state.

```bash
#!/bin/bash
# consistency_checker.sh — verifies system invariants
# Returns 0 if consistent, 1 if any violations found
set -euo pipefail

source "$(dirname "$0")/common.sh"
load_ips

VIOLATIONS=0

log_violation() {
    echo "  ✗ VIOLATION: $1"
    VIOLATIONS=$((VIOLATIONS + 1))
}

log_ok() {
    echo "  ✓ $1"
}

echo "Running consistency check..."

# ── Invariant 1: Every 'running' user has containers on exactly one machine ──
RUNNING_USERS=$(psql "$DATABASE_URL" -t -A -c "SELECT user_id FROM users WHERE status='running'")
for user in $RUNNING_USERS; do
    container_count=0
    container_machines=""
    for entry in "$FLEET1_IP:fleet-1" "$FLEET2_IP:fleet-2" "$FLEET3_IP:fleet-3"; do
        IFS=: read -r ip name <<< "$entry"
        running=$(ssh root@$ip "docker inspect ${user}-agent 2>/dev/null | jq -r '.[0].State.Running'" 2>/dev/null || echo "false")
        if [ "$running" = "true" ]; then
            container_count=$((container_count + 1))
            container_machines="$container_machines $name"
        fi
    done
    if [ "$container_count" -eq 1 ]; then
        log_ok "User $user: container running on$container_machines"
    elif [ "$container_count" -eq 0 ]; then
        log_violation "User $user: status=running but no container on any machine"
    else
        log_violation "User $user: container running on MULTIPLE machines:$container_machines"
    fi
done

# ── Invariant 2: Every 'running' user has exactly 2 bipod entries ──
for user in $RUNNING_USERS; do
    bipod_count=$(psql "$DATABASE_URL" -t -A -c "SELECT COUNT(*) FROM bipods WHERE user_id='$user' AND state NOT IN ('failed','torn_down')")
    if [ "$bipod_count" -eq 2 ]; then
        log_ok "User $user: 2 healthy bipod members"
    else
        log_violation "User $user: expected 2 bipod members, found $bipod_count"
    fi
done

# ── Invariant 3: DRBD roles match DB ──
for user in $RUNNING_USERS; do
    db_primary=$(psql "$DATABASE_URL" -t -A -c "SELECT machine_id FROM bipods WHERE user_id='$user' AND role='primary' AND state NOT IN ('failed','torn_down')")
    if [ -n "$db_primary" ]; then
        ip=$(get_pub_ip "$db_primary")
        actual_role=$(ssh root@$ip "drbdadm status user-$user 2>/dev/null | head -1 | grep -oP 'role:\K\w+'" 2>/dev/null || echo "unknown")
        if [ "$actual_role" = "Primary" ]; then
            log_ok "User $user: DRBD primary on $db_primary matches"
        else
            log_violation "User $user: DB says primary=$db_primary but actual DRBD role=$actual_role"
        fi
    fi
done

# ── Invariant 4: Primary has Btrfs mounted, secondary does not ──
for user in $RUNNING_USERS; do
    primary=$(psql "$DATABASE_URL" -t -A -c "SELECT machine_id FROM bipods WHERE user_id='$user' AND role='primary' AND state NOT IN ('failed','torn_down')")
    secondary=$(psql "$DATABASE_URL" -t -A -c "SELECT machine_id FROM bipods WHERE user_id='$user' AND role='secondary' AND state NOT IN ('failed','torn_down')")
    
    if [ -n "$primary" ]; then
        ip=$(get_pub_ip "$primary")
        mounted=$(ssh root@$ip "mountpoint -q /mnt/users/$user && echo yes || echo no" 2>/dev/null)
        if [ "$mounted" = "yes" ]; then
            log_ok "User $user: Btrfs mounted on primary $primary"
        else
            log_violation "User $user: Btrfs NOT mounted on primary $primary"
        fi
    fi
    
    if [ -n "$secondary" ]; then
        ip=$(get_pub_ip "$secondary")
        mounted=$(ssh root@$ip "mountpoint -q /mnt/users/$user && echo yes || echo no" 2>/dev/null)
        if [ "$mounted" = "no" ]; then
            log_ok "User $user: Btrfs correctly NOT mounted on secondary $secondary"
        else
            log_violation "User $user: Btrfs MOUNTED on secondary $secondary (should not be)"
        fi
    fi
done

# ── Invariant 5: No same-machine bipod pairs ──
SAME_MACHINE=$(psql "$DATABASE_URL" -t -A -c "
    SELECT b1.user_id FROM bipods b1 JOIN bipods b2 
    ON b1.user_id = b2.user_id AND b1.machine_id = b2.machine_id 
    AND b1.role != b2.role
    WHERE b1.state NOT IN ('failed','torn_down') AND b2.state NOT IN ('failed','torn_down')
")
if [ -z "$SAME_MACHINE" ]; then
    log_ok "No same-machine bipod pairs"
else
    log_violation "Same-machine bipod pairs found: $SAME_MACHINE"
fi

# ── Invariant 6: No orphaned resources ──
for entry in "$FLEET1_IP:fleet-1" "$FLEET2_IP:fleet-2" "$FLEET3_IP:fleet-3"; do
    IFS=: read -r ip name <<< "$entry"
    # Get images on this machine
    images=$(ssh root@$ip "ls /data/images/*.img 2>/dev/null | xargs -I{} basename {} .img" 2>/dev/null || echo "")
    for img_user in $images; do
        bipod_exists=$(psql "$DATABASE_URL" -t -A -c "SELECT COUNT(*) FROM bipods WHERE user_id='$img_user' AND machine_id='$name' AND state NOT IN ('failed','torn_down')")
        if [ "$bipod_exists" -eq 0 ]; then
            log_violation "Orphaned image for $img_user on $name (no active bipod entry)"
        fi
    done
done

# ── Invariant 7: DRBD port uniqueness ──
DUPE_PORTS=$(psql "$DATABASE_URL" -t -A -c "SELECT drbd_port, COUNT(*) FROM users GROUP BY drbd_port HAVING COUNT(*) > 1")
if [ -z "$DUPE_PORTS" ]; then
    log_ok "All DRBD ports unique"
else
    log_violation "Duplicate DRBD ports: $DUPE_PORTS"
fi

# ── Invariant 8: Minor uniqueness per machine ──
DUPE_MINORS=$(psql "$DATABASE_URL" -t -A -c "
    SELECT machine_id, drbd_minor, COUNT(*) FROM bipods 
    WHERE state NOT IN ('failed','torn_down') 
    GROUP BY machine_id, drbd_minor HAVING COUNT(*) > 1
")
if [ -z "$DUPE_MINORS" ]; then
    log_ok "All DRBD minors unique per machine"
else
    log_violation "Duplicate DRBD minors: $DUPE_MINORS"
fi

echo ""
if [ "$VIOLATIONS" -eq 0 ]; then
    echo "Consistency check PASSED (0 violations)"
    exit 0
else
    echo "Consistency check FAILED ($VIOLATIONS violations)"
    exit 1
fi
```

### scripts/test_suite.sh

This file contains all test phases. It's long — here's the structure:

```bash
#!/bin/bash
set -euo pipefail

source "$(dirname "$0")/common.sh"
load_ips

TOTAL_CHECKS=0
PASSED_CHECKS=0

check() {
    TOTAL_CHECKS=$((TOTAL_CHECKS + 1))
    PHASE_TOTAL=$((PHASE_TOTAL + 1))
    if eval "$2"; then
        echo "  ✓ $1"
        PASSED_CHECKS=$((PASSED_CHECKS + 1))
        PHASE_PASSED=$((PHASE_PASSED + 1))
    else
        echo "  ✗ FAILED: $1"
    fi
}

phase_summary() {
    echo "  [$PHASE_PASSED/$PHASE_TOTAL checks passed]"
    echo ""
}

# Helper: create user via API
create_user() {
    curl -s -X POST "http://$COORD_IP:8080/api/users" \
        -H "Content-Type: application/json" \
        -d "{\"user_id\": \"$1\"}" | jq .
}

# Helper: get user status
get_user() {
    curl -s "http://$COORD_IP:8080/api/users/$1" | jq .
}

# Helper: wait for user to reach status
wait_for_status() {
    local user_id=$1 expected=$2 timeout=${3:-60}
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        status=$(curl -s "http://$COORD_IP:8080/api/users/$user_id" | jq -r '.status')
        if [ "$status" = "$expected" ]; then return 0; fi
        sleep 2
        elapsed=$((elapsed + 2))
    done
    return 1
}

# Helper: kill and restart coordinator with optional fault injection
restart_coordinator() {
    local fail_at="${1:-}"
    ssh root@$COORD_IP "systemctl stop coordinator" 2>/dev/null || true
    sleep 1
    if [ -n "$fail_at" ]; then
        ssh root@$COORD_IP "sed -i '/^Environment=FAIL_AT/d' /etc/systemd/system/coordinator.service"
        ssh root@$COORD_IP "sed -i '/\[Service\]/a Environment=FAIL_AT=$fail_at' /etc/systemd/system/coordinator.service"
        ssh root@$COORD_IP "systemctl daemon-reload"
    else
        ssh root@$COORD_IP "sed -i '/^Environment=FAIL_AT/d' /etc/systemd/system/coordinator.service"
        ssh root@$COORD_IP "systemctl daemon-reload"
    fi
    ssh root@$COORD_IP "systemctl start coordinator"
    sleep 3  # wait for reconciliation
}

# ══════════════════════════════════════════════════════════════
# Phase 1: Single User Provisioning (Happy Path)
# ══════════════════════════════════════════════════════════════
echo "Phase 1: Single User Provisioning (Happy Path)"
PHASE_PASSED=0; PHASE_TOTAL=0

create_user "alice"
wait_for_status "alice" "running" 60

check "User alice status is running" '[ "$(curl -s http://$COORD_IP:8080/api/users/alice | jq -r .status)" = "running" ]'

# Get assigned machines from API
ALICE_PRIMARY=$(curl -s "http://$COORD_IP:8080/api/users/alice" | jq -r '.primary_machine')
ALICE_SECONDARY=$(psql "$DATABASE_URL" -t -A -c "SELECT machine_id FROM bipods WHERE user_id='alice' AND role='secondary' AND state='ready'")
ALICE_PRIMARY_IP=$(get_pub_ip "$ALICE_PRIMARY")
ALICE_SECONDARY_IP=$(get_pub_ip "$ALICE_SECONDARY")

check "[SSH $ALICE_PRIMARY] Image exists" 'ssh root@$ALICE_PRIMARY_IP "test -f /data/images/alice.img"'
check "[SSH $ALICE_SECONDARY] Image exists" 'ssh root@$ALICE_SECONDARY_IP "test -f /data/images/alice.img"'
check "[SSH $ALICE_PRIMARY] DRBD role is Primary" '[[ "$(ssh root@$ALICE_PRIMARY_IP "drbdadm status user-alice 2>/dev/null | head -1")" == *"role:Primary"* ]]'
check "[SSH $ALICE_SECONDARY] DRBD role is Secondary" '[[ "$(ssh root@$ALICE_SECONDARY_IP "drbdadm status user-alice 2>/dev/null | head -1")" == *"role:Secondary"* ]]'
check "[SSH $ALICE_PRIMARY] Btrfs mounted" 'ssh root@$ALICE_PRIMARY_IP "mountpoint -q /mnt/users/alice"'
check "[SSH $ALICE_PRIMARY] Container running" '[ "$(ssh root@$ALICE_PRIMARY_IP "docker inspect alice-agent 2>/dev/null | jq -r .[0].State.Running")" = "true" ]'
check "[Consistency] Full check passes" '"$SCRIPT_DIR/consistency_checker.sh"'

phase_summary

# ══════════════════════════════════════════════════════════════
# Phase 2: Multi-User Provisioning + Balanced Placement
# ══════════════════════════════════════════════════════════════
echo "Phase 2: Multi-User Provisioning + Balanced Placement"
PHASE_PASSED=0; PHASE_TOTAL=0

for user in bob charlie dave eve frank; do
    create_user "$user"
    sleep 2  # stagger slightly
done

# Wait for all to be running
for user in bob charlie dave eve frank; do
    wait_for_status "$user" "running" 90
done

check "All 6 users status: running" '[ "$(psql "$DATABASE_URL" -t -A -c "SELECT COUNT(*) FROM users WHERE status='"'"'running'"'"'")" = "6" ]'

# Check placement balance (12 bipod members across 3 machines = 4 each)
for machine in fleet-1 fleet-2 fleet-3; do
    count=$(psql "$DATABASE_URL" -t -A -c "SELECT COUNT(*) FROM bipods WHERE machine_id='$machine' AND state='ready'")
    check "Machine $machine has 4 bipod members" '[ "$count" -eq 4 ]'
done

check "No same-machine bipod pairs" '[ -z "$(psql "$DATABASE_URL" -t -A -c "SELECT b1.user_id FROM bipods b1 JOIN bipods b2 ON b1.user_id=b2.user_id AND b1.machine_id=b2.machine_id AND b1.role!=b2.role WHERE b1.state='"'"'ready'"'"' AND b2.state='"'"'ready'"'"'")" ]'
check "[Consistency] Full check passes" '"$SCRIPT_DIR/consistency_checker.sh"'

phase_summary

# ══════════════════════════════════════════════════════════════
# Phase 3: Machine Failure + Automatic Failover
# ══════════════════════════════════════════════════════════════
echo "Phase 3: Machine Failure + Automatic Failover"
PHASE_PASSED=0; PHASE_TOTAL=0

# Record which users have primary on fleet-1
AFFECTED_PRIMARY=$(psql "$DATABASE_URL" -t -A -c "SELECT user_id FROM bipods WHERE machine_id='fleet-1' AND role='primary' AND state='ready'")
AFFECTED_SECONDARY=$(psql "$DATABASE_URL" -t -A -c "SELECT user_id FROM bipods WHERE machine_id='fleet-1' AND role='secondary' AND state='ready'")

# Kill fleet-1
echo "  Killing fleet-1..."
hcloud server shutdown poc4-fleet-1

# Wait for coordinator to detect failure (15s threshold + processing)
echo "  Waiting for failure detection..."
sleep 25

check "Coordinator detected fleet-1 offline" '[ "$(psql "$DATABASE_URL" -t -A -c "SELECT status FROM machines WHERE machine_id='"'"'fleet-1'"'"'")" = "offline" ]'

# Wait for failovers to complete
sleep 15

check "All users still running" '[ "$(psql "$DATABASE_URL" -t -A -c "SELECT COUNT(*) FROM users WHERE status='"'"'running'"'"'")" = "6" ]'

# Verify affected primary users failed over
for user in $AFFECTED_PRIMARY; do
    check "User $user failed over (container running elsewhere)" "
        surviving=\$(psql \"\$DATABASE_URL\" -t -A -c \"SELECT machine_id FROM bipods WHERE user_id='$user' AND role='primary' AND state='ready'\")
        ip=\$(get_pub_ip \"\$surviving\")
        [ \"\$(ssh root@\$ip \"docker inspect ${user}-agent 2>/dev/null | jq -r .[0].State.Running\")\" = 'true' ]
    "
done

check "[Consistency] Post-failover check passes" '"$SCRIPT_DIR/consistency_checker.sh"'

phase_summary

# ══════════════════════════════════════════════════════════════
# Phase 4: Bipod Reformation + Dead Machine Return
# ══════════════════════════════════════════════════════════════
echo "Phase 4: Bipod Reformation + Dead Machine Return"
PHASE_PASSED=0; PHASE_TOTAL=0

# Bring fleet-1 back
echo "  Bringing fleet-1 back online..."
hcloud server poweron poc4-fleet-1
sleep 30  # wait for boot + cloud-init + machine agent start

# Restart machine agent (it was killed with the server)
ssh root@$FLEET1_IP "systemctl start machine-agent" 2>/dev/null || true
sleep 5

check "Fleet-1 back to active" '[ "$(psql "$DATABASE_URL" -t -A -c "SELECT status FROM machines WHERE machine_id='"'"'fleet-1'"'"'")" = "active" ]'

# Wait for reformation
echo "  Waiting for bipod reformation..."
sleep 60

check "All users have 2 healthy bipod members" '[ "$(psql "$DATABASE_URL" -t -A -c "SELECT COUNT(*) FROM users u WHERE (SELECT COUNT(*) FROM bipods b WHERE b.user_id=u.user_id AND b.state='"'"'ready'"'"')=2 AND u.status='"'"'running'"'"'")" = "6" ]'
check "No orphaned resources on fleet-1" '"$SCRIPT_DIR/consistency_checker.sh"'

phase_summary

# ══════════════════════════════════════════════════════════════
# Phase 5: Suspension + Reactivation
# ══════════════════════════════════════════════════════════════
echo "Phase 5: Suspension + Reactivation"
PHASE_PASSED=0; PHASE_TOTAL=0

curl -s -X POST "http://$COORD_IP:8080/api/users/alice/suspend"
sleep 5

check "Alice status is suspended" '[ "$(curl -s http://$COORD_IP:8080/api/users/alice | jq -r .status)" = "suspended" ]'

ALICE_PRIMARY=$(psql "$DATABASE_URL" -t -A -c "SELECT machine_id FROM bipods WHERE user_id='alice' AND role='primary' LIMIT 1")
ALICE_PRIMARY_IP=$(get_pub_ip "$ALICE_PRIMARY")

check "[SSH] Alice container stopped" '[ "$(ssh root@$ALICE_PRIMARY_IP "docker inspect alice-agent 2>/dev/null | jq -r .[0].State.Running" 2>/dev/null)" != "true" ]'

curl -s -X POST "http://$COORD_IP:8080/api/users/alice/reactivate"
sleep 5

check "Alice status is running again" '[ "$(curl -s http://$COORD_IP:8080/api/users/alice | jq -r .status)" = "running" ]'
check "[SSH] Alice container running again" '[ "$(ssh root@$ALICE_PRIMARY_IP "docker inspect alice-agent 2>/dev/null | jq -r .[0].State.Running")" = "true" ]'
check "[Consistency] Post-reactivation check passes" '"$SCRIPT_DIR/consistency_checker.sh"'

phase_summary

# ══════════════════════════════════════════════════════════════
# Phase 6: Deterministic Fault Injection — Provisioning
# ══════════════════════════════════════════════════════════════
echo "Phase 6: Deterministic Fault Injection — Provisioning"
PHASE_PASSED=0; PHASE_TOTAL=0

PROVISION_FAULTS=(
    "provision-user-created"
    "provision-primary-image-created"
    "provision-secondary-image-created"
    "provision-primary-drbd-configured"
    "provision-secondary-drbd-configured"
    "provision-drbd-synced"
    "provision-primary-promoted"
    "provision-btrfs-formatted"
    "provision-btrfs-mounted"
    "provision-containers-started"
)

fault_idx=0
for fault in "${PROVISION_FAULTS[@]}"; do
    fault_idx=$((fault_idx + 1))
    test_user="fault-p${fault_idx}"
    
    echo "  F${fault_idx}: Testing crash at ${fault}..."
    
    # Reset for clean test
    "$SCRIPT_DIR/reset.sh" >/dev/null 2>&1
    "$SCRIPT_DIR/seed_db.sh" >/dev/null 2>&1
    
    # Start coordinator with fault injection
    restart_coordinator "$fault"
    sleep 3
    
    # Trigger provisioning — coordinator will crash at the fault point
    create_user "$test_user" >/dev/null 2>&1 || true
    sleep 10  # give time for provisioning to reach the fault point
    
    # Coordinator should have crashed — restart without fault
    restart_coordinator ""
    sleep 15  # wait for reconciliation + provisioning to complete
    
    check "F${fault_idx}: Crash at ${fault} → recovered, user running" "wait_for_status '$test_user' 'running' 60"
done

phase_summary

# ══════════════════════════════════════════════════════════════
# Phase 7: Deterministic Fault Injection — Failover
# ══════════════════════════════════════════════════════════════
echo "Phase 7: Deterministic Fault Injection — Failover"
PHASE_PASSED=0; PHASE_TOTAL=0

FAILOVER_FAULTS=(
    "failover-detected"
    "failover-promoted"
    "failover-mounted"
    "failover-containers-started"
)

fault_idx=0
for fault in "${FAILOVER_FAULTS[@]}"; do
    fault_idx=$((fault_idx + 1))
    
    echo "  F$((fault_idx + 10)): Testing crash at ${fault}..."
    
    # Setup: clean state with one running user
    "$SCRIPT_DIR/reset.sh" >/dev/null 2>&1
    "$SCRIPT_DIR/seed_db.sh" >/dev/null 2>&1
    restart_coordinator ""
    sleep 3
    create_user "failtest" >/dev/null 2>&1
    wait_for_status "failtest" "running" 60
    
    # Now set fault and kill a machine to trigger failover
    restart_coordinator "$fault"
    sleep 3
    
    PRIMARY=$(psql "$DATABASE_URL" -t -A -c "SELECT primary_machine FROM users WHERE user_id='failtest'")
    hcloud server shutdown "poc4-${PRIMARY}"
    sleep 25  # wait for detection + fault trigger
    
    # Coordinator should have crashed during failover — restart clean
    restart_coordinator ""
    sleep 15  # reconciliation
    
    # Bring machine back for reformation
    hcloud server poweron "poc4-${PRIMARY}"
    sleep 30
    
    check "F$((fault_idx + 10)): Crash at ${fault} → recovered, user running" "wait_for_status 'failtest' 'running' 60"
done

phase_summary

# ══════════════════════════════════════════════════════════════
# Phase 8: Deterministic Fault Injection — Bipod Reformation
# ══════════════════════════════════════════════════════════════
echo "Phase 8: Deterministic Fault Injection — Bipod Reformation"
PHASE_PASSED=0; PHASE_TOTAL=0

REFORM_FAULTS=(
    "reform-machine-picked"
    "reform-image-created"
    "reform-drbd-configured"
    "reform-synced"
)

fault_idx=0
for fault in "${REFORM_FAULTS[@]}"; do
    fault_idx=$((fault_idx + 1))
    
    echo "  F$((fault_idx + 14)): Testing crash at ${fault}..."
    
    # Setup: running user, then kill secondary to trigger reformation
    "$SCRIPT_DIR/reset.sh" >/dev/null 2>&1
    "$SCRIPT_DIR/seed_db.sh" >/dev/null 2>&1
    restart_coordinator ""
    sleep 3
    create_user "reformtest" >/dev/null 2>&1
    wait_for_status "reformtest" "running" 60
    
    SECONDARY=$(psql "$DATABASE_URL" -t -A -c "SELECT machine_id FROM bipods WHERE user_id='reformtest' AND role='secondary'")
    
    # Set fault, then kill secondary (triggers reformation, not failover)
    restart_coordinator "$fault"
    sleep 3
    hcloud server shutdown "poc4-${SECONDARY}"
    sleep 25  # detection
    sleep 30  # reformation attempt hits fault
    
    # Restart clean
    hcloud server poweron "poc4-${SECONDARY}"
    sleep 30
    restart_coordinator ""
    sleep 30  # reconciliation + reformation
    
    bipod_count=$(psql "$DATABASE_URL" -t -A -c "SELECT COUNT(*) FROM bipods WHERE user_id='reformtest' AND state='ready'")
    check "F$((fault_idx + 14)): Crash at ${fault} → recovered, bipod complete" '[ "$bipod_count" -eq 2 ]'
done

phase_summary

# ══════════════════════════════════════════════════════════════
# Phase 9: Chaos Mode
# ══════════════════════════════════════════════════════════════
echo "Phase 9: Chaos Mode (50 iterations, 5% crash probability)"
PHASE_PASSED=0; PHASE_TOTAL=0

"$SCRIPT_DIR/reset.sh" >/dev/null 2>&1
"$SCRIPT_DIR/seed_db.sh" >/dev/null 2>&1

# Start coordinator in chaos mode
ssh root@$COORD_IP "
    sed -i '/^Environment=CHAOS/d' /etc/systemd/system/coordinator.service
    sed -i '/\[Service\]/a Environment=CHAOS_MODE=true' /etc/systemd/system/coordinator.service
    sed -i '/\[Service\]/a Environment=CHAOS_PROBABILITY=0.05' /etc/systemd/system/coordinator.service
    systemctl daemon-reload
    systemctl restart coordinator
"
sleep 3

CHAOS_CRASHES=0
CHAOS_ITERATIONS=50

for i in $(seq 1 $CHAOS_ITERATIONS); do
    user_id="chaos-user-$(printf '%03d' $i)"
    
    # Try to create a user
    create_user "$user_id" >/dev/null 2>&1 || true
    sleep 3
    
    # Check if coordinator is alive
    if ! curl -s -o /dev/null -w '%{http_code}' "http://$COORD_IP:8080/api/fleet" | grep -q 200; then
        CHAOS_CRASHES=$((CHAOS_CRASHES + 1))
        echo "  Chaos crash #$CHAOS_CRASHES at iteration $i — restarting..."
        ssh root@$COORD_IP "systemctl restart coordinator" 2>/dev/null || true
        sleep 5  # reconciliation
    fi
done

# Final: restart without chaos, let reconciliation clean up
ssh root@$COORD_IP "
    sed -i '/^Environment=CHAOS/d' /etc/systemd/system/coordinator.service
    systemctl daemon-reload
    systemctl restart coordinator
"
sleep 15  # final reconciliation

check "Completed $CHAOS_ITERATIONS iterations" 'true'
check "Coordinator crashed $CHAOS_CRASHES times, recovered each time" 'true'

# Count users in valid states (running, provisioning, failed — not stuck)
VALID_STATES=$(psql "$DATABASE_URL" -t -A -c "SELECT COUNT(*) FROM users WHERE status IN ('running','suspended','failed')")
TOTAL_USERS=$(psql "$DATABASE_URL" -t -A -c "SELECT COUNT(*) FROM users")
check "All users in valid final state ($VALID_STATES/$TOTAL_USERS)" '[ "$VALID_STATES" = "$TOTAL_USERS" ]'

check "[Consistency] Post-chaos check passes" '"$SCRIPT_DIR/consistency_checker.sh"'
check "No orphaned resources" '"$SCRIPT_DIR/consistency_checker.sh"'

phase_summary

# ══════════════════════════════════════════════════════════════
# Summary
# ══════════════════════════════════════════════════════════════
echo "═══════════════════════════════════════════════════"
echo "ALL PHASES COMPLETE: $PASSED_CHECKS/$TOTAL_CHECKS checks passed"
echo "═══════════════════════════════════════════════════"
```

---

## Key Implementation Notes

### 1. DRBD Config Must Be Identical on Both Machines

Both sides of a DRBD resource must have the exact same config file. The coordinator constructs the full config (both `on` blocks) and sends it to both machine agents. Each machine agent writes the identical file to `/etc/drbd.d/user-{user_id}.res`.

DRBD determines which `on` block is local by matching the hostname. The machine agent must set its hostname to match its `machine_id` (done in deploy.sh via `hostnamectl set-hostname`).

### 2. DRBD Must Be Set Up Before Filesystem

Proven in PoC 3 Patch 3.1. When using `meta-disk internal`, DRBD reserves space at the end of the backing device. The rule: always create an empty image → loop device → DRBD metadata → DRBD up → promote → mkfs.btrfs on /dev/drbdN. Never format a filesystem before DRBD metadata is written.

This PoC enforces this via the provisioning sequence: Steps 2-3 create empty sparse files, Steps 4-7 set up and promote DRBD, Step 8 calls `format-btrfs` which formats the DRBD device and creates the workspace structure, Step 9 mounts. No templates are used.

### 3. Hostname Matching for DRBD

DRBD uses the system hostname to determine which `on` block in the config is local. Each fleet machine must have its hostname set to match its machine_id: `fleet-1`, `fleet-2`, `fleet-3`. The deploy script handles this with `hostnamectl set-hostname`.

### 4. Coordinator Advisory Lock

On startup, the coordinator runs:
```sql
SELECT pg_try_advisory_lock(12345)
```
If this returns false, another coordinator instance is running. The new instance logs an error and exits. This prevents split-brain.

The lock is automatically released when the Postgres connection closes (which happens when the coordinator process dies).

### 5. Structured Logging

Both the coordinator and machine agent use JSON structured logging:
```json
{"time":"2026-02-28T12:00:00Z","level":"INFO","component":"provisioner","user":"alice","step":"primary_image_created","msg":"Image created on fleet-1"}
```

This makes it easy to grep logs for a specific user or component during debugging.

### 6. HTTP Client Timeouts

The coordinator's HTTP client for calling machine agents should have:
- Connect timeout: 5 seconds
- Request timeout: 30 seconds (some operations like DRBD sync polling may take longer — use separate longer timeouts for those)
- Retry policy: 3 retries with 2-second backoff for transient failures

### 7. Graceful Shutdown (When NOT Fault Injecting)

On SIGTERM (normal shutdown), the coordinator should:
1. Stop accepting new API requests
2. Wait for in-progress operations to reach a checkpoint (max 10 seconds)
3. Close database connections
4. Exit

This is for clean upgrades. Fault injection bypasses this entirely (os.Exit(1)).

---

## Expected Test Output

```
=== LAYER 4: COORDINATOR & MACHINE AGENT PoC ===

Phase 0: Infrastructure Setup
  ✓ 4 CX23 machines created
  ✓ DRBD modules loaded on fleet machines
  ✓ Docker running on fleet machines
  ✓ Storage directories created (/data/images, /mnt/users)
  ✓ Machine agents running and reachable
  ✓ Coordinator running
  ✓ Database seeded with 3 machines
  ✓ All 3 machine agents heartbeating
  [8/8 checks passed]

Phase 1: Single User Provisioning (Happy Path)
  ✓ User alice status is running
  ✓ [SSH fleet-1] Image exists
  ✓ [SSH fleet-2] Image exists
  ✓ [SSH fleet-1] DRBD role is Primary
  ✓ [SSH fleet-2] DRBD role is Secondary
  ✓ [SSH fleet-1] Btrfs mounted at /mnt/users/alice
  ✓ [SSH fleet-1] Container alice-agent running
  ✓ [Consistency] Full check passes
  [8/8 checks passed]

Phase 2: Multi-User Provisioning + Balanced Placement
  ✓ All 6 users status: running
  ✓ Machine fleet-1 has 4 bipod members
  ✓ Machine fleet-2 has 4 bipod members
  ✓ Machine fleet-3 has 4 bipod members
  ✓ No same-machine bipod pairs
  ✓ [Consistency] Full check passes
  [6/6 checks passed]

Phase 3: Machine Failure + Automatic Failover
  ✓ Coordinator detected fleet-1 offline
  ✓ All users still running
  ✓ Affected users failed over to surviving machines
  ✓ [Consistency] Post-failover check passes
  [4/4 checks passed]

Phase 4: Bipod Reformation + Dead Machine Return
  ✓ Fleet-1 back to active
  ✓ Orphaned resources cleaned up
  ✓ All users have 2 healthy bipod members
  ✓ [Consistency] Post-reformation check passes
  [4/4 checks passed]

Phase 5: Suspension + Reactivation
  ✓ Alice status is suspended
  ✓ [SSH] Alice container stopped
  ✓ Alice status is running again
  ✓ [SSH] Alice container running again
  ✓ [Consistency] Post-reactivation check passes
  [5/5 checks passed]

Phase 6: Deterministic Fault Injection — Provisioning
  ✓ F1: Crash at provision-user-created → recovered
  ✓ F2: Crash at provision-primary-image-created → recovered
  ✓ F3: Crash at provision-secondary-image-created → recovered
  ✓ F4: Crash at provision-primary-drbd-configured → recovered
  ✓ F5: Crash at provision-secondary-drbd-configured → recovered
  ✓ F6: Crash at provision-drbd-synced → recovered
  ✓ F7: Crash at provision-primary-promoted → recovered
  ✓ F8: Crash at provision-btrfs-formatted → recovered
  ✓ F9: Crash at provision-btrfs-mounted → recovered
  ✓ F10: Crash at provision-containers-started → recovered
  [10/10 checks passed]

Phase 7: Deterministic Fault Injection — Failover
  ✓ F11: Crash at failover-detected → recovered
  ✓ F12: Crash at failover-promoted → recovered
  ✓ F13: Crash at failover-mounted → recovered
  ✓ F14: Crash at failover-containers-started → recovered
  [4/4 checks passed]

Phase 8: Deterministic Fault Injection — Bipod Reformation
  ✓ F15: Crash at reform-machine-picked → recovered
  ✓ F16: Crash at reform-image-created → recovered
  ✓ F17: Crash at reform-drbd-configured → recovered
  ✓ F18: Crash at reform-synced → recovered
  [4/4 checks passed]

Phase 9: Chaos Mode (50 iterations, 5% crash probability)
  ✓ Completed 50 iterations
  ✓ Coordinator crashed N times, recovered each time
  ✓ All users in valid final state
  ✓ [Consistency] Post-chaos check passes
  ✓ No orphaned resources
  [5/5 checks passed]

═══════════════════════════════════════════════════
ALL PHASES COMPLETE: 58/58 checks passed
═══════════════════════════════════════════════════
```

---

## Summary of Fault Injection Checkpoints

### Provisioning Checkpoints
| ID | Checkpoint name | After this completes | Before this starts |
|----|----------------|---------------------|-------------------|
| F1 | `provision-user-created` | DB: user + bipod entries created | Image creation |
| F2 | `provision-primary-image-created` | Image on primary machine | Image on secondary |
| F3 | `provision-secondary-image-created` | Images on both machines | DRBD config |
| F4 | `provision-primary-drbd-configured` | DRBD config on primary | DRBD config on secondary |
| F5 | `provision-secondary-drbd-configured` | DRBD config on both | DRBD connect |
| F6 | `provision-drbd-synced` | DRBD connected and synced | Promote |
| F7 | `provision-primary-promoted` | DRBD promoted | Format Btrfs |
| F8 | `provision-btrfs-formatted` | Btrfs formatted + workspace created | Mount |
| F9 | `provision-btrfs-mounted` | Btrfs mounted | Container start |
| F10 | `provision-containers-started` | Container running | DB finalization |

### Failover Checkpoints
| ID | Checkpoint name | After | Before |
|----|----------------|-------|--------|
| F11 | `failover-detected` | Machine marked offline | DRBD promote |
| F12 | `failover-promoted` | DRBD promoted on survivor | Mount |
| F13 | `failover-mounted` | Btrfs mounted | Container start |
| F14 | `failover-containers-started` | Container running | DB finalization |

### Bipod Reformation Checkpoints
| ID | Checkpoint name | After | Before |
|----|----------------|-------|--------|
| F15 | `reform-machine-picked` | New machine selected in DB | Image creation |
| F16 | `reform-image-created` | Image on new machine | DRBD config |
| F17 | `reform-drbd-configured` | DRBD configured | Sync |
| F18 | `reform-synced` | Fully synced | DB finalization |

---

## Go Dependencies

```
go mod init poc-coordinator

# Standard library only — no external dependencies needed for core functionality
# database/sql + lib/pq for Postgres
# net/http for HTTP server/client
# encoding/json for JSON
# os/exec for shell commands
# sync for mutexes
# log/slog for structured logging (Go 1.21+)

go get github.com/lib/pq
```

The only external dependency is the Postgres driver. Everything else is standard library.
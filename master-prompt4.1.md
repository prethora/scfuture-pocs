# Layer 4.1 Build Prompt: Machine Agent

## Overview

Build a Go HTTP server that runs on each fleet machine and wraps the proven block device stack behind idempotent API endpoints. This is Layer 4.1 in the proof-of-concept progression — the first Go code in the project. A bash test harness on macOS drives the machine agent through the full lifecycle via HTTP calls, playing the role that a coordinator will play in Layer 4.2.

### What This PoC Proves

1. **Go wrapping of system commands** — `losetup`, `drbdadm`, `mkfs.btrfs`, `btrfs subvolume`, `docker run` are all driven by a Go HTTP server, not shell scripts
2. **Idempotent API** — every endpoint is safe to call multiple times with the same result (critical for crash recovery in later layers)
3. **Device-mount container pattern** — production container isolation from day one: block device passed via `--device`, subvolume mounted inside container by init script, capabilities dropped, process runs as unprivileged user
4. **Full block device stack via API** — sparse image → loop device → DRBD → Btrfs → subvolume → device-mount container, all orchestrated through HTTP endpoints
5. **API-driven failover** — stop container, demote primary, promote secondary, start container on new primary, data survives
6. **Multi-user density** — 3 users on the same machine pair with no resource collisions (separate DRBD resources, ports, minors, containers)
7. **Status reporting** — machine agent accurately reports the state of all resources for all users

### What This PoC Does NOT Cover

- Coordinator (Layer 4.2)
- Database / state persistence (Layer 4.2)
- Heartbeat (Layer 4.2)
- Placement algorithm (Layer 4.2)
- Failure detection (Layer 4.4)
- Bipod reformation (Layer 4.5)
- Suspension/reactivation (Layer 4.6)
- Crash recovery / reconciliation (Layer 4.7)
- Backblaze B2 backups
- Snapshots / tweak mode
- Credential proxy / cost tracking
- Telegram gateway

### Prior Art

This PoC builds directly on three completed PoCs:

- **PoC 1 (poc-btrfs)**: Proved Btrfs world isolation with device-mount pattern. Key learning: containers receive block device via `--device`, mount subvolume internally via init script, drop to unprivileged user. No bind mounts. No metadata leakage in `/proc/mounts`.
- **PoC 1 Patch 1**: Proved the capability model: `--cap-drop ALL --cap-add SYS_ADMIN --cap-add SETUID --cap-add SETGID`. SYS_ADMIN for mount, SETUID+SETGID for `su` to switch to appuser. These capabilities exist only during init; gone once workload starts.
- **PoC 2 (poc-drbd)**: Proved DRBD bipod replication between two Hetzner servers. 47/47 checks. Key learnings: DRBD 9 requires LINBIT PPA on Ubuntu 24.04, hostname must match DRBD config `on` blocks, `drbdadm primary --force` needed for initial promotion.
- **PoC 3 (poc-backblaze)**: Proved Backblaze B2 backup and cold restore. Key learning (Patch 3.1): DRBD must always be set up before the filesystem. `create-md` with `meta-disk internal` writes metadata at the end of the backing device — if a filesystem already exists there, it gets corrupted. Rule: always blank image → DRBD → then format Btrfs on `/dev/drbdN`.

The shell commands used in PoCs 2 and 3 are the reference for the machine agent's Go implementation. This PoC replaces manual SSH commands with Go HTTP API calls.

---

## Infrastructure

### Machines

2 Hetzner Cloud CX23 instances (2 vCPU, 4GB RAM, 40GB disk), Ubuntu 24.04, private network 10.0.0.0/24.

| Machine | Hostname | Private IP | Public IP |
|---------|----------|------------|-----------|
| machine-1 | machine-1 | (assigned by Hetzner) | (assigned by Hetzner) |
| machine-2 | machine-2 | (assigned by Hetzner) | (assigned by Hetzner) |

Both machines are on the same Hetzner private network. DRBD replication uses private IPs. The test harness on macOS connects via public IPs (SSH and HTTP to port 8080).

NOTE: Hetzner assigns private IPs automatically. The `infra.sh` script discovers the assigned IPs after creation and saves them. All subsequent scripts use the discovered IPs. Do NOT hardcode IPs.

### No Database

This layer has no database. The machine agent's state is entirely in-memory (rebuilt from system state on startup). The test harness drives operations via HTTP and verifies via SSH.

---

## Network Topology

```
macOS (test harness)
  │
  ├── Public Internet ──→ machine-1 (public IP, port 22 SSH + port 8080 HTTP)
  └── Public Internet ──→ machine-2 (public IP, port 22 SSH + port 8080 HTTP)

Hetzner private network (10.0.0.0/24):
  machine-1 ←──DRBD──→ machine-2  (per-user ports, 7900+)
```

---

## Go Project Structure

Single Go module, single binary:

```
poc-coordinator/
├── go.mod                          (module: poc-coordinator)
├── go.sum
├── Makefile
├── cmd/
│   └── machine-agent/
│       └── main.go                 # Entry point, config, startup
├── internal/
│   └── machineagent/
│       ├── server.go               # HTTP server, route registration, request/response helpers
│       ├── images.go               # Image create/delete, loop device management
│       ├── drbd.go                 # DRBD lifecycle (config write, create-md, up, promote, demote, status, down, destroy)
│       ├── btrfs.go                # Btrfs format on DRBD device, subvolume + snapshot creation
│       ├── containers.go           # Docker container start/stop/status (device-mount pattern)
│       ├── state.go                # In-memory state map, startup discovery from system commands
│       ├── cleanup.go              # Full cleanup per-user (reverse order) and whole-machine
│       └── exec.go                 # Command execution helper (captures stdout+stderr, structured errors)
├── container/
│   ├── Dockerfile                  # App container image (device-mount pattern)
│   └── container-init.sh           # Init script: mount subvol → drop to appuser → exec workload
├── scripts/
│   ├── run.sh                      # Full PoC lifecycle: infra up → deploy → test → teardown
│   ├── common.sh                   # Shared functions (IP discovery, SSH helpers, check framework)
│   ├── infra.sh                    # Hetzner machine lifecycle (up/down/status)
│   ├── deploy.sh                   # Cross-compile Go binary + SCP + configure + build container image
│   ├── test_suite.sh               # All 9 test phases (~60 checks)
│   └── cloud-init/
│       └── fleet.yaml              # Cloud-init for fleet machines (DRBD 9, Docker, btrfs-progs)
└── BUILD_PROMPT.md                 # This file
```

The module is named `poc-coordinator` because future layers will add `cmd/coordinator/` and `internal/coordinator/` to the same module. This avoids renaming later.

### Makefile

```makefile
.PHONY: build deploy test clean

build:
	GOOS=linux GOARCH=amd64 go build -o bin/machine-agent ./cmd/machine-agent

deploy: build
	./scripts/deploy.sh

test:
	./scripts/test_suite.sh

clean:
	rm -rf bin/
```

### Go Dependencies

Zero external dependencies. Standard library only:

- `net/http` — HTTP server and router
- `os/exec` — running system commands
- `encoding/json` — JSON request/response
- `sync` — per-user mutexes
- `log/slog` — structured logging (Go 1.21+)
- `fmt`, `strings`, `path/filepath`, `os`, `time`, `io`

No database driver. No frameworks.

---

## Container Image: Device-Mount Pattern

This is the production container pattern proven in PoC 1 Patch 1. The container receives a raw block device, mounts the Btrfs subvolume internally, drops to an unprivileged user, and executes the workload. The host never mounts Btrfs for container operation (only during the one-time `format-btrfs` provisioning step).

### container/Dockerfile

```dockerfile
FROM alpine:latest

RUN apk add --no-cache btrfs-progs

RUN adduser -D -h /workspace appuser

COPY container-init.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/container-init.sh

ENTRYPOINT ["/usr/local/bin/container-init.sh"]
```

### container/container-init.sh

```bash
#!/bin/sh
set -e

# BLOCK_DEVICE and SUBVOL_NAME are passed as environment variables
# BLOCK_DEVICE = /dev/drbd0 (or whatever minor)
# SUBVOL_NAME = workspace

# Mount the specific subvolume directly to /workspace
mkdir -p /workspace
mount -t btrfs -o subvol="$SUBVOL_NAME" "$BLOCK_DEVICE" /workspace

# Drop to unprivileged user and exec workload
# After exec, this process is replaced — SYS_ADMIN, SETUID, SETGID caps are gone
exec su appuser -s /bin/sh -c "${WORKLOAD_CMD:-"while true; do sleep 60; done"}"
```

### How Containers Are Started

```bash
docker run -d \
  --name {user_id}-agent \
  --device /dev/drbd{minor} \
  --cap-drop ALL \
  --cap-add SYS_ADMIN \
  --cap-add SETUID \
  --cap-add SETGID \
  --network none \
  --memory 64m \
  -e BLOCK_DEVICE=/dev/drbd{minor} \
  -e SUBVOL_NAME=workspace \
  platform/app-container
```

**Why `--device` and not `-v` bind mount:**
- Bind mounts leak host paths in `/proc/mounts` inside the container (proven in PoC 1)
- With `--device`, the container has the raw block device and mounts it internally
- `/proc/mounts` inside the container shows only `workspace` — no host paths, no other user names, no mount topology leakage
- This is the production pattern. We test the production pattern.

**Why these capabilities:**
- `SYS_ADMIN`: required for `mount` syscall inside the container
- `SETUID` + `SETGID`: required for `su` to switch to `appuser` (kernel requirement, discovered in PoC 1 Patch 1)
- All three exist only during the init script execution. After `exec su appuser ...`, the process is replaced and runs with zero capabilities as a non-root user.

**Why `--network none`:**
- Each container is network-isolated. In production, network whitelisting happens at the platform level outside the container.
- For this layer, no container needs network access.

### When the Host Mounts Btrfs

The host-side mount of Btrfs happens in exactly one situation in this layer:

1. **`format-btrfs` endpoint** — temporarily mounts `/dev/drbd{minor}` on the host to create the workspace subvolume, seed directories, and layer-000 snapshot, then unmounts.

The host does NOT mount Btrfs for normal container operation. The container handles its own mount via the init script. This means after provisioning completes, the host has no Btrfs mount point for the user.

---

## Component: Machine Agent

The machine agent is a Go HTTP server that translates HTTP requests into local system operations. All endpoints are **idempotent** — safe to retry any number of times.

### Startup Sequence

```
1. Read configuration from environment variables:
     NODE_ID      (required — e.g. "machine-1")
     LISTEN_ADDR  (default: "0.0.0.0:8080")
     DATA_DIR     (default: "/data")

2. Discover existing local state (see State Discovery section)

3. Verify the platform/app-container Docker image exists:
   → If not: build from /opt/platform/container/ (Dockerfile + init script)
   → If yes: log "Container image already built"

4. Start HTTP server on LISTEN_ADDR

5. Log: "Machine agent ready: {NODE_ID}, {N} existing users discovered"
```

### State Discovery (On Startup)

The machine agent builds an in-memory state map from system reality. This is critical — the agent may have been restarted while resources were active.

```go
type UserResources struct {
    ImagePath        string // /data/images/alice.img
    LoopDevice       string // /dev/loop0
    DRBDResource     string // user-alice
    DRBDMinor        int    // 0
    DRBDDevice       string // /dev/drbd0
    HostMounted      bool   // true if /mnt/users/alice is mounted (should only be during format)
    ContainerRunning bool   // true if alice-agent container is running
}
```

Discovery steps:

```
1. Scan losetup -a for active loop devices backed by /data/images/*.img
   Parse output format: "/dev/loop0: [64768]:12345 (/data/images/alice.img)"
   Extract: loop device path, user_id from filename

2. Scan /etc/drbd.d/user-*.res for DRBD config files
   Extract: user_id from filename, minor number from config content

3. Run drbdadm status all and parse output
   For each resource: role, connection state, disk state, peer disk state
   Handle multiple resources in output (new for this layer — PoCs only had one)

4. Scan mount output for /mnt/users/* (should be empty during normal operation)

5. Scan docker ps --format '{{.Names}}' for *-agent containers

6. Cross-reference all sources to build per-user state map
```

### Per-User Locking

Every endpoint that operates on a user's resources acquires a per-user mutex first. This prevents races if the caller sends rapid sequential requests for the same user.

```go
type Agent struct {
    nodeID    string
    dataDir   string
    users     map[string]*UserResources
    usersMu   sync.RWMutex          // protects the users map
    locks     sync.Map               // map[string]*sync.Mutex — per-user operation lock
}

func (a *Agent) getUserLock(userID string) *sync.Mutex {
    val, _ := a.locks.LoadOrStore(userID, &sync.Mutex{})
    return val.(*sync.Mutex)
}
```

### Command Execution Helper

Every system command goes through a helper that captures stdout, stderr, and exit code:

```go
type CmdResult struct {
    Stdout   string
    Stderr   string
    ExitCode int
}

func runCmd(name string, args ...string) (*CmdResult, error) {
    cmd := exec.Command(name, args...)
    var stdout, stderr bytes.Buffer
    cmd.Stdout = &stdout
    cmd.Stderr = &stderr
    err := cmd.Run()
    result := &CmdResult{
        Stdout: stdout.String(),
        Stderr: stderr.String(),
    }
    if exitErr, ok := err.(*exec.ExitError); ok {
        result.ExitCode = exitErr.ExitCode()
    }
    return result, err
}
```

All endpoints use this helper and include stdout/stderr in error responses for debugging.

---

## API Endpoints

### GET /status

Returns machine health and per-user resource state. This is the primary state reporting endpoint.

Response:
```json
{
    "machine_id": "machine-1",
    "disk_total_mb": 40000,
    "disk_used_mb": 3500,
    "ram_total_mb": 4096,
    "ram_used_mb": 1200,
    "users": {
        "alice": {
            "image_exists": true,
            "image_path": "/data/images/alice.img",
            "loop_device": "/dev/loop0",
            "drbd_resource": "user-alice",
            "drbd_minor": 0,
            "drbd_device": "/dev/drbd0",
            "drbd_role": "Primary",
            "drbd_connection": "Connected",
            "drbd_disk_state": "UpToDate",
            "drbd_peer_disk_state": "UpToDate",
            "host_mounted": false,
            "container_running": true,
            "container_name": "alice-agent"
        }
    }
}
```

Implementation: refreshes state from system commands (not just in-memory cache) for accuracy. Calls `losetup -a`, `drbdadm status all`, `mount`, `docker ps --format json` and aggregates. Disk and RAM from `df` and `/proc/meminfo`.

### POST /images/{user_id}/create

Creates a sparse image file and attaches a loop device. The image is NOT formatted — Btrfs formatting happens later via `format-btrfs` on the DRBD device (DRBD-first rule from PoC 3 Patch 3.1).

Request body:
```json
{
    "image_size_mb": 512
}
```

Response:
```json
{
    "loop_device": "/dev/loop0",
    "image_path": "/data/images/alice.img",
    "already_existed": false
}
```

Implementation:
```
1. Validate user_id: alphanumeric + hyphens, 3-32 chars
2. Check if /data/images/{user_id}.img already exists
   → If yes AND loop device attached: return 200 with already_existed=true, existing loop device
   → If yes but no loop device: attach with losetup, return 200
3. truncate -s {image_size_mb}M /data/images/{user_id}.img
4. losetup -f --show /data/images/{user_id}.img → capture loop device path
5. Update in-memory state
6. Return 200
```

### DELETE /images/{user_id}

Tears down ALL resources for a user in reverse order. Each step checks if the resource exists before acting. This is the full cleanup endpoint.

Implementation:
```
1. If container {user_id}-agent exists → docker stop (timeout 10s) + docker rm
2. If /mnt/users/{user_id} mounted → umount
3. If DRBD resource user-{user_id} exists → drbdadm down user-{user_id}
4. If /etc/drbd.d/user-{user_id}.res exists → rm
5. If loop device attached to {user_id}.img → losetup -d {loop_device}
6. If /data/images/{user_id}.img exists → rm
7. If /mnt/users/{user_id} dir exists → rmdir
8. Remove from in-memory state
9. Return 200
```

Every step is guarded — if the resource doesn't exist, skip silently. This makes the endpoint safe to call at any point in the lifecycle (partial provisioning, full teardown, already clean).

### POST /images/{user_id}/drbd/create

Configures DRBD resource for this user. Both machines in the bipod receive the SAME request body and write the SAME config file. DRBD determines which `on` block is local by matching the system hostname.

**Prerequisite:** Images must be created on BOTH machines first (test harness needs both loop device paths).

Request body:
```json
{
    "resource_name": "user-alice",
    "nodes": [
        {
            "hostname": "machine-1",
            "minor": 0,
            "disk": "/dev/loop0",
            "address": "10.0.0.2"
        },
        {
            "hostname": "machine-2",
            "minor": 1,
            "disk": "/dev/loop1",
            "address": "10.0.0.3"
        }
    ],
    "port": 7900
}
```

Implementation:
```
1. Check if DRBD resource user-{user_id} already exists (via drbdadm status)
   → If exists: return 200 with already_existed=true
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
5. Update in-memory state with minor, device path
6. Return 200
```

**Critical:** The config file must be byte-identical on both machines. The test harness constructs the full config body once and sends it to both agents. DRBD matches the local node by hostname (`hostnamectl set-hostname` done during deploy).

**Note on `create-md --force`:** The `--force` flag is needed because the backing device may have had previous DRBD metadata (from a prior test run). Without `--force`, `create-md` prompts interactively and hangs. Learned in PoC 2 (Issue 8).

### POST /images/{user_id}/drbd/promote

Promotes DRBD resource to primary. For initial promotion of a new (blank) resource, uses `--force` because the peer's disk state will be Inconsistent.

Implementation:
```
1. Check current role via drbdadm status user-{user_id}
   → If already Primary: return 200 with already_existed=true
2. drbdadm primary --force user-{user_id}
   (--force needed for initial sync when peer is Inconsistent.
    Harmless if peer is already UpToDate.)
3. Update in-memory state
4. Return 200
```

### POST /images/{user_id}/drbd/demote

Demotes DRBD resource to secondary. The host MUST NOT have the Btrfs mounted (container uses device-mount, so normally the host has no mount — but check anyway for safety).

Implementation:
```
1. Check current role → if already Secondary, return 200
2. If /mnt/users/{user_id} is mounted → umount first (safety check)
3. drbdadm secondary user-{user_id}
4. Update in-memory state
5. Return 200
```

### GET /images/{user_id}/drbd/status

Returns DRBD resource status. The parser must handle multiple output formats.

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

**DRBD 9 status output parsing (critical — multiple formats discovered in PoCs):**

Connected:
```
user-alice role:Primary
  disk:UpToDate
  machine-2 role:Secondary
    peer-disk:UpToDate
```

Syncing:
```
user-alice role:Primary
  disk:UpToDate
  machine-2 role:Secondary
    replication:SyncSource peer-disk:Inconsistent done:45.20
```

Disconnected (peer unreachable or not configured yet):
```
user-alice role:Primary
  disk:UpToDate
```

StandAlone (just started, peer not up yet):
```
user-alice role:Secondary
  disk:Inconsistent
```

**The parser must handle all variants.** Parse the first line for resource name and role. Look for `disk:` line for local disk state. Look for a peer section (hostname followed by `role:`) for connection info. Extract `peer-disk:` and optionally `done:` for sync progress. If no peer section exists, connection_state is "StandAlone" and peer fields are empty.

**Multi-resource output:** When called as `drbdadm status all`, DRBD 9 outputs all resources separated by blank lines. The `status` endpoint for a specific user calls `drbdadm status user-{user_id}` which returns only that resource. But the startup discovery uses `drbdadm status all` and must parse multiple resources.

### DELETE /images/{user_id}/drbd

Tears down DRBD resource only (not the full user teardown).

Implementation:
```
1. drbdadm down user-{user_id} (ignore error if already down or doesn't exist)
2. rm -f /etc/drbd.d/user-{user_id}.res
3. Update in-memory state
4. Return 200
```

### POST /images/{user_id}/format-btrfs

Formats the DRBD device with Btrfs, creates the workspace subvolume, seed directories, and layer-000 snapshot. This is the ONLY operation that mounts Btrfs on the host. It unmounts before returning.

**Prerequisite:** DRBD must be Primary on this machine.

Implementation:
```
1. Determine DRBD device path: /dev/drbd{minor} (from in-memory state)
2. Check if already formatted:
   a. Temporarily mount /dev/drbd{minor} at /mnt/users/{user_id}
   b. Check if workspace subvolume exists
   c. If yes: unmount, return 200 with already_formatted=true
3. If not formatted:
   a. mkfs.btrfs -f /dev/drbd{minor}
   b. mkdir -p /mnt/users/{user_id}
   c. mount /dev/drbd{minor} /mnt/users/{user_id}
4. btrfs subvolume create /mnt/users/{user_id}/workspace
5. mkdir -p /mnt/users/{user_id}/workspace/memory
6. mkdir -p /mnt/users/{user_id}/workspace/apps
7. mkdir -p /mnt/users/{user_id}/workspace/data
8. echo '{"created":"<timestamp>","user":"<user_id>"}' > /mnt/users/{user_id}/workspace/data/config.json
9. mkdir -p /mnt/users/{user_id}/snapshots
10. btrfs subvolume snapshot -r /mnt/users/{user_id}/workspace /mnt/users/{user_id}/snapshots/layer-000
11. umount /mnt/users/{user_id}
12. Return 200
```

**Critical:** Step 11 unmounts. The host does NOT keep Btrfs mounted. The container will mount its own subvolume internally via the device-mount pattern.

### POST /containers/{user_id}/start

Starts a container using the device-mount pattern. The container receives the DRBD block device, mounts the workspace subvolume internally, and drops to an unprivileged user.

**Prerequisite:** DRBD must be Primary. Btrfs must be formatted (workspace subvolume must exist).

Implementation:
```
1. Check if container {user_id}-agent already running
   → If yes: return 200 with already_existed=true
2. Determine DRBD device path: /dev/drbd{minor} (from in-memory state)
3. docker run -d \
     --name {user_id}-agent \
     --device /dev/drbd{minor} \
     --cap-drop ALL \
     --cap-add SYS_ADMIN \
     --cap-add SETUID \
     --cap-add SETGID \
     --network none \
     --memory 64m \
     -e BLOCK_DEVICE=/dev/drbd{minor} \
     -e SUBVOL_NAME=workspace \
     platform/app-container
4. Wait 2 seconds, verify container is running: docker inspect
5. If not running: return 500 with container logs (docker logs {user_id}-agent)
6. Update in-memory state
7. Return 200 with container_name
```

### POST /containers/{user_id}/stop

Stops and removes container.

Implementation:
```
1. Check if container {user_id}-agent exists
   → If not: return 200 (idempotent — already stopped/removed)
2. docker stop {user_id}-agent --time 10
3. docker rm {user_id}-agent
4. Update in-memory state
5. Return 200
```

### GET /containers/{user_id}/status

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

If container doesn't exist: `{"exists": false, "running": false}`.

### POST /cleanup

Tears down ALL user resources on this machine. Used by the test harness between test phases.

Implementation:
```
1. List all running containers matching *-agent → stop and rm each
2. Unmount all /mnt/users/* mounts
3. For each DRBD resource (from /etc/drbd.d/user-*.res): drbdadm down
4. rm /etc/drbd.d/user-*.res
5. For each loop device attached to /data/images/*.img: losetup -d
6. rm -f /data/images/*.img
7. rm -rf /mnt/users/*
8. Clear in-memory state
9. Return 200
```

---

## HTTP Response Contract

All endpoints follow the same contract:

| Status | Meaning |
|--------|---------|
| 200 | Success. Includes `already_existed: true` if the operation was a no-op because the resource was already in the desired state. |
| 400 | Bad request. Missing parameters, invalid user_id format. |
| 500 | Operation failed. Body includes error details. |

Error response format:
```json
{
    "error": "drbdadm create-md failed",
    "details": "stderr: Device size too small",
    "command": "drbdadm create-md --force user-alice",
    "exit_code": 1
}
```

**Idempotency is the core principle.** The caller (test harness now, coordinator later) may call any endpoint multiple times. The endpoint must always produce the correct result without side effects on repeated calls.

---

## Structured Logging

The machine agent uses Go 1.21+ `log/slog` with JSON output:

```json
{"time":"2026-02-28T12:00:00Z","level":"INFO","msg":"Image created","component":"images","user":"alice","loop_device":"/dev/loop0"}
{"time":"2026-02-28T12:00:00Z","level":"ERROR","msg":"DRBD promote failed","component":"drbd","user":"alice","error":"exit code 11","stderr":"..."}
```

Every log entry includes `component` (images, drbd, btrfs, containers, cleanup) and `user` when applicable.

---

## Scripts

### scripts/run.sh — Main Entry Point

```bash
#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

source "$SCRIPT_DIR/common.sh"

echo "=== LAYER 4.1: MACHINE AGENT PoC ==="
echo "Started: $(date)"

# ── Phase 0: Infrastructure ──
echo ""
echo "Phase 0: Infrastructure Setup"

"$SCRIPT_DIR/infra.sh" up

wait_for_cloud_init

load_ips

cd "$PROJECT_DIR"
make build
"$SCRIPT_DIR/deploy.sh"

# Start machine agents
for ip in $MACHINE1_IP $MACHINE2_IP; do
    ssh -o StrictHostKeyChecking=no root@$ip "systemctl start machine-agent"
done

# Wait for agents to be ready
sleep 5

# Verify agents are responding
for ip in $MACHINE1_IP $MACHINE2_IP; do
    curl -sf "http://$ip:8080/status" > /dev/null || { echo "ERROR: Agent on $ip not responding"; exit 1; }
done

phase_result 0

# ── Run test suite ──
"$SCRIPT_DIR/test_suite.sh"

# ── Teardown ──
echo ""
echo "Tearing down infrastructure..."
"$SCRIPT_DIR/infra.sh" down

echo ""
echo "=== LAYER 4.1 COMPLETE ==="
echo "Finished: $(date)"
```

### scripts/common.sh — Shared Functions

```bash
#!/bin/bash
# common.sh — shared functions sourced by all scripts

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
IP_FILE="$SCRIPT_DIR/.ips"

NETWORK_NAME="poc41-net"
SSH_KEY_NAME="poc41"
LOCATION="nbg1"
SERVER_TYPE="cx23"
IMAGE="ubuntu-24.04"

# Check counters
TOTAL_CHECKS=0
TOTAL_PASSED=0
PHASE_CHECKS=0
PHASE_PASSED=0

save_ips() {
    echo "Discovering machine IPs..."
    cat > "$IP_FILE" << EOF
MACHINE1_IP=$(hcloud server ip poc41-machine-1)
MACHINE1_PRIV=$(hcloud server describe poc41-machine-1 -o json | jq -r '.private_net[0].ip')
MACHINE2_IP=$(hcloud server ip poc41-machine-2)
MACHINE2_PRIV=$(hcloud server describe poc41-machine-2 -o json | jq -r '.private_net[0].ip')
EOF
    echo "IPs saved to $IP_FILE"
    cat "$IP_FILE"
}

load_ips() {
    if [ ! -f "$IP_FILE" ]; then
        echo "ERROR: IP file not found. Run infra.sh up first."
        exit 1
    fi
    source "$IP_FILE"
    export MACHINE1_IP MACHINE1_PRIV MACHINE2_IP MACHINE2_PRIV
}

wait_for_cloud_init() {
    load_ips
    echo "Waiting for cloud-init to complete..."
    for ip in $MACHINE1_IP $MACHINE2_IP; do
        for attempt in $(seq 1 60); do
            if ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 root@$ip \
                "cloud-init status --wait" 2>/dev/null | grep -q "done"; then
                echo "  $ip: cloud-init complete"
                break
            fi
            if [ "$attempt" -eq 60 ]; then
                echo "ERROR: cloud-init timeout on $ip"
                exit 1
            fi
            sleep 5
        done
    done
}

# Test framework
check() {
    local description="$1"
    local test_cmd="$2"
    TOTAL_CHECKS=$((TOTAL_CHECKS + 1))
    PHASE_CHECKS=$((PHASE_CHECKS + 1))
    if eval "$test_cmd" 2>/dev/null; then
        echo "  ✓ $description"
        TOTAL_PASSED=$((TOTAL_PASSED + 1))
        PHASE_PASSED=$((PHASE_PASSED + 1))
    else
        echo "  ✗ FAILED: $description"
    fi
}

phase_start() {
    PHASE_CHECKS=0
    PHASE_PASSED=0
    echo ""
    echo "Phase $1: $2"
}

phase_result() {
    echo "  [$PHASE_PASSED/$PHASE_CHECKS checks passed]"
}

final_result() {
    echo ""
    echo "═══════════════════════════════════════════════════"
    echo "ALL PHASES COMPLETE: $TOTAL_PASSED/$TOTAL_CHECKS checks passed"
    echo "═══════════════════════════════════════════════════"
    if [ "$TOTAL_PASSED" -ne "$TOTAL_CHECKS" ]; then
        exit 1
    fi
}

# API helpers
api() {
    local machine_ip="$1"
    local method="$2"
    local path="$3"
    local body="${4:-}"
    if [ -n "$body" ]; then
        curl -sf -X "$method" "http://$machine_ip:8080$path" \
            -H "Content-Type: application/json" \
            -d "$body"
    else
        curl -sf -X "$method" "http://$machine_ip:8080$path"
    fi
}

ssh_cmd() {
    local ip="$1"
    shift
    ssh -o StrictHostKeyChecking=no root@"$ip" "$@"
}

docker_exec() {
    local ip="$1"
    local container="$2"
    shift 2
    ssh_cmd "$ip" "docker exec $container $*"
}
```

### scripts/infra.sh — Hetzner Lifecycle

```bash
#!/bin/bash
set -euo pipefail

source "$(dirname "$0")/common.sh"

MACHINES=("poc41-machine-1" "poc41-machine-2")

case "${1:-}" in
    up)
        # Create SSH key if needed
        if ! hcloud ssh-key describe "$SSH_KEY_NAME" &>/dev/null; then
            hcloud ssh-key create --name "$SSH_KEY_NAME" \
                --public-key-from-file ~/.ssh/id_ed25519.pub
        fi

        # Create network
        if ! hcloud network describe "$NETWORK_NAME" &>/dev/null; then
            hcloud network create --name "$NETWORK_NAME" --ip-range "10.0.0.0/24"
            hcloud network add-subnet "$NETWORK_NAME" \
                --ip-range "10.0.0.0/24" --type cloud --network-zone eu-central
        fi

        # Create machines
        for name in "${MACHINES[@]}"; do
            if hcloud server describe "$name" &>/dev/null; then
                echo "$name already exists, skipping..."
                continue
            fi

            hcloud server create \
                --name "$name" \
                --type "$SERVER_TYPE" \
                --image "$IMAGE" \
                --location "$LOCATION" \
                --ssh-key "$SSH_KEY_NAME" \
                --network "$NETWORK_NAME" \
                --user-data-from-file "$(dirname "$0")/cloud-init/fleet.yaml"

            echo "Created $name"
        done

        echo "Waiting for servers to be ready..."
        sleep 10

        save_ips
        ;;

    down)
        for name in "${MACHINES[@]}"; do
            hcloud server delete "$name" 2>/dev/null && echo "Deleted $name" || true
        done
        hcloud network delete "$NETWORK_NAME" 2>/dev/null && echo "Deleted network" || true
        hcloud ssh-key delete "$SSH_KEY_NAME" 2>/dev/null && echo "Deleted SSH key" || true
        rm -f "$IP_FILE"
        ;;

    status)
        for name in "${MACHINES[@]}"; do
            hcloud server describe "$name" \
                -o format='{{.Name}}: {{.Status}} (pub={{.PublicNet.IPv4.IP}})' \
                2>/dev/null || echo "$name: not found"
        done
        ;;

    *)
        echo "Usage: $0 {up|down|status}"
        exit 1
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
  - docker-buildx
  - xfsprogs
  - curl
  - jq
  - kmod
  - software-properties-common

runcmd:
  # DRBD 9 from LINBIT PPA (Ubuntu 24.04 ships DRBD 8.4 in-tree, incompatible with drbd-utils 9.x)
  - add-apt-repository -y ppa:linbit/linbit-drbd9-stack
  - apt install -y drbd-dkms linux-headers-$(uname -r)
  # Pre-seed postfix to avoid interactive prompt during drbd-utils install
  - |
    echo "postfix postfix/main_mailer_type select No configuration" | debconf-set-selections
    echo "postfix postfix/mailname string localhost" | debconf-set-selections
  - DEBIAN_FRONTEND=noninteractive apt install -y drbd-utils
  - dkms autoinstall
  - modprobe drbd

  # Docker
  - systemctl enable docker
  - systemctl start docker
  # Pre-pull alpine so container builds don't need to download at test time
  - docker pull alpine:latest

  # Storage directories
  - mkdir -p /data/images /mnt/users

  # Machine agent systemd service (binary + container files deployed separately)
  - |
    cat > /etc/systemd/system/machine-agent.service << 'EOF'
    [Unit]
    Description=Platform Machine Agent
    After=network.target docker.service
    Requires=docker.service
    [Service]
    Type=simple
    ExecStart=/usr/local/bin/machine-agent
    Environment=NODE_ID=PLACEHOLDER_NODE_ID
    Environment=LISTEN_ADDR=0.0.0.0:8080
    Environment=DATA_DIR=/data
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

### scripts/deploy.sh

```bash
#!/bin/bash
set -euo pipefail

source "$(dirname "$0")/common.sh"
load_ips

DEPLOY_CONFIGS=(
    "$MACHINE1_IP:machine-1"
    "$MACHINE2_IP:machine-2"
)

for config in "${DEPLOY_CONFIGS[@]}"; do
    IFS=: read -r pub_ip node_id <<< "$config"

    echo "Deploying to $node_id ($pub_ip)..."

    # Copy binary
    scp -o StrictHostKeyChecking=no \
        "$PROJECT_DIR/bin/machine-agent" root@$pub_ip:/usr/local/bin/

    # Copy container files
    ssh_cmd "$pub_ip" "mkdir -p /opt/platform/container"
    scp -o StrictHostKeyChecking=no \
        "$PROJECT_DIR/container/Dockerfile" \
        "$PROJECT_DIR/container/container-init.sh" \
        root@$pub_ip:/opt/platform/container/

    # Configure systemd with actual node ID
    ssh_cmd "$pub_ip" "
        sed -i 's/PLACEHOLDER_NODE_ID/$node_id/' /etc/systemd/system/machine-agent.service
        systemctl daemon-reload
    "

    # Set hostname (DRBD matches config on blocks by hostname)
    ssh_cmd "$pub_ip" "hostnamectl set-hostname $node_id"

    # Build container image
    ssh_cmd "$pub_ip" "cd /opt/platform/container && docker build -t platform/app-container ."

    echo "  $node_id deployed."
done

echo "Deploy complete."
```

### scripts/test_suite.sh

```bash
#!/bin/bash
set -euo pipefail

source "$(dirname "$0")/common.sh"
load_ips

# ══════════════════════════════════════════════════════════════
# Phase 0: Prerequisites
# ══════════════════════════════════════════════════════════════
phase_start 0 "Prerequisites"

check "Machine-1 reachable via SSH" \
    'ssh_cmd "$MACHINE1_IP" "true"'

check "Machine-2 reachable via SSH" \
    'ssh_cmd "$MACHINE2_IP" "true"'

check "DRBD module loaded on machine-1" \
    'ssh_cmd "$MACHINE1_IP" "lsmod | grep -q drbd"'

check "DRBD module loaded on machine-2" \
    'ssh_cmd "$MACHINE2_IP" "lsmod | grep -q drbd"'

check "Machine agent responding on machine-1" \
    'api "$MACHINE1_IP" GET /status | jq -e .machine_id'

check "Machine agent responding on machine-2" \
    'api "$MACHINE2_IP" GET /status | jq -e .machine_id'

check "Container image built on machine-1" \
    'ssh_cmd "$MACHINE1_IP" "docker images platform/app-container --format={{.Repository}}" | grep -q platform/app-container'

check "Container image built on machine-2" \
    'ssh_cmd "$MACHINE2_IP" "docker images platform/app-container --format={{.Repository}}" | grep -q platform/app-container'

phase_result

# ══════════════════════════════════════════════════════════════
# Phase 1: Single User Provisioning — Full Stack
# ══════════════════════════════════════════════════════════════
phase_start 1 "Single User Provisioning — Full Stack"

# Create images
M1_LOOP=$(api "$MACHINE1_IP" POST /images/alice/create '{"image_size_mb": 512}' | jq -r .loop_device)
M2_LOOP=$(api "$MACHINE2_IP" POST /images/alice/create '{"image_size_mb": 512}' | jq -r .loop_device)

check "Image created on machine-1 (loop=$M1_LOOP)" '[ -n "$M1_LOOP" ] && [ "$M1_LOOP" != "null" ]'
check "Image created on machine-2 (loop=$M2_LOOP)" '[ -n "$M2_LOOP" ] && [ "$M2_LOOP" != "null" ]'

check "[SSH] Image file exists on machine-1" \
    'ssh_cmd "$MACHINE1_IP" "test -f /data/images/alice.img"'
check "[SSH] Image file exists on machine-2" \
    'ssh_cmd "$MACHINE2_IP" "test -f /data/images/alice.img"'

# Configure DRBD
DRBD_CONFIG="{
    \"resource_name\": \"user-alice\",
    \"nodes\": [
        {\"hostname\": \"machine-1\", \"minor\": 0, \"disk\": \"$M1_LOOP\", \"address\": \"$MACHINE1_PRIV\"},
        {\"hostname\": \"machine-2\", \"minor\": 0, \"disk\": \"$M2_LOOP\", \"address\": \"$MACHINE2_PRIV\"}
    ],
    \"port\": 7900
}"

api "$MACHINE1_IP" POST /images/alice/drbd/create "$DRBD_CONFIG" > /dev/null
api "$MACHINE2_IP" POST /images/alice/drbd/create "$DRBD_CONFIG" > /dev/null

check "DRBD configured on machine-1" \
    'ssh_cmd "$MACHINE1_IP" "test -f /etc/drbd.d/user-alice.res"'
check "DRBD configured on machine-2" \
    'ssh_cmd "$MACHINE2_IP" "test -f /etc/drbd.d/user-alice.res"'

# Wait for DRBD sync
echo "  Waiting for DRBD sync..."
for attempt in $(seq 1 30); do
    PEER_STATE=$(api "$MACHINE1_IP" GET /images/alice/drbd/status | jq -r .peer_disk_state)
    if [ "$PEER_STATE" = "UpToDate" ]; then break; fi
    sleep 2
done

check "DRBD synced (UpToDate/UpToDate)" '[ "$PEER_STATE" = "UpToDate" ]'

# Promote
api "$MACHINE1_IP" POST /images/alice/drbd/promote > /dev/null
check "[SSH] DRBD role is Primary on machine-1" \
    'ssh_cmd "$MACHINE1_IP" "drbdadm status user-alice" | head -1 | grep -q "role:Primary"'

# Format Btrfs
api "$MACHINE1_IP" POST /images/alice/format-btrfs > /dev/null
check "[SSH] Host does NOT have alice mounted after format" \
    '! ssh_cmd "$MACHINE1_IP" "mountpoint -q /mnt/users/alice"'

# Start container (device-mount)
api "$MACHINE1_IP" POST /containers/alice/start > /dev/null
check "[SSH] Container alice-agent is running" \
    '[ "$(ssh_cmd "$MACHINE1_IP" "docker inspect alice-agent 2>/dev/null | jq -r .[0].State.Running")" = "true" ]'

phase_result

# ══════════════════════════════════════════════════════════════
# Phase 2: Device-Mount Verification
# ══════════════════════════════════════════════════════════════
phase_start 2 "Device-Mount Verification"

check "[container] /workspace is a mount point" \
    'docker_exec "$MACHINE1_IP" alice-agent mountpoint -q /workspace'

check "[container] Seed data readable (config.json)" \
    'docker_exec "$MACHINE1_IP" alice-agent cat /workspace/data/config.json | jq -e .created'

check "[container] Running as appuser (not root)" \
    '[ "$(docker_exec "$MACHINE1_IP" alice-agent id -un)" = "appuser" ]'

check "[container] /proc/mounts has no host paths" \
    'MOUNTS=$(docker_exec "$MACHINE1_IP" alice-agent cat /proc/mounts);
     echo "$MOUNTS" | grep -q "/workspace" &&
     ! echo "$MOUNTS" | grep -q "/mnt/users" &&
     ! echo "$MOUNTS" | grep -q "/data/images"'

check "[SSH] Host has NO Btrfs mount for alice" \
    '! ssh_cmd "$MACHINE1_IP" "mount | grep /mnt/users/alice"'

phase_result

# ══════════════════════════════════════════════════════════════
# Phase 3: Data Write + DRBD Replication
# ══════════════════════════════════════════════════════════════
phase_start 3 "Data Write + DRBD Replication"

docker_exec "$MACHINE1_IP" alice-agent sh -c "'echo hello-from-alice > /workspace/data/test.txt'"

check "[container] Data written and readable" \
    '[ "$(docker_exec "$MACHINE1_IP" alice-agent cat /workspace/data/test.txt)" = "hello-from-alice" ]'

check "[SSH] DRBD connection is Connected" \
    'ssh_cmd "$MACHINE1_IP" "drbdadm status user-alice" | grep -q "role:Secondary"'

check "[SSH] DRBD peer disk is UpToDate (write replicated)" \
    'api "$MACHINE1_IP" GET /images/alice/drbd/status | jq -r .peer_disk_state | grep -q UpToDate'

# Give DRBD a moment to replicate (Protocol A is async)
sleep 2

check "DRBD status via API confirms healthy bipod" \
    'STATUS=$(api "$MACHINE1_IP" GET /images/alice/drbd/status);
     [ "$(echo "$STATUS" | jq -r .role)" = "Primary" ] &&
     [ "$(echo "$STATUS" | jq -r .disk_state)" = "UpToDate" ] &&
     [ "$(echo "$STATUS" | jq -r .peer_disk_state)" = "UpToDate" ]'

phase_result

# ══════════════════════════════════════════════════════════════
# Phase 4: Failover via API
# ══════════════════════════════════════════════════════════════
phase_start 4 "Failover via API"

# Stop container on machine-1
api "$MACHINE1_IP" POST /containers/alice/stop > /dev/null
check "Container stopped on machine-1" \
    '[ "$(ssh_cmd "$MACHINE1_IP" "docker inspect alice-agent 2>/dev/null | jq -r .[0].State.Running" 2>/dev/null)" != "true" ]'

# Demote machine-1
api "$MACHINE1_IP" POST /images/alice/drbd/demote > /dev/null
check "Machine-1 demoted to Secondary" \
    'ssh_cmd "$MACHINE1_IP" "drbdadm status user-alice" | head -1 | grep -q "role:Secondary"'

# Promote machine-2
api "$MACHINE2_IP" POST /images/alice/drbd/promote > /dev/null
check "Machine-2 promoted to Primary" \
    'ssh_cmd "$MACHINE2_IP" "drbdadm status user-alice" | head -1 | grep -q "role:Primary"'

# Start container on machine-2 (device-mount — no host mount needed)
api "$MACHINE2_IP" POST /containers/alice/start > /dev/null
check "Container running on machine-2" \
    '[ "$(ssh_cmd "$MACHINE2_IP" "docker inspect alice-agent 2>/dev/null | jq -r .[0].State.Running")" = "true" ]'

# Data survived failover
check "[container m2] Data survived failover" \
    '[ "$(docker_exec "$MACHINE2_IP" alice-agent cat /workspace/data/test.txt)" = "hello-from-alice" ]'

# Config.json from provisioning survived
check "[container m2] Seed data survived failover" \
    'docker_exec "$MACHINE2_IP" alice-agent cat /workspace/data/config.json | jq -e .created'

# Device-mount pattern preserved on failover
check "[container m2] /proc/mounts clean (no host paths)" \
    'MOUNTS=$(docker_exec "$MACHINE2_IP" alice-agent cat /proc/mounts);
     echo "$MOUNTS" | grep -q "/workspace" &&
     ! echo "$MOUNTS" | grep -q "/mnt/users" &&
     ! echo "$MOUNTS" | grep -q "/data/images"'

check "[container m2] Running as appuser" \
    '[ "$(docker_exec "$MACHINE2_IP" alice-agent id -un)" = "appuser" ]'

phase_result

# ══════════════════════════════════════════════════════════════
# Phase 5: Idempotency Tests
# ══════════════════════════════════════════════════════════════
phase_start 5 "Idempotency Tests"

# Call start on already-running container
check "Start on already-running container → 200" \
    'api "$MACHINE2_IP" POST /containers/alice/start | jq -e .already_existed'

# Call promote on already-Primary
check "Promote on already-Primary → 200" \
    'api "$MACHINE2_IP" POST /images/alice/drbd/promote | jq -e .already_existed'

# Call demote on already-Secondary
check "Demote on already-Secondary → 200" \
    'api "$MACHINE1_IP" POST /images/alice/drbd/demote | jq -e .already_existed'

# Call create on already-existing image
check "Create image that already exists → 200" \
    'api "$MACHINE1_IP" POST /images/alice/create "{\"image_size_mb\": 512}" | jq -e .already_existed'

# Stop, then stop again
api "$MACHINE2_IP" POST /containers/alice/stop > /dev/null
check "Stop already-stopped container → 200" \
    'api "$MACHINE2_IP" POST /containers/alice/stop > /dev/null'

# Delete non-existent user
check "Delete non-existent user → 200" \
    'api "$MACHINE1_IP" DELETE /images/nonexistent > /dev/null'

# DRBD create with same config (already exists)
check "DRBD create with existing resource → 200" \
    'api "$MACHINE1_IP" POST /images/alice/drbd/create "$DRBD_CONFIG" | jq -e .already_existed'

# Format already-formatted filesystem
# (need to re-promote machine-1 briefly to test format idempotency)
api "$MACHINE2_IP" POST /images/alice/drbd/demote > /dev/null
api "$MACHINE1_IP" POST /images/alice/drbd/promote > /dev/null
check "Format on already-formatted → 200 with already_formatted" \
    'api "$MACHINE1_IP" POST /images/alice/format-btrfs | jq -e .already_formatted'

phase_result

# ══════════════════════════════════════════════════════════════
# Phase 6: Full Teardown
# ══════════════════════════════════════════════════════════════
phase_start 6 "Full Teardown"

api "$MACHINE1_IP" DELETE /images/alice > /dev/null
api "$MACHINE2_IP" DELETE /images/alice > /dev/null

check "[SSH m1] No images" \
    '[ -z "$(ssh_cmd "$MACHINE1_IP" "ls /data/images/*.img 2>/dev/null")" ]'
check "[SSH m1] No loop devices" \
    '! ssh_cmd "$MACHINE1_IP" "losetup -a | grep /data/images/"'
check "[SSH m1] No DRBD resources" \
    '[ -z "$(ssh_cmd "$MACHINE1_IP" "ls /etc/drbd.d/user-*.res 2>/dev/null")" ]'
check "[SSH m1] No containers" \
    '[ -z "$(ssh_cmd "$MACHINE1_IP" "docker ps -q --filter name=-agent")" ]'

check "[SSH m2] No images" \
    '[ -z "$(ssh_cmd "$MACHINE2_IP" "ls /data/images/*.img 2>/dev/null")" ]'
check "[SSH m2] No loop devices" \
    '! ssh_cmd "$MACHINE2_IP" "losetup -a | grep /data/images/"'
check "[SSH m2] No DRBD resources" \
    '[ -z "$(ssh_cmd "$MACHINE2_IP" "ls /etc/drbd.d/user-*.res 2>/dev/null")" ]'
check "[SSH m2] No containers" \
    '[ -z "$(ssh_cmd "$MACHINE2_IP" "docker ps -q --filter name=-agent")" ]'

phase_result

# ══════════════════════════════════════════════════════════════
# Phase 7: Multi-User Density
# ══════════════════════════════════════════════════════════════
phase_start 7 "Multi-User Density (3 users on same bipod)"

# Provision 3 users
USERS=("alice:0:7900" "bob:1:7901" "charlie:2:7902")
declare -A USER_M1_LOOP USER_M2_LOOP

for entry in "${USERS[@]}"; do
    IFS=: read -r user minor port <<< "$entry"

    # Create images
    USER_M1_LOOP[$user]=$(api "$MACHINE1_IP" POST /images/$user/create '{"image_size_mb": 512}' | jq -r .loop_device)
    USER_M2_LOOP[$user]=$(api "$MACHINE2_IP" POST /images/$user/create '{"image_size_mb": 512}' | jq -r .loop_device)

    # Configure DRBD
    CONFIG="{
        \"resource_name\": \"user-$user\",
        \"nodes\": [
            {\"hostname\": \"machine-1\", \"minor\": $minor, \"disk\": \"${USER_M1_LOOP[$user]}\", \"address\": \"$MACHINE1_PRIV\"},
            {\"hostname\": \"machine-2\", \"minor\": $minor, \"disk\": \"${USER_M2_LOOP[$user]}\", \"address\": \"$MACHINE2_PRIV\"}
        ],
        \"port\": $port
    }"
    api "$MACHINE1_IP" POST /images/$user/drbd/create "$CONFIG" > /dev/null
    api "$MACHINE2_IP" POST /images/$user/drbd/create "$CONFIG" > /dev/null
done

# Wait for all DRBD syncs
echo "  Waiting for DRBD sync on all 3 resources..."
sleep 10
for entry in "${USERS[@]}"; do
    IFS=: read -r user _ _ <<< "$entry"
    for attempt in $(seq 1 30); do
        PEER=$(api "$MACHINE1_IP" GET /images/$user/drbd/status | jq -r .peer_disk_state)
        if [ "$PEER" = "UpToDate" ]; then break; fi
        sleep 2
    done
done

# Promote and provision all
for entry in "${USERS[@]}"; do
    IFS=: read -r user _ _ <<< "$entry"
    api "$MACHINE1_IP" POST /images/$user/drbd/promote > /dev/null
    api "$MACHINE1_IP" POST /images/$user/format-btrfs > /dev/null
    api "$MACHINE1_IP" POST /containers/$user/start > /dev/null
done

check "All 3 containers running" \
    '[ "$(ssh_cmd "$MACHINE1_IP" "docker ps --filter name=-agent --format={{.Names}}" | wc -l)" -eq 3 ]'

check "[SSH] 3 DRBD resources active" \
    '[ "$(ssh_cmd "$MACHINE1_IP" "ls /etc/drbd.d/user-*.res | wc -l")" -eq 3 ]'

check "All 3 DRBD resources UpToDate" \
    'for entry in "${USERS[@]}"; do
        IFS=: read -r user _ _ <<< "$entry"
        STATE=$(api "$MACHINE1_IP" GET /images/$user/drbd/status | jq -r .peer_disk_state)
        [ "$STATE" = "UpToDate" ] || exit 1
    done'

# Write unique data to each user
docker_exec "$MACHINE1_IP" alice-agent sh -c "'echo alice-data > /workspace/data/identity.txt'"
docker_exec "$MACHINE1_IP" bob-agent sh -c "'echo bob-data > /workspace/data/identity.txt'"
docker_exec "$MACHINE1_IP" charlie-agent sh -c "'echo charlie-data > /workspace/data/identity.txt'"

# Isolation: each user sees only their own data
check "[container alice] Sees only alice-data" \
    '[ "$(docker_exec "$MACHINE1_IP" alice-agent cat /workspace/data/identity.txt)" = "alice-data" ]'
check "[container bob] Sees only bob-data" \
    '[ "$(docker_exec "$MACHINE1_IP" bob-agent cat /workspace/data/identity.txt)" = "bob-data" ]'
check "[container charlie] Sees only charlie-data" \
    '[ "$(docker_exec "$MACHINE1_IP" charlie-agent cat /workspace/data/identity.txt)" = "charlie-data" ]'

# Metadata isolation: no cross-user leakage in /proc/mounts
check "[container alice] /proc/mounts has no bob or charlie" \
    'MOUNTS=$(docker_exec "$MACHINE1_IP" alice-agent cat /proc/mounts);
     ! echo "$MOUNTS" | grep -q "bob" &&
     ! echo "$MOUNTS" | grep -q "charlie"'

# Resource independence: stopping one user's DRBD doesn't affect others
api "$MACHINE1_IP" POST /containers/alice/stop > /dev/null
check "Bob container still running after alice stopped" \
    '[ "$(ssh_cmd "$MACHINE1_IP" "docker inspect bob-agent 2>/dev/null | jq -r .[0].State.Running")" = "true" ]'
check "Charlie container still running after alice stopped" \
    '[ "$(ssh_cmd "$MACHINE1_IP" "docker inspect charlie-agent 2>/dev/null | jq -r .[0].State.Running")" = "true" ]'

phase_result

# ══════════════════════════════════════════════════════════════
# Phase 8: Status Endpoint Accuracy
# ══════════════════════════════════════════════════════════════
phase_start 8 "Status Endpoint Accuracy"

# Bob and charlie still running from Phase 7
STATUS=$(api "$MACHINE1_IP" GET /status)

check "Status shows bob as running" \
    'echo "$STATUS" | jq -e ".users.bob.container_running == true"'

check "Status shows charlie as running" \
    'echo "$STATUS" | jq -e ".users.charlie.container_running == true"'

check "Status shows alice as NOT running (stopped in Phase 7)" \
    'echo "$STATUS" | jq -e ".users.alice.container_running == false"'

check "Status shows alice image still exists" \
    'echo "$STATUS" | jq -e ".users.alice.image_exists == true"'

check "Status shows bob DRBD as Primary" \
    'echo "$STATUS" | jq -e ".users.bob.drbd_role == \"Primary\""'

# Full cleanup
api "$MACHINE1_IP" POST /cleanup > /dev/null
api "$MACHINE2_IP" POST /cleanup > /dev/null

STATUS_CLEAN=$(api "$MACHINE1_IP" GET /status)
check "Status shows no users after cleanup" \
    '[ "$(echo "$STATUS_CLEAN" | jq ".users | length")" -eq 0 ]'

phase_result

# ══════════════════════════════════════════════════════════════
# Summary
# ══════════════════════════════════════════════════════════════
final_result
```

---

## Expected Test Output

```
=== LAYER 4.1: MACHINE AGENT PoC ===

Phase 0: Prerequisites
  ✓ Machine-1 reachable via SSH
  ✓ Machine-2 reachable via SSH
  ✓ DRBD module loaded on machine-1
  ✓ DRBD module loaded on machine-2
  ✓ Machine agent responding on machine-1
  ✓ Machine agent responding on machine-2
  ✓ Container image built on machine-1
  ✓ Container image built on machine-2
  [8/8 checks passed]

Phase 1: Single User Provisioning — Full Stack
  ✓ Image created on machine-1 (loop=/dev/loop0)
  ✓ Image created on machine-2 (loop=/dev/loop0)
  ✓ [SSH] Image file exists on machine-1
  ✓ [SSH] Image file exists on machine-2
  ✓ DRBD configured on machine-1
  ✓ DRBD configured on machine-2
  ✓ DRBD synced (UpToDate/UpToDate)
  ✓ [SSH] DRBD role is Primary on machine-1
  ✓ [SSH] Host does NOT have alice mounted after format
  ✓ [SSH] Container alice-agent is running
  [10/10 checks passed]

Phase 2: Device-Mount Verification
  ✓ [container] /workspace is a mount point
  ✓ [container] Seed data readable (config.json)
  ✓ [container] Running as appuser (not root)
  ✓ [container] /proc/mounts has no host paths
  ✓ [SSH] Host has NO Btrfs mount for alice
  [5/5 checks passed]

Phase 3: Data Write + DRBD Replication
  ✓ [container] Data written and readable
  ✓ [SSH] DRBD connection is Connected
  ✓ [SSH] DRBD peer disk is UpToDate (write replicated)
  ✓ DRBD status via API confirms healthy bipod
  [4/4 checks passed]

Phase 4: Failover via API
  ✓ Container stopped on machine-1
  ✓ Machine-1 demoted to Secondary
  ✓ Machine-2 promoted to Primary
  ✓ Container running on machine-2
  ✓ [container m2] Data survived failover
  ✓ [container m2] Seed data survived failover
  ✓ [container m2] /proc/mounts clean (no host paths)
  ✓ [container m2] Running as appuser
  [8/8 checks passed]

Phase 5: Idempotency Tests
  ✓ Start on already-running container → 200
  ✓ Promote on already-Primary → 200
  ✓ Demote on already-Secondary → 200
  ✓ Create image that already exists → 200
  ✓ Stop already-stopped container → 200
  ✓ Delete non-existent user → 200
  ✓ DRBD create with existing resource → 200
  ✓ Format on already-formatted → 200 with already_formatted
  [8/8 checks passed]

Phase 6: Full Teardown
  ✓ [SSH m1] No images
  ✓ [SSH m1] No loop devices
  ✓ [SSH m1] No DRBD resources
  ✓ [SSH m1] No containers
  ✓ [SSH m2] No images
  ✓ [SSH m2] No loop devices
  ✓ [SSH m2] No DRBD resources
  ✓ [SSH m2] No containers
  [8/8 checks passed]

Phase 7: Multi-User Density (3 users on same bipod)
  ✓ All 3 containers running
  ✓ [SSH] 3 DRBD resources active
  ✓ All 3 DRBD resources UpToDate
  ✓ [container alice] Sees only alice-data
  ✓ [container bob] Sees only bob-data
  ✓ [container charlie] Sees only charlie-data
  ✓ [container alice] /proc/mounts has no bob or charlie
  ✓ Bob container still running after alice stopped
  ✓ Charlie container still running after alice stopped
  [9/9 checks passed]

Phase 8: Status Endpoint Accuracy
  ✓ Status shows bob as running
  ✓ Status shows charlie as running
  ✓ Status shows alice as NOT running (stopped in Phase 7)
  ✓ Status shows alice image still exists
  ✓ Status shows bob DRBD as Primary
  ✓ Status shows no users after cleanup
  [6/6 checks passed]

═══════════════════════════════════════════════════
ALL PHASES COMPLETE: 66/66 checks passed
═══════════════════════════════════════════════════
```

---

## Key Implementation Notes

### 1. DRBD Config Must Be Identical on Both Machines

Both sides of a DRBD resource must have the exact same config file. The test harness constructs the full config body (both `on` blocks) once and sends it to both machine agents. Each agent writes the identical file to `/etc/drbd.d/user-{user_id}.res`.

DRBD determines which `on` block is local by matching the hostname. The deploy script sets each machine's hostname via `hostnamectl set-hostname` to match the `on` block names in the config.

### 2. DRBD Must Be Set Up Before the Filesystem

Proven in PoC 3 Patch 3.1. When using `meta-disk internal`, DRBD reserves space at the end of the backing device. If a filesystem already occupies the full device, `create-md` overwrites the filesystem's tail, corrupting it.

**The rule:** Always create an empty image → loop device → DRBD metadata → DRBD up → promote → `mkfs.btrfs` on `/dev/drbdN`. Never format before DRBD metadata is written.

This is enforced by the endpoint design: `POST /images/{id}/create` creates a blank image (no filesystem), and `POST /images/{id}/format-btrfs` only formats the DRBD device (not the raw image or loop device).

### 3. DRBD Minor Numbers Are Per-Machine

Each machine has its own kernel, so minor numbers start from 0 on each machine independently. The test harness specifies minors in the DRBD config body. For the multi-user test: alice=0, bob=1, charlie=2 on both machines.

### 4. Device-Mount Means No Host Mount During Normal Operation

After `format-btrfs` completes (which temporarily mounts on the host to create subvolumes, then unmounts), the host never mounts the user's Btrfs again. The container mounts its own subvolume internally via the init script. This means:

- The host's `/mnt/users/{user_id}` directory is empty after provisioning
- `mount` on the host shows no Btrfs mounts for any user during normal operation
- Failover does not require a host-side mount step — just promote DRBD and start the device-mount container

### 5. DRBD Status Parsing Must Be Defensive

DRBD 9 status output varies by connection state. The agent's parser must handle:
- Full format with peer section (Connected, syncing)
- Minimal format without peer section (Disconnected, StandAlone)
- Multi-resource output from `drbdadm status all`
- Peer role shown as `machine-2 role:Secondary` (hostname prefix, learned in PoC 3 Issue 23)

### 6. `drbdadm primary --force` for Initial Promotion

When a new DRBD resource is created and both sides have `Inconsistent` disk state (no initial sync yet), `drbdadm primary` without `--force` may fail because DRBD can't determine which side has valid data. `--force` tells DRBD "this side has the good data, sync from here." This flag is harmless when the peer is already UpToDate. Learned in PoC 2.

### 7. Container Init Script Must Handle Missing Subvolume

If the `format-btrfs` step didn't run (or the container is started before format), the `mount -o subvol=workspace` will fail because the subvolume doesn't exist. The container will exit with an error. The `start` endpoint verifies the container is actually running after a brief wait and returns 500 with container logs if it exited.
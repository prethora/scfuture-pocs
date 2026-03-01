# scfuture — Architecture Document

**Layer:** 4.1 — Machine Agent
**Module:** `scfuture` (Go 1.22, standard library only, no external dependencies)
**Status:** 66/66 test checks passing
**Last updated:** 2026-03-01

---

## 1. System Overview

scfuture is a per-machine HTTP agent that manages the full lifecycle of isolated user environments on a two-node (bipod) infrastructure. Each user gets:

1. A **sparse disk image** backed by a **loop device**
2. A **DRBD 9 replicated block device** (Protocol A, async) across two machines
3. A **Btrfs filesystem** with subvolumes and snapshots on the DRBD device
4. A **Docker container** that mounts the DRBD block device directly (device-mount pattern — the host never mounts Btrfs)

The agent runs on each machine independently. There is no coordinator yet (that is Layer 4.2). An external caller (test suite, future coordinator) drives both agents via their HTTP APIs.

### Key Design Decisions

- **Device-mount pattern:** Containers receive the raw `/dev/drbdN` device and mount Btrfs internally. The host never mounts the user's filesystem, eliminating host-path leakage in `/proc/mounts`.
- **Idempotent API:** Every endpoint returns success if the desired state already exists (`already_existed`, `already_formatted`). No endpoint fails on repeated calls.
- **Per-user locking:** Concurrent requests for different users proceed in parallel. Requests for the same user are serialized via `sync.Mutex` per user ID.
- **State discovery on startup:** The agent rebuilds its in-memory state from system reality (losetup, DRBD configs, DRBD status, mount table, Docker) rather than persisting state to disk.
- **Standard library only:** No external Go dependencies. No `go.sum` file.

---

## 2. Directory Structure

```
scfuture/
├── go.mod                                 # module scfuture, go 1.22
├── Makefile                               # build (linux/amd64), deploy, test, clean
├── .gitignore                             # bin/, scripts/.ips
├── architecture.md                        # this file
│
├── cmd/
│   └── machine-agent/
│       └── main.go                        # entrypoint — env config, discover, serve
│
├── internal/
│   ├── shared/
│   │   └── types.go                       # 13 API request/response types
│   └── machineagent/
│       ├── server.go                      # HTTP routing, handlers, helpers
│       ├── images.go                      # loop device image management
│       ├── drbd.go                        # DRBD lifecycle + status parsing
│       ├── btrfs.go                       # Btrfs format + provisioning
│       ├── containers.go                  # Docker container lifecycle
│       ├── state.go                       # in-memory state, discovery
│       ├── cleanup.go                     # per-user and full-machine teardown
│       └── exec.go                        # command execution helper
│
├── container/
│   ├── Dockerfile                         # alpine + btrfs-progs, appuser
│   └── container-init.sh                  # mount subvol, drop to appuser
│
└── scripts/
    ├── run.sh                             # full orchestration: infra → deploy → test → teardown
    ├── common.sh                          # shared functions, test framework, API helpers
    ├── infra.sh                           # Hetzner Cloud infra (up/down/status)
    ├── deploy.sh                          # scp binary + container files, configure systemd
    ├── test_suite.sh                      # 66 checks across 9 phases
    └── cloud-init/
        └── fleet.yaml                     # cloud-init: DRBD 9, Docker, storage dirs, systemd unit
```

---

## 3. Package: `internal/shared` — API Types

All types that cross the HTTP boundary live here. Field names and JSON tags are validated by the test suite via `jq`.

### `types.go` — 13 type definitions

```go
// ─── Image types ───
type ImageCreateRequest struct {
    ImageSizeMB int `json:"image_size_mb"`
}

type ImageCreateResponse struct {
    LoopDevice     string `json:"loop_device"`
    ImagePath      string `json:"image_path"`
    AlreadyExisted bool   `json:"already_existed"`
}

// ─── DRBD types ───
type DRBDNode struct {
    Hostname string `json:"hostname"`
    Minor    int    `json:"minor"`
    Disk     string `json:"disk"`
    Address  string `json:"address"`
}

type DRBDCreateRequest struct {
    ResourceName string     `json:"resource_name"`
    Nodes        []DRBDNode `json:"nodes"`
    Port         int        `json:"port"`
}

type DRBDCreateResponse struct {
    AlreadyExisted bool `json:"already_existed"`
}

type DRBDPromoteResponse struct {
    OK             bool `json:"ok,omitempty"`
    AlreadyExisted bool `json:"already_existed,omitempty"`
}

type DRBDDemoteResponse struct {
    OK             bool `json:"ok,omitempty"`
    AlreadyExisted bool `json:"already_existed,omitempty"`
}

type DRBDStatusResponse struct {
    Resource        string  `json:"resource"`
    Role            string  `json:"role"`
    ConnectionState string  `json:"connection_state"`
    DiskState       string  `json:"disk_state"`
    PeerDiskState   string  `json:"peer_disk_state"`
    SyncProgress    *string `json:"sync_progress"`
    Exists          bool    `json:"exists"`
}

// ─── Btrfs types ───
type FormatBtrfsResponse struct {
    AlreadyFormatted bool `json:"already_formatted"`
}

// ─── Container types ───
type ContainerStartResponse struct {
    ContainerName  string `json:"container_name"`
    AlreadyExisted bool   `json:"already_existed"`
}

type ContainerStatusResponse struct {
    Exists        bool   `json:"exists"`
    Running       bool   `json:"running"`
    ContainerName string `json:"container_name,omitempty"`
    StartedAt     string `json:"started_at,omitempty"`
}

// ─── Status types ───
type StatusResponse struct {
    MachineID   string                    `json:"machine_id"`
    DiskTotalMB int64                     `json:"disk_total_mb"`
    DiskUsedMB  int64                     `json:"disk_used_mb"`
    RAMTotalMB  int64                     `json:"ram_total_mb"`
    RAMUsedMB   int64                     `json:"ram_used_mb"`
    Users       map[string]*UserStatusDTO `json:"users"`
}

type UserStatusDTO struct {
    ImageExists      bool   `json:"image_exists"`
    ImagePath        string `json:"image_path"`
    LoopDevice       string `json:"loop_device"`
    DRBDResource     string `json:"drbd_resource"`
    DRBDMinor        int    `json:"drbd_minor"`
    DRBDDevice       string `json:"drbd_device"`
    DRBDRole         string `json:"drbd_role"`
    DRBDConnection   string `json:"drbd_connection"`
    DRBDDiskState    string `json:"drbd_disk_state"`
    DRBDPeerDisk     string `json:"drbd_peer_disk_state"`
    HostMounted      bool   `json:"host_mounted"`
    ContainerRunning bool   `json:"container_running"`
    ContainerName    string `json:"container_name"`
}
```

---

## 4. Package: `internal/machineagent` — Core Agent

### 4.1 Internal Types (not in shared)

```go
// state.go
type UserResources struct {
    ImagePath, LoopDevice                          string
    DRBDResource, DRBDDevice, DRBDRole             string
    DRBDConnection, DRBDDiskState, DRBDPeerDisk    string
    DRBDMinor                                      int
    HostMounted, ContainerRunning                  bool
    ContainerName                                  string
}

type Agent struct {
    nodeID  string
    dataDir string
    users   map[string]*UserResources   // guarded by usersMu
    usersMu sync.RWMutex
    locks   sync.Map                    // map[string]*sync.Mutex — per-user operation lock
}

// drbd.go
type DRBDInfo struct {
    Role, ConnectionState, DiskState, PeerDiskState string
    SyncProgress                                    *string
}

// exec.go
type CmdResult struct {
    Stdout   string `json:"stdout,omitempty"`
    Stderr   string `json:"stderr,omitempty"`
    ExitCode int    `json:"exit_code"`
}
```

### 4.2 Function Signatures by File

#### `exec.go` — Command Execution

```go
func runCmd(name string, args ...string) (*CmdResult, error)
func cmdString(name string, args ...string) string
func cmdError(msg, command string, result *CmdResult) error
```

- `runCmd` wraps `os/exec.Command`, captures stdout/stderr/exit code.
- `cmdError` formats structured error messages with command, exit code, and stderr.

#### `state.go` — State Management & Discovery

```go
func NewAgent(nodeID, dataDir string) *Agent

// Thread-safe user state accessors
func (a *Agent) getUserLock(userID string) *sync.Mutex
func (a *Agent) getUser(userID string) *UserResources
func (a *Agent) setUser(userID string, u *UserResources)
func (a *Agent) deleteUser(userID string)
func (a *Agent) allUsers() map[string]*UserResources    // returns deep copy

// Path helpers
func (a *Agent) imagePath(userID string) string          // {dataDir}/images/{userID}.img
func (a *Agent) mountPath(userID string) string          // /mnt/users/{userID}

// Discovery — rebuilds in-memory state from system reality
func (a *Agent) Discover()
func (a *Agent) ensureUser(userID string) *UserResources // NOT locked — caller must hold usersMu

// Internal discovery helpers (called within Discover, under lock)
func (a *Agent) discoverLoopDevices()    // parses losetup -a
func (a *Agent) discoverDRBDConfigs()    // scans /etc/drbd.d/user-*.res
func (a *Agent) discoverDRBDStatus()     // parses drbdadm status all
func (a *Agent) discoverMounts()         // parses mount output for /mnt/users/
func (a *Agent) discoverContainers()     // parses docker ps for *-agent containers
```

Package-level regex used by discovery and images:
```go
var loopRe = regexp.MustCompile(`^(/dev/loop\d+):\s+\[\d+\]:\d+\s+\((.+)\)`)
var minorRe = regexp.MustCompile(`minor\s+(\d+)`)
```

#### `images.go` — Disk Image Lifecycle

```go
var validUserID = regexp.MustCompile(`^[a-zA-Z0-9-]{3,32}$`)

func validateUserID(userID string) error
func (a *Agent) CreateImage(userID string, sizeMB int) (*shared.ImageCreateResponse, error)
func (a *Agent) findLoopDevice(imgPath string) string
func (a *Agent) attachLoop(imgPath string) (string, error)
```

**CreateImage behavior:**
1. Validates user ID (3-32 alphanumeric/hyphen chars)
2. If image file exists with loop device → return `already_existed: true`
3. If image file exists without loop device → attach loop, return `already_existed: true`
4. Otherwise → `truncate -s {size}M`, `losetup -f --show`, update state

#### `drbd.go` — DRBD 9 Lifecycle

```go
func (a *Agent) DRBDCreate(userID string, req *shared.DRBDCreateRequest) (*shared.DRBDCreateResponse, error)
func (a *Agent) DRBDPromote(userID string) (*shared.DRBDPromoteResponse, error)
func (a *Agent) DRBDDemote(userID string) (*shared.DRBDDemoteResponse, error)
func (a *Agent) DRBDStatus(userID string) (*shared.DRBDStatusResponse, error)
func (a *Agent) DRBDDestroy(userID string) error

func (a *Agent) getDRBDStatus(resName string) *DRBDInfo
func parseDRBDStatusAll(output string) map[string]*DRBDInfo
func splitResourceBlocks(output string) []string
func isMounted(path string) bool
```

**DRBDCreate behavior:**
1. Validates exactly 2 nodes in request
2. If `drbdadm status {resName}` succeeds → already exists
3. Writes config to `/etc/drbd.d/{resName}.res` (Protocol A, internal meta-disk)
4. `drbdadm create-md --force {resName}` → `drbdadm up {resName}`
5. Matches local hostname to set DRBDMinor and DRBDDevice in state

**DRBD config template (written to `/etc/drbd.d/{resName}.res`):**
```
resource {resName} {
    net { protocol A; max-buffers 8000; max-epoch-size 8000; sndbuf-size 0; rcvbuf-size 0; }
    disk { on-io-error detach; }
    on {node0.Hostname} { device /dev/drbd{minor} minor {minor}; disk {disk}; address {addr}:{port}; meta-disk internal; }
    on {node1.Hostname} { device /dev/drbd{minor} minor {minor}; disk {disk}; address {addr}:{port}; meta-disk internal; }
}
```

**DRBDPromote:** checks role, skips if already Primary. Runs `drbdadm primary --force {resName}`.

**DRBDDemote:** checks role, skips if already Secondary. Unmounts host mount if present before demoting. Runs `drbdadm secondary {resName}`.

**parseDRBDStatusAll:** Parses multi-resource `drbdadm status` output. Handles Connected, Syncing, Disconnected, and StandAlone states. Extracts role, disk state, peer disk state, connection state, and sync progress from key:value tokens.

**Resource naming convention:** `user-{userID}` (e.g., `user-alice`).

#### `btrfs.go` — Filesystem Provisioning

```go
func (a *Agent) FormatBtrfs(userID string) (*shared.FormatBtrfsResponse, error)
```

**FormatBtrfs behavior:**
1. Requires DRBD device in state
2. Tries `mount -t btrfs {drbdDev} {mountPath}` — if workspace subvol exists → already formatted
3. Formats: `mkfs.btrfs -f {drbdDev}`
4. Mounts, creates subvolume `workspace` with seed directories: `memory/`, `apps/`, `data/`
5. Writes `data/config.json` with `{"created": "...", "user": "..."}`
6. Creates `snapshots/` dir and read-only snapshot `snapshots/layer-000`
7. **Unmounts** — host does NOT keep Btrfs mounted (device-mount pattern)

#### `containers.go` — Docker Container Lifecycle

```go
func (a *Agent) ContainerStart(userID string) (*shared.ContainerStartResponse, error)
func (a *Agent) ContainerStop(userID string) error
func (a *Agent) ContainerStatus(userID string) (*shared.ContainerStatusResponse, error)

func (a *Agent) isContainerRunning(name string) bool
func (a *Agent) containerExists(name string) bool
```

**Container naming convention:** `{userID}-agent` (e.g., `alice-agent`).

**ContainerStart docker run flags (security-critical — do not modify):**
```
docker run -d
    --name {userID}-agent
    --device {drbdDev}
    --cap-drop ALL
    --cap-add SYS_ADMIN
    --cap-add SETUID
    --cap-add SETGID
    --security-opt apparmor=unconfined
    --network none
    --memory 64m
    -e BLOCK_DEVICE={drbdDev}
    -e SUBVOL_NAME=workspace
    platform/app-container
```

Waits 2 seconds after start, verifies running, fetches logs on failure.

#### `server.go` — HTTP API

```go
func (a *Agent) RegisterRoutes(mux *http.ServeMux)
func (a *Agent) EnsureContainerImage() error

// Handlers (all private)
func (a *Agent) handleStatus(w, r)
func (a *Agent) handleImageCreate(w, r)
func (a *Agent) handleImageDelete(w, r)
func (a *Agent) handleDRBDCreate(w, r)
func (a *Agent) handleDRBDPromote(w, r)
func (a *Agent) handleDRBDDemote(w, r)
func (a *Agent) handleDRBDStatus(w, r)
func (a *Agent) handleDRBDDestroy(w, r)
func (a *Agent) handleFormatBtrfs(w, r)
func (a *Agent) handleContainerStart(w, r)
func (a *Agent) handleContainerStop(w, r)
func (a *Agent) handleContainerStatus(w, r)
func (a *Agent) handleCleanup(w, r)

// Helpers
func writeJSON(w http.ResponseWriter, status int, v interface{})
func writeError(w http.ResponseWriter, status int, errMsg, details string)
func getDiskTotalMB() int64
func getDiskUsedMB() int64
func parseDfMB(output string) int64
func getRAMTotalMB() int64
func getRAMUsedMB() int64
func parseMemInfoKB(content, key string) int64
```

**EnsureContainerImage:** checks `docker images platform/app-container`, builds from `/opt/platform/container/` if missing.

#### `cleanup.go` — Teardown

```go
func (a *Agent) DeleteUser(userID string) error
func (a *Agent) Cleanup() error
```

**DeleteUser** tears down in reverse order:
1. `docker stop` + `docker rm -f` container
2. `umount` if mounted
3. `drbdadm down` resource
4. Remove `/etc/drbd.d/{resName}.res`
5. `losetup -d` loop device
6. Remove image file
7. Remove mount directory
8. Clear in-memory state and per-user lock

**Cleanup** tears down ALL users:
1. Stop all `*-agent` containers
2. Unmount all `/mnt/users/*`
3. `drbdadm down` all `user-*` resources
4. Remove all DRBD configs
5. Detach all loop devices for `{dataDir}/images/`
6. Remove all image files
7. Remove all mount dirs
8. Reset in-memory state and locks map

---

## 5. HTTP API Reference

All endpoints listen on `{LISTEN_ADDR}` (default `0.0.0.0:8080`).
All responses are `Content-Type: application/json`.
Errors return `{"error": "...", "details": "..."}`.

| Method | Path | Request Body | Response | Locked |
|--------|------|-------------|----------|--------|
| `GET` | `/status` | — | `StatusResponse` | No (calls Discover) |
| `POST` | `/images/{user_id}/create` | `ImageCreateRequest` | `ImageCreateResponse` | Yes |
| `DELETE` | `/images/{user_id}` | — | `{"ok": true}` | Yes |
| `POST` | `/images/{user_id}/drbd/create` | `DRBDCreateRequest` | `DRBDCreateResponse` | Yes |
| `POST` | `/images/{user_id}/drbd/promote` | — | `DRBDPromoteResponse` | Yes |
| `POST` | `/images/{user_id}/drbd/demote` | — | `DRBDDemoteResponse` | Yes |
| `GET` | `/images/{user_id}/drbd/status` | — | `DRBDStatusResponse` | No |
| `DELETE` | `/images/{user_id}/drbd` | — | `{"ok": true}` | Yes |
| `POST` | `/images/{user_id}/format-btrfs` | — | `FormatBtrfsResponse` | Yes |
| `POST` | `/containers/{user_id}/start` | — | `ContainerStartResponse` | Yes |
| `POST` | `/containers/{user_id}/stop` | — | `{"ok": true}` | Yes |
| `GET` | `/containers/{user_id}/status` | — | `ContainerStatusResponse` | No |
| `POST` | `/cleanup` | — | `{"ok": true}` | No |

**Locking:** "Yes" means the handler acquires `getUserLock(userID)` before proceeding. Different user IDs do not block each other.

---

## 6. Entrypoint: `cmd/machine-agent/main.go`

```go
func main()
```

1. Configures JSON structured logging (`slog.NewJSONHandler`)
2. Reads env vars: `NODE_ID` (required), `LISTEN_ADDR` (default `0.0.0.0:8080`), `DATA_DIR` (default `/data`)
3. Creates `Agent` via `NewAgent(nodeID, dataDir)`
4. Calls `agent.Discover()` to rebuild state from system
5. Calls `agent.EnsureContainerImage()` (warns on failure — may not be deployed yet)
6. Registers routes on `http.NewServeMux()`
7. Starts `http.ListenAndServe`

---

## 7. Container Image: `platform/app-container`

**Dockerfile:**
- Base: `alpine:latest`
- Installs: `btrfs-progs`
- Creates unprivileged user: `appuser`
- Entrypoint: `/usr/local/bin/container-init.sh`

**container-init.sh:**
1. `mkdir -p /workspace`
2. `mount -t btrfs -o subvol="$SUBVOL_NAME" "$BLOCK_DEVICE" /workspace`
3. `exec su appuser -s /bin/sh -c "${WORKLOAD_CMD:-"while true; do sleep 60; done"}"`

After `exec`, the process replaces init — SYS_ADMIN, SETUID, SETGID caps are dropped. The container runs as `appuser` with only the workspace subvolume visible.

---

## 8. Infrastructure & Deployment

### Target Environment
- 2x Hetzner Cloud servers (`cx23`, Ubuntu 24.04, `nbg1`)
- Private network: `10.0.0.0/24` (DRBD replication traffic)
- Cloud-init installs: DRBD 9 (LINBIT PPA), Docker, btrfs-progs, curl, jq

### Deployment Flow (`scripts/deploy.sh`)
For each machine:
1. `scp` binary to `/usr/local/bin/machine-agent`
2. `scp` Dockerfile + container-init.sh to `/opt/platform/container/`
3. Set `NODE_ID` in systemd unit, set hostname
4. `docker build -t platform/app-container` on the machine

### Systemd Unit (created by cloud-init)
```ini
[Service]
Type=simple
ExecStart=/usr/local/bin/machine-agent
Environment=NODE_ID={node-id}
Environment=LISTEN_ADDR=0.0.0.0:8080
Environment=DATA_DIR=/data
Restart=on-failure
RestartSec=5
```

### Storage Layout on Each Machine
```
/data/images/{userID}.img          # sparse disk image files
/mnt/users/{userID}/               # temporary mount point (used only during format)
/etc/drbd.d/user-{userID}.res     # DRBD config files
/opt/platform/container/           # Dockerfile + container-init.sh
/usr/local/bin/machine-agent       # the agent binary
```

---

## 9. Test Suite Summary

The test suite (`scripts/test_suite.sh`) runs 66 checks across 9 phases:

| Phase | Name | Checks | What It Validates |
|-------|------|--------|-------------------|
| 0 | Prerequisites | 8 | SSH, DRBD module, agent responding, container image |
| 1 | Single User Provisioning | 10 | Image creation, DRBD setup, sync, format, container start |
| 2 | Device-Mount Verification | 5 | /workspace mount, seed data, appuser, no host paths in /proc/mounts |
| 3 | Data Write + Replication | 4 | Write data in container, DRBD connected, peer UpToDate |
| 4 | Failover via API | 8 | Stop → demote → promote → start on other machine, data survives |
| 5 | Idempotency | 8 | Repeat every operation, all return 200 with appropriate flags |
| 6 | Full Teardown | 8 | DELETE user, verify no images/loops/DRBD/containers on both machines |
| 7 | Multi-User Density | 10 | 3 users (alice/bob/charlie) on same bipod, data isolation, independence |
| 8 | Status Endpoint | 5 | Accurate container/DRBD state, cleanup empties status |

### Test Helpers (from `common.sh`)
```bash
api "$ip" METHOD /path [body]        # curl wrapper to agent API
ssh_cmd "$ip" "command"              # SSH to machine as root
docker_exec "$ip" container cmd      # docker exec inside container via SSH
check "description" 'test_command'   # assertion framework with counters
phase_start N "Name"                 # reset phase counters
phase_result                         # print phase summary
final_result                         # print total summary, exit 1 if any failed
```

---

## 10. Provisioning Sequence (Full Stack for One User)

This is the sequence an external caller must follow to provision a user across the bipod:

```
1. POST /images/{user}/create  {"image_size_mb": 512}     → both machines
2. POST /images/{user}/drbd/create  {config with both nodes}  → both machines
3. POST /images/{user}/drbd/promote                         → primary machine only
4. (wait for DRBD sync — poll GET /images/{user}/drbd/status until peer_disk_state=UpToDate)
5. POST /images/{user}/format-btrfs                         → primary machine only
6. POST /containers/{user}/start                            → primary machine only
```

### Failover Sequence

```
1. POST /containers/{user}/stop     → current primary
2. POST /images/{user}/drbd/demote  → current primary
3. POST /images/{user}/drbd/promote → new primary
4. POST /containers/{user}/start    → new primary
```

### Teardown Sequence

```
1. DELETE /images/{user}            → both machines
   (or POST /cleanup for full machine reset)
```

---

## 11. Conventions & Constraints

- **User ID format:** 3-32 chars, `[a-zA-Z0-9-]` only (validated by `validateUserID`)
- **Resource naming:** DRBD resource = `user-{userID}`, container = `{userID}-agent`
- **DRBD port convention:** starts at 7900, incremented per user (7900, 7901, 7902...)
- **DRBD minor convention:** starts at 0, incremented per user (0, 1, 2...)
- **Image path:** `{dataDir}/images/{userID}.img`
- **Mount path:** `/mnt/users/{userID}` (temporary, only used during Btrfs format)
- **DRBD config path:** `/etc/drbd.d/user-{userID}.res`
- **Container image:** `platform/app-container` (built from `/opt/platform/container/`)
- **Btrfs layout inside device:** `workspace/` subvol (with `memory/`, `apps/`, `data/`), `snapshots/layer-000`
- **No host-level Btrfs mounts at runtime** — only during format, then unmounted
- **Build target:** `GOOS=linux GOARCH=amd64`

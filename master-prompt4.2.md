# Layer 4.2 Build Prompt — Coordinator Happy Path

## What This Is

This is a build prompt for Layer 4.2 of the scfuture distributed agent platform. You are Claude Code. Your job is to:

1. Read all referenced existing files to understand the current codebase
2. Write all new code and scripts described below
3. Report back when code is written
4. When told "yes" / "ready", run the full test lifecycle (infra up → deploy → test → iterate on failures → teardown)
5. When all tests pass, update `SESSION.md` (in the parent directory) with what happened
6. Give a final report

The project lives in `scfuture/` (a subdirectory of the current working directory). All Go code paths are relative to `scfuture/`. All script paths are relative to `scfuture/`. The `SESSION.md` file is in the parent directory (current working directory).

---

## Context: What Exists

Layer 4.1 is complete and committed. Read these files first to understand the existing codebase:

### Existing code (read-only reference — do NOT rewrite these):

```
scfuture/go.mod
scfuture/Makefile
scfuture/cmd/machine-agent/main.go
scfuture/internal/shared/types.go
scfuture/internal/machineagent/server.go
scfuture/internal/machineagent/images.go
scfuture/internal/machineagent/drbd.go
scfuture/internal/machineagent/btrfs.go
scfuture/internal/machineagent/containers.go
scfuture/internal/machineagent/state.go
scfuture/internal/machineagent/cleanup.go
scfuture/internal/machineagent/exec.go
scfuture/container/Dockerfile
scfuture/container/container-init.sh
```

Read ALL of these before writing any code. Pay close attention to:
- How API types are defined in `internal/shared/types.go`
- How the machine agent's HTTP handlers work in `server.go`
- The provisioning sequence documented in `scfuture-architecture.md` Section 10
- The DRBD create request structure (`DRBDCreateRequest` with `DRBDNode` array)

### Reference documents (in the parent directory):

```
SESSION.md                  — Full project history, all PoC results, critical learnings
architecture-v3.md          — Architecture design document
scfuture-architecture.md    — Layer 4.1 implementation reference
```

Read `SESSION.md` Section 10 ("Critical Learnings") carefully. Every learning there was hard-won and must be respected.

---

## What Layer 4.2 Builds

Layer 4.2 adds a **coordinator** — a Go HTTP service that manages the fleet of machine agents and orchestrates user provisioning. This is the "happy path" layer: provisioning works, fleet is healthy, no failures.

### What's In Scope
- Coordinator binary (`cmd/coordinator/main.go`)
- Fleet management: machine registration, heartbeats, placement
- User provisioning state machine: create user → allocate resources → call machine agents → user is running
- In-memory state store (no external database dependencies)
- Test harness proving multi-user provisioning across 3 fleet machines
- Balanced placement algorithm

### What's Explicitly NOT In Scope
- Failure detection / automatic failover (Layer 4.3)
- Bipod reformation after failure (Layer 4.4)
- Suspension / reactivation / deletion (Layer 4.5)
- Crash recovery / reconciliation (Layer 4.6)
- Live migration (Layer 5)
- Backblaze integration
- Dashboard UI
- Postgres (we use in-memory store; Postgres comes when crash recovery matters)

---

## Architecture

### Test Topology

```
macOS (this machine — test harness runs here)
  │
  ├── SSH/HTTP → coordinator (Hetzner CX23, public IP, :8080)
  │               └── coordinator binary
  │               └── Private IP: 10.0.0.2
  │
  ├── (coordinator calls) → fleet-1 (Hetzner CX23, machine-agent on :8080)
  │                          Private IP: 10.0.0.11
  │
  ├── (coordinator calls) → fleet-2 (Hetzner CX23, machine-agent on :8080)
  │                          Private IP: 10.0.0.12
  │
  └── (coordinator calls) → fleet-3 (Hetzner CX23, machine-agent on :8080)
                              Private IP: 10.0.0.13

Private network: 10.0.0.0/24
  - Coordinator ↔ fleet machines (HTTP API calls)
  - Fleet ↔ fleet (DRBD replication per user)
```

4 Hetzner Cloud CX23 servers in `nbg1` (or `fsn1` — try `nbg1` first, fall back if unavailable). All on the same private network.

The coordinator uses **private IPs** to communicate with fleet machines (10.0.0.x). The test harness on macOS uses **public IPs** to reach all machines.

### How It Works

1. Fleet machines start, machine-agent registers itself with the coordinator via `POST /api/fleet/register`
2. Machine agents send heartbeats every 10 seconds via `POST /api/fleet/heartbeat`
3. Test harness calls coordinator: `POST /api/users` to create a user
4. Test harness calls coordinator: `POST /api/users/{id}/provision` to start provisioning
5. Coordinator's provisioning state machine:
   - Selects 2 least-loaded machines
   - Calls machine agent APIs in sequence (see Provisioning State Machine below)
   - Updates internal state at each step
6. Test harness polls `GET /api/users/{id}` until status is `running`
7. Coordinator exposes fleet view via `GET /api/fleet`

---

## Provisioning State Machine

The coordinator drives this sequence for each user. Each step is a state checkpoint. All machine agent API calls are idempotent (proven in Layer 4.1), so retrying any step is safe.

```
State: REGISTERED
  → User record created in coordinator state
  → DRBD port allocated (globally unique, starting from 7900)
  → Primary and secondary machines selected (least-loaded)
  → DRBD minors allocated (per-machine counter, starting from 0)

State: IMAGES_CREATED
  → POST {primary}/images/{user}/create   {"image_size_mb": 512}
  → POST {secondary}/images/{user}/create {"image_size_mb": 512}
  → Capture loop_device from each response (needed for DRBD config)

State: DRBD_CONFIGURED
  → Build DRBDCreateRequest with both nodes:
    {
      "resource_name": "user-{userID}",
      "nodes": [
        {"hostname": "{primary_node_id}", "minor": {primary_minor}, "disk": "{primary_loop_device}", "address": "{primary_private_ip}"},
        {"hostname": "{secondary_node_id}", "minor": {secondary_minor}, "disk": "{secondary_loop_device}", "address": "{secondary_private_ip}"}
      ],
      "port": {allocated_port}
    }
  → POST {primary}/images/{user}/drbd/create   {above config}
  → POST {secondary}/images/{user}/drbd/create {above config}

State: PRIMARY_PROMOTED
  → POST {primary}/images/{user}/drbd/promote
  → CRITICAL: This MUST happen BEFORE waiting for sync.
    DRBD initial sync does not begin until one side is promoted.
    (See SESSION.md Issue #26)

State: DRBD_SYNCED
  → Poll GET {primary}/images/{user}/drbd/status every 2 seconds
  → Wait until peer_disk_state == "UpToDate"
  → Timeout after 120 seconds (fail if not synced)

State: BTRFS_FORMATTED
  → POST {primary}/images/{user}/format-btrfs

State: CONTAINER_STARTED
  → POST {primary}/containers/{user}/start

State: RUNNING
  → User is live, provisioning complete
```

If any step fails after 1 retry, set user status to `failed` with an error message.

### Machine Address Convention

The coordinator stores two addresses per machine:
- `address`: private IP + port (e.g., `10.0.0.11:8080`) — used for coordinator→machine API calls
- `public_address`: public IP + port — stored but not used by coordinator (used by test harness)

The DRBD config uses private IPs WITHOUT port (e.g., `10.0.0.11`) since DRBD uses its own port.

### Port and Minor Allocation

- **DRBD ports**: globally unique, starting at 7900. Coordinator maintains a counter. Each user gets the next port.
- **DRBD minors**: per-machine, starting at 0. Coordinator tracks next available minor per machine. When a user's bipod is assigned to fleet-1 and fleet-2, fleet-1's next minor is allocated for fleet-1's side, and fleet-2's next minor for fleet-2's side.

### Placement Algorithm

```
1. Get all machines with status = "active"
2. Filter: disk_used_mb < disk_total_mb * 0.85
3. Sort by: active_agents ascending (least loaded first)
4. Pick top 2
5. First = primary, second = secondary
```

The placement decision is made under a mutex to prevent two concurrent provisionings from picking the same "least loaded" pair before counts are updated.

---

## Small Modifications to Machine Agent

The machine agent from Layer 4.1 needs two small additions. These do NOT change any existing endpoints or behavior. The machine agent continues to work standalone if `COORDINATOR_URL` is not set.

### 1. New env var: `COORDINATOR_URL`

In `cmd/machine-agent/main.go`, read `COORDINATOR_URL` from env. If set, start the registration and heartbeat goroutine.

### 2. New file: `internal/machineagent/heartbeat.go`

```go
// StartHeartbeat begins the registration and heartbeat loop.
// Called from main.go only if COORDINATOR_URL is set.
func (a *Agent) StartHeartbeat(coordinatorURL string)
```

Behavior:
- Immediately POST to `{coordinatorURL}/api/fleet/register` with:
  ```json
  {
    "machine_id": "{nodeID}",
    "address": "{nodeID's private address — from LISTEN_ADDR or NODE_ADDRESS env}",
    "disk_total_mb": ...,
    "disk_used_mb": ...,
    "ram_total_mb": ...,
    "ram_used_mb": ...,
    "max_agents": 200
  }
  ```
- Retry registration every 5 seconds until it succeeds (coordinator may not be up yet)
- Once registered, start a goroutine that every 10 seconds POSTs to `{coordinatorURL}/api/fleet/heartbeat` with:
  ```json
  {
    "machine_id": "{nodeID}",
    "disk_total_mb": ...,
    "disk_used_mb": ...,
    "ram_total_mb": ...,
    "ram_used_mb": ...,
    "active_agents": 3,
    "running_agents": ["alice", "bob", "charlie"]
  }
  ```
  The running_agents list comes from iterating the agent's in-memory user state.
- Heartbeat failures are logged but don't crash the agent

### 3. New env var: `NODE_ADDRESS`

The machine agent needs to know its own private IP so it can tell the coordinator. Add `NODE_ADDRESS` env var (e.g., `10.0.0.11:8080`). This is the address the coordinator will use to call this machine.

### 4. Types for registration and heartbeat

Add to `internal/shared/types.go`:

```go
type FleetRegisterRequest struct {
    MachineID    string `json:"machine_id"`
    Address      string `json:"address"`
    DiskTotalMB  int64  `json:"disk_total_mb"`
    DiskUsedMB   int64  `json:"disk_used_mb"`
    RAMTotalMB   int64  `json:"ram_total_mb"`
    RAMUsedMB    int64  `json:"ram_used_mb"`
    MaxAgents    int    `json:"max_agents"`
}

type FleetHeartbeatRequest struct {
    MachineID     string   `json:"machine_id"`
    DiskTotalMB   int64    `json:"disk_total_mb"`
    DiskUsedMB    int64    `json:"disk_used_mb"`
    RAMTotalMB    int64    `json:"ram_total_mb"`
    RAMUsedMB     int64    `json:"ram_used_mb"`
    ActiveAgents  int      `json:"active_agents"`
    RunningAgents []string `json:"running_agents"`
}
```

---

## Coordinator Implementation

### New files to create:

```
scfuture/cmd/coordinator/main.go
scfuture/internal/coordinator/server.go
scfuture/internal/coordinator/store.go
scfuture/internal/coordinator/provisioner.go
scfuture/internal/coordinator/fleet.go
scfuture/internal/coordinator/machineapi.go
```

### `cmd/coordinator/main.go`

Entry point:
1. Configure JSON structured logging (`slog.NewJSONHandler`)
2. Read env vars:
   - `LISTEN_ADDR` (default `0.0.0.0:8080`)
   - `DATA_DIR` (default `/data`) — for JSON state persistence file
3. Create `coordinator.NewCoordinator(dataDir)`
4. Register routes on `http.NewServeMux()`
5. Start `http.ListenAndServe`

### `internal/coordinator/store.go` — In-Memory State Store

An in-memory state store protected by `sync.RWMutex`. Periodically persists to `{dataDir}/state.json`.

```go
type Store struct {
    mu       sync.RWMutex
    machines map[string]*Machine
    users    map[string]*User
    bipods   map[string]*Bipod   // keyed by "{userID}:{machineID}"

    nextPort  int  // next DRBD port to allocate (starts at 7900)
    nextMinor map[string]int  // per-machine next DRBD minor (starts at 0)

    dataDir  string
}

type Machine struct {
    MachineID     string
    Address       string    // private IP:port (for coordinator → machine API calls)
    PublicAddress string    // public IP:port (for test harness, set by coordinator from outside)
    Status        string    // "active"
    DiskTotalMB   int64
    DiskUsedMB    int64
    RAMTotalMB    int64
    RAMUsedMB     int64
    ActiveAgents  int
    MaxAgents     int
    RunningAgents []string
    LastHeartbeat time.Time
}

type User struct {
    UserID         string
    Status         string    // registered | provisioning | running | failed
    PrimaryMachine string
    DRBDPort       int
    ImageSizeMB    int
    Error          string    // populated on failure
    CreatedAt      time.Time
}

type Bipod struct {
    UserID     string
    MachineID  string
    Role       string    // "primary" | "secondary"
    DRBDMinor  int
    LoopDevice string    // captured from image create response
}
```

Methods:
```go
func NewStore(dataDir string) *Store
func (s *Store) RegisterMachine(req *shared.FleetRegisterRequest) 
func (s *Store) UpdateHeartbeat(req *shared.FleetHeartbeatRequest)
func (s *Store) GetMachine(id string) *Machine
func (s *Store) AllMachines() []*Machine
func (s *Store) CreateUser(userID string, imageSizeMB int) (*User, error)
func (s *Store) GetUser(userID string) *User
func (s *Store) AllUsers() []*User
func (s *Store) SetUserStatus(userID, status, errMsg string)
func (s *Store) SetUserPrimary(userID, machineID string)
func (s *Store) SetUserPort(userID string, port int)
func (s *Store) CreateBipod(userID, machineID, role string, minor int)
func (s *Store) SetBipodLoopDevice(userID, machineID, loopDev string)
func (s *Store) GetBipods(userID string) []*Bipod
func (s *Store) AllocatePort() int
func (s *Store) AllocateMinor(machineID string) int
func (s *Store) SelectMachines() (primary *Machine, secondary *Machine, err error)
func (s *Store) persist()  // write JSON to disk (called after mutations)
```

`SelectMachines()` implements the placement algorithm described above. It holds the write lock during selection so concurrent provisioning can't pick the same stale view.

Persistence: call `persist()` after every mutation. The `persist()` method writes the full state to `{dataDir}/state.json`. This is a convenience for debugging, not a crash recovery mechanism (that's Layer 4.6).

### `internal/coordinator/machineapi.go` — Machine Agent Client

A thin HTTP client that wraps the machine agent API. Uses Go standard library `net/http` only.

```go
type MachineClient struct {
    address string    // e.g., "10.0.0.11:8080"
    client  *http.Client
}

func NewMachineClient(address string) *MachineClient

// All methods return typed responses and errors
func (c *MachineClient) CreateImage(userID string, sizeMB int) (*shared.ImageCreateResponse, error)
func (c *MachineClient) DRBDCreate(userID string, req *shared.DRBDCreateRequest) (*shared.DRBDCreateResponse, error)
func (c *MachineClient) DRBDPromote(userID string) (*shared.DRBDPromoteResponse, error)
func (c *MachineClient) DRBDDemote(userID string) (*shared.DRBDDemoteResponse, error)
func (c *MachineClient) DRBDStatus(userID string) (*shared.DRBDStatusResponse, error)
func (c *MachineClient) FormatBtrfs(userID string) (*shared.FormatBtrfsResponse, error)
func (c *MachineClient) ContainerStart(userID string) (*shared.ContainerStartResponse, error)
func (c *MachineClient) ContainerStop(userID string) error
func (c *MachineClient) ContainerStatus(userID string) (*shared.ContainerStatusResponse, error)
func (c *MachineClient) DeleteUser(userID string) error
func (c *MachineClient) Status() (*shared.StatusResponse, error)
func (c *MachineClient) Cleanup() error
```

Each method:
- Constructs the URL from `c.address` + endpoint path
- Marshals request body (if any) to JSON
- Makes HTTP request with 30-second timeout
- Reads response, checks status code
- Unmarshals response body into the typed struct
- Returns error with context on non-2xx status

### `internal/coordinator/provisioner.go` — Provisioning State Machine

```go
func (coord *Coordinator) ProvisionUser(userID string)
```

This function runs in its own goroutine (started by the provision endpoint handler). It drives the full state machine described above:

```
REGISTERED → IMAGES_CREATED → DRBD_CONFIGURED → PRIMARY_PROMOTED → DRBD_SYNCED → BTRFS_FORMATTED → CONTAINER_STARTED → RUNNING
```

At each step:
1. Create `MachineClient` for the target machine(s) using private addresses from the store
2. Make the API call(s)
3. On success: update store state, log progress
4. On failure: retry once after 2 seconds. If still fails: set user status to `failed` with error, return

The DRBD sync wait step polls every 2 seconds with a 120-second timeout.

The provisioner retrieves loop devices from the image creation responses and uses them when building the `DRBDCreateRequest`.

### `internal/coordinator/fleet.go` — Fleet Management

```go
func (coord *Coordinator) HandleRegister(req *shared.FleetRegisterRequest)
func (coord *Coordinator) HandleHeartbeat(req *shared.FleetHeartbeatRequest)
```

Simple pass-through to the store. Heartbeat updates machine stats (disk, RAM, agent count, timestamp).

### `internal/coordinator/server.go` — HTTP Routing

```go
type Coordinator struct {
    store *Store
}

func NewCoordinator(dataDir string) *Coordinator
func (coord *Coordinator) RegisterRoutes(mux *http.ServeMux)
```

Routes:

```
# Fleet management (called by machine agents)
POST   /api/fleet/register            → handleFleetRegister
POST   /api/fleet/heartbeat           → handleFleetHeartbeat
GET    /api/fleet                      → handleFleetStatus

# User management (called by test harness / external systems)
POST   /api/users                      → handleCreateUser
GET    /api/users                      → handleListUsers
GET    /api/users/{id}                 → handleGetUser
POST   /api/users/{id}/provision       → handleProvisionUser
GET    /api/users/{id}/bipod           → handleGetBipod
```

Handler behaviors:

**POST /api/fleet/register**: Decode `FleetRegisterRequest`, call `store.RegisterMachine()`. Return `{"ok": true}`.

**POST /api/fleet/heartbeat**: Decode `FleetHeartbeatRequest`, call `store.UpdateHeartbeat()`. Return `{"ok": true}`.

**GET /api/fleet**: Return all machines with their current stats. Response:
```json
{
  "machines": [
    {
      "machine_id": "fleet-1",
      "address": "10.0.0.11:8080",
      "status": "active",
      "disk_total_mb": ...,
      "disk_used_mb": ...,
      "ram_total_mb": ...,
      "ram_used_mb": ...,
      "active_agents": 2,
      "max_agents": 200,
      "running_agents": ["alice", "bob"],
      "last_heartbeat": "..."
    },
    ...
  ]
}
```

**POST /api/users**: Body `{"user_id": "alice", "image_size_mb": 512}`. `image_size_mb` defaults to 512 if not provided. Creates user in store with status `registered`. Returns:
```json
{
  "user_id": "alice",
  "status": "registered"
}
```

**GET /api/users**: Returns list of all users with their status.

**GET /api/users/{id}**: Returns user details:
```json
{
  "user_id": "alice",
  "status": "running",
  "primary_machine": "fleet-1",
  "drbd_port": 7900,
  "error": "",
  "bipod": [
    {"machine_id": "fleet-1", "role": "primary", "drbd_minor": 0, "loop_device": "/dev/loop0"},
    {"machine_id": "fleet-2", "role": "secondary", "drbd_minor": 0, "loop_device": "/dev/loop0"}
  ]
}
```

**POST /api/users/{id}/provision**: Validate user exists and is in `registered` state. Set status to `provisioning`. Start `coord.ProvisionUser(userID)` in a goroutine. Return immediately:
```json
{
  "user_id": "alice",
  "status": "provisioning"
}
```

**GET /api/users/{id}/bipod**: Returns the bipod entries for this user.

### Add to `internal/shared/types.go`

Add the `FleetRegisterRequest` and `FleetHeartbeatRequest` types described above in the machine agent section. Also add coordinator-specific response types:

```go
// Coordinator API types

type CreateUserRequest struct {
    UserID      string `json:"user_id"`
    ImageSizeMB int    `json:"image_size_mb,omitempty"`
}

type CreateUserResponse struct {
    UserID string `json:"user_id"`
    Status string `json:"status"`
}

type UserDetailResponse struct {
    UserID         string            `json:"user_id"`
    Status         string            `json:"status"`
    PrimaryMachine string            `json:"primary_machine"`
    DRBDPort       int               `json:"drbd_port"`
    Error          string            `json:"error,omitempty"`
    Bipod          []BipodEntry      `json:"bipod"`
}

type BipodEntry struct {
    MachineID  string `json:"machine_id"`
    Role       string `json:"role"`
    DRBDMinor  int    `json:"drbd_minor"`
    LoopDevice string `json:"loop_device"`
}

type FleetStatusResponse struct {
    Machines []MachineStatus `json:"machines"`
}

type MachineStatus struct {
    MachineID     string   `json:"machine_id"`
    Address       string   `json:"address"`
    Status        string   `json:"status"`
    DiskTotalMB   int64    `json:"disk_total_mb"`
    DiskUsedMB    int64    `json:"disk_used_mb"`
    RAMTotalMB    int64    `json:"ram_total_mb"`
    RAMUsedMB     int64    `json:"ram_used_mb"`
    ActiveAgents  int      `json:"active_agents"`
    MaxAgents     int      `json:"max_agents"`
    RunningAgents []string `json:"running_agents"`
    LastHeartbeat string   `json:"last_heartbeat"`
}
```

---

## Makefile Update

Add the coordinator build target:

```makefile
build:
	GOOS=linux GOARCH=amd64 go build -o bin/machine-agent ./cmd/machine-agent
	GOOS=linux GOARCH=amd64 go build -o bin/coordinator ./cmd/coordinator
```

Keep all existing targets. The `deploy` and `test` targets can remain as they are (Layer 4.1 specific) — the Layer 4.2 scripts handle their own deployment.

---

## Test Scripts

All test scripts go in `scfuture/scripts/layer-4.2/`. They are self-contained — they don't depend on Layer 4.1 scripts (though they follow the same patterns).

### `scfuture/scripts/layer-4.2/cloud-init/fleet.yaml`

Same as Layer 4.1's `fleet.yaml` — installs DRBD 9 (LINBIT PPA + DKMS), Docker, btrfs-progs, creates storage dirs, sets up systemd unit for machine-agent. The key additions:

- The systemd unit for machine-agent now also sets `COORDINATOR_URL` and `NODE_ADDRESS`:
  ```ini
  Environment=COORDINATOR_URL=http://10.0.0.2:8080
  Environment=NODE_ADDRESS={private_ip}:8080
  ```
  (The deploy script will substitute the actual private IP)

Reference Layer 4.1's `cloud-init/fleet.yaml` for the DRBD 9 installation commands (LINBIT PPA, DKMS, linux-headers-$(uname -r), modprobe drbd).

### `scfuture/scripts/layer-4.2/cloud-init/coordinator.yaml`

Minimal cloud-init for the coordinator machine:

```yaml
packages:
  - curl
  - jq

write_files:
  - path: /etc/systemd/system/coordinator.service
    content: |
      [Unit]
      Description=scfuture coordinator
      After=network.target
      [Service]
      Type=simple
      ExecStart=/usr/local/bin/coordinator
      Environment=LISTEN_ADDR=0.0.0.0:8080
      Environment=DATA_DIR=/data
      Restart=on-failure
      RestartSec=5
      [Install]
      WantedBy=multi-user.target

runcmd:
  - mkdir -p /data
  - systemctl daemon-reload
```

No DRBD, no Docker, no btrfs-progs needed on the coordinator machine.

### `scfuture/scripts/layer-4.2/infra.sh`

Creates/destroys 4 Hetzner Cloud servers and a private network.

```bash
#!/usr/bin/env bash
# Usage: ./infra.sh up | down | status

SERVERS: l42-coordinator, l42-fleet-1, l42-fleet-2, l42-fleet-3
NETWORK: l42-net (10.0.0.0/24)
SUBNET: 10.0.0.0/24 in eu-central zone
SSH_KEY: l42-key

Private IPs:
  l42-coordinator: 10.0.0.2
  l42-fleet-1:     10.0.0.11
  l42-fleet-2:     10.0.0.12
  l42-fleet-3:     10.0.0.13

Server type: cx23 at nbg1 (fall back to fsn1 or hel1 if unavailable)
Image: ubuntu-24.04

up:
  1. Create SSH key from ~/.ssh/id_rsa.pub (or id_ed25519.pub — check which exists)
  2. Create network + subnet
  3. Create coordinator server with coordinator.yaml cloud-init
  4. Create 3 fleet servers with fleet.yaml cloud-init
  5. Save IPs to .ips file

down:
  1. Delete servers
  2. Delete network
  3. Delete SSH key
  4. Remove .ips file

status:
  Show all resources
```

Follow the patterns from Layer 4.1's `infra.sh`. Use `hcloud` CLI.

### `scfuture/scripts/layer-4.2/deploy.sh`

Deploys binaries and configuration to all 4 machines.

```bash
#!/usr/bin/env bash
# Assumes infra.sh has been run and .ips file exists

For coordinator machine:
  1. Wait for SSH (retry loop)
  2. Wait for cloud-init complete (cloud-init status --wait)
  3. SCP bin/coordinator to /usr/local/bin/coordinator
  4. Set hostname to "coordinator"
  5. Start coordinator service

For each fleet machine (fleet-1, fleet-2, fleet-3):
  1. Wait for SSH
  2. Wait for cloud-init complete
  3. SCP bin/machine-agent to /usr/local/bin/machine-agent
  4. SCP container/Dockerfile and container/container-init.sh to /opt/platform/container/
  5. Set hostname to fleet-N
  6. Configure systemd unit with:
     - NODE_ID=fleet-N
     - NODE_ADDRESS=10.0.0.{11+N-1}:8080
     - COORDINATOR_URL=http://10.0.0.2:8080
  7. Build Docker image: docker build -t platform/app-container /opt/platform/container/
  8. Start machine-agent service
  9. Verify DRBD module loaded (modprobe drbd)
```

### `scfuture/scripts/layer-4.2/common.sh`

Shared bash functions. Include:

```bash
# Source the .ips file to get COORD_PUB_IP, FLEET1_PUB_IP, etc.
# (or read from infra.sh output)

# API helper — calls coordinator or machine agent
coord_api() {
    local method="$1" path="$2" body="${3:-}"
    if [ -n "$body" ]; then
        curl -sf -X "$method" -H "Content-Type: application/json" \
            -d "$body" "http://${COORD_PUB_IP}:8080${path}"
    else
        curl -sf -X "$method" "http://${COORD_PUB_IP}:8080${path}"
    fi
}

machine_api() {
    local ip="$1" method="$2" path="$3" body="${4:-}"
    if [ -n "$body" ]; then
        curl -sf -X "$method" -H "Content-Type: application/json" \
            -d "$body" "http://${ip}:8080${path}"
    else
        curl -sf -X "$method" "http://${ip}:8080${path}"
    fi
}

ssh_cmd() {
    local ip="$1"; shift
    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=5 root@"$ip" "$@"
}

docker_exec() {
    local ip="$1" container="$2"; shift 2
    ssh_cmd "$ip" "docker exec $container $*"
}

# Test framework
TOTAL_PASS=0
TOTAL_FAIL=0
PHASE_PASS=0
PHASE_FAIL=0

check() {
    local desc="$1"; shift
    if eval "$@" >/dev/null 2>&1; then
        echo "  ✓ $desc"
        ((PHASE_PASS++))
        ((TOTAL_PASS++))
    else
        echo "  ✗ FAIL: $desc"
        ((PHASE_FAIL++))
        ((TOTAL_FAIL++))
    fi
}

phase_start() {
    PHASE_PASS=0
    PHASE_FAIL=0
    echo ""
    echo "═══ Phase $1: $2 ═══"
}

phase_result() {
    echo "  Phase result: ${PHASE_PASS} passed, ${PHASE_FAIL} failed"
}

final_result() {
    echo ""
    echo "═══════════════════════════════════════════════════"
    if [ "$TOTAL_FAIL" -eq 0 ]; then
        echo " ALL PHASES COMPLETE: ${TOTAL_PASS}/${TOTAL_PASS} checks passed"
    else
        echo " FAILURES: ${TOTAL_PASS} passed, ${TOTAL_FAIL} failed"
    fi
    echo "═══════════════════════════════════════════════════"
    [ "$TOTAL_FAIL" -eq 0 ]
}

# Poll helper — waits for user to reach a given status
wait_for_user_status() {
    local user_id="$1" target_status="$2" timeout="${3:-120}"
    local elapsed=0
    while [ "$elapsed" -lt "$timeout" ]; do
        local status
        status=$(coord_api GET "/api/users/${user_id}" | jq -r '.status // empty')
        if [ "$status" = "$target_status" ]; then
            return 0
        fi
        if [ "$status" = "failed" ]; then
            echo "  ✗ User $user_id provisioning FAILED:"
            coord_api GET "/api/users/${user_id}" | jq -r '.error // "unknown error"'
            return 1
        fi
        sleep 2
        elapsed=$((elapsed + 2))
    done
    echo "  ✗ Timeout waiting for $user_id to reach $target_status (stuck at $status)"
    return 1
}
```

### `scfuture/scripts/layer-4.2/test_suite.sh`

The test suite. Target: ~55-70 checks across 7 phases.

```bash
#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/common.sh"

# ══════════════════════════════════════════
# Phase 0: Prerequisites
# ══════════════════════════════════════════
phase_start 0 "Prerequisites"

# Coordinator responding
check "Coordinator responding" 'coord_api GET /api/fleet | jq -e .machines'

# Wait for fleet machines to register (they register on startup)
echo "  Waiting for 3 fleet machines to register..."
for i in $(seq 1 60); do
    count=$(coord_api GET /api/fleet | jq '.machines | length')
    [ "$count" -ge 3 ] && break
    sleep 2
done

check "3 fleet machines registered" '[ "$(coord_api GET /api/fleet | jq ".machines | length")" -ge 3 ]'

# Check each fleet machine
for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Machine agent responding at $ip" 'machine_api "$ip" GET /status | jq -e .machine_id'
done

# DRBD module on each fleet machine
for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "DRBD module loaded on $ip" 'ssh_cmd "$ip" "lsmod | grep -q drbd"'
done

# Container image on each fleet machine
for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Container image on $ip" 'ssh_cmd "$ip" "docker images platform/app-container -q" | grep -q .'
done

phase_result

# ══════════════════════════════════════════
# Phase 1: Provision First User (alice)
# ══════════════════════════════════════════
phase_start 1 "Provision First User (alice)"

check "Create user alice" 'coord_api POST /api/users "{\"user_id\":\"alice\"}" | jq -e ".status == \"registered\""'
check "Provision alice" 'coord_api POST /api/users/alice/provision | jq -e ".status == \"provisioning\""'
check "Alice reaches running" 'wait_for_user_status alice running 180'

# Verify through coordinator API
check "Alice status is running" '[ "$(coord_api GET /api/users/alice | jq -r .status)" = "running" ]'
check "Alice has primary machine" '[ -n "$(coord_api GET /api/users/alice | jq -r .primary_machine)" ]'
check "Alice has DRBD port" '[ "$(coord_api GET /api/users/alice | jq -r .drbd_port)" -ge 7900 ]'
check "Alice has 2 bipod entries" '[ "$(coord_api GET /api/users/alice | jq ".bipod | length")" -eq 2 ]'

# Verify on actual machines — find alice's primary
ALICE_PRIMARY_ID=$(coord_api GET /api/users/alice | jq -r .primary_machine)
# Get the public IP of alice's primary machine for direct verification
ALICE_PRIMARY_PUB=$(get_public_ip "$ALICE_PRIMARY_ID")  # helper to map machine_id → public IP

check "Container running on primary" 'machine_api "$ALICE_PRIMARY_PUB" GET /containers/alice/status | jq -e .running'

# Verify data accessible inside container
check "Data accessible in container" 'docker_exec "$ALICE_PRIMARY_PUB" alice-agent "cat /workspace/data/config.json" | jq -e .user'

phase_result

# ══════════════════════════════════════════
# Phase 2: Provision Second User (bob)
# ══════════════════════════════════════════
phase_start 2 "Provision Second User (bob)"

check "Create user bob" 'coord_api POST /api/users "{\"user_id\":\"bob\"}" | jq -e ".status == \"registered\""'
check "Provision bob" 'coord_api POST /api/users/bob/provision | jq -e ".status == \"provisioning\""'
check "Bob reaches running" 'wait_for_user_status bob running 180'
check "Bob status is running" '[ "$(coord_api GET /api/users/bob | jq -r .status)" = "running" ]'

# Check placement diversity — bob should ideally use a different primary
BOB_PRIMARY_ID=$(coord_api GET /api/users/bob | jq -r .primary_machine)
check "Bob placed (primary: $BOB_PRIMARY_ID)" 'true'  # informational

check "Bob container running" '
    BOB_PUB=$(get_public_ip "$BOB_PRIMARY_ID")
    machine_api "$BOB_PUB" GET /containers/bob/status | jq -e .running
'

phase_result

# ══════════════════════════════════════════
# Phase 3: Provision Third User (charlie)
# ══════════════════════════════════════════
phase_start 3 "Provision Third User (charlie)"

check "Create + provision charlie" '
    coord_api POST /api/users "{\"user_id\":\"charlie\"}"
    coord_api POST /api/users/charlie/provision
    wait_for_user_status charlie running 180
'
check "Charlie is running" '[ "$(coord_api GET /api/users/charlie | jq -r .status)" = "running" ]'

CHARLIE_PRIMARY_ID=$(coord_api GET /api/users/charlie | jq -r .primary_machine)
check "Charlie placed (primary: $CHARLIE_PRIMARY_ID)" 'true'

phase_result

# ══════════════════════════════════════════
# Phase 4: Provision More Users (dave, eve)
# ══════════════════════════════════════════
phase_start 4 "Provision Users dave and eve"

for user in dave eve; do
    check "Create + provision $user" '
        coord_api POST /api/users "{\"user_id\":\"'$user'\"}"
        coord_api POST /api/users/'$user'/provision
        wait_for_user_status '$user' running 180
    '
    check "$user is running" '[ "$(coord_api GET /api/users/'$user' | jq -r .status)" = "running" ]'
done

check "5 users total in coordinator" '[ "$(coord_api GET /api/users | jq ". | length")" -eq 5 ]'

phase_result

# ══════════════════════════════════════════
# Phase 5: Fleet Status Verification
# ══════════════════════════════════════════
phase_start 5 "Fleet Status Verification"

check "Fleet shows 3 machines" '[ "$(coord_api GET /api/fleet | jq ".machines | length")" -eq 3 ]'

# Check total agent count across fleet
check "Total agents = 5 across fleet" '
    total=$(coord_api GET /api/fleet | jq "[.machines[].active_agents] | add")
    [ "$total" -eq 5 ]
'

# Verify balanced placement — no machine should have more than 3 primaries
check "Balanced: no machine has >3 agents" '
    max=$(coord_api GET /api/fleet | jq "[.machines[].active_agents] | max")
    [ "$max" -le 4 ]
'

# Verify coordinator view matches machine agent reality
for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Machine $ip status consistent" 'machine_api "$ip" GET /status | jq -e .machine_id'
done

# All users accessible
for user in alice bob charlie dave eve; do
    check "User $user accessible via coordinator" '
        [ "$(coord_api GET /api/users/'$user' | jq -r .status)" = "running" ]
    '
done

phase_result

# ══════════════════════════════════════════
# Phase 6: Data Isolation
# ══════════════════════════════════════════
phase_start 6 "Data Isolation"

# Write unique data to alice and bob
ALICE_PUB=$(get_public_ip "$(coord_api GET /api/users/alice | jq -r .primary_machine)")
BOB_PUB=$(get_public_ip "$(coord_api GET /api/users/bob | jq -r .primary_machine)")

check "Write data to alice" '
    docker_exec "$ALICE_PUB" alice-agent "sh -c \"echo alice-secret > /workspace/data/secret.txt\""
'
check "Write data to bob" '
    docker_exec "$BOB_PUB" bob-agent "sh -c \"echo bob-secret > /workspace/data/secret.txt\""
'
check "Alice reads her own data" '
    result=$(docker_exec "$ALICE_PUB" alice-agent "cat /workspace/data/secret.txt")
    [ "$result" = "alice-secret" ]
'
check "Bob reads his own data" '
    result=$(docker_exec "$BOB_PUB" bob-agent "cat /workspace/data/secret.txt")
    [ "$result" = "bob-secret" ]
'

# Verify DRBD replication status for a user
check "Alice DRBD healthy" '
    ALICE_PRIMARY_PUB=$(get_public_ip "$(coord_api GET /api/users/alice | jq -r .primary_machine)")
    machine_api "$ALICE_PRIMARY_PUB" GET /images/alice/drbd/status | jq -e ".peer_disk_state == \"UpToDate\""
'

phase_result

# ══════════════════════════════════════════
# Phase 7: Cleanup
# ══════════════════════════════════════════
phase_start 7 "Cleanup"

# Clean up all users via machine agent cleanup endpoints
for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Cleanup $ip" 'machine_api "$ip" POST /cleanup'
done

# Verify clean state
for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Machine $ip clean" '
        users=$(machine_api "$ip" GET /status | jq ".users | length")
        [ "$users" -eq 0 ]
    '
done

phase_result

# ══════════════════════════════════════════
final_result
```

**IMPORTANT NOTES FOR THE TEST SCRIPT:**

1. The `get_public_ip` helper needs to map machine IDs (like "fleet-1") to public IPs. This mapping comes from the .ips file and should be implemented in `common.sh`.

2. The `docker_exec` commands need `-u root` because the container runs as appuser, but writing files may need root. Check the Layer 4.1 test suite for how this was handled — they used `-u root` for write operations.

3. The `check` function uses `eval`, so quoting in the test commands needs care. Follow the patterns from Layer 4.1's test suite.

4. Adjust the check count as needed during implementation. The target is 55-70 total checks, but correctness matters more than hitting a number.

### `scfuture/scripts/layer-4.2/run.sh`

Main orchestration script:

```bash
#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCFUTURE_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"

echo "═══ Layer 4.2: Coordinator Happy Path ═══"
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
echo "Tearing down infrastructure..."
./infra.sh down

exit $TEST_RESULT
```

---

## Schema Documentation

Create `scfuture/schema.sql` as documentation for the future Postgres migration. This is NOT used by the code — it documents the intended relational schema:

```sql
-- scfuture schema — for reference when migrating to Postgres
-- Currently the coordinator uses in-memory state (Layer 4.2)

CREATE TABLE machines (
    machine_id      TEXT PRIMARY KEY,
    address         TEXT NOT NULL,
    public_address  TEXT,
    status          TEXT NOT NULL DEFAULT 'active',
    disk_total_mb   BIGINT NOT NULL DEFAULT 0,
    disk_used_mb    BIGINT NOT NULL DEFAULT 0,
    ram_total_mb    BIGINT NOT NULL DEFAULT 0,
    ram_used_mb     BIGINT NOT NULL DEFAULT 0,
    active_agents   INTEGER DEFAULT 0,
    max_agents      INTEGER DEFAULT 200,
    last_heartbeat  TIMESTAMP
);

CREATE TABLE users (
    user_id         TEXT PRIMARY KEY,
    status          TEXT NOT NULL DEFAULT 'registered',
    primary_machine TEXT REFERENCES machines(machine_id),
    drbd_port       INTEGER UNIQUE,
    image_size_mb   INTEGER DEFAULT 512,
    error           TEXT,
    created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE bipods (
    user_id         TEXT NOT NULL REFERENCES users(user_id),
    machine_id      TEXT NOT NULL REFERENCES machines(machine_id),
    role            TEXT NOT NULL,
    drbd_minor      INTEGER,
    loop_device     TEXT,
    created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, machine_id)
);

CREATE TABLE provisioning_log (
    id              SERIAL PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users(user_id),
    state           TEXT NOT NULL,
    details         TEXT,
    created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

---

## Critical Constraints (from SESSION.md — DO NOT VIOLATE)

1. **Promote before sync**: The provisioning state machine MUST call `drbd/promote` BEFORE waiting for DRBD sync. Sync does not begin until one side is Primary. (Issue #26)

2. **Device-mount pattern**: Containers mount Btrfs internally via the block device. The host NEVER mounts the user's Btrfs at runtime. `format-btrfs` temporarily mounts and unmounts — that's the only host mount.

3. **AppArmor unconfined**: Already baked into the machine agent's container start code. No action needed from the coordinator.

4. **DRBD-first ordering**: Images are blank when DRBD metadata is created. The provisioning flow naturally satisfies this: blank image → DRBD → format Btrfs.

5. **Hostname must match DRBD config**: The machine agent's hostname (from NODE_ID) MUST match the `on {hostname}` block in the DRBD config. The coordinator uses the machine's `machine_id` for both.

6. **Token-based DRBD parsing**: Already handled by the machine agent. Coordinator reads JSON responses.

7. **DRBD on separate kernels**: Each Hetzner Cloud server has its own kernel. DRBD works natively. Never try to run DRBD peers in containers on the same host.

8. **DRBD 9 on Ubuntu 24.04**: Requires LINBIT PPA. Cloud-init must install `linux-headers-$(uname -r)`, then `dkms autoinstall`, then `modprobe drbd`. (Issue #17)

9. **Teardown dependency chain**: Stop container → unmount → DRBD down (while config exists) → remove config → detach loop → delete image. (Issue #30)

---

## Directory Structure After Layer 4.2

```
scfuture/
├── go.mod                                      # (existing, unchanged)
├── Makefile                                    # (modified: add coordinator target)
├── schema.sql                                  # (new: Postgres schema documentation)
├── cmd/
│   ├── machine-agent/
│   │   └── main.go                             # (modified: add COORDINATOR_URL, NODE_ADDRESS, heartbeat)
│   └── coordinator/
│       └── main.go                             # (new)
├── internal/
│   ├── shared/
│   │   └── types.go                            # (modified: add fleet + coordinator types)
│   ├── machineagent/
│   │   ├── server.go                           # (existing, unchanged)
│   │   ├── images.go                           # (existing, unchanged)
│   │   ├── drbd.go                             # (existing, unchanged)
│   │   ├── btrfs.go                            # (existing, unchanged)
│   │   ├── containers.go                       # (existing, unchanged)
│   │   ├── state.go                            # (existing, unchanged)
│   │   ├── cleanup.go                          # (existing, unchanged)
│   │   ├── exec.go                             # (existing, unchanged)
│   │   └── heartbeat.go                        # (new: registration + heartbeat goroutine)
│   └── coordinator/
│       ├── server.go                           # (new)
│       ├── store.go                            # (new)
│       ├── provisioner.go                      # (new)
│       ├── fleet.go                            # (new)
│       └── machineapi.go                       # (new)
├── container/
│   ├── Dockerfile                              # (existing, unchanged)
│   └── container-init.sh                       # (existing, unchanged)
└── scripts/
    ├── (existing Layer 4.1 scripts)            # (unchanged)
    └── layer-4.2/
        ├── run.sh                              # (new)
        ├── common.sh                           # (new)
        ├── infra.sh                            # (new)
        ├── deploy.sh                           # (new)
        ├── test_suite.sh                       # (new)
        └── cloud-init/
            ├── fleet.yaml                      # (new)
            └── coordinator.yaml                # (new)
```

---

## Execution Instructions for Claude Code

### Phase 1: Write Code

1. Read ALL existing files listed in "Existing code" section above
2. Read `SESSION.md`, `architecture-v3.md`, and `scfuture-architecture.md` for context
3. Write all new files described above
4. Modify the existing files as described (main.go, types.go, Makefile)
5. Make sure everything compiles: `cd scfuture && go build ./...`
6. Report back: "All code written and compiling. Ready to run?"

### Phase 2: Run Tests

When told "yes" or "ready":

1. Run `cd scfuture/scripts/layer-4.2 && ./run.sh`
2. If tests fail:
   - Read the error output carefully
   - Fix the issue (code, script, or infra)
   - **Do NOT tear down infra between fix iterations** — modify `run.sh` to allow re-running just the test suite, or run `test_suite.sh` directly while infra is up
   - Re-run until all tests pass
3. If infra needs to be torn down and recreated, that's fine — but try to iterate without teardown first

### Phase 3: Update SESSION.md

When all tests pass:

1. Open `SESSION.md` (in the parent directory, NOT in scfuture/)
2. Add a new section under the PoC Progression Plan for Layer 4.2
3. Document:
   - What was built (coordinator, fleet management, provisioning state machine)
   - Test results (X/X checks passed)
   - Any issues encountered and how they were solved (numbered continuing from Issue #30)
   - Drift analysis: do any fixes deviate from the established architecture?
   - Changes that affect future layers
   - Updated PoC Progression Plan (mark 4.2 as ✅)
4. Follow the same format as existing SESSION.md entries

### Phase 4: Final Report

Give a summary:
- Total checks passed
- Issues encountered
- Any architectural discoveries
- Confirmation that SESSION.md has been updated
# scfuture — Architecture Document

**Layer:** 4.4 — Bipod Reformation & Dead Machine Re-integration
**Module:** `scfuture` (Go 1.22, standard library only, no external dependencies)
**Status:** 91/91 (Layer 4.4), 62/62 (Layer 4.3), 55/55 (Layer 4.2), 66/66 (Layer 4.1)
**Last updated:** 2026-03-01

---

## 1. System Overview

scfuture is a distributed agent platform consisting of two components:

1. **Coordinator** — a central HTTP server that manages fleet registration, user provisioning, placement, and orchestration across multiple machines.
2. **Machine Agent** — a per-machine HTTP agent that manages the full lifecycle of isolated user environments (images, DRBD, Btrfs, containers).

Each user gets:
1. A **sparse disk image** backed by a **loop device** on two machines
2. A **DRBD 9 replicated block device** (Protocol A, async) forming a bipod
3. A **Btrfs filesystem** with subvolumes and snapshots on the DRBD device
4. A **Docker container** that mounts the DRBD block device directly (device-mount pattern)

The coordinator drives provisioning by calling machine agent HTTP APIs. Machine agents self-register with the coordinator and send heartbeats every 10 seconds. The coordinator runs a health checker that detects machine failures and automatically fails over affected users. A reformer goroutine restores 2-copy replication after failover by provisioning new secondaries, and cleans up stale resources on machines that return from the dead.

### Key Design Decisions

- **Device-mount pattern:** Containers receive the raw `/dev/drbdN` device and mount Btrfs internally. The host never mounts the user's filesystem, eliminating host-path leakage in `/proc/mounts`.
- **Idempotent API:** Every endpoint returns success if the desired state already exists (`already_existed`, `already_formatted`). No endpoint fails on repeated calls.
- **Per-user locking:** Concurrent requests for different users proceed in parallel. Requests for the same user are serialized via `sync.Mutex` per user ID.
- **State discovery on startup (machine agent):** Rebuilds in-memory state from system reality (losetup, DRBD configs, DRBD status, mount table, Docker).
- **In-memory state with JSON persistence (coordinator):** All coordinator state lives in `sync.RWMutex`-protected maps. Persisted to `state.json` for debugging, not crash recovery.
- **Standard library only:** No external Go dependencies. No `go.sum` file.
- **Balanced placement:** Coordinator selects the 2 least-loaded active machines, excluding any above 85% disk usage.

---

## 2. Directory Structure

```
scfuture/
├── go.mod                                 # module scfuture, go 1.22
├── Makefile                               # build (linux/amd64) both binaries
├── .gitignore                             # bin/, scripts/.ips
├── architecture.md                        # this file
├── schema.sql                             # SQL schema reference (future Postgres migration)
│
├── cmd/
│   ├── machine-agent/
│   │   └── main.go                        # machine agent entrypoint
│   └── coordinator/
│       └── main.go                        # coordinator entrypoint
│
├── internal/
│   ├── shared/
│   │   └── types.go                       # all API request/response types (38 types)
│   ├── machineagent/
│   │   ├── server.go                      # HTTP routing, handlers, system info helpers
│   │   ├── images.go                      # loop device image management
│   │   ├── drbd.go                        # DRBD lifecycle + status parsing
│   │   ├── btrfs.go                       # Btrfs format + provisioning
│   │   ├── containers.go                  # Docker container lifecycle
│   │   ├── state.go                       # in-memory state, discovery
│   │   ├── cleanup.go                     # per-user and full-machine teardown
│   │   ├── exec.go                        # command execution helper
│   │   └── heartbeat.go                   # coordinator registration + heartbeat loop
│   └── coordinator/
│       ├── server.go                      # HTTP routing, handlers, Coordinator struct
│       ├── store.go                       # in-memory state store (machines, users, bipods, events)
│       ├── fleet.go                       # fleet register/heartbeat handling
│       ├── provisioner.go                 # 8-step provisioning state machine
│       ├── machineapi.go                  # HTTP client for calling machine agents
│       ├── healthcheck.go                 # health checker goroutine + failover logic
│       └── reformer.go                    # bipod reformation + stale cleanup goroutine
│
├── container/
│   ├── Dockerfile                         # alpine + btrfs-progs, appuser
│   └── container-init.sh                  # mount subvol, drop to appuser
│
├── bin/
│   ├── machine-agent                      # build output (linux/amd64)
│   └── coordinator                        # build output (linux/amd64)
│
└── scripts/
    ├── layer-4.1/                         # Layer 4.1 test infrastructure (2 machines)
    │   ├── run.sh, common.sh, infra.sh, deploy.sh, test_suite.sh
    │   └── cloud-init/fleet.yaml
    ├── layer-4.2/                         # Layer 4.2 test infrastructure (1 coord + 3 fleet)
    │   ├── run.sh, common.sh, infra.sh, deploy.sh, test_suite.sh
    │   └── cloud-init/
    │       ├── coordinator.yaml
    │       └── fleet.yaml
    ├── layer-4.3/                         # Layer 4.3 test infrastructure (1 coord + 3 fleet)
    │   ├── run.sh, common.sh, infra.sh, deploy.sh, test_suite.sh
    │   └── cloud-init/
    │       ├── coordinator.yaml
    │       └── fleet.yaml
    └── layer-4.4/                         # Layer 4.4 test infrastructure (1 coord + 3 fleet)
        ├── run.sh, common.sh, infra.sh, deploy.sh, test_suite.sh
        └── cloud-init/
            ├── coordinator.yaml
            └── fleet.yaml
```

---

## 3. Package: `internal/shared` — API Types

All types that cross the HTTP boundary live here. Used by both machine agent and coordinator.

### `types.go` — 38 type definitions

```go
// ─── Machine Agent: Image types ───
ImageCreateRequest       { ImageSizeMB int }
ImageCreateResponse      { LoopDevice, ImagePath string; AlreadyExisted bool }

// ─── Machine Agent: DRBD types ───
DRBDNode                 { Hostname string; Minor int; Disk, Address string }
DRBDCreateRequest        { ResourceName string; Nodes []DRBDNode; Port int }
DRBDCreateResponse       { AlreadyExisted bool }
DRBDPromoteResponse      { OK, AlreadyExisted bool }
DRBDDemoteResponse       { OK, AlreadyExisted bool }
DRBDStatusResponse       { Resource, Role, ConnectionState, DiskState, PeerDiskState string; SyncProgress *string; Exists bool }
DRBDDisconnectResponse   { Status string; WasConnected bool }                       // Layer 4.4
DRBDReconfigureRequest   { Nodes []DRBDNode; Port int; Force bool }                 // Layer 4.4
DRBDReconfigureResponse  { Status, Method string }                                  // Layer 4.4

// ─── Machine Agent: Btrfs types ───
FormatBtrfsResponse      { AlreadyFormatted bool }

// ─── Machine Agent: Container types ───
ContainerStartResponse   { ContainerName string; AlreadyExisted bool }
ContainerStatusResponse  { Exists, Running bool; ContainerName, StartedAt string }

// ─── Machine Agent: Status types ───
StatusResponse           { MachineID string; DiskTotalMB, DiskUsedMB, RAMTotalMB, RAMUsedMB int64; Users map[string]*UserStatusDTO }
UserStatusDTO            { ImageExists bool; ImagePath, LoopDevice string; DRBD* fields; HostMounted, ContainerRunning bool; ContainerName string }

// ─── Fleet types (machine agent → coordinator) ───
FleetRegisterRequest     { MachineID, Address string; DiskTotalMB, DiskUsedMB, RAMTotalMB, RAMUsedMB int64; MaxAgents int }
FleetHeartbeatRequest    { MachineID string; DiskTotalMB, DiskUsedMB, RAMTotalMB, RAMUsedMB int64; ActiveAgents int; RunningAgents []string }

// ─── Coordinator API types ───
CreateUserRequest        { UserID string; ImageSizeMB int }
CreateUserResponse       { UserID, Status string }
UserDetailResponse       { UserID, Status, PrimaryMachine string; DRBDPort int; Error string; Bipod []BipodEntry }
BipodEntry               { MachineID, Role string; DRBDMinor int; LoopDevice string }
FleetStatusResponse      { Machines []MachineStatus }
MachineStatus            { MachineID, Address, Status string; Disk*, RAM* int64; ActiveAgents, MaxAgents int; RunningAgents []string; LastHeartbeat string }

// ─── Failover types (coordinator healthcheck) ───
FailoverEventResponse    { UserID, FromMachine, ToMachine, Type string; Success bool; Error string; DurationMS int64; Timestamp string }

// ─── Reformation types (coordinator reformer) ─── Layer 4.4
ReformationEventResponse { UserID, OldSecondary, NewSecondary string; Success bool; Error, Method string; DurationMS int64; Timestamp string }
```

---

## 4. Package: `internal/coordinator` — Coordinator

### 4.1 Coordinator Struct & HTTP Routes (`server.go`)

```go
type Coordinator struct {
    store *Store
}

func NewCoordinator(dataDir string) *Coordinator
func (coord *Coordinator) RegisterRoutes(mux *http.ServeMux)
func (coord *Coordinator) GetStore() *Store
```

**Routes:**

| Method | Path | Handler | Description |
|--------|------|---------|-------------|
| `POST` | `/api/fleet/register` | `handleFleetRegister` | Machine agent self-registration |
| `POST` | `/api/fleet/heartbeat` | `handleFleetHeartbeat` | Machine agent heartbeat |
| `GET` | `/api/fleet` | `handleFleetStatus` | List all machines with status |
| `POST` | `/api/users` | `handleCreateUser` | Create user (status: registered) |
| `GET` | `/api/users` | `handleListUsers` | List all users with bipod details |
| `GET` | `/api/users/{id}` | `handleGetUser` | Get single user details |
| `POST` | `/api/users/{id}/provision` | `handleProvisionUser` | Start async provisioning |
| `GET` | `/api/users/{id}/bipod` | `handleGetBipod` | Get bipod details for user |
| `GET` | `/api/failovers` | `handleGetFailovers` | List all recorded failover events |
| `GET` | `/api/reformations` | `handleGetReformations` | List all recorded reformation events |

`handleProvisionUser` validates user is in `"registered"` state, sets status to `"provisioning"`, launches `coord.ProvisionUser(userID)` in a goroutine, and returns immediately.

### 4.2 State Store (`store.go`)

```go
type Store struct {
    mu                sync.RWMutex
    machines          map[string]*Machine
    users             map[string]*User
    bipods            map[string]*Bipod     // keyed by "{userID}:{machineID}"
    nextPort          int                   // starts at 7900
    nextMinor         map[string]int        // per-machine, starts at 0
    failoverEvents    []FailoverEvent
    reformationEvents []ReformationEvent    // Layer 4.4
    dataDir           string
}
```

**Machine:**
```go
type Machine struct {
    MachineID, Address, PublicAddress, Status string
    StatusChangedAt time.Time
    DiskTotalMB, DiskUsedMB, RAMTotalMB, RAMUsedMB int64
    ActiveAgents, MaxAgents int
    RunningAgents []string
    LastHeartbeat time.Time
}
```
Machine statuses: `"active"` → `"suspect"` (30s no heartbeat) → `"dead"` (60s no heartbeat) → `"active"` (heartbeat resumes).

**User:**
```go
type User struct {
    UserID, Status, PrimaryMachine string
    StatusChangedAt time.Time       // set on every status change (Layer 4.4)
    DRBDPort, ImageSizeMB int
    Error string
    CreatedAt time.Time
}
```
Statuses: `"registered"` → `"provisioning"` → `"running"` or `"failed"`.
Failover statuses: `"running"` → `"failing_over"` → `"running_degraded"` (primary died, now single-copy) or `"unavailable"` (total failure).
Secondary death: `"running"` → `"running_degraded"`.
Reformation statuses: `"running_degraded"` → `"reforming"` → `"running"` (bipod restored).

**Bipod:**
```go
type Bipod struct {
    UserID, MachineID, Role string  // Role: "primary", "secondary", or "stale"
    DRBDMinor int
    LoopDevice string
}
```

**FailoverEvent:**
```go
type FailoverEvent struct {
    UserID, FromMachine, ToMachine, Type string  // Type: "primary_failed", "secondary_failed", "both_dead"
    Success bool
    Error string
    DurationMS int64
    Timestamp time.Time
}
```

**ReformationEvent (Layer 4.4):**
```go
type ReformationEvent struct {
    UserID, OldSecondary, NewSecondary string
    Success bool
    Error, Method string   // Method: "adjust" or "down_up"
    DurationMS int64
    Timestamp time.Time
}
```

**StaleBipod (Layer 4.4):**
```go
type StaleBipod struct {
    UserID, MachineID string
}
```

**Key methods:**
```go
// Existing (Layer 4.2)
func NewStore(dataDir string) *Store
func (s *Store) RegisterMachine(req *shared.FleetRegisterRequest)
func (s *Store) UpdateHeartbeat(req *shared.FleetHeartbeatRequest)    // now handles resurrection
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
func (s *Store) SelectMachines() (primary, secondary *Machine, err error)

// New (Layer 4.3)
func (s *Store) CheckMachineHealth(suspectThreshold, deadThreshold time.Duration) []string
func (s *Store) GetUsersOnMachine(machineID string) []string
func (s *Store) GetSurvivingBipod(userID, deadMachineID string) *Bipod
func (s *Store) SetBipodRole(userID, machineID, role string)
func (s *Store) RecordFailoverEvent(event FailoverEvent)
func (s *Store) GetFailoverEvents() []FailoverEvent

// New (Layer 4.4)
func (s *Store) GetDegradedUsers(stabilizationPeriod time.Duration) []*User
func (s *Store) GetStaleBipodsOnActiveMachines(userID string) []StaleBipod
func (s *Store) GetAllStaleBipodsOnActiveMachines() []StaleBipod
func (s *Store) RemoveBipod(userID, machineID string)
func (s *Store) SelectOneSecondary(excludeIDs []string) (*Machine, error)
func (s *Store) RecordReformationEvent(event ReformationEvent)
func (s *Store) GetReformationEvents() []ReformationEvent
```

`SelectMachines` holds the write lock, filters active machines with <85% disk usage, sorts by `ActiveAgents` ascending, increments `ActiveAgents` on the selected pair to prevent double-placement.

`CheckMachineHealth` scans all machines, compares `time.Since(LastHeartbeat)` against thresholds, transitions statuses, returns list of machine IDs that just became `"dead"`.

`UpdateHeartbeat` now handles **resurrection**: if a heartbeat arrives from a `"dead"` or `"suspect"` machine, resets status to `"active"`. Stale bipod cleanup happens asynchronously via the reformer.

`GetSurvivingBipod` returns the bipod NOT on the dead machine, skipping bipods with role `"stale"`.

`GetDegradedUsers` returns users in `"running_degraded"` state whose `StatusChangedAt` is older than the stabilization period (prevents thrashing during cascading failures).

`GetAllStaleBipodsOnActiveMachines` returns all bipods with role `"stale"` on machines that are currently `"active"` — used by the reformer's cleanup pass to clean up resources on machines that have returned from the dead.

`SelectOneSecondary` is like `SelectMachines` but picks a single least-loaded active machine, excluding specified machine IDs (the current primary).

**Persistence:** `persist()` writes all state (including failover events) as JSON to `{dataDir}/state.json` after every mutation. This is for debugging only — the coordinator does NOT reload from state.json on restart.

### 4.3 Fleet Handling (`fleet.go`)

```go
func (coord *Coordinator) HandleRegister(req *shared.FleetRegisterRequest)
func (coord *Coordinator) HandleHeartbeat(req *shared.FleetHeartbeatRequest)
```

Thin wrappers that delegate to `store.RegisterMachine` and `store.UpdateHeartbeat`.

### 4.4 Provisioner (`provisioner.go`)

```go
func (coord *Coordinator) ProvisionUser(userID string)
```

Runs in its own goroutine (launched by `handleProvisionUser`). Drives the full provisioning state machine:

```
Step 1: SelectMachines() → pick 2 least-loaded
Step 2: CreateImage on both machines (retry once)
Step 3: DRBDCreate on both machines (retry once)
Step 4: DRBDPromote on primary (MUST happen before sync)
Step 5: Wait for DRBD sync (poll DRBDStatus until PeerDiskState=UpToDate, 120s timeout)
Step 6: FormatBtrfs on primary (retry once)
Step 7: ContainerStart on primary (retry once)
Step 8: SetUserStatus → "running"
```

Each step uses a `retry` helper that retries once after 2s on failure. On failure after retry, sets user status to `"failed"` with error message.

Helper: `stripPort(address string) string` — extracts IP from `"10.0.0.11:8080"` for DRBD config addresses.

### 4.5 Machine API Client (`machineapi.go`)

```go
type MachineClient struct {
    address string          // e.g., "10.0.0.11:8080"
    client  *http.Client    // 30s timeout
}

func NewMachineClient(address string) *MachineClient

// Typed wrappers around machine agent endpoints:
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

// New (Layer 4.4)
func (c *MachineClient) DRBDDisconnect(userID string) (*shared.DRBDDisconnectResponse, error)
func (c *MachineClient) DRBDReconfigure(userID string, req *shared.DRBDReconfigureRequest) (*shared.DRBDReconfigureResponse, error)
func (c *MachineClient) DRBDDestroy(userID string) error

func (c *MachineClient) doJSON(method, path string, reqBody, respBody interface{}) error
```

`doJSON` handles marshal/unmarshal, HTTP errors, and non-2xx status codes.

### 4.6 Health Checker & Failover (`healthcheck.go`)

```go
const (
    HealthCheckInterval = 10 * time.Second
    SuspectThreshold    = 30 * time.Second  // 3 missed heartbeats
    DeadThreshold       = 60 * time.Second  // 6 missed heartbeats
)

func StartHealthChecker(store *Store, client *MachineClient, coord *Coordinator)
func (coord *Coordinator) failoverMachine(deadMachineID string)
func (coord *Coordinator) failoverUser(userID, deadMachineID string)
```

**StartHealthChecker** launches a background goroutine that ticks every 10 seconds. On each tick, calls `store.CheckMachineHealth()`. For each newly-dead machine, spawns a goroutine calling `failoverMachine()`.

**failoverMachine** gets all users on the dead machine via `GetUsersOnMachine()`, then calls `failoverUser()` for each.

**failoverUser** handles two cases:

1. **Primary died** → Set user `"failing_over"`, mark dead bipod `"stale"`, promote DRBD on surviving machine (`--force`), start container, update bipod roles and primary, set user `"running"`. On promote failure → `"unavailable"`. On container failure → `"running_degraded"`.

2. **Secondary died** → Mark dead bipod `"stale"`, set user `"running_degraded"`. No DRBD or container actions needed.

Each failover records a `FailoverEvent` with timing.

**Concurrency:** Each dead machine's failover runs in its own goroutine. The health checker is never blocked by a slow failover. User failovers within a machine are sequential (simple, avoids race conditions on the same machine's resources).

**Idempotency:** `failoverUser` checks user status before acting — skips if not `"running"` or `"running_degraded"`. DRBD promote with `--force` returns success if already Primary.

**Key fix (Layer 4.4):** Primary-died failover now sets user status to `"running_degraded"` (not `"running"`) after successful promotion. This is critical — without it, the reformer would never pick up users whose primary died because it scans for `"running_degraded"` users.

### 4.7 Reformer (`reformer.go`) — Layer 4.4

```go
const (
    ReformerInterval    = 30 * time.Second
    StabilizationPeriod = 30 * time.Second  // wait after status change before reforming
    SyncTimeout         = 300 * time.Second // 5 minutes for initial DRBD sync
)

func StartReformer(store *Store, coord *Coordinator)
func (coord *Coordinator) cleanStaleBipodsOnActiveMachines()
func (coord *Coordinator) reformDegradedUsers()
func (coord *Coordinator) reformUser(userID string)
func reformerStripPort(address string) string
```

**StartReformer** launches a background goroutine that ticks every 30 seconds. On each tick, runs two independent passes:

1. **`cleanStaleBipodsOnActiveMachines`** — finds ALL stale bipods on machines that have returned to `"active"` status. For each: calls `DRBDDestroy` + `DeleteUser` on the machine, then `RemoveBipod` in the store. Runs every tick, independent of user status. This handles the case where a user has already been reformed to "running" but the dead machine returns later with orphaned resources.

2. **`reformDegradedUsers`** — finds users in `"running_degraded"` state past the stabilization period (30s since `StatusChangedAt`). Calls `reformUser` for each.

**reformUser** — 8-step reformation sequence:

```
Step 0: Clean stale bipods on active machines (for this user specifically)
Step 1: Set user status → "reforming"
Step 2: SelectOneSecondary (excluding current primary) → pick new machine, allocate minor
Step 3: CreateImage on new secondary
Step 4: DRBDCreate on new secondary (same config as primary, new peer address)
Step 5: DRBDDisconnect on primary (disconnect dead peer — idempotent, handles StandAlone)
Step 6: DRBDReconfigure on primary (rewrite config pointing to new peer):
        → Try drbdadm adjust first (zero downtime)
        → If adjust fails: stop container, force reconfigure (down/up/promote), restart container
Step 7: Wait for DRBD sync (poll DRBDStatus until PeerDiskState=UpToDate, 5min timeout)
Step 8: CreateBipod in store, set user status → "running"
```

Each step records a `ReformationEvent` on success or failure with timing and method used.

**Key finding:** `drbdadm adjust` works for live peer replacement — the primary's container keeps running throughout. The down/up fallback has never been needed in testing. Reformation takes ~8.2 seconds per user (dominated by DRBD sync of the disk image).

---

## 5. Package: `internal/machineagent` — Machine Agent

### 5.1 Internal Types

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
    Stdout, Stderr string
    ExitCode       int
}
```

### 5.2 Function Signatures by File

#### `exec.go` — Command Execution

```go
func runCmd(name string, args ...string) (*CmdResult, error)
func cmdString(name string, args ...string) string
func cmdError(msg, command string, result *CmdResult) error
```

#### `state.go` — State Management & Discovery

```go
func NewAgent(nodeID, dataDir string) *Agent
func (a *Agent) getUserLock(userID string) *sync.Mutex
func (a *Agent) getUser(userID string) *UserResources
func (a *Agent) setUser(userID string, u *UserResources)
func (a *Agent) deleteUser(userID string)
func (a *Agent) allUsers() map[string]*UserResources    // deep copy
func (a *Agent) imagePath(userID string) string          // {dataDir}/images/{userID}.img
func (a *Agent) mountPath(userID string) string          // /mnt/users/{userID}
func (a *Agent) Discover()
func (a *Agent) ensureUser(userID string) *UserResources // caller must hold usersMu
```

Discovery scans: `losetup -a`, `/etc/drbd.d/user-*.res`, `drbdadm status all`, `mount`, `docker ps`.

#### `images.go` — Disk Image Lifecycle

```go
func validateUserID(userID string) error                  // [a-zA-Z0-9-]{3,32}
func (a *Agent) CreateImage(userID string, sizeMB int) (*shared.ImageCreateResponse, error)
func (a *Agent) findLoopDevice(imgPath string) string
func (a *Agent) attachLoop(imgPath string) (string, error)
```

#### `drbd.go` — DRBD 9 Lifecycle

```go
func (a *Agent) DRBDCreate(userID string, req *shared.DRBDCreateRequest) (*shared.DRBDCreateResponse, error)
func (a *Agent) DRBDPromote(userID string) (map[string]interface{}, error)
func (a *Agent) DRBDDemote(userID string) (map[string]interface{}, error)
func (a *Agent) DRBDStatus(userID string) (*shared.DRBDStatusResponse, error)
func (a *Agent) DRBDDestroy(userID string) error
func (a *Agent) DRBDDisconnect(userID string) (*shared.DRBDDisconnectResponse, error)      // Layer 4.4
func (a *Agent) DRBDReconfigure(userID string, req *shared.DRBDReconfigureRequest) (*shared.DRBDReconfigureResponse, error) // Layer 4.4
func (a *Agent) getDRBDStatus(resName string) *DRBDInfo
func parseDRBDStatusAll(output string) map[string]*DRBDInfo
func splitResourceBlocks(output string) []string
func isMounted(path string) bool
```

**DRBDPromote** uses `drbdadm primary --force {resName}`. The `--force` flag allows promotion without a connected peer (needed for initial setup and failover).

**DRBDDisconnect** (Layer 4.4) runs `drbdadm disconnect {resName}`. Idempotent — returns success if already StandAlone (no connected peer). Used to cleanly disconnect a dead peer before reconfiguring to a new one.

**DRBDReconfigure** (Layer 4.4) rewrites the DRBD config file with new peer info, then:
- If `Force=false`: runs `drbdadm adjust {resName}`. DRBD reads the new config, sees the new peer address, and connects. **Zero downtime** — the container keeps running.
- If `Force=true`: unmounts host if mounted, runs `drbdadm down`, `drbdadm up`, `drbdadm primary --force`. Used as fallback if adjust fails (requires container stop/start by the coordinator).

**DRBD config:** Protocol A, internal meta-disk, resource name `user-{userID}`.

#### `btrfs.go` — Filesystem Provisioning

```go
func (a *Agent) FormatBtrfs(userID string) (*shared.FormatBtrfsResponse, error)
```

Formats DRBD device, creates `workspace/` subvol with seed dirs (`memory/`, `apps/`, `data/`), writes `data/config.json`, creates `snapshots/layer-000`, then **unmounts** (host does NOT keep Btrfs mounted).

#### `containers.go` — Docker Container Lifecycle

```go
func (a *Agent) ContainerStart(userID string) (*shared.ContainerStartResponse, error)
func (a *Agent) ContainerStop(userID string) error
func (a *Agent) ContainerStatus(userID string) (*shared.ContainerStatusResponse, error)
```

Container name: `{userID}-agent`. Docker run flags: `--device {drbdDev}`, `--cap-drop ALL`, `--cap-add SYS_ADMIN,SETUID,SETGID`, `--security-opt apparmor=unconfined`, `--network none`, `--memory 64m`.

#### `cleanup.go` — Teardown

```go
func (a *Agent) DeleteUser(userID string) error   // reverse-order teardown of one user
func (a *Agent) Cleanup() error                   // tear down ALL users on the machine
```

#### `heartbeat.go` — Coordinator Registration & Heartbeat

```go
func (a *Agent) StartHeartbeat(coordinatorURL, nodeAddress string)
```

Runs in a goroutine. Registers with coordinator (retries every 5s until success), then sends heartbeats every 10s with disk/RAM metrics and running agent list. Called from `main.go` only if `COORDINATOR_URL` env var is set.

#### `server.go` — HTTP API

13 endpoints. See API Reference below.

### 5.3 Machine Agent HTTP API

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
| `POST` | `/images/{user_id}/drbd/disconnect` | — | `DRBDDisconnectResponse` | Yes |
| `POST` | `/images/{user_id}/drbd/reconfigure` | `DRBDReconfigureRequest` | `DRBDReconfigureResponse` | Yes |
| `POST` | `/images/{user_id}/format-btrfs` | — | `FormatBtrfsResponse` | Yes |
| `POST` | `/containers/{user_id}/start` | — | `ContainerStartResponse` | Yes |
| `POST` | `/containers/{user_id}/stop` | — | `{"ok": true}` | Yes |
| `GET` | `/containers/{user_id}/status` | — | `ContainerStatusResponse` | No |
| `POST` | `/cleanup` | — | `{"ok": true}` | No |

---

## 6. Entrypoints

### `cmd/coordinator/main.go`

1. JSON structured logging
2. Reads env: `LISTEN_ADDR` (default `0.0.0.0:8080`), `DATA_DIR` (default `/data`)
3. Creates `Coordinator` via `NewCoordinator(dataDir)`
4. Starts health checker goroutine via `StartHealthChecker(coord.GetStore(), ...)`
5. Starts reformer goroutine via `StartReformer(coord.GetStore(), coord)`
6. Registers routes, starts `http.ListenAndServe`

### `cmd/machine-agent/main.go`

1. JSON structured logging
2. Reads env: `NODE_ID` (required), `LISTEN_ADDR`, `DATA_DIR`, `NODE_ADDRESS`, `COORDINATOR_URL`
3. Creates `Agent`, calls `Discover()` and `EnsureContainerImage()`
4. Registers routes
5. If `COORDINATOR_URL` set → starts heartbeat goroutine with `NODE_ADDRESS`
6. Starts `http.ListenAndServe`

---

## 7. Container Image: `platform/app-container`

**Dockerfile:** alpine + btrfs-progs, creates `appuser`, entrypoint: `container-init.sh`

**container-init.sh:** mount Btrfs subvol → `exec su appuser` (drops all capabilities)

---

## 8. Infrastructure & Deployment

### Layer 4.4 Topology

- 1x coordinator (`l44-coordinator`, private `10.0.0.2`)
- 3x fleet machines (`l44-fleet-{1,2,3}`, private IPs auto-assigned)
- All Hetzner Cloud `cx23`, Ubuntu 24.04, private network `10.0.0.0/24`
- Same topology as Layer 4.2/4.3 (with `l44-` prefix)

### Deployment Flow (`scripts/layer-4.4/deploy.sh`)

**Coordinator:**
1. Wait for SSH + cloud-init
2. `scp` coordinator binary to `/usr/local/bin/coordinator`
3. Set hostname, `systemctl enable --now coordinator`

**Fleet machines (each):**
1. Wait for SSH + cloud-init
2. `scp` machine-agent binary + container files
3. Set hostname, configure systemd with `NODE_ID`, `NODE_ADDRESS`, `COORDINATOR_URL`
4. Build `platform/app-container` image, verify DRBD module, `systemctl enable --now machine-agent`

Note: `enable --now` (not just `start`) ensures services auto-restart on reboot — critical for the dead machine return test.

### Systemd Units

**Coordinator:**
```ini
ExecStart=/usr/local/bin/coordinator
Environment=LISTEN_ADDR=0.0.0.0:8080
Environment=DATA_DIR=/data
```

**Machine Agent:**
```ini
ExecStart=/usr/local/bin/machine-agent
Environment=NODE_ID={node-id}
Environment=LISTEN_ADDR=0.0.0.0:8080
Environment=DATA_DIR=/data
Environment=NODE_ADDRESS={private-ip}:8080
Environment=COORDINATOR_URL=http://10.0.0.2:8080
```

### Storage Layout (each fleet machine)

```
/data/images/{userID}.img          # sparse disk images
/mnt/users/{userID}/               # temporary mount point (format only)
/etc/drbd.d/user-{userID}.res     # DRBD config files
/opt/platform/container/           # Dockerfile + container-init.sh
/usr/local/bin/machine-agent       # agent binary
```

### Storage Layout (coordinator)

```
/data/state.json                   # JSON state dump (debug only)
/usr/local/bin/coordinator         # coordinator binary
```

---

## 9. Provisioning Flow

### Full Stack (coordinator-driven)

```
POST /api/users {"user_id":"alice"}          → creates user (registered)
POST /api/users/alice/provision              → launches async provisioning
  ↓ (goroutine)
  Step 1: SelectMachines() → fleet-1 (primary), fleet-2 (secondary)
  Step 2: POST /images/alice/create          → both machines (get loop devices)
  Step 3: POST /images/alice/drbd/create     → both machines (with DRBD config)
  Step 4: POST /images/alice/drbd/promote    → primary only (BEFORE sync)
  Step 5: Poll GET /images/alice/drbd/status → wait for PeerDiskState=UpToDate
  Step 6: POST /images/alice/format-btrfs    → primary only
  Step 7: POST /containers/alice/start       → primary only
  Step 8: SetUserStatus("running")
```

### Automatic Failover (Layer 4.3 — coordinator-driven)

```
Health checker detects machine heartbeat timeout (60s)
  ↓ (goroutine per dead machine)
  For each user with bipod on dead machine:
    Case 1: Primary died
      → Set user "failing_over"
      → Mark dead bipod "stale"
      → POST /images/{user}/drbd/promote  → surviving machine (--force)
      → POST /containers/{user}/start     → surviving machine
      → Update bipod roles, user primary
      → Set user "running"
    Case 2: Secondary died
      → Mark dead bipod "stale"
      → Set user "running_degraded"
```

### Automatic Bipod Reformation (Layer 4.4 — reformer-driven)

```
Reformer goroutine ticks every 30 seconds:
  Pass 1: Clean stale bipods on active machines
    → For each stale bipod on a machine that is now "active":
      → DRBDDestroy + DeleteUser on the machine
      → RemoveBipod in store
  Pass 2: Reform degraded users (past 30s stabilization)
    → For each user in "running_degraded":
      → Set user "reforming"
      → SelectOneSecondary (exclude primary)
      → CreateImage on new secondary
      → DRBDCreate on new secondary
      → DRBDDisconnect on primary (disconnect dead peer)
      → DRBDReconfigure on primary (adjust to new peer — zero downtime)
      → Wait for DRBD sync (PeerDiskState=UpToDate)
      → CreateBipod, set user "running"
```

### Manual Failover (Layer 4.1 pattern, still works)

```
POST /containers/{user}/stop      → current primary
POST /images/{user}/drbd/demote   → current primary
POST /images/{user}/drbd/promote  → new primary
POST /containers/{user}/start     → new primary
```

---

## 10. Test Suites

### Layer 4.1 — Machine Agent (66 checks, 9 phases)

Tests machine agent directly (no coordinator). 2 Hetzner servers.

| Phase | Checks | What |
|-------|--------|------|
| 0 | 8 | Prerequisites |
| 1 | 10 | Single user provisioning (full stack) |
| 2 | 5 | Device-mount verification |
| 3 | 4 | Data write + DRBD replication |
| 4 | 8 | Manual failover via API |
| 5 | 8 | Idempotency |
| 6 | 8 | Full teardown |
| 7 | 10 | Multi-user density (3 users) |
| 8 | 5 | Status endpoint accuracy |

### Layer 4.2 — Coordinator Happy Path (55 checks, 8 phases)

Tests coordinator-driven provisioning. 4 Hetzner servers (1 coord + 3 fleet).

| Phase | Checks | What |
|-------|--------|------|
| 0 | 11 | Prerequisites (coordinator, 3 machines, DRBD, images) |
| 1 | 8 | Provision first user (alice) — full verification |
| 2 | 4 | Provision second user (bob) — placement diversity |
| 3 | 2 | Provision third user (charlie) |
| 4 | 5 | Provision dave and eve — 5 users total |
| 5 | 9 | Fleet status — balanced placement, consistency |
| 6 | 5 | Data isolation — unique writes, DRBD health |
| 7 | 6 | Cleanup — all machines clean |

### Layer 4.3 — Heartbeat Failure Detection & Failover (62 checks, 9 phases)

Tests automatic failure detection and failover. 4 Hetzner servers (1 coord + 3 fleet). Kills fleet-1 mid-test.

| Phase | Checks | What |
|-------|--------|------|
| 0 | 13 | Prerequisites (coordinator, 3 machines, DRBD, images, no failover events) |
| 1 | 12 | Provision 3 users, write test data, verify DRBD healthy |
| 2 | 2 | Kill fleet-1 via `hcloud server shutdown` |
| 3 | 3 | Wait for coordinator to detect fleet-1 as dead |
| 4 | 8 | Verify automatic failover (DRBD promote, container start, events) |
| 5 | 9 | Data integrity — pre-failover data survived, new writes work |
| 6 | 5 | Degraded state — secondary loss handled, bipod roles correct |
| 7 | 6 | State consistency — fleet status, no stale primaries, valid states |
| 8 | 4 | Cleanup surviving machines |

### Layer 4.4 — Bipod Reformation & Dead Machine Return (91 checks, 11 phases)

Tests automatic reformation after failover and stale cleanup after dead machine returns. 4 Hetzner servers (1 coord + 3 fleet). Kills fleet-1, waits for failover + reformation, then powers fleet-1 back on and verifies cleanup.

| Phase | Checks | What |
|-------|--------|------|
| 0 | 14 | Prerequisites (coordinator, 3 machines, DRBD, images, no events) |
| 1 | 15 | Provision 3 users, write test data, verify DRBD healthy |
| 2 | 2 | Kill fleet-1 via `hcloud server shutdown` |
| 3 | 6 | Failure detection + automatic failover |
| 4 | 9 | Verify degraded state (placement-aware — only checks users with bipods on fleet-1) |
| 5 | 9 | Wait for bipod reformation (users transition running_degraded → reforming → running) |
| 6 | 12 | DRBD sync complete + data integrity (pre-failover data survived, new writes work) |
| 7 | 7 | Dead machine return (power on fleet-1, wait for active, verify stale bipod cleanup) |
| 8 | 8 | Coordinator state consistency (all users running, 2 bipods each, no stale bipods) |
| 9 | 3 | Reformation events recorded with correct structure |
| 10 | 6 | Cleanup surviving machines |

### Test Helpers (`common.sh`)

```bash
coord_api METHOD /path [body]                    # curl to coordinator via public IP
machine_api $ip METHOD /path [body]              # curl to machine agent via public IP
ssh_cmd $ip "command"                            # SSH as root
docker_exec $ip container cmd                    # docker exec via SSH
check "description" 'test_command'               # assertion with counters
wait_for_user_status user_id status timeout      # poll coordinator until user reaches status
wait_for_machine_status machine_id status timeout # poll coordinator until machine reaches status
wait_for_user_status_multi user_id s1 s2 timeout # poll until user reaches either status
wait_for_user_bipod_count user_id count timeout  # poll until user has N non-stale bipods
get_public_ip machine_id                         # map fleet-N → public IP
phase_start/phase_result/final_result            # test framework
```

---

## 11. Conventions & Constraints

- **User ID format:** 3-32 chars, `[a-zA-Z0-9-]` only
- **DRBD resource name:** `user-{userID}`
- **Container name:** `{userID}-agent`
- **DRBD port:** sequential from 7900
- **DRBD minor:** sequential per-machine from 0
- **Image path:** `/data/images/{userID}.img`
- **DRBD config:** `/etc/drbd.d/user-{userID}.res`
- **Private network:** `10.0.0.0/24` — coordinator at `.2`, fleet at `.11-.13`
- **No host-level Btrfs mounts at runtime**
- **Build target:** `GOOS=linux GOARCH=amd64`
- **DRBD promote-before-sync** — always promote primary before waiting for sync
- **Protocol A** — async replication, last few seconds of writes may be lost on failover
- **Machine statuses:** `active` → `suspect` (30s) → `dead` (60s) → `active` (resurrection)
- **User statuses:** `registered` → `provisioning` → `running` / `failed` / `failing_over` / `running_degraded` / `reforming` / `unavailable`
- **Bipod roles:** `primary`, `secondary`, `stale` (machine dead, bipod no longer valid)
- **Failover is idempotent** — skips users not in `running` or `running_degraded`
- **Failover does not block health checker** — each dead machine gets its own goroutine
- **Primary-died failover sets `running_degraded`** — not `running`, so the reformer picks them up
- **Reformation uses `drbdadm adjust`** — zero downtime peer replacement on live primary
- **Stale cleanup is independent of user status** — runs on every reformer tick, not just during reformation
- **Reformation has a 30s stabilization period** — prevents thrashing during cascading failures
- **Internal states must not leak to users** — `running_degraded`, `failing_over`, `reforming` are operational; user-facing APIs must map them to `running`

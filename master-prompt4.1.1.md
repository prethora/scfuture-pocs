# Prompt: Establish scfuture Project from Proven Layer 4.1 Code

## Context

We are transitioning from proof-of-concept territory into layered implementation. The `poc-coordinator/` directory contains a proven, working machine agent (Go HTTP server, 66/66 test checks passed) that wraps the full block device stack (losetup, DRBD, Btrfs, Docker device-mount containers) behind idempotent API endpoints.

This prompt establishes the permanent project repository: `scfuture/`. From this point forward, layers are stacked via git commits on the same codebase — not by creating new directories. Layer 4.2 will build ON TOP of this code, not alongside it.

## What You're Doing

1. Create `scfuture/` directory with the same Go module structure
2. Copy ALL code from `poc-coordinator/` into `scfuture/`
3. Change the Go module name from `poc-coordinator` to `scfuture`
4. Update all import paths accordingly
5. Extract HTTP API types into a new `internal/shared/` package
6. Add two missing typed responses for DRBD promote/demote (currently `map[string]interface{}`)
7. Verify it compiles

## Critical Rules

1. **Do NOT modify any business logic, control flow, error handling, or system command invocations.** The code is proven. You are reorganizing, not rewriting.
2. **Do NOT change the container launch command, DRBD config generation, status parsing, or any `runCmd` calls.**
3. **Do NOT add external dependencies.** Standard library only. No `go.sum`.
4. **Copy `container/`, `scripts/`, and `scripts/cloud-init/` files byte-for-byte.** Do not modify them at all.
5. **Do NOT create a coordinator package, test files, or README.** Those come in future layers.

## Target Directory Structure

```
scfuture/
├── go.mod                              (module: scfuture, go 1.22)
├── Makefile
├── .gitignore
├── cmd/
│   └── machine-agent/
│       └── main.go                     # Only change: import path
├── internal/
│   ├── shared/
│   │   └── types.go                    # NEW: API request/response types
│   └── machineagent/
│       ├── server.go                   # Uses shared types
│       ├── images.go                   # Uses shared types
│       ├── drbd.go                     # Uses shared types
│       ├── btrfs.go                    # Uses shared types
│       ├── containers.go              # Uses shared types
│       ├── state.go                    # Unchanged (internal types)
│       ├── cleanup.go                  # Unchanged
│       └── exec.go                     # Unchanged
├── container/
│   ├── Dockerfile                      # Exact copy from poc-coordinator
│   └── container-init.sh              # Exact copy from poc-coordinator
└── scripts/
    ├── run.sh                          # Exact copy from poc-coordinator
    ├── common.sh                       # Exact copy from poc-coordinator
    ├── infra.sh                        # Exact copy from poc-coordinator
    ├── deploy.sh                       # Exact copy from poc-coordinator
    ├── test_suite.sh                  # Exact copy from poc-coordinator
    └── cloud-init/
        └── fleet.yaml                  # Exact copy from poc-coordinator
```

## Step-by-Step

### Step 1: Create directories

```bash
mkdir -p scfuture/{cmd/machine-agent,internal/shared,internal/machineagent,container,scripts/cloud-init,bin}
```

### Step 2: Copy unchanged files (byte-for-byte)

```bash
cp poc-coordinator/container/Dockerfile scfuture/container/
cp poc-coordinator/container/container-init.sh scfuture/container/
cp poc-coordinator/scripts/run.sh scfuture/scripts/
cp poc-coordinator/scripts/common.sh scfuture/scripts/
cp poc-coordinator/scripts/infra.sh scfuture/scripts/
cp poc-coordinator/scripts/deploy.sh scfuture/scripts/
cp poc-coordinator/scripts/test_suite.sh scfuture/scripts/
cp poc-coordinator/scripts/cloud-init/fleet.yaml scfuture/scripts/cloud-init/
chmod +x scfuture/scripts/*.sh scfuture/container/container-init.sh
```

### Step 3: Create `go.mod`

```go
module scfuture

go 1.22
```

### Step 4: Create `.gitignore`

```
bin/
scripts/.ips
```

### Step 5: Create `Makefile`

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

### Step 6: Create `internal/shared/types.go`

This file contains ALL types that cross the HTTP API boundary between the machine agent and its callers. These are extracted from `images.go`, `drbd.go`, `btrfs.go`, `containers.go`, and `server.go`. The field names, types, and JSON tags must be exactly as shown — the test suite validates these via `jq`.

```go
package shared

// ─── Image types (from images.go) ───

type ImageCreateRequest struct {
	ImageSizeMB int `json:"image_size_mb"`
}

type ImageCreateResponse struct {
	LoopDevice     string `json:"loop_device"`
	ImagePath      string `json:"image_path"`
	AlreadyExisted bool   `json:"already_existed"`
}

// ─── DRBD types (from drbd.go) ───

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

// ─── Btrfs types (from btrfs.go) ───

type FormatBtrfsResponse struct {
	AlreadyFormatted bool `json:"already_formatted"`
}

// ─── Container types (from containers.go) ───

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

// ─── Status types (from server.go) ───

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

### Step 7: Update `cmd/machine-agent/main.go`

Copy from `poc-coordinator/cmd/machine-agent/main.go`. The ONLY change is the import path:

```
OLD: "poc-coordinator/internal/machineagent"
NEW: "scfuture/internal/machineagent"
```

Everything else stays identical.

### Step 8: Update `internal/machineagent/` files

Copy all 8 files from `poc-coordinator/internal/machineagent/`. Apply these changes:

**Files that need changes:**

**`images.go`:**
- Remove the `ImageCreateRequest` and `ImageCreateResponse` type definitions
- Add `import "scfuture/internal/shared"`
- Replace all references: `ImageCreateResponse` → `shared.ImageCreateResponse`, `ImageCreateRequest` → `shared.ImageCreateRequest`
- The `validateUserID` function and `validUserID` regex stay in this file (they're internal validation, not API types)

**`drbd.go`:**
- Remove the `DRBDNode`, `DRBDCreateRequest`, `DRBDCreateResponse`, `DRBDStatusResponse` type definitions
- Add `import "scfuture/internal/shared"`
- Replace all references with `shared.` prefix
- `DRBDInfo` stays in this file (internal parser type, not part of API)
- Change `DRBDPromote` return type from `(map[string]interface{}, error)` to `(*shared.DRBDPromoteResponse, error)`:
  - Where it returns `map[string]interface{}{"already_existed": true}`, return `&shared.DRBDPromoteResponse{AlreadyExisted: true}`
  - Where it returns `map[string]interface{}{"ok": true}`, return `&shared.DRBDPromoteResponse{OK: true}`
- Same change for `DRBDDemote` → return `(*shared.DRBDDemoteResponse, error)`
- `isMounted` function stays (internal helper)

**`btrfs.go`:**
- Remove the `FormatBtrfsResponse` type definition
- Add `import "scfuture/internal/shared"`
- Replace: `FormatBtrfsResponse` → `shared.FormatBtrfsResponse`

**`containers.go`:**
- Remove the `ContainerStartResponse` and `ContainerStatusResponse` type definitions
- Add `import "scfuture/internal/shared"`
- Replace all references with `shared.` prefix

**`server.go`:**
- Remove the `StatusResponse` and `UserStatusDTO` type definitions
- Add `import "scfuture/internal/shared"`
- Replace all references with `shared.` prefix
- Update `handleDRBDPromote` and `handleDRBDDemote` — they no longer need special handling since the return types are now proper structs (just pass them to `writeJSON` like all other handlers)
- All helper functions (`writeJSON`, `writeError`, `getDiskTotalMB`, etc.) stay in this file

**Files that need NO changes (besides ensuring they compile):**

**`state.go`:** No API types here. `UserResources`, `Agent`, and all discovery methods stay as-is. No shared import needed.

**`cleanup.go`:** No API types. Stays as-is.

**`exec.go`:** `CmdResult`, `runCmd`, `cmdString`, `cmdError` all stay. No shared import needed.

### Step 9: Verify compilation

```bash
cd scfuture
go build ./...
```

This MUST succeed with zero errors. If there are compilation issues, they will be import path mismatches or missed type references — fix those only. Do not change logic.

### Step 10: Verify structure

```bash
find scfuture -type f | sort
```

Expected output should show:
- `scfuture/go.mod` — module name `scfuture`
- `scfuture/internal/shared/types.go` — 13 type definitions
- `scfuture/internal/machineagent/*.go` — 8 files
- `scfuture/container/` — 2 files
- `scfuture/scripts/` — 6 files + cloud-init subdirectory
- No `go.sum`

### Step 11: Verify byte-identical copies

```bash
diff scfuture/container/Dockerfile poc-coordinator/container/Dockerfile
diff scfuture/container/container-init.sh poc-coordinator/container/container-init.sh
diff scfuture/scripts/run.sh poc-coordinator/scripts/run.sh
diff scfuture/scripts/common.sh poc-coordinator/scripts/common.sh
diff scfuture/scripts/infra.sh poc-coordinator/scripts/infra.sh
diff scfuture/scripts/deploy.sh poc-coordinator/scripts/deploy.sh
diff scfuture/scripts/test_suite.sh poc-coordinator/scripts/test_suite.sh
diff scfuture/scripts/cloud-init/fleet.yaml poc-coordinator/scripts/cloud-init/fleet.yaml
```

ALL diffs must produce zero output.

## What NOT To Do

- Do not create any packages beyond `shared` and `machineagent`
- Do not create `internal/coordinator/` (that's Layer 4.2)
- Do not create `_test.go` files
- Do not add TODO comments about future layers
- Do not rename any functions or methods
- Do not change any JSON tag names
- Do not abstract or generalize any code
- Do not modify any script or container files
- Do not create a README
- Do not change any `runCmd` invocations or their arguments
- Do not change the container launch flags (`--cap-drop ALL --cap-add SYS_ADMIN --cap-add SETUID --cap-add SETGID --security-opt apparmor=unconfined --network none --memory 64m`)

## Summary of Type Movements

| Type | From | To | Notes |
|------|------|----|-------|
| `ImageCreateRequest` | images.go | shared/types.go | |
| `ImageCreateResponse` | images.go | shared/types.go | |
| `DRBDNode` | drbd.go | shared/types.go | |
| `DRBDCreateRequest` | drbd.go | shared/types.go | |
| `DRBDCreateResponse` | drbd.go | shared/types.go | |
| `DRBDPromoteResponse` | *new* | shared/types.go | Replaces `map[string]interface{}` |
| `DRBDDemoteResponse` | *new* | shared/types.go | Replaces `map[string]interface{}` |
| `DRBDStatusResponse` | drbd.go | shared/types.go | |
| `FormatBtrfsResponse` | btrfs.go | shared/types.go | |
| `ContainerStartResponse` | containers.go | shared/types.go | |
| `ContainerStatusResponse` | containers.go | shared/types.go | |
| `StatusResponse` | server.go | shared/types.go | |
| `UserStatusDTO` | server.go | shared/types.go | |
| `DRBDInfo` | drbd.go | *stays* | Internal parser type |
| `UserResources` | state.go | *stays* | Internal state |
| `Agent` | state.go | *stays* | Internal struct |
| `CmdResult` | exec.go | *stays* | Internal helper |
| `validateUserID` | images.go | *stays* | Internal validation |

## Verification Checklist

After completion, verify all of these:

1. `cd scfuture && go build ./...` succeeds with zero errors
2. `shared/types.go` contains exactly 13 types (11 moved + 2 new promote/demote responses)
3. No type definitions remain in `images.go`, `drbd.go`, `btrfs.go`, `containers.go`, or `server.go` (except `DRBDInfo` in `drbd.go`)
4. `state.go` still contains `UserResources` and `Agent`
5. `exec.go` still contains `CmdResult`, `runCmd`, `cmdString`, `cmdError`
6. `drbd.go` still contains `DRBDInfo` and `isMounted`
7. `images.go` still contains `validateUserID` and `validUserID`
8. All 8 container/script files are byte-identical to poc-coordinator originals
9. No external dependencies in go.mod
10. `DRBDPromote` and `DRBDDemote` return proper typed responses, not `map[string]interface{}`
11. The JSON output for promote/demote is backward-compatible (same keys: `ok`, `already_existed`)
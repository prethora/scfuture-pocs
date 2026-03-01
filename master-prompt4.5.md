# Layer 4.5 Build Prompt — Suspension, Reactivation & Deletion Lifecycle

## What This Is

This is a build prompt for Layer 4.5 of the scfuture distributed agent platform. You are Claude Code. Your job is to:

1. Read all referenced existing files to understand the current codebase
2. Write all new code and scripts described below
3. Report back when code is written
4. When told "yes" / "ready", run the full test lifecycle (infra up → deploy → test → iterate on failures → teardown)
5. When all tests pass, update `SESSION.md` (in the parent directory) with what happened
6. Give a final report

The project lives in `scfuture/` (a subdirectory of the current working directory). All Go code paths are relative to `scfuture/`. All script paths are relative to `scfuture/`. The `SESSION.md` file is in the parent directory (current working directory).

---

## Context: What Exists

Layers 4.1 (machine agent), 4.2 (coordinator happy path), 4.3 (heartbeat failure detection & automatic failover), and 4.4 (bipod reformation & dead machine re-integration) are complete and committed. Read these files first to understand the existing codebase:

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
scfuture/internal/coordinator/healthcheck.go
scfuture/internal/coordinator/reformer.go
scfuture/container/Dockerfile
scfuture/container/container-init.sh
```

Read ALL of these before writing any code. Pay close attention to:
- How `FormatBtrfs` in `btrfs.go` temporarily mounts the DRBD device on the host, creates subvolumes, then **unmounts** — the host never keeps user filesystems mounted
- How `DRBDDemote` in `drbd.go` checks `isMounted()` and unmounts before demoting (safety)
- How `DRBDDisconnect` is idempotent — returns success if already StandAlone
- How `DRBDCreate` writes the config, creates metadata, and runs `drbdadm up`
- How `DRBDPromote` uses `--force` flag (works with disconnected peer)
- How `ContainerStop` does `docker stop --time 10` then `docker rm -f`
- How `ProvisionUser` in `provisioner.go` drives the 8-step state machine (select machines → images → DRBD → promote → sync wait → format → container → running)
- How `stripPort()` exists in both `provisioner.go` and `reformer.go` (duplicated)
- How `NewMachineClient` uses `doJSON()` for all HTTP calls with 30-second timeout — backup/restore operations are slow, so the client needs a longer timeout for those calls
- How `cmd/machine-agent/main.go` reads `NODE_ID`, `LISTEN_ADDR`, `DATA_DIR`, `NODE_ADDRESS`, `COORDINATOR_URL` from env vars
- How `cmd/coordinator/main.go` starts `StartHealthChecker` and `StartReformer` after creating the coordinator
- How `failoverUser` in `healthcheck.go` only processes users with status `"running"` or `"running_degraded"` — it skips other statuses
- How the `User` struct in `store.go` has `StatusChangedAt` which is set by `SetUserStatus`
- How `persistState` and `persist()` in `store.go` save all state to `state.json`
- How the existing `reformer.go` uses `cleanStaleBipodsOnActiveMachines()` as a separate cleanup pass

### Reference documents (in parent directory):

```
SESSION.md
architecture-v3.md
master-prompt4.4.md
```

---

## What Layer 4.5 Builds

**In scope:**
- Suspension endpoint — stop container, take final Btrfs snapshot, backup to B2, demote DRBD, set status → `suspended`
- Warm reactivation endpoint — promote DRBD, start container, set status → `running` (images still on fleet)
- Cold reactivation endpoint — select machines, create images, DRBD, format bare, download from B2, btrfs receive, create workspace, start container (images were evicted)
- Eviction endpoint — verify B2 backup exists, destroy DRBD + delete images on all fleet machines, set status → `evicted`
- Retention enforcer goroutine — background process that enforces time-based transitions: DRBD disconnect after warm retention period, full eviction after eviction period
- New machine agent endpoints: Btrfs snapshot, B2 backup (btrfs send → zstd → b2 upload), B2 restore (b2 download → zstd → btrfs receive), DRBD connect, backup status check
- Bare format mode for `FormatBtrfs` — mkfs only, no workspace/seed data (used by cold restore)
- Lifecycle event recording for observability
- Full integration test: provision → suspend → warm reactivate → suspend again with B2 backup → evict → cold reactivate → retention enforcer auto-transitions

**Explicitly NOT in scope (future layers):**
- Crash recovery / reconciliation (Layer 4.6)
- Live migration (Layer 5)
- Incremental B2 backups (full sends only in this layer)

---

## Architecture

### Test Topology

Same as Layers 4.3/4.4: 1 coordinator + 3 fleet machines on Hetzner Cloud. Additionally requires Backblaze B2 credentials for backup/restore tests.

```
macOS (test harness, runs test_suite.sh via SSH / curl)
  │
  ├── l45-coordinator (CX23, coordinator :8080, private 10.0.0.2)
  │     │
  │     ├── l45-fleet-1 (CX23, machine-agent :8080, private 10.0.0.11)
  │     ├── l45-fleet-2 (CX23, machine-agent :8080, private 10.0.0.12)
  │     └── l45-fleet-3 (CX23, machine-agent :8080, private 10.0.0.13)
  │
  Private network: l45-net / 10.0.0.0/24

Backblaze B2:
  Bucket: l45-test-{random} (created/destroyed by run.sh)
  Env vars: B2_KEY_ID, B2_APP_KEY (required, set by user)
```

### How Suspension Works

The coordinator receives `POST /api/users/{id}/suspend`:

```
1. Validate: user status must be "running" or "running_degraded"
2. Set user status → "suspending"
3. Stop container on primary: POST /containers/{id}/stop
4. Take final snapshot on primary: POST /images/{id}/snapshot
   → Temporarily mounts DRBD device, creates read-only snapshot, unmounts
5. Backup to B2 on primary: POST /images/{id}/backup
   → btrfs send snapshot | zstd > temp file → b2 file upload → upload manifest.json
6. Record backup in coordinator state (BackupExists, BackupPath, BackupBucket)
7. Demote primary to Secondary: POST /images/{id}/drbd/demote
8. Set user status → "suspended"
9. Record lifecycle event
```

If the B2 backup fails (step 5), the suspension still completes — the user's data is safe on 2 fleet copies. The coordinator records `BackupExists=false`. Eviction will be blocked until a backup succeeds.

If the user is in `running_degraded` (only 1 copy), the flow is the same but the B2 backup is critical — it may be the only second copy. Step 7 (demote) is a no-op if DRBD is already StandAlone.

### How Warm Reactivation Works

The coordinator receives `POST /api/users/{id}/reactivate` and detects images are on fleet:

```
1. Validate: user status must be "suspended"
2. Set user status → "reactivating"
3. Find user's bipods on active machines
4. If DRBD is disconnected (user.DRBDDisconnected == true):
   a. POST /images/{id}/drbd/connect on BOTH machines
   b. Wait for bitmap resync (poll for PeerDiskState == "UpToDate", should be instant)
5. Promote primary: POST /images/{id}/drbd/promote (uses --force)
6. Start container: POST /containers/{id}/start
7. Set user status → "running", DRBDDisconnected → false
8. Record lifecycle event
```

### How Cold Reactivation Works

The coordinator receives `POST /api/users/{id}/reactivate` and detects no images on fleet (user is evicted):

```
1. Validate: user status must be "evicted"
2. Verify BackupExists and BackupPath are set
3. Set user status → "reactivating"
4. Select 2 machines (same placement algorithm as provisioning)
5. Create images on both: POST /images/{id}/create
6. Configure DRBD on both: POST /images/{id}/drbd/create
7. Promote primary: POST /images/{id}/drbd/promote (--force)
8. Wait for DRBD sync (blank → blank, fast)
9. Format Btrfs (BARE mode): POST /images/{id}/format-btrfs {"bare": true}
   → mkfs.btrfs + create /snapshots directory, NO workspace subvol, NO seed data
10. Restore from B2: POST /images/{id}/restore
    → b2 download → zstd -d → btrfs receive into /snapshots/
    → Create writable workspace: btrfs subvolume snapshot snapshots/{name} workspace
11. Unmount (restore endpoint handles this)
12. Start container: POST /containers/{id}/start
13. Set user status → "running"
14. Update bipods and primary machine in coordinator state
15. Record lifecycle event
```

### How Eviction Works

The coordinator receives `POST /api/users/{id}/evict`:

```
1. Validate: user status must be "suspended"
2. Safety check: if user.BackupExists == false, attempt backup first
   a. Find a machine with the user's bipod (prefer active)
   b. If machine is active and bipod exists:
      - Promote DRBD (temporarily)
      - POST /images/{id}/snapshot
      - POST /images/{id}/backup
      - Demote DRBD
   c. If backup fails: return error, refuse to evict
3. Set user status → "evicting"
4. For each bipod's machine (if machine is active):
   a. POST /images/{id}/drbd/disconnect (disconnect from peer first)
   b. DELETE /images/{id}/drbd (drbdadm down + remove config)
   c. DELETE /images/{id} (loop detach + image delete)
5. Remove all bipods from coordinator state
6. Set user status → "evicted", clear PrimaryMachine
7. Record lifecycle event
```

### Retention Enforcer

The coordinator runs a **retention enforcer goroutine** that ticks every 60 seconds:

```
For each user with status == "suspended":
  elapsed = time.Since(user.StatusChangedAt)

  If elapsed > WarmRetentionPeriod AND user.DRBDDisconnected == false:
    → Disconnect DRBD on both bipod machines
    → Set user.DRBDDisconnected = true
    → Record lifecycle event ("drbd_disconnect")

  If elapsed > EvictionPeriod:
    → Run eviction flow (same as explicit evict, including backup safety check)
    → Record lifecycle event ("auto_eviction")
```

**Configurable thresholds via environment variables:**
- `WARM_RETENTION_SECONDS` — default: `604800` (7 days). For tests: `15`
- `EVICTION_SECONDS` — default: `2592000` (30 days). For tests: `30`

### State Transitions

**New user statuses:**
```
running ──(suspend)──▶ suspending ──▶ suspended
running_degraded ──(suspend)──▶ suspending ──▶ suspended
suspended ──(reactivate, images on fleet)──▶ reactivating ──▶ running
suspended ──(retention enforcer, >eviction period)──▶ evicting ──▶ evicted
suspended ──(explicit evict)──▶ evicting ──▶ evicted
evicted ──(reactivate, cold path)──▶ reactivating ──▶ running
```

**DRBD state during suspension:**
```
Suspension:   Primary/Secondary → Secondary/Secondary (both sides demoted)
After 7d:     Secondary/Secondary → StandAlone/StandAlone (DRBD disconnected)
Warm reactivate (connected):    Secondary/Secondary → Primary/Secondary (promote one)
Warm reactivate (disconnected): StandAlone → connect → resync → Primary/Secondary
```

### Address Conventions

Same as Layers 4.3/4.4:
- Coordinator private: `10.0.0.2:8080`
- Fleet private: `10.0.0.11:8080`, `10.0.0.12:8080`, `10.0.0.13:8080`
- Machine agents register with their private IP as `NODE_ADDRESS`
- Coordinator calls machine agents via their registered address (private IP)
- Test harness calls coordinator and machine agents via public IPs

### B2 Credential Flow

B2 credentials are **environment variables on the machine agent**, not passed through the coordinator API:
- `B2_KEY_ID` — Backblaze application key ID
- `B2_APP_KEY` — Backblaze application key
- `B2_BUCKET_NAME` — bucket name (set during deployment)

The coordinator's suspend/evict flows call the machine agent's backup/restore endpoints. The machine agent uses its own env vars for B2 authentication. The coordinator tracks the B2 path and bucket in its User struct so it can pass them to restore requests.

The `b2` CLI tool (Python-based, installed via `pip3`) is used for all B2 operations — no Go B2 SDK (maintaining standard-library-only constraint).

---

## Modifications to Existing Code

### 1. `internal/shared/types.go` — Add snapshot, backup, restore, connect, and lifecycle event types

Add:

```go
// ─── Btrfs snapshot types (from btrfs.go, Layer 4.5) ───

type SnapshotRequest struct {
	SnapshotName string `json:"snapshot_name"` // e.g., "suspend-20260301T120000Z"
}

type SnapshotResponse struct {
	SnapshotName string `json:"snapshot_name"`
}

// ─── Btrfs format request (Layer 4.5 — bare mode) ───

type FormatBtrfsRequest struct {
	Bare bool `json:"bare"` // true = mkfs only, no workspace/seed data
}

// ─── B2 backup/restore types (from backup.go, Layer 4.5) ───

type BackupRequest struct {
	SnapshotName string `json:"snapshot_name"` // Which snapshot to send
	BucketName   string `json:"bucket_name"`   // B2 bucket name
	B2KeyPrefix  string `json:"b2_key_prefix"` // e.g., "users/alice" — path prefix in bucket
}

type BackupResponse struct {
	B2Path    string `json:"b2_path"`    // Full key in bucket, e.g., "users/alice/suspend-20260301T120000Z.btrfs.zst"
	SizeBytes int64  `json:"size_bytes"`
}

type RestoreRequest struct {
	BucketName   string `json:"bucket_name"`    // B2 bucket name
	B2Path       string `json:"b2_path"`        // Full key, e.g., "users/alice/suspend-20260301T120000Z.btrfs.zst"
	SnapshotName string `json:"snapshot_name"`  // Name to use for received snapshot
}

type RestoreResponse struct {
	SnapshotName string `json:"snapshot_name"`
}

type BackupStatusResponse struct {
	Exists    bool   `json:"exists"`
	B2Path    string `json:"b2_path,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
}

// ─── DRBD connect types (from drbd.go, Layer 4.5) ───

type DRBDConnectResponse struct {
	Status       string `json:"status"`        // "connected"
	WasConnected bool   `json:"was_connected"` // true if already connected before call
}

// ─── Lifecycle event types (from coordinator, Layer 4.5) ───

type LifecycleEventResponse struct {
	UserID     string `json:"user_id"`
	Type       string `json:"type"`       // "suspension", "reactivation_warm", "reactivation_cold", "eviction", "auto_eviction", "drbd_disconnect"
	Success    bool   `json:"success"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Timestamp  string `json:"timestamp"`
}
```

### 2. `internal/machineagent/btrfs.go` — Add `Snapshot` method, modify `FormatBtrfs` for bare mode

Modify the `FormatBtrfs` signature and implementation to accept an optional request body. The handler in `server.go` will decode the body if present:

```go
func (a *Agent) FormatBtrfs(userID string, bare bool) (*shared.FormatBtrfsResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}

	u := a.getUser(userID)
	if u == nil {
		return nil, fmt.Errorf("user %q not found in state", userID)
	}
	if u.DRBDDevice == "" {
		return nil, fmt.Errorf("no DRBD device for user %q", userID)
	}

	mountPath := a.mountPath(userID)
	drbdDev := u.DRBDDevice

	// Check if already formatted: try mounting and look for workspace subvol
	if err := os.MkdirAll(mountPath, 0755); err != nil {
		return nil, fmt.Errorf("mkdir mount path: %w", err)
	}

	// Try to mount — if it succeeds the device has a filesystem
	result, err := runCmd("mount", "-t", "btrfs", drbdDev, mountPath)
	if err == nil {
		if bare {
			// For bare mode, check if snapshots dir exists
			if _, statErr := os.Stat(mountPath + "/snapshots"); statErr == nil {
				runCmd("umount", mountPath)
				slog.Info("Btrfs already formatted (bare)", "component", "btrfs", "user", userID)
				return &shared.FormatBtrfsResponse{AlreadyFormatted: true}, nil
			}
		} else {
			// Check for workspace subvolume
			if _, statErr := os.Stat(mountPath + "/workspace"); statErr == nil {
				runCmd("umount", mountPath)
				slog.Info("Btrfs already formatted", "component", "btrfs", "user", userID)
				return &shared.FormatBtrfsResponse{AlreadyFormatted: true}, nil
			}
		}
		// Mounted but missing expected structure — unmount and reformat
		runCmd("umount", mountPath)
	}

	// Format
	result, err = runCmd("mkfs.btrfs", "-f", drbdDev)
	if err != nil {
		return nil, cmdError("mkfs.btrfs failed", cmdString("mkfs.btrfs", "-f", drbdDev), result)
	}

	// Mount
	result, err = runCmd("mount", "-t", "btrfs", drbdDev, mountPath)
	if err != nil {
		return nil, cmdError("mount failed", cmdString("mount", "-t", "btrfs", drbdDev, mountPath), result)
	}

	if bare {
		// Bare mode: just create snapshots directory, no workspace or seed data
		os.MkdirAll(mountPath+"/snapshots", 0755)
		slog.Info("Btrfs formatted (bare — no workspace)", "component", "btrfs", "user", userID)
	} else {
		// Full mode: create workspace subvolume with seed data
		result, err = runCmd("btrfs", "subvolume", "create", mountPath+"/workspace")
		if err != nil {
			runCmd("umount", mountPath)
			return nil, cmdError("btrfs subvolume create failed", "btrfs subvolume create workspace", result)
		}

		// Create seed directories
		for _, dir := range []string{"memory", "apps", "data"} {
			os.MkdirAll(mountPath+"/workspace/"+dir, 0755)
		}

		// Write config.json
		configData := map[string]string{
			"created": time.Now().UTC().Format(time.RFC3339),
			"user":    userID,
		}
		configJSON, _ := json.Marshal(configData)
		os.WriteFile(mountPath+"/workspace/data/config.json", configJSON, 0644)

		// Create snapshots directory and layer-000 snapshot
		os.MkdirAll(mountPath+"/snapshots", 0755)
		result, err = runCmd("btrfs", "subvolume", "snapshot", "-r",
			mountPath+"/workspace", mountPath+"/snapshots/layer-000")
		if err != nil {
			slog.Warn("Snapshot creation failed (non-fatal)", "component", "btrfs", "user", userID, "error", err)
		}
	}

	// Unmount — host does NOT keep Btrfs mounted
	result, err = runCmd("umount", mountPath)
	if err != nil {
		return nil, cmdError("umount after format failed", cmdString("umount", mountPath), result)
	}

	slog.Info("Btrfs formatted and provisioned", "component", "btrfs", "user", userID)
	return &shared.FormatBtrfsResponse{}, nil
}
```

Add the new `Snapshot` method:

```go
// Snapshot temporarily mounts the DRBD device and creates a read-only Btrfs snapshot.
// The DRBD resource must be Primary. After the snapshot is created, the filesystem is unmounted.
func (a *Agent) Snapshot(userID string, req *shared.SnapshotRequest) (*shared.SnapshotResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}
	if req.SnapshotName == "" {
		return nil, fmt.Errorf("snapshot_name is required")
	}

	u := a.getUser(userID)
	if u == nil {
		return nil, fmt.Errorf("user %q not found in state", userID)
	}
	if u.DRBDDevice == "" {
		return nil, fmt.Errorf("no DRBD device for user %q", userID)
	}

	mountPath := a.mountPath(userID)
	drbdDev := u.DRBDDevice

	// Mount
	os.MkdirAll(mountPath, 0755)
	result, err := runCmd("mount", "-t", "btrfs", drbdDev, mountPath)
	if err != nil {
		return nil, cmdError("mount failed for snapshot", cmdString("mount", "-t", "btrfs", drbdDev, mountPath), result)
	}

	// Create snapshots directory if needed
	os.MkdirAll(mountPath+"/snapshots", 0755)

	// Create read-only snapshot
	snapPath := mountPath + "/snapshots/" + req.SnapshotName
	result, err = runCmd("btrfs", "subvolume", "snapshot", "-r",
		mountPath+"/workspace", snapPath)
	if err != nil {
		runCmd("umount", mountPath)
		return nil, cmdError("btrfs snapshot failed", "btrfs subvolume snapshot -r workspace "+req.SnapshotName, result)
	}

	// Unmount
	result, err = runCmd("umount", mountPath)
	if err != nil {
		return nil, cmdError("umount after snapshot failed", cmdString("umount", mountPath), result)
	}

	slog.Info("Snapshot created", "component", "btrfs", "user", userID, "snapshot", req.SnapshotName)
	return &shared.SnapshotResponse{SnapshotName: req.SnapshotName}, nil
}
```

### 3. `internal/machineagent/drbd.go` — Add `DRBDConnect` method

Add:

```go
func (a *Agent) DRBDConnect(userID string) (*shared.DRBDConnectResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}

	resName := "user-" + userID

	// Check current status
	info := a.getDRBDStatus(resName)
	if info == nil {
		return nil, fmt.Errorf("DRBD resource %s does not exist", resName)
	}

	// If already connected, return success
	if info.ConnectionState == "Connected" {
		slog.Info("DRBD already connected", "component", "drbd", "user", userID)
		return &shared.DRBDConnectResponse{Status: "connected", WasConnected: true}, nil
	}

	result, err := runCmd("drbdadm", "connect", resName)
	if err != nil {
		return nil, cmdError("drbdadm connect failed", cmdString("drbdadm", "connect", resName), result)
	}

	slog.Info("DRBD connected to peer", "component", "drbd", "user", userID)
	return &shared.DRBDConnectResponse{Status: "connected", WasConnected: false}, nil
}
```

### 4. `internal/machineagent/server.go` — Add routes for new endpoints, modify FormatBtrfs handler

Add to `RegisterRoutes`:

```go
mux.HandleFunc("POST /images/{user_id}/snapshot", a.handleSnapshot)
mux.HandleFunc("POST /images/{user_id}/backup", a.handleBackup)
mux.HandleFunc("POST /images/{user_id}/restore", a.handleRestore)
mux.HandleFunc("GET /images/{user_id}/backup/status", a.handleBackupStatus)
mux.HandleFunc("POST /images/{user_id}/drbd/connect", a.handleDRBDConnect)
```

Modify `handleFormatBtrfs` to decode optional request body:

```go
func (a *Agent) handleFormatBtrfs(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	lock := a.getUserLock(userID)
	lock.Lock()
	defer lock.Unlock()

	// Decode optional request body for bare mode
	var req shared.FormatBtrfsRequest
	json.NewDecoder(r.Body).Decode(&req) // Ignore error — body may be empty (backward compat)

	resp, err := a.FormatBtrfs(userID, req.Bare)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
```

Add handlers:

```go
func (a *Agent) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	lock := a.getUserLock(userID)
	lock.Lock()
	defer lock.Unlock()

	var req shared.SnapshotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	resp, err := a.Snapshot(userID, &req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Agent) handleBackup(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	lock := a.getUserLock(userID)
	lock.Lock()
	defer lock.Unlock()

	var req shared.BackupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	resp, err := a.Backup(userID, &req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Agent) handleRestore(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	lock := a.getUserLock(userID)
	lock.Lock()
	defer lock.Unlock()

	var req shared.RestoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	resp, err := a.Restore(userID, &req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Agent) handleBackupStatus(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")

	resp, err := a.BackupStatus(userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Agent) handleDRBDConnect(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	lock := a.getUserLock(userID)
	lock.Lock()
	defer lock.Unlock()

	resp, err := a.DRBDConnect(userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
```

### 5. `internal/coordinator/store.go` — Add backup fields to User, lifecycle events, and new methods

Add fields to `User` struct:

```go
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
```

Add `LifecycleEvent` struct:

```go
type LifecycleEvent struct {
	UserID     string    `json:"user_id"`
	Type       string    `json:"type"` // "suspension", "reactivation_warm", "reactivation_cold", "eviction", "auto_eviction", "drbd_disconnect", "backup_during_eviction"
	Success    bool      `json:"success"`
	Error      string    `json:"error,omitempty"`
	DurationMS int64     `json:"duration_ms"`
	Timestamp  time.Time `json:"timestamp"`
}
```

Add `lifecycleEvents` to `Store` struct:

```go
type Store struct {
	// ... existing fields ...
	lifecycleEvents []LifecycleEvent
}
```

Add to `persistState`:

```go
type persistState struct {
	// ... existing fields ...
	LifecycleEvents []LifecycleEvent `json:"lifecycle_events"`
}
```

Update `persist()` to include `lifecycleEvents` in `persistState`.

Add new methods:

```go
// SetUserBackup records a successful B2 backup for a user.
func (s *Store) SetUserBackup(userID, b2Path, bucketName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return
	}
	u.BackupExists = true
	u.BackupPath = b2Path
	u.BackupBucket = bucketName
	u.BackupTimestamp = time.Now()
	s.persist()
}

// SetUserDRBDDisconnected marks a user's DRBD as disconnected by the retention enforcer.
func (s *Store) SetUserDRBDDisconnected(userID string, disconnected bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return
	}
	u.DRBDDisconnected = disconnected
	s.persist()
}

// ClearUserBipods removes all bipods for a user and clears the primary machine.
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
	s.persist()
}

// GetSuspendedUsers returns all users with status "suspended".
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

// RecordLifecycleEvent appends a lifecycle event.
func (s *Store) RecordLifecycleEvent(event LifecycleEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lifecycleEvents = append(s.lifecycleEvents, event)
	s.persist()
}

// GetLifecycleEvents returns all recorded lifecycle events.
func (s *Store) GetLifecycleEvents() []LifecycleEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]LifecycleEvent, len(s.lifecycleEvents))
	copy(result, s.lifecycleEvents)
	return result
}
```

### 6. `internal/coordinator/server.go` — Add lifecycle endpoints

Add routes to `RegisterRoutes`:

```go
// Lifecycle management (Layer 4.5)
mux.HandleFunc("POST /api/users/{id}/suspend", coord.handleSuspendUser)
mux.HandleFunc("POST /api/users/{id}/reactivate", coord.handleReactivateUser)
mux.HandleFunc("POST /api/users/{id}/evict", coord.handleEvictUser)
mux.HandleFunc("GET /api/lifecycle-events", coord.handleGetLifecycleEvents)
```

Add handlers:

```go
func (coord *Coordinator) handleSuspendUser(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	go coord.suspendUser(userID)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "suspending", "user_id": userID})
}

func (coord *Coordinator) handleReactivateUser(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	go coord.reactivateUser(userID)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "reactivating", "user_id": userID})
}

func (coord *Coordinator) handleEvictUser(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	go coord.evictUser(userID)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "evicting", "user_id": userID})
}

func (coord *Coordinator) handleGetLifecycleEvents(w http.ResponseWriter, r *http.Request) {
	events := coord.store.GetLifecycleEvents()
	if events == nil {
		events = []LifecycleEvent{}
	}
	writeJSON(w, http.StatusOK, events)
}
```

### 7. `internal/coordinator/machineapi.go` — Add client methods for new endpoints

Add:

```go
func (c *MachineClient) Snapshot(userID string, req *shared.SnapshotRequest) (*shared.SnapshotResponse, error) {
	var resp shared.SnapshotResponse
	if err := c.doJSON("POST", fmt.Sprintf("/images/%s/snapshot", userID), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) Backup(userID string, req *shared.BackupRequest) (*shared.BackupResponse, error) {
	// Backup can be slow — use a client with longer timeout
	longClient := &MachineClient{
		address: c.address,
		client:  &http.Client{Timeout: 300 * time.Second},
	}
	var resp shared.BackupResponse
	if err := longClient.doJSON("POST", fmt.Sprintf("/images/%s/backup", userID), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) Restore(userID string, req *shared.RestoreRequest) (*shared.RestoreResponse, error) {
	// Restore can be slow — use a client with longer timeout
	longClient := &MachineClient{
		address: c.address,
		client:  &http.Client{Timeout: 300 * time.Second},
	}
	var resp shared.RestoreResponse
	if err := longClient.doJSON("POST", fmt.Sprintf("/images/%s/restore", userID), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) BackupStatus(userID string) (*shared.BackupStatusResponse, error) {
	var resp shared.BackupStatusResponse
	if err := c.doJSON("GET", fmt.Sprintf("/images/%s/backup/status", userID), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) DRBDConnect(userID string) (*shared.DRBDConnectResponse, error) {
	var resp shared.DRBDConnectResponse
	if err := c.doJSON("POST", fmt.Sprintf("/images/%s/drbd/connect", userID), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) FormatBtrfsBare(userID string) (*shared.FormatBtrfsResponse, error) {
	var resp shared.FormatBtrfsResponse
	req := shared.FormatBtrfsRequest{Bare: true}
	if err := c.doJSON("POST", fmt.Sprintf("/images/%s/format-btrfs", userID), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
```

### 8. `internal/coordinator/healthcheck.go` — Handle suspended users on dead machines

Modify `failoverUser` to handle suspended users. Add a check near the top, after the existing status check:

```go
// failoverUser handles failover for a single user when a machine dies.
func (coord *Coordinator) failoverUser(userID, deadMachineID string) {
	start := time.Now()
	logger := slog.With("component", "failover", "user_id", userID, "dead_machine", deadMachineID)

	user := coord.store.GetUser(userID)
	if user == nil {
		logger.Warn("User not found in store")
		return
	}

	// Handle suspended users — just mark bipod as stale, no failover needed
	if user.Status == "suspended" || user.Status == "evicted" {
		logger.Info("User is suspended/evicted — marking bipod stale only", "status", user.Status)
		coord.store.SetBipodRole(userID, deadMachineID, "stale")
		return
	}

	// Skip if user is not in a state that needs failover
	if user.Status != "running" && user.Status != "running_degraded" {
		logger.Info("Skipping user — not in running state", "status", user.Status)
		return
	}

	// ... rest of existing failoverUser code unchanged ...
```

### 9. `cmd/coordinator/main.go` — Start retention enforcer, read B2 bucket env var

Add after the `StartReformer` call:

```go
// Start retention enforcer
coordinator.StartRetentionEnforcer(coord.GetStore(), coord)
```

Also read `B2_BUCKET_NAME` env var and pass it to the coordinator. Add to the coordinator a way to access the bucket name. The simplest approach: store it on the Coordinator struct.

Modify the coordinator creation:

```go
b2Bucket := os.Getenv("B2_BUCKET_NAME")

coord := coordinator.NewCoordinator(dataDir, b2Bucket)
```

This requires modifying `NewCoordinator` and the `Coordinator` struct — see server.go modifications below.

### 10. `internal/coordinator/server.go` — Add B2BucketName to Coordinator

```go
type Coordinator struct {
	store        *Store
	B2BucketName string
}

func NewCoordinator(dataDir string, b2BucketName string) *Coordinator {
	return &Coordinator{
		store:        NewStore(dataDir),
		B2BucketName: b2BucketName,
	}
}
```

### 11. `internal/coordinator/provisioner.go` — Update FormatBtrfs call

The `FormatBtrfs` client method no longer takes zero arguments. Update the provisioner to call the existing endpoint with no body (backward compatible — the handler decodes optional body):

No change needed — the existing `FormatBtrfs` client method in `machineapi.go` sends `nil` body, and the modified handler will decode it as `{bare: false}` by default.

---

## New Implementation

### New file: `internal/machineagent/backup.go`

```go
package machineagent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"scfuture/internal/shared"
)

// Backup performs a full btrfs send of a snapshot, compresses with zstd, and uploads to B2.
// Requires B2_KEY_ID, B2_APP_KEY env vars to be set on the machine agent.
// The DRBD resource must be Primary.
func (a *Agent) Backup(userID string, req *shared.BackupRequest) (*shared.BackupResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}
	if req.SnapshotName == "" {
		return nil, fmt.Errorf("snapshot_name is required")
	}
	if req.BucketName == "" {
		return nil, fmt.Errorf("bucket_name is required")
	}

	u := a.getUser(userID)
	if u == nil {
		return nil, fmt.Errorf("user %q not found in state", userID)
	}
	if u.DRBDDevice == "" {
		return nil, fmt.Errorf("no DRBD device for user %q", userID)
	}

	// Check B2 credentials
	if os.Getenv("B2_KEY_ID") == "" || os.Getenv("B2_APP_KEY") == "" {
		return nil, fmt.Errorf("B2_KEY_ID and B2_APP_KEY environment variables are required")
	}

	mountPath := a.mountPath(userID)
	drbdDev := u.DRBDDevice
	snapPath := mountPath + "/snapshots/" + req.SnapshotName

	// Mount
	os.MkdirAll(mountPath, 0755)
	result, err := runCmd("mount", "-t", "btrfs", drbdDev, mountPath)
	if err != nil {
		return nil, cmdError("mount failed for backup", cmdString("mount", "-t", "btrfs", drbdDev, mountPath), result)
	}
	defer func() {
		runCmd("umount", mountPath)
	}()

	// Verify snapshot exists
	if _, err := os.Stat(snapPath); err != nil {
		return nil, fmt.Errorf("snapshot %q does not exist at %s", req.SnapshotName, snapPath)
	}

	// Create temp file for btrfs send output
	tmpDir := "/tmp"
	tmpFile := filepath.Join(tmpDir, fmt.Sprintf("%s-%s.btrfs.zst", userID, req.SnapshotName))
	defer os.Remove(tmpFile)

	// btrfs send | zstd > temp file
	sendCmd := fmt.Sprintf("btrfs send %s | zstd -o %s", snapPath, tmpFile)
	result, err = runCmd("bash", "-c", sendCmd)
	if err != nil {
		return nil, cmdError("btrfs send | zstd failed", sendCmd, result)
	}

	// Get file size
	info, err := os.Stat(tmpFile)
	if err != nil {
		return nil, fmt.Errorf("stat temp file: %w", err)
	}

	// Determine B2 key
	b2KeyPrefix := req.B2KeyPrefix
	if b2KeyPrefix == "" {
		b2KeyPrefix = "users/" + userID
	}
	b2Key := b2KeyPrefix + "/" + req.SnapshotName + ".btrfs.zst"

	// Upload to B2
	result, err = runCmd("b2", "file", "upload", req.BucketName, tmpFile, b2Key)
	if err != nil {
		return nil, cmdError("b2 upload failed", "b2 file upload "+req.BucketName+" "+b2Key, result)
	}

	// Upload manifest.json
	manifest := map[string]interface{}{
		"snapshot":  req.SnapshotName,
		"b2_key":    b2Key,
		"size":      info.Size(),
		"timestamp": strings.TrimSpace(result.Stdout),
	}
	manifestJSON, _ := json.Marshal(manifest)
	manifestPath := filepath.Join(tmpDir, fmt.Sprintf("%s-manifest.json", userID))
	os.WriteFile(manifestPath, manifestJSON, 0644)
	defer os.Remove(manifestPath)

	manifestB2Key := b2KeyPrefix + "/manifest.json"
	runCmd("b2", "file", "upload", req.BucketName, manifestPath, manifestB2Key)

	slog.Info("Backup complete", "component", "backup", "user", userID,
		"snapshot", req.SnapshotName, "b2_key", b2Key, "size", info.Size())

	return &shared.BackupResponse{
		B2Path:    b2Key,
		SizeBytes: info.Size(),
	}, nil
}

// Restore downloads a snapshot from B2, decompresses, and applies via btrfs receive.
// Then creates a writable workspace subvolume from the received snapshot.
// The DRBD resource must be Primary and the filesystem must already be formatted (bare mode OK).
func (a *Agent) Restore(userID string, req *shared.RestoreRequest) (*shared.RestoreResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}
	if req.BucketName == "" || req.B2Path == "" {
		return nil, fmt.Errorf("bucket_name and b2_path are required")
	}

	u := a.getUser(userID)
	if u == nil {
		return nil, fmt.Errorf("user %q not found in state", userID)
	}
	if u.DRBDDevice == "" {
		return nil, fmt.Errorf("no DRBD device for user %q", userID)
	}

	// Check B2 credentials
	if os.Getenv("B2_KEY_ID") == "" || os.Getenv("B2_APP_KEY") == "" {
		return nil, fmt.Errorf("B2_KEY_ID and B2_APP_KEY environment variables are required")
	}

	mountPath := a.mountPath(userID)
	drbdDev := u.DRBDDevice

	// Mount
	os.MkdirAll(mountPath, 0755)
	result, err := runCmd("mount", "-t", "btrfs", drbdDev, mountPath)
	if err != nil {
		return nil, cmdError("mount failed for restore", cmdString("mount", "-t", "btrfs", drbdDev, mountPath), result)
	}
	defer func() {
		runCmd("umount", mountPath)
	}()

	// Create snapshots directory if needed
	os.MkdirAll(mountPath+"/snapshots", 0755)

	// Download from B2
	tmpDir := "/tmp"
	tmpZst := filepath.Join(tmpDir, fmt.Sprintf("%s-restore.btrfs.zst", userID))
	tmpRaw := filepath.Join(tmpDir, fmt.Sprintf("%s-restore.btrfs", userID))
	defer os.Remove(tmpZst)
	defer os.Remove(tmpRaw)

	result, err = runCmd("b2", "file", "download", fmt.Sprintf("b2://%s/%s", req.BucketName, req.B2Path), tmpZst)
	if err != nil {
		return nil, cmdError("b2 download failed", "b2 file download "+req.B2Path, result)
	}

	// Decompress
	result, err = runCmd("zstd", "-d", tmpZst, "-o", tmpRaw)
	if err != nil {
		return nil, cmdError("zstd decompress failed", "zstd -d "+tmpZst, result)
	}

	// btrfs receive
	receiveCmd := fmt.Sprintf("btrfs receive %s/snapshots/ < %s", mountPath, tmpRaw)
	result, err = runCmd("bash", "-c", receiveCmd)
	if err != nil {
		return nil, cmdError("btrfs receive failed", receiveCmd, result)
	}

	// Determine snapshot name from the received subvolume
	// btrfs receive creates the subvolume with its original name
	snapName := req.SnapshotName
	if snapName == "" {
		// Try to discover from directory listing
		entries, _ := os.ReadDir(mountPath + "/snapshots")
		for _, e := range entries {
			if e.Name() != "layer-000" {
				snapName = e.Name()
				break
			}
		}
	}

	// Delete existing workspace subvolume if present (e.g., from a failed previous restore)
	if _, err := os.Stat(mountPath + "/workspace"); err == nil {
		runCmd("btrfs", "subvolume", "delete", mountPath+"/workspace")
	}

	// Create writable workspace from the received snapshot
	snapFullPath := mountPath + "/snapshots/" + snapName
	result, err = runCmd("btrfs", "subvolume", "snapshot",
		snapFullPath, mountPath+"/workspace")
	if err != nil {
		return nil, cmdError("create workspace from snapshot failed",
			"btrfs subvolume snapshot "+snapName+" workspace", result)
	}

	slog.Info("Restore complete", "component", "backup", "user", userID,
		"snapshot", snapName, "source", req.B2Path)

	return &shared.RestoreResponse{SnapshotName: snapName}, nil
}

// BackupStatus checks if a B2 backup exists for this user by checking for manifest.json.
func (a *Agent) BackupStatus(userID string) (*shared.BackupStatusResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}

	bucketName := os.Getenv("B2_BUCKET_NAME")
	if bucketName == "" {
		return &shared.BackupStatusResponse{Exists: false}, nil
	}

	if os.Getenv("B2_KEY_ID") == "" || os.Getenv("B2_APP_KEY") == "" {
		return &shared.BackupStatusResponse{Exists: false}, nil
	}

	// Check for manifest.json
	manifestKey := "users/" + userID + "/manifest.json"
	result, err := runCmd("b2", "ls", "--recursive", fmt.Sprintf("b2://%s", bucketName), "--prefix", manifestKey)
	if err != nil {
		return &shared.BackupStatusResponse{Exists: false}, nil
	}

	exists := strings.TrimSpace(result.Stdout) != ""

	resp := &shared.BackupStatusResponse{Exists: exists}
	if exists {
		resp.B2Path = manifestKey
	}

	return resp, nil
}
```

### New file: `internal/coordinator/lifecycle.go`

```go
package coordinator

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"scfuture/internal/shared"
)

// suspendUser drives the full suspension flow for a user.
func (coord *Coordinator) suspendUser(userID string) {
	start := time.Now()
	logger := slog.With("component", "lifecycle", "user_id", userID, "action", "suspend")

	user := coord.store.GetUser(userID)
	if user == nil {
		logger.Error("User not found")
		return
	}

	if user.Status != "running" && user.Status != "running_degraded" {
		logger.Warn("Cannot suspend — invalid status", "status", user.Status)
		return
	}

	coord.store.SetUserStatus(userID, "suspending", "")

	// Find the primary machine
	primaryMachine := coord.store.GetMachine(user.PrimaryMachine)
	if primaryMachine == nil {
		logger.Error("Primary machine not found")
		coord.store.SetUserStatus(userID, user.Status, "primary machine not found for suspension")
		return
	}
	primaryClient := NewMachineClient(primaryMachine.Address)

	// ── Step 1: Stop container ──
	if err := primaryClient.ContainerStop(userID); err != nil {
		logger.Error("Container stop failed", "error", err)
		coord.store.SetUserStatus(userID, user.Status, "container stop failed: "+err.Error())
		coord.store.RecordLifecycleEvent(LifecycleEvent{
			UserID: userID, Type: "suspension", Success: false,
			Error: "container stop: " + err.Error(),
			DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
		})
		return
	}
	logger.Info("Container stopped")

	// ── Step 2: Take final snapshot ──
	snapName := fmt.Sprintf("suspend-%s", time.Now().UTC().Format("20060102T150405Z"))
	_, err := primaryClient.Snapshot(userID, &shared.SnapshotRequest{SnapshotName: snapName})
	if err != nil {
		logger.Error("Snapshot failed", "error", err)
		// Non-fatal for suspension — proceed but log warning
		logger.Warn("Continuing suspension without snapshot")
	} else {
		logger.Info("Snapshot created", "name", snapName)
	}

	// ── Step 3: Backup to B2 ──
	backupSuccess := false
	if coord.B2BucketName != "" {
		backupReq := &shared.BackupRequest{
			SnapshotName: snapName,
			BucketName:   coord.B2BucketName,
			B2KeyPrefix:  "users/" + userID,
		}
		backupResp, err := primaryClient.Backup(userID, backupReq)
		if err != nil {
			logger.Warn("B2 backup failed (non-fatal for suspension)", "error", err)
		} else {
			coord.store.SetUserBackup(userID, backupResp.B2Path, coord.B2BucketName)
			backupSuccess = true
			logger.Info("B2 backup complete", "b2_path", backupResp.B2Path, "size", backupResp.SizeBytes)
		}
	} else {
		logger.Warn("No B2 bucket configured — skipping backup")
	}

	// ── Step 4: Demote primary to Secondary ──
	// This is safe because the container is stopped and we don't need to write anymore
	_, err = primaryClient.DRBDDemote(userID)
	if err != nil {
		// Non-fatal — DRBD may already be Secondary or StandAlone
		logger.Warn("DRBD demote failed (non-fatal)", "error", err)
	} else {
		logger.Info("DRBD demoted to Secondary")
	}

	// ── Step 5: Set status → "suspended" ──
	coord.store.SetUserStatus(userID, "suspended", "")
	coord.store.SetUserDRBDDisconnected(userID, false)

	coord.store.RecordLifecycleEvent(LifecycleEvent{
		UserID: userID, Type: "suspension", Success: true,
		DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
	})

	logger.Info("Suspension complete",
		"backup_success", backupSuccess,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

// reactivateUser determines whether to use warm or cold path and proceeds.
func (coord *Coordinator) reactivateUser(userID string) {
	user := coord.store.GetUser(userID)
	if user == nil {
		slog.Error("[LIFECYCLE] User not found", "user_id", userID)
		return
	}

	switch user.Status {
	case "suspended":
		coord.warmReactivate(userID)
	case "evicted":
		coord.coldReactivate(userID)
	default:
		slog.Warn("[LIFECYCLE] Cannot reactivate — invalid status", "user_id", userID, "status", user.Status)
	}
}

// warmReactivate brings back a suspended user whose images are still on fleet.
func (coord *Coordinator) warmReactivate(userID string) {
	start := time.Now()
	logger := slog.With("component", "lifecycle", "user_id", userID, "action", "reactivate_warm")

	user := coord.store.GetUser(userID)
	if user == nil || user.Status != "suspended" {
		return
	}

	coord.store.SetUserStatus(userID, "reactivating", "")

	// Find bipods on active machines
	bipods := coord.store.GetBipods(userID)
	var activeBipods []*Bipod
	for _, b := range bipods {
		if b.Role != "stale" {
			m := coord.store.GetMachine(b.MachineID)
			if m != nil && m.Status == "active" {
				activeBipods = append(activeBipods, b)
			}
		}
	}

	if len(activeBipods) == 0 {
		logger.Error("No active bipods found — cannot warm reactivate")
		coord.store.SetUserStatus(userID, "suspended", "no active bipods for warm reactivation")
		coord.store.RecordLifecycleEvent(LifecycleEvent{
			UserID: userID, Type: "reactivation_warm", Success: false,
			Error: "no active bipods",
			DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
		})
		return
	}

	// Pick primary — use the user's recorded primary if it has an active bipod, else pick first active
	var primaryBipod *Bipod
	for _, b := range activeBipods {
		if b.MachineID == user.PrimaryMachine {
			primaryBipod = b
			break
		}
	}
	if primaryBipod == nil {
		primaryBipod = activeBipods[0]
		coord.store.SetUserPrimary(userID, primaryBipod.MachineID)
	}

	primaryMachine := coord.store.GetMachine(primaryBipod.MachineID)
	primaryClient := NewMachineClient(primaryMachine.Address)

	// ── Step 1: Reconnect DRBD if disconnected ──
	if user.DRBDDisconnected {
		logger.Info("DRBD was disconnected — reconnecting")
		for _, b := range activeBipods {
			m := coord.store.GetMachine(b.MachineID)
			if m != nil {
				client := NewMachineClient(m.Address)
				if _, err := client.DRBDConnect(userID); err != nil {
					logger.Warn("DRBD connect failed on machine (non-fatal)", "machine", b.MachineID, "error", err)
				}
			}
		}
		// Wait briefly for resync
		time.Sleep(3 * time.Second)
	}

	// ── Step 2: Promote DRBD ──
	if _, err := primaryClient.DRBDPromote(userID); err != nil {
		logger.Error("DRBD promote failed", "error", err)
		coord.store.SetUserStatus(userID, "suspended", "promote failed: "+err.Error())
		coord.store.RecordLifecycleEvent(LifecycleEvent{
			UserID: userID, Type: "reactivation_warm", Success: false,
			Error: "promote: " + err.Error(),
			DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
		})
		return
	}
	logger.Info("DRBD promoted")

	// ── Step 3: Start container ──
	if _, err := primaryClient.ContainerStart(userID); err != nil {
		logger.Error("Container start failed", "error", err)
		coord.store.SetUserStatus(userID, "suspended", "container start failed: "+err.Error())
		coord.store.RecordLifecycleEvent(LifecycleEvent{
			UserID: userID, Type: "reactivation_warm", Success: false,
			Error: "container start: " + err.Error(),
			DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
		})
		return
	}
	logger.Info("Container started")

	// ── Step 4: Update state ──
	coord.store.SetBipodRole(userID, primaryBipod.MachineID, "primary")
	coord.store.SetUserStatus(userID, "running", "")
	coord.store.SetUserDRBDDisconnected(userID, false)

	coord.store.RecordLifecycleEvent(LifecycleEvent{
		UserID: userID, Type: "reactivation_warm", Success: true,
		DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
	})

	logger.Info("Warm reactivation complete", "duration_ms", time.Since(start).Milliseconds())
}

// coldReactivate provisions a user from B2 backup after eviction.
func (coord *Coordinator) coldReactivate(userID string) {
	start := time.Now()
	logger := slog.With("component", "lifecycle", "user_id", userID, "action", "reactivate_cold")

	user := coord.store.GetUser(userID)
	if user == nil || user.Status != "evicted" {
		return
	}

	if !user.BackupExists || user.BackupPath == "" || user.BackupBucket == "" {
		logger.Error("No B2 backup found — cannot cold reactivate")
		coord.store.RecordLifecycleEvent(LifecycleEvent{
			UserID: userID, Type: "reactivation_cold", Success: false,
			Error: "no B2 backup",
			DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
		})
		return
	}

	coord.store.SetUserStatus(userID, "reactivating", "")

	// Helper to fail
	fail := func(step string, err error) {
		msg := fmt.Sprintf("%s: %v", step, err)
		logger.Error("Cold reactivation failed", "step", step, "error", err)
		coord.store.SetUserStatus(userID, "evicted", msg)
		coord.store.RecordLifecycleEvent(LifecycleEvent{
			UserID: userID, Type: "reactivation_cold", Success: false,
			Error: msg,
			DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
		})
	}

	// ── Step 1: Select machines ──
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

	logger.Info("Machines selected", "primary", primary.MachineID, "secondary", secondary.MachineID)

	primaryClient := NewMachineClient(primary.Address)
	secondaryClient := NewMachineClient(secondary.Address)

	// ── Step 2: Create images ──
	primaryResp, err := primaryClient.CreateImage(userID, user.ImageSizeMB)
	if err != nil {
		fail("image_primary", err)
		return
	}
	secondaryResp, err := secondaryClient.CreateImage(userID, user.ImageSizeMB)
	if err != nil {
		fail("image_secondary", err)
		return
	}

	coord.store.SetBipodLoopDevice(userID, primary.MachineID, primaryResp.LoopDevice)
	coord.store.SetBipodLoopDevice(userID, secondary.MachineID, secondaryResp.LoopDevice)

	// ── Step 3: Configure DRBD ──
	primaryAddr := stripPort(primary.Address)
	secondaryAddr := stripPort(secondary.Address)

	drbdReq := &shared.DRBDCreateRequest{
		ResourceName: "user-" + userID,
		Nodes: []shared.DRBDNode{
			{Hostname: primary.MachineID, Minor: primaryMinor, Disk: primaryResp.LoopDevice, Address: primaryAddr},
			{Hostname: secondary.MachineID, Minor: secondaryMinor, Disk: secondaryResp.LoopDevice, Address: secondaryAddr},
		},
		Port: port,
	}

	if _, err := primaryClient.DRBDCreate(userID, drbdReq); err != nil {
		fail("drbd_primary", err)
		return
	}
	if _, err := secondaryClient.DRBDCreate(userID, drbdReq); err != nil {
		fail("drbd_secondary", err)
		return
	}

	// ── Step 4: Promote primary ──
	if _, err := primaryClient.DRBDPromote(userID); err != nil {
		fail("drbd_promote", err)
		return
	}

	// ── Step 5: Wait for DRBD sync (blank → blank, fast) ──
	syncTimeout := 60 * time.Second
	syncStart := time.Now()
	for {
		if time.Since(syncStart) > syncTimeout {
			fail("drbd_sync", fmt.Errorf("sync timeout"))
			return
		}
		status, err := primaryClient.DRBDStatus(userID)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		if status.PeerDiskState == "UpToDate" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	logger.Info("DRBD sync complete")

	// ── Step 6: Format Btrfs (bare mode — no workspace) ──
	if _, err := primaryClient.FormatBtrfsBare(userID); err != nil {
		fail("format_btrfs_bare", err)
		return
	}
	logger.Info("Btrfs formatted (bare)")

	// ── Step 7: Restore from B2 ──
	restoreReq := &shared.RestoreRequest{
		BucketName:   user.BackupBucket,
		B2Path:       user.BackupPath,
		SnapshotName: "", // Will be discovered from the received subvolume
	}
	restoreResp, err := primaryClient.Restore(userID, restoreReq)
	if err != nil {
		fail("restore", err)
		return
	}
	logger.Info("Restore complete", "snapshot", restoreResp.SnapshotName)

	// ── Step 8: Start container ──
	if _, err := primaryClient.ContainerStart(userID); err != nil {
		fail("container_start", err)
		return
	}
	logger.Info("Container started")

	// ── Step 9: Update state ──
	coord.store.SetUserStatus(userID, "running", "")
	coord.store.SetUserDRBDDisconnected(userID, false)

	coord.store.RecordLifecycleEvent(LifecycleEvent{
		UserID: userID, Type: "reactivation_cold", Success: true,
		DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
	})

	logger.Info("Cold reactivation complete", "duration_ms", time.Since(start).Milliseconds())
}

// evictUser removes all fleet copies of a user's data after verifying B2 backup exists.
func (coord *Coordinator) evictUser(userID string) {
	start := time.Now()
	logger := slog.With("component", "lifecycle", "user_id", userID, "action", "evict")

	user := coord.store.GetUser(userID)
	if user == nil {
		logger.Error("User not found")
		return
	}

	if user.Status != "suspended" {
		logger.Warn("Cannot evict — user is not suspended", "status", user.Status)
		return
	}

	// ── Safety check: B2 backup must exist ──
	if !user.BackupExists {
		logger.Warn("No B2 backup — attempting backup before eviction")

		// Find a machine with the user's bipod
		bipods := coord.store.GetBipods(userID)
		backupDone := false
		for _, b := range bipods {
			if b.Role == "stale" {
				continue
			}
			m := coord.store.GetMachine(b.MachineID)
			if m == nil || m.Status != "active" {
				continue
			}

			client := NewMachineClient(m.Address)

			// Promote temporarily for snapshot/backup
			client.DRBDPromote(userID)

			snapName := fmt.Sprintf("evict-%s", time.Now().UTC().Format("20060102T150405Z"))
			if _, err := client.Snapshot(userID, &shared.SnapshotRequest{SnapshotName: snapName}); err != nil {
				logger.Warn("Pre-eviction snapshot failed", "error", err)
				client.DRBDDemote(userID)
				continue
			}

			if coord.B2BucketName != "" {
				backupReq := &shared.BackupRequest{
					SnapshotName: snapName,
					BucketName:   coord.B2BucketName,
					B2KeyPrefix:  "users/" + userID,
				}
				backupResp, err := client.Backup(userID, backupReq)
				if err != nil {
					logger.Warn("Pre-eviction backup failed", "error", err)
					client.DRBDDemote(userID)
					continue
				}
				coord.store.SetUserBackup(userID, backupResp.B2Path, coord.B2BucketName)
				backupDone = true
			}
			client.DRBDDemote(userID)
			if backupDone {
				break
			}
		}

		if !backupDone {
			logger.Error("Cannot evict — no B2 backup and backup attempt failed")
			coord.store.RecordLifecycleEvent(LifecycleEvent{
				UserID: userID, Type: "eviction", Success: false,
				Error: "no backup exists and backup attempt failed",
				DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
			})
			return
		}
	}

	// ── Set status → "evicting" ──
	coord.store.SetUserStatus(userID, "evicting", "")

	// ── Destroy DRBD and delete images on all machines ──
	bipods := coord.store.GetBipods(userID)
	for _, b := range bipods {
		m := coord.store.GetMachine(b.MachineID)
		if m == nil || m.Status != "active" {
			logger.Warn("Skipping bipod cleanup on unavailable machine", "machine", b.MachineID)
			continue
		}
		client := NewMachineClient(m.Address)

		// Disconnect DRBD first (ignore errors)
		client.DRBDDisconnect(userID)

		// Destroy DRBD resource
		if err := client.DRBDDestroy(userID); err != nil {
			logger.Warn("DRBD destroy failed (non-fatal)", "machine", b.MachineID, "error", err)
		}

		// Delete image
		if err := client.DeleteUser(userID); err != nil {
			logger.Warn("Image delete failed (non-fatal)", "machine", b.MachineID, "error", err)
		}

		logger.Info("Cleaned up on machine", "machine", b.MachineID)
	}

	// ── Update state ──
	coord.store.ClearUserBipods(userID)
	coord.store.SetUserStatus(userID, "evicted", "")
	coord.store.SetUserDRBDDisconnected(userID, false)

	coord.store.RecordLifecycleEvent(LifecycleEvent{
		UserID: userID, Type: "eviction", Success: true,
		DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
	})

	logger.Info("Eviction complete", "duration_ms", time.Since(start).Milliseconds())
}

// stripPort extracts the IP from an "ip:port" address string.
func stripPortLifecycle(address string) string {
	idx := strings.LastIndex(address, ":")
	if idx == -1 {
		return address
	}
	return address[:idx]
}
```

**Note:** `stripPort` is duplicated from `provisioner.go` and `reformer.go`. If you want to share it, move it to a common location. But for the PoC, duplication is fine — it's a 4-line function. In `lifecycle.go`, use the `stripPort` function from `provisioner.go` (same package) — do NOT create another copy. If the linker complains about the duplicate in `reformer.go`, remove the one from `reformer.go` since the provisioner's version is already available in the same package.

### New file: `internal/coordinator/retention.go`

```go
package coordinator

import (
	"log/slog"
	"os"
	"strconv"
	"time"
)

var (
	RetentionEnforcerInterval = 60 * time.Second
	WarmRetentionPeriod       = 7 * 24 * time.Hour  // 7 days — DRBD disconnected after this
	EvictionPeriod            = 30 * 24 * time.Hour  // 30 days — evicted after this
)

func init() {
	// Allow override via environment variables (for testing with short durations)
	if v := os.Getenv("WARM_RETENTION_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			WarmRetentionPeriod = time.Duration(n) * time.Second
		}
	}
	if v := os.Getenv("EVICTION_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			EvictionPeriod = time.Duration(n) * time.Second
		}
	}
}

// StartRetentionEnforcer launches a background goroutine that enforces
// time-based retention transitions for suspended users.
func StartRetentionEnforcer(store *Store, coord *Coordinator) {
	go func() {
		slog.Info("[RETENTION] Retention enforcer started",
			"interval", RetentionEnforcerInterval.String(),
			"warm_retention", WarmRetentionPeriod.String(),
			"eviction_period", EvictionPeriod.String(),
		)

		ticker := time.NewTicker(RetentionEnforcerInterval)
		defer ticker.Stop()
		for range ticker.C {
			coord.enforceRetention()
		}
	}()
}

// enforceRetention scans suspended users and enforces time-based transitions.
func (coord *Coordinator) enforceRetention() {
	suspended := coord.store.GetSuspendedUsers()
	if len(suspended) == 0 {
		return
	}

	now := time.Now()

	for _, user := range suspended {
		elapsed := now.Sub(user.StatusChangedAt)

		// Check for eviction first (longer threshold)
		if elapsed > EvictionPeriod {
			slog.Info("[RETENTION] Auto-evicting user",
				"user_id", user.UserID,
				"suspended_for", elapsed.String(),
			)
			coord.evictUser(user.UserID)
			continue
		}

		// Check for DRBD disconnect (shorter threshold)
		if elapsed > WarmRetentionPeriod && !user.DRBDDisconnected {
			slog.Info("[RETENTION] Disconnecting DRBD for suspended user",
				"user_id", user.UserID,
				"suspended_for", elapsed.String(),
			)
			coord.disconnectSuspendedDRBD(user.UserID)
		}
	}
}

// disconnectSuspendedDRBD disconnects DRBD on both machines for a suspended user.
func (coord *Coordinator) disconnectSuspendedDRBD(userID string) {
	start := time.Now()
	logger := slog.With("component", "retention", "user_id", userID)

	bipods := coord.store.GetBipods(userID)
	for _, b := range bipods {
		if b.Role == "stale" {
			continue
		}
		m := coord.store.GetMachine(b.MachineID)
		if m == nil || m.Status != "active" {
			continue
		}
		client := NewMachineClient(m.Address)
		if _, err := client.DRBDDisconnect(userID); err != nil {
			logger.Warn("DRBD disconnect failed on machine (non-fatal)", "machine", b.MachineID, "error", err)
		} else {
			logger.Info("DRBD disconnected on machine", "machine", b.MachineID)
		}
	}

	coord.store.SetUserDRBDDisconnected(userID, true)
	coord.store.RecordLifecycleEvent(LifecycleEvent{
		UserID: userID, Type: "drbd_disconnect", Success: true,
		DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
	})
}
```

---

## Test Scripts

All test scripts go in `scfuture/scripts/layer-4.5/`.

### `scripts/layer-4.5/run.sh`

```bash
#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCFUTURE_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"

echo "═══ Layer 4.5: Suspension, Reactivation & Deletion Lifecycle ═══"
echo "Started: $(date)"
echo ""

# Verify B2 credentials
if [ -z "${B2_KEY_ID:-}" ] || [ -z "${B2_APP_KEY:-}" ]; then
    echo "ERROR: B2_KEY_ID and B2_APP_KEY environment variables are required"
    echo "  export B2_KEY_ID=your-key-id"
    echo "  export B2_APP_KEY=your-app-key"
    exit 1
fi

# Create B2 bucket for this test
BUCKET_NAME="l45-test-$(head -c 8 /dev/urandom | xxd -p)"
echo "Creating B2 bucket: $BUCKET_NAME"
b2 account authorize "$B2_KEY_ID" "$B2_APP_KEY" > /dev/null
b2 bucket create "$BUCKET_NAME" allPrivate > /dev/null
echo "Bucket created: $BUCKET_NAME"
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
echo "B2 bucket deleted."

echo ""
echo "═══ Layer 4.5 Complete ═══"
echo "Finished: $(date)"

exit $TEST_RESULT
```

### `scripts/layer-4.5/common.sh`

Same as Layer 4.4's `common.sh` with all `l44` changed to `l45`:

- `NETWORK_NAME="l45-net"`
- `SSH_KEY_NAME="l45-key"`
- All server name references: `l45-coordinator`, `l45-fleet-1`, etc.
- All helpers copied: `save_ips`, `load_ips`, `get_public_ip`, `ssh_cmd`, `docker_exec`, `coord_api`, `machine_api`, `check`, `phase_start`, `phase_result`, `final_result`, `wait_for_user_status`, `wait_for_machine_status`, `wait_for_user_bipod_count`, `wait_for_user_status_multi`

### `scripts/layer-4.5/infra.sh`

Same as Layer 4.4's `infra.sh` with `l44` → `l45` prefix everywhere.

### `scripts/layer-4.5/deploy.sh`

Same as Layer 4.4's `deploy.sh` with `l44` → `l45` header, plus:
- Pass `B2_KEY_ID`, `B2_APP_KEY`, and `B2_BUCKET_NAME` as environment variables to fleet machine agents
- Pass `B2_BUCKET_NAME` as environment variable to the coordinator

Add to the fleet machine systemd configuration block (after the existing `sed` commands):

```bash
# Add B2 credentials (for backup/restore endpoints)
ssh $SSH_OPTS root@"$pub_ip" "
    cat >> /etc/systemd/system/machine-agent.service.d/override.conf << 'ENVEOF'
Environment=B2_KEY_ID=${B2_KEY_ID}
Environment=B2_APP_KEY=${B2_APP_KEY}
Environment=B2_BUCKET_NAME=${B2_BUCKET_NAME}
ENVEOF
    systemctl daemon-reload
"
```

Actually, simpler approach: add the env vars directly to the sed commands that configure the systemd service:

```bash
ssh $SSH_OPTS root@"$pub_ip" "
    sed -i 's/PLACEHOLDER_NODE_ID/$node_id/' /etc/systemd/system/machine-agent.service
    sed -i '/Environment=DATA_DIR/a Environment=NODE_ADDRESS=${priv_ip}:8080' /etc/systemd/system/machine-agent.service
    sed -i '/Environment=NODE_ADDRESS/a Environment=COORDINATOR_URL=http://10.0.0.2:8080' /etc/systemd/system/machine-agent.service
    sed -i '/Environment=COORDINATOR_URL/a Environment=B2_KEY_ID=${B2_KEY_ID}' /etc/systemd/system/machine-agent.service
    sed -i '/Environment=B2_KEY_ID/a Environment=B2_APP_KEY=${B2_APP_KEY}' /etc/systemd/system/machine-agent.service
    sed -i '/Environment=B2_APP_KEY/a Environment=B2_BUCKET_NAME=${B2_BUCKET_NAME}' /etc/systemd/system/machine-agent.service
    systemctl daemon-reload
"
```

For the coordinator, add `B2_BUCKET_NAME`:

```bash
ssh $SSH_OPTS root@"$COORD_PUB_IP" "
    sed -i '/Environment=DATA_DIR/a Environment=B2_BUCKET_NAME=${B2_BUCKET_NAME}' /etc/systemd/system/coordinator.service
    systemctl daemon-reload
"
```

Also add `WARM_RETENTION_SECONDS=15` and `EVICTION_SECONDS=30` to the coordinator service for testing:

```bash
ssh $SSH_OPTS root@"$COORD_PUB_IP" "
    sed -i '/Environment=DATA_DIR/a Environment=B2_BUCKET_NAME=${B2_BUCKET_NAME}' /etc/systemd/system/coordinator.service
    sed -i '/Environment=B2_BUCKET_NAME/a Environment=WARM_RETENTION_SECONDS=15' /etc/systemd/system/coordinator.service
    sed -i '/Environment=WARM_RETENTION_SECONDS/a Environment=EVICTION_SECONDS=30' /etc/systemd/system/coordinator.service
    systemctl daemon-reload
"
```

### `scripts/layer-4.5/cloud-init/coordinator.yaml`

Identical to Layer 4.4.

### `scripts/layer-4.5/cloud-init/fleet.yaml`

Same as Layer 4.4 but add `python3-pip` and B2 CLI installation to packages/runcmd:

```yaml
packages:
  # ... existing packages ...
  - python3-pip
  - zstd

runcmd:
  # ... existing runcmd ...
  - pip3 install --break-system-packages b2
```

### `scripts/layer-4.5/test_suite.sh`

```bash
#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/common.sh"
load_ips

echo "═══ Layer 4.5: Suspension, Reactivation & Deletion Lifecycle — Test Suite ═══"

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

for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "DRBD module loaded on $ip" 'ssh_cmd "'"$ip"'" "lsmod | grep -q drbd"'
done

for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Container image on $ip" 'ssh_cmd "'"$ip"'" "docker images platform/app-container -q" | grep -q .'
done

for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "B2 CLI available on $ip" 'ssh_cmd "'"$ip"'" "which b2"'
done

check "No lifecycle events initially" '[ "$(coord_api GET /api/lifecycle-events | jq ". | length")" -eq 0 ]'

phase_result

# ══════════════════════════════════════════
# Phase 1: Provision Users (baseline)
# ══════════════════════════════════════════
phase_start 1 "Provision Users (baseline)"

for user in alice bob; do
    coord_api POST /api/users "{\"user_id\":\"$user\"}" > /dev/null
    coord_api POST /api/users/$user/provision > /dev/null
done

for user in alice bob; do
    wait_for_user_status "$user" "running" 120
    check "$user is running" '[ "$(coord_api GET /api/users/'"$user"' | jq -r .status)" = "running" ]'
done

# Write test data
ALICE_PRIMARY=$(coord_api GET /api/users/alice | jq -r .primary_machine)
ALICE_PRIMARY_IP=$(get_public_ip "$ALICE_PRIMARY")
check "Write test data to alice" '
    docker_exec "'"$ALICE_PRIMARY_IP"'" alice-agent "sh -c \"echo hello-alice > /workspace/data/test.txt\""
'

BOB_PRIMARY=$(coord_api GET /api/users/bob | jq -r .primary_machine)
BOB_PRIMARY_IP=$(get_public_ip "$BOB_PRIMARY")
check "Write test data to bob" '
    docker_exec "'"$BOB_PRIMARY_IP"'" bob-agent "sh -c \"echo hello-bob > /workspace/data/test.txt\""
'

# Verify DRBD healthy
check "alice DRBD UpToDate" '
    machine_api "'"$ALICE_PRIMARY_IP"'" GET /images/alice/drbd/status | jq -e "select(.peer_disk_state == \"UpToDate\")"
'
check "bob DRBD UpToDate" '
    machine_api "'"$BOB_PRIMARY_IP"'" GET /images/bob/drbd/status | jq -e "select(.peer_disk_state == \"UpToDate\")"
'

phase_result

# ══════════════════════════════════════════
# Phase 2: Suspend alice
# ══════════════════════════════════════════
phase_start 2 "Suspend alice"

coord_api POST /api/users/alice/suspend > /dev/null

wait_for_user_status "alice" "suspended" 120
check "alice is suspended" '[ "$(coord_api GET /api/users/alice | jq -r .status)" = "suspended" ]'

# Container should be stopped
check "alice container stopped" '
    machine_api "'"$ALICE_PRIMARY_IP"'" GET /containers/alice/status | jq -e "select(.running == false)"
'

# DRBD should be Secondary (demoted)
check "alice DRBD role is Secondary" '
    machine_api "'"$ALICE_PRIMARY_IP"'" GET /images/alice/drbd/status | jq -e "select(.role == \"Secondary\")"
'

# B2 backup should exist
check "alice has B2 backup" '[ "$(coord_api GET /api/users/alice | jq -r .backup_exists)" = "true" ]'

# Lifecycle event recorded
check "Suspension event recorded" '
    coord_api GET /api/lifecycle-events | jq -e "[.[] | select(.user_id == \"alice\" and .type == \"suspension\" and .success == true)] | length > 0"
'

phase_result

# ══════════════════════════════════════════
# Phase 3: Warm Reactivation
# ══════════════════════════════════════════
phase_start 3 "Warm Reactivation (alice)"

coord_api POST /api/users/alice/reactivate > /dev/null

wait_for_user_status "alice" "running" 120
check "alice is running again" '[ "$(coord_api GET /api/users/alice | jq -r .status)" = "running" ]'

# Container should be running
ALICE_PRIMARY=$(coord_api GET /api/users/alice | jq -r .primary_machine)
ALICE_PRIMARY_IP=$(get_public_ip "$ALICE_PRIMARY")

check "alice container running" '
    machine_api "'"$ALICE_PRIMARY_IP"'" GET /containers/alice/status | jq -e "select(.running == true)"
'

# Data should be intact
check "alice test data intact" '
    docker_exec "'"$ALICE_PRIMARY_IP"'" alice-agent "cat /workspace/data/test.txt" | grep -q "hello-alice"
'

# DRBD should be Primary
check "alice DRBD is Primary" '
    machine_api "'"$ALICE_PRIMARY_IP"'" GET /images/alice/drbd/status | jq -e "select(.role == \"Primary\")"
'

check "Warm reactivation event recorded" '
    coord_api GET /api/lifecycle-events | jq -e "[.[] | select(.user_id == \"alice\" and .type == \"reactivation_warm\" and .success == true)] | length > 0"
'

phase_result

# ══════════════════════════════════════════
# Phase 4: Suspend alice again (for eviction test)
# ══════════════════════════════════════════
phase_start 4 "Suspend alice again (pre-eviction)"

# Write more data first
check "Write more data to alice" '
    docker_exec "'"$ALICE_PRIMARY_IP"'" alice-agent "sh -c \"echo post-reactivation-data > /workspace/data/test2.txt\""
'

coord_api POST /api/users/alice/suspend > /dev/null

wait_for_user_status "alice" "suspended" 120
check "alice suspended again" '[ "$(coord_api GET /api/users/alice | jq -r .status)" = "suspended" ]'
check "alice B2 backup updated" '[ "$(coord_api GET /api/users/alice | jq -r .backup_exists)" = "true" ]'

phase_result

# ══════════════════════════════════════════
# Phase 5: Evict alice
# ══════════════════════════════════════════
phase_start 5 "Evict alice"

coord_api POST /api/users/alice/evict > /dev/null

wait_for_user_status "alice" "evicted" 120
check "alice is evicted" '[ "$(coord_api GET /api/users/alice | jq -r .status)" = "evicted" ]'

# No bipods should remain
check "alice has no bipods" '[ "$(coord_api GET /api/users/alice/bipod | jq ". | length")" -eq 0 ]'

# Images should be deleted on fleet machines
check "alice image deleted on primary" '
    machine_api "'"$ALICE_PRIMARY_IP"'" GET /status | jq -e "select(.users.alice == null)"
'

check "Eviction event recorded" '
    coord_api GET /api/lifecycle-events | jq -e "[.[] | select(.user_id == \"alice\" and .type == \"eviction\" and .success == true)] | length > 0"
'

phase_result

# ══════════════════════════════════════════
# Phase 6: Cold Reactivation from B2
# ══════════════════════════════════════════
phase_start 6 "Cold Reactivation (alice from B2)"

coord_api POST /api/users/alice/reactivate > /dev/null

wait_for_user_status "alice" "running" 300
check "alice is running after cold reactivation" '[ "$(coord_api GET /api/users/alice | jq -r .status)" = "running" ]'

ALICE_PRIMARY=$(coord_api GET /api/users/alice | jq -r .primary_machine)
ALICE_PRIMARY_IP=$(get_public_ip "$ALICE_PRIMARY")

check "alice container running" '
    machine_api "'"$ALICE_PRIMARY_IP"'" GET /containers/alice/status | jq -e "select(.running == true)"
'

# Data should be intact — both the original and post-reactivation data
check "alice original data survived cold restore" '
    docker_exec "'"$ALICE_PRIMARY_IP"'" alice-agent "cat /workspace/data/test.txt" | grep -q "hello-alice"
'
check "alice post-reactivation data survived cold restore" '
    docker_exec "'"$ALICE_PRIMARY_IP"'" alice-agent "cat /workspace/data/test2.txt" | grep -q "post-reactivation-data"
'

# Should have 2 bipods (fully replicated)
check "alice has 2 bipods" '[ "$(coord_api GET /api/users/alice/bipod | jq "[.[] | select(.role != \"stale\")] | length")" -eq 2 ]'

check "Cold reactivation event recorded" '
    coord_api GET /api/lifecycle-events | jq -e "[.[] | select(.user_id == \"alice\" and .type == \"reactivation_cold\" and .success == true)] | length > 0"
'

phase_result

# ══════════════════════════════════════════
# Phase 7: Retention Enforcer — DRBD Disconnect
# ══════════════════════════════════════════
phase_start 7 "Retention Enforcer — DRBD Disconnect (bob)"

# Suspend bob
coord_api POST /api/users/bob/suspend > /dev/null
wait_for_user_status "bob" "suspended" 120
check "bob is suspended" '[ "$(coord_api GET /api/users/bob | jq -r .status)" = "suspended" ]'

# Wait for retention enforcer to disconnect DRBD (WARM_RETENTION_SECONDS=15)
echo "  Waiting for retention enforcer to disconnect DRBD (~15-75s)..."
for i in $(seq 1 90); do
    disconnected=$(coord_api GET /api/users/bob | jq -r '.drbd_disconnected // false')
    if [ "$disconnected" = "true" ]; then
        break
    fi
    sleep 1
done

check "bob DRBD disconnected by retention enforcer" '[ "$(coord_api GET /api/users/bob | jq -r .drbd_disconnected)" = "true" ]'

check "DRBD disconnect event recorded" '
    coord_api GET /api/lifecycle-events | jq -e "[.[] | select(.user_id == \"bob\" and .type == \"drbd_disconnect\" and .success == true)] | length > 0"
'

# Verify DRBD is actually StandAlone on the machine
BOB_PRIMARY=$(coord_api GET /api/users/bob | jq -r .primary_machine)
BOB_PRIMARY_IP=$(get_public_ip "$BOB_PRIMARY")
check "bob DRBD is StandAlone" '
    machine_api "'"$BOB_PRIMARY_IP"'" GET /images/bob/drbd/status | jq -e "select(.connection_state == \"StandAlone\")"
'

phase_result

# ══════════════════════════════════════════
# Phase 8: Retention Enforcer — Auto Eviction
# ══════════════════════════════════════════
phase_start 8 "Retention Enforcer — Auto Eviction (bob)"

# Wait for retention enforcer to auto-evict bob (EVICTION_SECONDS=30)
echo "  Waiting for retention enforcer to auto-evict bob (~30-90s from suspension)..."
wait_for_user_status "bob" "evicted" 120

check "bob is auto-evicted" '[ "$(coord_api GET /api/users/bob | jq -r .status)" = "evicted" ]'
check "bob has no bipods" '[ "$(coord_api GET /api/users/bob/bipod | jq ". | length")" -eq 0 ]'

phase_result

# ══════════════════════════════════════════
# Phase 9: Coordinator State Consistency
# ══════════════════════════════════════════
phase_start 9 "Coordinator State Consistency"

check "alice is running" '[ "$(coord_api GET /api/users/alice | jq -r .status)" = "running" ]'
check "bob is evicted" '[ "$(coord_api GET /api/users/bob | jq -r .status)" = "evicted" ]'

check "alice has backup" '[ "$(coord_api GET /api/users/alice | jq -r .backup_exists)" = "true" ]'
check "bob has backup" '[ "$(coord_api GET /api/users/bob | jq -r .backup_exists)" = "true" ]'

check "Lifecycle events count >= 6" '[ "$(coord_api GET /api/lifecycle-events | jq ". | length")" -ge 6 ]'

# state.json should exist and be valid
check "state.json persisted" 'ssh_cmd "$COORD_PUB_IP" "cat /data/state.json" | jq -e .users'

phase_result

# ══════════════════════════════════════════
# Phase 10: Cleanup
# ══════════════════════════════════════════
phase_start 10 "Cleanup"

for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Cleanup $ip" 'machine_api "'"$ip"'" POST /cleanup'
done

for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Verify clean $ip" '
        user_count=$(machine_api "'"$ip"'" GET /status | jq ".users | length")
        [ "$user_count" -eq 0 ]
    '
done

phase_result

# ══════════════════════════════════════════
# Final Result
# ══════════════════════════════════════════
final_result
```

---

## Summary of All Changes

### New machine agent endpoints (5):
| Method | Path | Purpose |
|--------|------|---------|
| POST | /images/{user_id}/snapshot | Create Btrfs snapshot |
| POST | /images/{user_id}/backup | btrfs send → zstd → B2 upload |
| POST | /images/{user_id}/restore | B2 download → zstd → btrfs receive → workspace |
| GET | /images/{user_id}/backup/status | Check B2 backup existence |
| POST | /images/{user_id}/drbd/connect | Reconnect DRBD peer |

### Modified machine agent endpoints (1):
| Method | Path | Change |
|--------|------|--------|
| POST | /images/{user_id}/format-btrfs | Accepts optional `{"bare": true}` for mkfs-only mode |

### New coordinator endpoints (4):
| Method | Path | Purpose |
|--------|------|---------|
| POST | /api/users/{id}/suspend | Suspend a running user |
| POST | /api/users/{id}/reactivate | Reactivate (warm or cold) |
| POST | /api/users/{id}/evict | Evict a suspended user |
| GET | /api/lifecycle-events | List lifecycle events |

### New files (3 Go + 7 scripts):
- `internal/machineagent/backup.go` — B2 backup and restore
- `internal/coordinator/lifecycle.go` — Suspend, reactivate, evict orchestration
- `internal/coordinator/retention.go` — Retention enforcer goroutine
- `scripts/layer-4.5/` — run.sh, common.sh, infra.sh, deploy.sh, test_suite.sh, cloud-init/coordinator.yaml, cloud-init/fleet.yaml

### Modified files (9):
- `internal/shared/types.go` — New types
- `internal/machineagent/btrfs.go` — Snapshot method + bare format mode
- `internal/machineagent/drbd.go` — DRBDConnect method
- `internal/machineagent/server.go` — New routes + modified FormatBtrfs handler
- `internal/coordinator/store.go` — User fields + lifecycle events + new methods
- `internal/coordinator/server.go` — New routes + B2BucketName on Coordinator
- `internal/coordinator/machineapi.go` — New client methods
- `internal/coordinator/healthcheck.go` — Handle suspended users on dead machines
- `cmd/coordinator/main.go` — Start retention enforcer + B2 bucket env var

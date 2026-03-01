# Layer 4.4 Build Prompt — Bipod Reformation & Dead Machine Re-integration

## What This Is

This is a build prompt for Layer 4.4 of the scfuture distributed agent platform. You are Claude Code. Your job is to:

1. Read all referenced existing files to understand the current codebase
2. Write all new code and scripts described below
3. Report back when code is written
4. When told "yes" / "ready", run the full test lifecycle (infra up → deploy → test → iterate on failures → teardown)
5. When all tests pass, update `SESSION.md` (in the parent directory) with what happened
6. Give a final report

The project lives in `scfuture/` (a subdirectory of the current working directory). All Go code paths are relative to `scfuture/`. All script paths are relative to `scfuture/`. The `SESSION.md` file is in the parent directory (current working directory).

---

## Context: What Exists

Layers 4.1 (machine agent), 4.2 (coordinator happy path), and 4.3 (heartbeat failure detection & automatic failover) are complete and committed. Read these files first to understand the existing codebase:

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
scfuture/container/Dockerfile
scfuture/container/container-init.sh
```

Read ALL of these before writing any code. Pay close attention to:
- How `DRBDCreate` in `drbd.go` writes the DRBD config file (`/etc/drbd.d/{resource}.res`) with the exact template format, including `minor` as both the device minor AND the `minor` keyword
- How `DRBDDestroy` runs `drbdadm down` + removes the config file
- How `DRBDPromote` uses `--force` flag (allows promotion without connected peer)
- How the `DRBDCreateRequest` uses `ResourceName` field (set to `"user-" + userID` by the provisioner)
- How `Store.SetUserStatus` takes `(userID, status, errMsg string)` — the User struct has no `StatusChangedAt` field currently
- How `Store.GetSurvivingBipod` filters by `Role != "stale"` to find the live bipod
- How the provisioner builds the `DRBDCreateRequest` with `stripPort()` for DRBD addresses (DRBD uses raw IP, not IP:port)
- How the `MachineClient` uses `doJSON()` for all HTTP calls with 30-second timeout
- How `StartHealthChecker` is called from `main.go` with `coord.GetStore()` and `coord` as arguments
- How the healthcheck's `failoverUser` sets bipod roles to `"stale"` and handles both primary-died and secondary-died cases

### Reference documents (in parent directory):

```
SESSION.md
architecture-v3.md (if present)
master-prompt4.3.md
```

---

## What Layer 4.4 Builds

**In scope:**
- Reformer goroutine — automatic bipod reformation for degraded users (restore 2-copy replication)
- DRBD disconnect endpoint — disconnect from a dead/unreachable peer
- DRBD reconfigure endpoint — update DRBD config to point to a new peer, apply with `drbdadm adjust` (fallback: `drbdadm down`/`up`/`primary --force`)
- Reformation workflow: detect degraded users → select new secondary → create image on new machine → configure DRBD on new secondary → disconnect dead peer on primary → reconfigure primary to new peer → wait for sync
- Dead machine cleanup on resurrection — when a dead machine comes back, clean up stale DRBD resources and images
- Reformation event recording for observability
- New API endpoint: `GET /api/reformations` — returns recorded reformation events
- New user status: `"reforming"` — user is undergoing bipod reformation
- Full integration test: provision users → kill a fleet machine (trigger 4.3 failover) → verify reformation restores full replication → power on dead machine → verify cleanup

**Explicitly NOT in scope (future layers):**
- Suspension / reactivation / deletion lifecycle (Layer 4.5)
- Crash recovery / reconciliation (Layer 4.6)
- Live migration (Layer 5)

---

## Architecture

### Test Topology

Same as Layer 4.3: 1 coordinator + 3 fleet machines on Hetzner Cloud.

```
macOS (test harness, runs test_suite.sh via SSH / curl)
  │
  ├── l44-coordinator (CX23, coordinator :8080, private 10.0.0.2)
  │     │
  │     ├── l44-fleet-1 (CX23, machine-agent :8080, private 10.0.0.11)
  │     ├── l44-fleet-2 (CX23, machine-agent :8080, private 10.0.0.12)
  │     └── l44-fleet-3 (CX23, machine-agent :8080, private 10.0.0.13)
  │
  Private network: l44-net / 10.0.0.0/24
```

### How Bipod Reformation Works

The coordinator runs a **reformer goroutine** that ticks every 30 seconds:

1. Scan all users for `status == "running_degraded"`
2. For each degraded user, check if `StatusChangedAt` is older than the stabilization period (30 seconds) — if not, skip (prevents thrashing)
3. Check if the user has stale bipods on machines that are now `"active"` — if so, clean them up first
4. Select a new secondary machine (active, not the user's current primary, least-loaded)
5. Run the reformation sequence (see below)

### Reformation Sequence (per user)

```
1. Set user status → "reforming"
2. Select new secondary machine, allocate DRBD minor on it
3. Create image on new secondary (POST /images/{user_id}/create)
4. Configure DRBD on new secondary (POST /images/{user_id}/drbd/create)
   → New secondary runs: write config, create-md, drbdadm up
   → Secondary is now listening, waiting for primary to connect
5. Disconnect dead peer on primary (POST /images/{user_id}/drbd/disconnect)
   → Idempotent — may already be disconnected/StandAlone
6. Reconfigure DRBD on primary (POST /images/{user_id}/drbd/reconfigure)
   → Write new config, try drbdadm adjust
   → If adjust fails: return error with "needs_restart" flag
   → Coordinator handles fallback: container stop → reconfigure force → container start
7. Wait for DRBD sync (poll primary's status for PeerDiskState == "UpToDate")
   → Timeout: 300 seconds
8. Update coordinator state:
   → Remove stale bipod (old dead machine)
   → Create new bipod (new secondary, role: "secondary")
   → Set user status → "running"
   → Record reformation event
```

**Error handling per step:**
- Step 2 fails (no available machines): stay `running_degraded`, retry next tick
- Step 3-6 fails: cleanup partial work on new secondary, stay `running_degraded`, retry next tick
- Step 7 timeout: leave new bipod in place but set user `running_degraded` with note about sync timeout
- Any step: each user's reformation is independent, one failure does not block others

### Dead Machine Cleanup

When the reformer encounters a degraded user with stale bipods on machines that are now `"active"`, it cleans them up before starting reformation:

```
For each stale bipod on an active machine:
  1. POST /images/{user_id}/drbd/destroy on returned machine (drbdadm down + remove config)
  2. DELETE /images/{user_id} on returned machine (remove loop device + image file)
  3. Remove stale bipod from coordinator state
```

This is integrated into the reformer's flow, not a separate goroutine. The cleanup runs as the first step when a degraded user is found with stale bipods on active machines.

### DRBD Reconfigure: `adjust` vs `down/up` Fallback

The key technical test of this layer: can we replace a DRBD peer on a running primary without downtime?

**Happy path (`drbdadm adjust` works):**
1. Primary's container keeps running
2. DRBD disconnects old peer, connects to new peer
3. Sync happens in background
4. Zero downtime

**Fallback path (`drbdadm adjust` fails):**
1. Coordinator stops container on primary
2. Calls reconfigure with `force: true` → machine agent does: `drbdadm down` → write config → `drbdadm up` → `drbdadm primary --force`
3. Coordinator starts container on primary
4. ~5-10 second downtime

The reconfigure endpoint reports which method was used via the `method` field in the response (`"adjust"` or `"down_up"`). The coordinator handles container lifecycle for the fallback path.

### State Transitions

**User statuses (new transitions):**
```
running_degraded ──(reformer tick, stabilized)──▶ reforming ──▶ running (fully replicated)
                                                              ──▶ running_degraded (reformation failed)
```

**Bipod roles during reformation:**
```
Before:  primary (on surviving machine), stale (on dead machine)
After:   primary (unchanged), secondary (on new machine)
         stale bipod removed from state
```

### Address Conventions

Same as Layer 4.3:
- Coordinator private: `10.0.0.2:8080`
- Fleet private: `10.0.0.11:8080`, `10.0.0.12:8080`, `10.0.0.13:8080`
- Machine agents register with their private IP as `NODE_ADDRESS`
- Coordinator calls machine agents via their registered address (private IP)
- Test harness calls coordinator and machine agents via public IPs

---

## Modifications to Existing Code

### 1. `internal/shared/types.go` — Add DRBD disconnect/reconfigure types and reformation event

Add:

```go
// ─── DRBD disconnect/reconfigure types (from drbd.go, Layer 4.4) ───

type DRBDDisconnectResponse struct {
	Status       string `json:"status"`
	WasConnected bool   `json:"was_connected"`
}

type DRBDReconfigureRequest struct {
	Nodes []DRBDNode `json:"nodes"`
	Port  int        `json:"port"`
	Force bool       `json:"force"` // false=adjust only, true=down/up/promote
}

type DRBDReconfigureResponse struct {
	Status string `json:"status"` // "reconfigured"
	Method string `json:"method"` // "adjust" or "down_up"
}

// ─── Reformation types (from coordinator reformer) ───

type ReformationEventResponse struct {
	UserID       string `json:"user_id"`
	OldSecondary string `json:"old_secondary"`
	NewSecondary string `json:"new_secondary"`
	Success      bool   `json:"success"`
	Error        string `json:"error,omitempty"`
	Method       string `json:"method,omitempty"` // "adjust" or "down_up"
	DurationMS   int64  `json:"duration_ms"`
	Timestamp    string `json:"timestamp"`
}
```

### 2. `internal/machineagent/drbd.go` — Add DRBDDisconnect and DRBDReconfigure

Add two new methods:

```go
func (a *Agent) DRBDDisconnect(userID string) (*shared.DRBDDisconnectResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}

	resName := "user-" + userID

	// Check current status
	info := a.getDRBDStatus(resName)
	if info == nil {
		return nil, fmt.Errorf("DRBD resource %s does not exist", resName)
	}

	// If already StandAlone or no peer connection, return success
	if info.ConnectionState == "StandAlone" {
		slog.Info("DRBD already disconnected (StandAlone)", "component", "drbd", "user", userID)
		return &shared.DRBDDisconnectResponse{Status: "disconnected", WasConnected: false}, nil
	}

	result, err := runCmd("drbdadm", "disconnect", resName)
	if err != nil {
		return nil, cmdError("drbdadm disconnect failed", cmdString("drbdadm", "disconnect", resName), result)
	}

	slog.Info("DRBD disconnected from peer", "component", "drbd", "user", userID)
	return &shared.DRBDDisconnectResponse{Status: "disconnected", WasConnected: true}, nil
}

func (a *Agent) DRBDReconfigure(userID string, req *shared.DRBDReconfigureRequest) (*shared.DRBDReconfigureResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}
	if len(req.Nodes) != 2 {
		return nil, fmt.Errorf("exactly 2 nodes required")
	}

	resName := "user-" + userID
	configPath := fmt.Sprintf("/etc/drbd.d/%s.res", resName)

	// Write new config file (same format as DRBDCreate)
	config := fmt.Sprintf(`resource %s {
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
    on %s {
        device /dev/drbd%d minor %d;
        disk %s;
        address %s:%d;
        meta-disk internal;
    }
    on %s {
        device /dev/drbd%d minor %d;
        disk %s;
        address %s:%d;
        meta-disk internal;
    }
}
`, resName,
		req.Nodes[0].Hostname, req.Nodes[0].Minor, req.Nodes[0].Minor, req.Nodes[0].Disk, req.Nodes[0].Address, req.Port,
		req.Nodes[1].Hostname, req.Nodes[1].Minor, req.Nodes[1].Minor, req.Nodes[1].Disk, req.Nodes[1].Address, req.Port,
	)

	if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
		return nil, fmt.Errorf("write DRBD config: %w", err)
	}

	if !req.Force {
		// Try adjust
		result, err := runCmd("drbdadm", "adjust", resName)
		if err == nil {
			slog.Info("DRBD reconfigured via adjust", "component", "drbd", "user", userID)

			// Update in-memory state with new peer info
			hostname, _ := os.Hostname()
			u := a.getUser(userID)
			if u != nil {
				for _, n := range req.Nodes {
					if n.Hostname == hostname {
						u.DRBDMinor = n.Minor
						u.DRBDDevice = fmt.Sprintf("/dev/drbd%d", n.Minor)
					}
				}
				a.setUser(userID, u)
			}

			return &shared.DRBDReconfigureResponse{Status: "reconfigured", Method: "adjust"}, nil
		}
		slog.Warn("drbdadm adjust failed", "component", "drbd", "user", userID, "error", err, "output", result.Stderr)
		// Return error — coordinator will handle fallback
		return nil, fmt.Errorf("adjust failed (stderr: %s), coordinator should retry with force=true", result.Stderr)
	}

	// Force path: full down/up/promote cycle
	// The coordinator is responsible for stopping/starting the container around this call
	slog.Info("DRBD reconfigure via down/up (force)", "component", "drbd", "user", userID)

	// Unmount host if mounted (safety)
	mountPath := a.mountPath(userID)
	if isMounted(mountPath) {
		runCmd("umount", mountPath)
	}

	// Down
	runCmd("drbdadm", "down", resName)

	// Up (uses new config)
	result, err := runCmd("drbdadm", "up", resName)
	if err != nil {
		return nil, cmdError("drbdadm up failed after reconfigure", cmdString("drbdadm", "up", resName), result)
	}

	// Promote back to primary
	result, err = runCmd("drbdadm", "primary", "--force", resName)
	if err != nil {
		return nil, cmdError("drbdadm primary failed after reconfigure", cmdString("drbdadm", "primary", "--force", resName), result)
	}

	// Update in-memory state
	hostname, _ := os.Hostname()
	u := a.getUser(userID)
	if u != nil {
		for _, n := range req.Nodes {
			if n.Hostname == hostname {
				u.DRBDMinor = n.Minor
				u.DRBDDevice = fmt.Sprintf("/dev/drbd%d", n.Minor)
			}
		}
		a.setUser(userID, u)
	}

	return &shared.DRBDReconfigureResponse{Status: "reconfigured", Method: "down_up"}, nil
}
```

### 3. `internal/machineagent/server.go` — Add routes for new DRBD endpoints

Add to `RegisterRoutes`:

```go
mux.HandleFunc("POST /images/{user_id}/drbd/disconnect", a.handleDRBDDisconnect)
mux.HandleFunc("POST /images/{user_id}/drbd/reconfigure", a.handleDRBDReconfigure)
```

Add handlers:

```go
func (a *Agent) handleDRBDDisconnect(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	lock := a.getUserLock(userID)
	lock.Lock()
	defer lock.Unlock()

	resp, err := a.DRBDDisconnect(userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Agent) handleDRBDReconfigure(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	lock := a.getUserLock(userID)
	lock.Lock()
	defer lock.Unlock()

	var req shared.DRBDReconfigureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	resp, err := a.DRBDReconfigure(userID, &req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
```

### 4. `internal/coordinator/machineapi.go` — Add DRBDDisconnect and DRBDReconfigure client methods

Add:

```go
func (c *MachineClient) DRBDDisconnect(userID string) (*shared.DRBDDisconnectResponse, error) {
	var resp shared.DRBDDisconnectResponse
	if err := c.doJSON("POST", fmt.Sprintf("/images/%s/drbd/disconnect", userID), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) DRBDReconfigure(userID string, req *shared.DRBDReconfigureRequest) (*shared.DRBDReconfigureResponse, error) {
	var resp shared.DRBDReconfigureResponse
	if err := c.doJSON("POST", fmt.Sprintf("/images/%s/drbd/reconfigure", userID), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
```

### 5. `internal/coordinator/store.go` — Add StatusChangedAt to User, reformation events, and new methods

Add `StatusChangedAt` to `User` struct:

```go
type User struct {
	UserID         string    `json:"user_id"`
	Status         string    `json:"status"`
	StatusChangedAt time.Time `json:"status_changed_at"`
	PrimaryMachine string    `json:"primary_machine"`
	DRBDPort       int       `json:"drbd_port"`
	ImageSizeMB    int       `json:"image_size_mb"`
	Error          string    `json:"error"`
	CreatedAt      time.Time `json:"created_at"`
}
```

Add `ReformationEvent` struct:

```go
type ReformationEvent struct {
	UserID       string    `json:"user_id"`
	OldSecondary string    `json:"old_secondary"`
	NewSecondary string    `json:"new_secondary"`
	Success      bool      `json:"success"`
	Error        string    `json:"error,omitempty"`
	Method       string    `json:"method,omitempty"` // "adjust" or "down_up"
	DurationMS   int64     `json:"duration_ms"`
	Timestamp    time.Time `json:"timestamp"`
}
```

Add `reformationEvents` to `Store` struct:

```go
type Store struct {
	// ... existing fields ...
	reformationEvents []ReformationEvent
}
```

Add to `persistState`:

```go
type persistState struct {
	// ... existing fields ...
	ReformationEvents []ReformationEvent `json:"reformation_events"`
}
```

Update `persist()` to include `reformationEvents` in `persistState`.

Modify `SetUserStatus` to set `StatusChangedAt`:

```go
func (s *Store) SetUserStatus(userID, status, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return
	}
	u.Status = status
	u.StatusChangedAt = time.Now()
	u.Error = errMsg
	s.persist()
}
```

Add new methods:

```go
// GetDegradedUsers returns users in running_degraded status whose
// StatusChangedAt is older than the stabilization period.
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

// GetStaleBipodsOnActiveMachines returns stale bipods for a user that are
// on machines currently in "active" status. These need cleanup before reformation.
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

// RemoveBipod removes a specific bipod entry for a user.
func (s *Store) RemoveBipod(userID, machineID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := userID + ":" + machineID
	delete(s.bipods, key)
	s.persist()
}

// SelectOneSecondary picks the least-loaded active machine, excluding the
// specified machine IDs. Increments active_agents on the selected machine.
func (s *Store) SelectOneSecondary(excludeMachineIDs []string) (*Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	exclude := make(map[string]bool)
	for _, id := range excludeMachineIDs {
		exclude[id] = true
	}

	var candidates []*Machine
	for _, m := range s.machines {
		if m.Status != "active" {
			continue
		}
		if exclude[m.MachineID] {
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
	result := *candidates[0]
	s.persist()
	return &result, nil
}

// RecordReformationEvent appends a reformation event.
func (s *Store) RecordReformationEvent(event ReformationEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reformationEvents = append(s.reformationEvents, event)
	s.persist()
}

// GetReformationEvents returns all recorded reformation events.
func (s *Store) GetReformationEvents() []ReformationEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]ReformationEvent, len(s.reformationEvents))
	copy(result, s.reformationEvents)
	return result
}
```

### 6. `internal/coordinator/server.go` — Add reformation events endpoint

Add route to `RegisterRoutes`:

```go
mux.HandleFunc("GET /api/reformations", coord.handleGetReformations)
```

Add handler:

```go
func (coord *Coordinator) handleGetReformations(w http.ResponseWriter, r *http.Request) {
	events := coord.store.GetReformationEvents()
	if events == nil {
		events = []ReformationEvent{}
	}
	writeJSON(w, http.StatusOK, events)
}
```

### 7. `cmd/coordinator/main.go` — Start reformer goroutine

After starting the health checker, add:

```go
// Start health checker
coordinator.StartHealthChecker(coord.GetStore(), coordinator.NewMachineClient(""), coord)

// Start reformer
coordinator.StartReformer(coord.GetStore(), coord)
```

---

## New Implementation

### New file: `internal/coordinator/reformer.go`

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
	ReformerInterval    = 30 * time.Second
	StabilizationPeriod = 30 * time.Second // Wait after status change before reforming
	SyncTimeout         = 300 * time.Second // 5 minutes for initial sync
)

// StartReformer launches a background goroutine that periodically
// scans for degraded users and reforms their bipods.
func StartReformer(store *Store, coord *Coordinator) {
	go func() {
		slog.Info("[REFORMER] Reformer started",
			"interval", ReformerInterval.String(),
			"stabilization_period", StabilizationPeriod.String(),
		)

		ticker := time.NewTicker(ReformerInterval)
		defer ticker.Stop()
		for range ticker.C {
			coord.reformDegradedUsers()
		}
	}()
}

// reformDegradedUsers scans for users needing reformation and processes them.
func (coord *Coordinator) reformDegradedUsers() {
	degraded := coord.store.GetDegradedUsers(StabilizationPeriod)
	if len(degraded) == 0 {
		return
	}

	slog.Info("[REFORMER] Found degraded users", "count", len(degraded))

	for _, user := range degraded {
		coord.reformUser(user.UserID)
	}
}

// reformUser handles bipod reformation for a single degraded user.
func (coord *Coordinator) reformUser(userID string) {
	start := time.Now()
	logger := slog.With("component", "reformer", "user_id", userID)

	// Re-check status (may have changed since scan)
	user := coord.store.GetUser(userID)
	if user == nil || user.Status != "running_degraded" {
		logger.Info("Skipping — user not in running_degraded state", "status", user.Status)
		return
	}

	// ── Step 0: Clean up stale bipods on active machines ──
	staleBipods := coord.store.GetStaleBipodsOnActiveMachines(userID)
	for _, stale := range staleBipods {
		logger.Info("Cleaning up stale bipod on returned machine",
			"machine_id", stale.MachineID,
		)
		staleMachine := coord.store.GetMachine(stale.MachineID)
		if staleMachine != nil {
			client := NewMachineClient(staleMachine.Address)
			// Destroy DRBD resource (ignore errors — may already be down)
			if err := client.DRBDDestroy(userID); err != nil {
				logger.Warn("Stale DRBD destroy failed (non-fatal)", "error", err)
			}
			// Delete image
			if err := client.DeleteUser(userID); err != nil {
				logger.Warn("Stale image delete failed (non-fatal)", "error", err)
			}
		}
		coord.store.RemoveBipod(userID, stale.MachineID)
		logger.Info("Stale bipod cleaned up", "machine_id", stale.MachineID)
	}

	// ── Step 1: Set user status → "reforming" ──
	coord.store.SetUserStatus(userID, "reforming", "")

	// ── Step 2: Select new secondary machine ──
	// Exclude the current primary
	excludeIDs := []string{user.PrimaryMachine}
	newSecondary, err := coord.store.SelectOneSecondary(excludeIDs)
	if err != nil {
		logger.Warn("No available machine for reformation", "error", err)
		coord.store.SetUserStatus(userID, "running_degraded", "no machine available: "+err.Error())
		return
	}

	newMinor := coord.store.AllocateMinor(newSecondary.MachineID)
	logger.Info("Selected new secondary",
		"new_secondary", newSecondary.MachineID,
		"minor", newMinor,
	)

	// Get primary machine info
	primaryMachine := coord.store.GetMachine(user.PrimaryMachine)
	if primaryMachine == nil {
		logger.Error("Primary machine not found in store")
		coord.store.SetUserStatus(userID, "running_degraded", "primary machine not found")
		return
	}

	// Get primary bipod to know its minor and loop device
	primaryBipod := coord.store.GetSurvivingBipod(userID, "") // Get the non-stale bipod
	if primaryBipod == nil {
		// Try another way — look for bipod on primary machine
		bipods := coord.store.GetBipods(userID)
		for _, b := range bipods {
			if b.MachineID == user.PrimaryMachine && b.Role != "stale" {
				primaryBipod = b
				break
			}
		}
	}
	if primaryBipod == nil {
		logger.Error("Cannot find primary bipod")
		coord.store.SetUserStatus(userID, "running_degraded", "primary bipod not found")
		return
	}

	primaryClient := NewMachineClient(primaryMachine.Address)
	secondaryClient := NewMachineClient(newSecondary.Address)

	primaryAddr := stripPort(primaryMachine.Address)
	secondaryAddr := stripPort(newSecondary.Address)

	// ── Step 3: Create image on new secondary ──
	imgResp, err := secondaryClient.CreateImage(userID, user.ImageSizeMB)
	if err != nil {
		logger.Error("Create image on new secondary failed", "error", err)
		coord.store.SetUserStatus(userID, "running_degraded", "image create failed: "+err.Error())
		coord.store.RecordReformationEvent(ReformationEvent{
			UserID: userID, OldSecondary: "", NewSecondary: newSecondary.MachineID,
			Success: false, Error: "image create: " + err.Error(),
			DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
		})
		return
	}
	secondaryLoop := imgResp.LoopDevice
	logger.Info("Image created on new secondary", "loop", secondaryLoop)

	// Build DRBD config request — same config for both sides
	drbdReq := &shared.DRBDCreateRequest{
		ResourceName: "user-" + userID,
		Nodes: []shared.DRBDNode{
			{
				Hostname: primaryMachine.MachineID,
				Minor:    primaryBipod.DRBDMinor,
				Disk:     primaryBipod.LoopDevice,
				Address:  primaryAddr,
			},
			{
				Hostname: newSecondary.MachineID,
				Minor:    newMinor,
				Disk:     secondaryLoop,
				Address:  secondaryAddr,
			},
		},
		Port: user.DRBDPort,
	}

	// ── Step 4: Configure DRBD on new secondary ──
	// New secondary writes config, creates metadata, runs drbdadm up
	_, err = secondaryClient.DRBDCreate(userID, drbdReq)
	if err != nil {
		logger.Error("DRBD create on new secondary failed", "error", err)
		// Cleanup: delete image on new secondary
		secondaryClient.DeleteUser(userID)
		coord.store.SetUserStatus(userID, "running_degraded", "drbd create failed: "+err.Error())
		coord.store.RecordReformationEvent(ReformationEvent{
			UserID: userID, OldSecondary: "", NewSecondary: newSecondary.MachineID,
			Success: false, Error: "drbd create: " + err.Error(),
			DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
		})
		return
	}
	logger.Info("DRBD configured on new secondary")

	// ── Step 5: Disconnect dead peer on primary ──
	_, err = primaryClient.DRBDDisconnect(userID)
	if err != nil {
		logger.Warn("DRBD disconnect on primary failed (non-fatal, may already be disconnected)", "error", err)
		// Continue — the disconnect may fail because the resource is already StandAlone
		// The reconfigure step will handle reconnection
	}
	logger.Info("Dead peer disconnected on primary")

	// ── Step 6: Reconfigure DRBD on primary ──
	reconfigReq := &shared.DRBDReconfigureRequest{
		Nodes: drbdReq.Nodes,
		Port:  drbdReq.Port,
		Force: false, // Try adjust first
	}

	var reconfigMethod string
	reconfigResp, err := primaryClient.DRBDReconfigure(userID, reconfigReq)
	if err != nil {
		// Adjust failed — try fallback with container lifecycle
		logger.Warn("drbdadm adjust failed, attempting down/up fallback", "error", err)

		// Stop container on primary
		if stopErr := primaryClient.ContainerStop(userID); stopErr != nil {
			logger.Error("Container stop failed during fallback", "error", stopErr)
			// Cleanup
			secondaryClient.DRBDDestroy(userID)
			secondaryClient.DeleteUser(userID)
			coord.store.SetUserStatus(userID, "running_degraded", "container stop failed during reconfigure: "+stopErr.Error())
			coord.store.RecordReformationEvent(ReformationEvent{
				UserID: userID, OldSecondary: "", NewSecondary: newSecondary.MachineID,
				Success: false, Error: "container stop for fallback: " + stopErr.Error(),
				DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
			})
			return
		}

		// Force reconfigure (down/up/promote)
		reconfigReq.Force = true
		reconfigResp, err = primaryClient.DRBDReconfigure(userID, reconfigReq)
		if err != nil {
			logger.Error("Force reconfigure failed", "error", err)
			// Try to restart container even after failure
			primaryClient.ContainerStart(userID)
			secondaryClient.DRBDDestroy(userID)
			secondaryClient.DeleteUser(userID)
			coord.store.SetUserStatus(userID, "running_degraded", "drbd reconfigure (force) failed: "+err.Error())
			coord.store.RecordReformationEvent(ReformationEvent{
				UserID: userID, OldSecondary: "", NewSecondary: newSecondary.MachineID,
				Success: false, Error: "drbd reconfigure force: " + err.Error(),
				DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
			})
			return
		}

		// Restart container
		if _, startErr := primaryClient.ContainerStart(userID); startErr != nil {
			logger.Error("Container restart after force reconfigure failed", "error", startErr)
			coord.store.SetUserStatus(userID, "running_degraded", "container restart after reconfigure failed: "+startErr.Error())
			coord.store.RecordReformationEvent(ReformationEvent{
				UserID: userID, OldSecondary: "", NewSecondary: newSecondary.MachineID,
				Success: false, Error: "container restart: " + startErr.Error(), Method: "down_up",
				DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
			})
			return
		}

		reconfigMethod = "down_up"
	} else {
		reconfigMethod = reconfigResp.Method
	}
	logger.Info("DRBD reconfigured on primary", "method", reconfigMethod)

	// ── Step 7: Wait for DRBD sync ──
	syncStart := time.Now()
	for {
		if time.Since(syncStart) > SyncTimeout {
			logger.Warn("DRBD sync timeout — leaving bipod in place")
			// Don't cleanup — the sync may complete eventually
			coord.store.CreateBipod(userID, newSecondary.MachineID, "secondary", newMinor)
			coord.store.SetBipodLoopDevice(userID, newSecondary.MachineID, secondaryLoop)
			coord.store.SetUserStatus(userID, "running_degraded", "drbd sync timeout after reformation")
			coord.store.RecordReformationEvent(ReformationEvent{
				UserID: userID, OldSecondary: "", NewSecondary: newSecondary.MachineID,
				Success: false, Error: "sync timeout", Method: reconfigMethod,
				DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
			})
			return
		}

		status, err := primaryClient.DRBDStatus(userID)
		if err != nil {
			logger.Warn("DRBD status check failed", "error", err)
			time.Sleep(2 * time.Second)
			continue
		}

		if status.PeerDiskState == "UpToDate" {
			logger.Info("DRBD sync complete")
			break
		}

		progress := "unknown"
		if status.SyncProgress != nil {
			progress = *status.SyncProgress
		}
		logger.Info("DRBD syncing", "peer_disk", status.PeerDiskState, "progress", progress)
		time.Sleep(2 * time.Second)
	}

	// ── Step 8: Update state ──
	coord.store.CreateBipod(userID, newSecondary.MachineID, "secondary", newMinor)
	coord.store.SetBipodLoopDevice(userID, newSecondary.MachineID, secondaryLoop)
	coord.store.SetUserStatus(userID, "running", "")

	coord.store.RecordReformationEvent(ReformationEvent{
		UserID:       userID,
		OldSecondary: "", // Was already cleaned up
		NewSecondary: newSecondary.MachineID,
		Success:      true,
		Method:       reconfigMethod,
		DurationMS:   time.Since(start).Milliseconds(),
		Timestamp:    time.Now(),
	})

	logger.Info("Reformation complete — user has 2-copy replication again",
		"primary", user.PrimaryMachine,
		"new_secondary", newSecondary.MachineID,
		"method", reconfigMethod,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

// stripPort extracts the IP from an "ip:port" address string.
// Duplicated here to avoid dependency on provisioner.go.
func stripPort(address string) string {
	idx := strings.LastIndex(address, ":")
	if idx == -1 {
		return address
	}
	return address[:idx]
}
```

**Note:** The `stripPort` function is duplicated from `provisioner.go`. If this feels wrong, you can move it to a shared utility or just keep the duplication — it's a trivial function and the PoC is pragmatic.

**IMPORTANT:** The `reformUser` function references `coord.store.GetSurvivingBipod(userID, "")` — this won't work with the current implementation because `GetSurvivingBipod` requires a `deadMachineID` to filter against. Instead, find the primary bipod by iterating through `GetBipods(userID)` and looking for the one on `user.PrimaryMachine`. The code above handles this with a fallback loop. Make sure the bipod lookup finds the primary's minor and loop device correctly.

---

## Test Scripts

All test scripts go in `scfuture/scripts/layer-4.4/`.

### `scripts/layer-4.4/run.sh`

```bash
#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCFUTURE_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"

echo "═══ Layer 4.4: Bipod Reformation & Dead Machine Re-integration ═══"
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
echo "═══ Layer 4.4 Complete ═══"
echo "Finished: $(date)"

exit $TEST_RESULT
```

### `scripts/layer-4.4/common.sh`

Same as Layer 4.3's `common.sh` with all `l43` changed to `l44`:

- `NETWORK_NAME="l44-net"`
- `SSH_KEY_NAME="l44-key"`
- All server name references: `l44-coordinator`, `l44-fleet-1`, etc.
- All helpers copied: `save_ips`, `load_ips`, `get_public_ip`, `ssh_cmd`, `docker_exec`, `coord_api`, `machine_api`, `check`, `phase_start`, `phase_result`, `final_result`, `wait_for_user_status`, `wait_for_machine_status`
- Add new poll helper:

```bash
# Wait for a user to have N bipods with a specific role
wait_for_user_bipod_count() {
    local user_id="$1" min_count="$2" timeout="${3:-300}"
    local elapsed=0
    local count=0
    while [ "$elapsed" -lt "$timeout" ]; do
        count=$(coord_api GET "/api/users/${user_id}/bipod" | jq '[.[] | select(.role != "stale")] | length')
        if [ "$count" -ge "$min_count" ]; then
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    echo "  ✗ Timeout waiting for $user_id to have $min_count live bipods (has $count)"
    return 1
}

# Wait for user to reach one of multiple target statuses
wait_for_user_status_multi() {
    local user_id="$1" timeout="${2:-120}"
    shift 2
    local targets=("$@")
    local elapsed=0
    local status=""
    while [ "$elapsed" -lt "$timeout" ]; do
        status=$(coord_api GET "/api/users/${user_id}" | jq -r '.status // empty')
        for target in "${targets[@]}"; do
            if [ "$status" = "$target" ]; then
                return 0
            fi
        done
        if [ "$status" = "failed" ]; then
            echo "  ✗ User $user_id is FAILED:"
            coord_api GET "/api/users/${user_id}" | jq -r '.error // "unknown error"'
            return 1
        fi
        sleep 2
        elapsed=$((elapsed + 2))
    done
    echo "  ✗ Timeout waiting for $user_id to reach [${targets[*]}] (stuck at $status)"
    return 1
}
```

### `scripts/layer-4.4/infra.sh`

Same as Layer 4.3's `infra.sh` with `l43` → `l44` prefix everywhere:
- `COORD="l44-coordinator"`
- `FLEET_MACHINES=("l44-fleet-1" "l44-fleet-2" "l44-fleet-3")`
- Rest identical.

### `scripts/layer-4.4/deploy.sh`

Same as Layer 4.3's `deploy.sh` with `l43` → `l44` header. No structural changes.

### `scripts/layer-4.4/cloud-init/coordinator.yaml`

Identical to Layer 4.3.

### `scripts/layer-4.4/cloud-init/fleet.yaml`

Identical to Layer 4.3.

### `scripts/layer-4.4/test_suite.sh`

This is the core of Layer 4.4. The test proves automatic bipod reformation after failover.

```bash
#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/common.sh"
load_ips

echo "═══ Layer 4.4: Bipod Reformation & Dead Machine Re-integration — Test Suite ═══"

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

# No failover or reformation events initially
check "No failover events initially" '[ "$(coord_api GET /api/failovers | jq ". | length")" -eq 0 ]'
check "No reformation events initially" '[ "$(coord_api GET /api/reformations | jq ". | length")" -eq 0 ]'

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
    secondary=$(coord_api GET /api/users/$user/bipod | jq -r '.[] | select(.role == "secondary") | .machine_id')
    echo "    $user → primary: $primary, secondary: $secondary"
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

# All users should have exactly 2 bipods
for user in alice bob charlie; do
    check "$user has 2 bipods" '
        count=$(coord_api GET /api/users/'"$user"'/bipod | jq ". | length")
        [ "$count" -eq 2 ]
    '
done

phase_result

# ══════════════════════════════════════════
# Phase 2: Kill a Fleet Machine (trigger failover)
# ══════════════════════════════════════════
phase_start 2 "Kill a Fleet Machine (trigger failover)"

# Kill fleet-1
KILL_MACHINE_ID="fleet-1"
KILL_PUB_IP="$FLEET1_PUB_IP"

echo "  Target: $KILL_MACHINE_ID ($KILL_PUB_IP)"
echo "  Users on this machine (as primary or secondary):"
coord_api GET /api/users | jq -r '.[] | select(.bipod[].machine_id == "fleet-1") | "    \(.user_id) (primary: \(.primary_machine))"'

# Record user placement pre-kill
FAILOVER_USERS=$(coord_api GET /api/users | jq -r '.[] | select(.primary_machine == "fleet-1") | .user_id')
DEGRADED_USERS=$(coord_api GET /api/users | jq -r '.[] | select(.primary_machine != "fleet-1") | select(.bipod[].machine_id == "fleet-1") | .user_id')

echo "  Users needing failover (primary on fleet-1): $FAILOVER_USERS"
echo "  Users becoming degraded (secondary on fleet-1): $DEGRADED_USERS"

# Shutdown the machine
check "Shutdown fleet-1" 'hcloud server shutdown l44-fleet-1'

# Verify machine is actually down
sleep 5
check "fleet-1 unreachable via SSH" '! ssh_cmd "$KILL_PUB_IP" "true" 2>/dev/null'

phase_result

# ══════════════════════════════════════════
# Phase 3: Failure Detection & Failover (Layer 4.3 behavior)
# ══════════════════════════════════════════
phase_start 3 "Failure Detection & Failover"

echo "  Waiting for coordinator to detect fleet-1 as dead (up to 90s)..."
check "fleet-1 detected as dead" 'wait_for_machine_status "fleet-1" "dead" 90'

# Wait for failover to complete
sleep 15  # Give the failover goroutine time

# All users should be in running or running_degraded
for user in alice bob charlie; do
    check "$user survived failover (running or running_degraded)" '
        status=$(coord_api GET /api/users/'"$user"' | jq -r .status)
        [ "$status" = "running" ] || [ "$status" = "running_degraded" ]
    '
done

# No user should have fleet-1 as primary
check "No user has fleet-1 as primary" '
    count=$(coord_api GET /api/users | jq "[.[] | select(.primary_machine == \"fleet-1\")] | length")
    [ "$count" -eq 0 ]
'

# Failover events should exist
check "Failover events recorded" '[ "$(coord_api GET /api/failovers | jq ". | length")" -gt 0 ]'

phase_result

# ══════════════════════════════════════════
# Phase 4: Verify Degraded State (pre-reformation)
# ══════════════════════════════════════════
phase_start 4 "Verify Degraded State (pre-reformation)"

# At this point, users should have only 1 live bipod each
for user in alice bob charlie; do
    live_count=$(coord_api GET /api/users/$user/bipod | jq '[.[] | select(.role != "stale")] | length')
    check "$user has exactly 1 live bipod (got: $live_count)" '
        count=$(coord_api GET /api/users/'"$user"'/bipod | jq "[.[] | select(.role != \"stale\")] | length")
        [ "$count" -eq 1 ]
    '
done

# All users should have a stale bipod on fleet-1
for user in alice bob charlie; do
    check "$user has stale bipod on fleet-1" '
        coord_api GET /api/users/'"$user"'/bipod | jq -e ".[] | select(.machine_id == \"fleet-1\") | select(.role == \"stale\")"
    ' 2>/dev/null || true  # Some users might not have bipods on fleet-1
done

echo ""
echo "  Current state (post-failover, pre-reformation):"
for user in alice bob charlie; do
    status=$(coord_api GET /api/users/$user | jq -r .status)
    primary=$(coord_api GET /api/users/$user | jq -r .primary_machine)
    bipods=$(coord_api GET /api/users/$user/bipod | jq -r '.[] | "\(.machine_id):\(.role)"' | tr '\n' ' ')
    echo "    $user → status: $status, primary: $primary, bipods: $bipods"
done

phase_result

# ══════════════════════════════════════════
# Phase 5: Wait for Bipod Reformation
# ══════════════════════════════════════════
phase_start 5 "Wait for Bipod Reformation"

echo "  Waiting for reformation to complete (stabilization + reformer tick + sync)..."
echo "  Expected timeline: ~30s stabilization + ~30s reformer tick + ~15s sync = ~75s"
echo "  Timeout: 300s"

# Wait for all users to reach "running" with 2 live bipods
for user in alice bob charlie; do
    check "$user reformation complete (running)" '
        wait_for_user_status '"$user"' running 300
    '
done

# All users should have 2 live bipods now
for user in alice bob charlie; do
    check "$user has 2 live bipods after reformation" '
        count=$(coord_api GET /api/users/'"$user"'/bipod | jq "[.[] | select(.role != \"stale\")] | length")
        [ "$count" -eq 2 ]
    '
done

# The new secondary should NOT be fleet-1 (it's dead)
for user in alice bob charlie; do
    check "$user new secondary is not fleet-1" '
        secondary=$(coord_api GET /api/users/'"$user"'/bipod | jq -r ".[] | select(.role == \"secondary\") | .machine_id")
        [ "$secondary" != "fleet-1" ]
    '
done

echo ""
echo "  Post-reformation state:"
for user in alice bob charlie; do
    status=$(coord_api GET /api/users/$user | jq -r .status)
    primary=$(coord_api GET /api/users/$user | jq -r .primary_machine)
    bipods=$(coord_api GET /api/users/$user/bipod | jq -r '.[] | "\(.machine_id):\(.role)"' | tr '\n' ' ')
    echo "    $user → status: $status, primary: $primary, bipods: $bipods"
done

phase_result

# ══════════════════════════════════════════
# Phase 6: DRBD Sync & Data Integrity After Reformation
# ══════════════════════════════════════════
phase_start 6 "DRBD Sync & Data Integrity After Reformation"

# Verify DRBD is fully synced on primary
for user in alice bob charlie; do
    PRIMARY_ID=$(coord_api GET /api/users/$user | jq -r .primary_machine)
    PRIMARY_PUB=$(get_public_ip "$PRIMARY_ID")
    check "$user DRBD fully synced (UpToDate)" '
        machine_api "'"$PRIMARY_PUB"'" GET /images/'"$user"'/drbd/status | jq -e ".peer_disk_state == \"UpToDate\""
    '
done

# Verify data written BEFORE failover survived
for user in alice bob charlie; do
    PRIMARY_ID=$(coord_api GET /api/users/$user | jq -r .primary_machine)
    PRIMARY_PUB=$(get_public_ip "$PRIMARY_ID")
    check "$user pre-failover data survived" '
        result=$(docker_exec "'"$PRIMARY_PUB"'" '"$user"'-agent "cat /workspace/data/test.txt")
        [ "$result" = "'"$user"'-data-before" ]
    '
done

# Write new data after reformation
for user in alice bob charlie; do
    PRIMARY_ID=$(coord_api GET /api/users/$user | jq -r .primary_machine)
    PRIMARY_PUB=$(get_public_ip "$PRIMARY_ID")
    docker_exec "$PRIMARY_PUB" ${user}-agent "sh -c 'echo ${user}-data-after > /workspace/data/test2.txt'" 2>/dev/null || true
    check "$user can write new data after reformation" '
        result=$(docker_exec "'"$PRIMARY_PUB"'" '"$user"'-agent "cat /workspace/data/test2.txt")
        [ "$result" = "'"$user"'-data-after" ]
    '
done

# Config.json from initial provisioning should still be there
for user in alice bob charlie; do
    PRIMARY_ID=$(coord_api GET /api/users/$user | jq -r .primary_machine)
    PRIMARY_PUB=$(get_public_ip "$PRIMARY_ID")
    check "$user config.json survived" '
        docker_exec "'"$PRIMARY_PUB"'" '"$user"'-agent "cat /workspace/data/config.json" | jq -e .user
    '
done

phase_result

# ══════════════════════════════════════════
# Phase 7: Dead Machine Return & Cleanup
# ══════════════════════════════════════════
phase_start 7 "Dead Machine Return & Cleanup"

# Power on fleet-1
echo "  Powering on fleet-1..."
check "Power on fleet-1" 'hcloud server poweron l44-fleet-1'

# Wait for fleet-1 to come back
echo "  Waiting for fleet-1 to resume heartbeats (up to 120s)..."
check "fleet-1 back to active" 'wait_for_machine_status "fleet-1" "active" 120'

# Wait a bit for the reformer to clean up stale bipods
echo "  Waiting for stale bipod cleanup (up to 90s)..."
sleep 60  # Wait for reformer tick + cleanup

# Verify fleet-1 has no DRBD resources
check "fleet-1 has no DRBD resources" '
    ssh_cmd "$FLEET1_PUB_IP" "drbdadm status all 2>&1" | grep -qv "user-" || true
'

# Verify fleet-1 has no user images
check "fleet-1 has no user images" '
    result=$(machine_api "$FLEET1_PUB_IP" GET /status | jq ".users | length")
    [ "$result" -eq 0 ]
'

# Verify no stale bipods remain in coordinator state
for user in alice bob charlie; do
    check "$user has no stale bipods" '
        stale_count=$(coord_api GET /api/users/'"$user"'/bipod | jq "[.[] | select(.role == \"stale\")] | length")
        [ "$stale_count" -eq 0 ]
    '
done

phase_result

# ══════════════════════════════════════════
# Phase 8: Coordinator State Consistency
# ══════════════════════════════════════════
phase_start 8 "Coordinator State Consistency"

# All machines active
check "All machines active" '
    dead=$(coord_api GET /api/fleet | jq "[.machines[] | select(.status != \"active\")] | length")
    [ "$dead" -eq 0 ]
'

# All users running with 2 bipods
for user in alice bob charlie; do
    check "$user is running" '[ "$(coord_api GET /api/users/'"$user"' | jq -r .status)" = "running" ]'
    check "$user has 2 bipods (primary + secondary)" '
        roles=$(coord_api GET /api/users/'"$user"'/bipod | jq -r ".[].role" | sort | tr "\n" " ")
        [ "$roles" = "primary secondary " ]
    '
done

# state.json should be persisted
check "Coordinator state.json persisted" '
    ssh_cmd "$COORD_PUB_IP" "test -f /data/state.json" &&
    ssh_cmd "$COORD_PUB_IP" "cat /data/state.json | jq -e .machines"
'

phase_result

# ══════════════════════════════════════════
# Phase 9: Reformation Events
# ══════════════════════════════════════════
phase_start 9 "Reformation Events"

# Reformation events should exist
check "Reformation events recorded" '[ "$(coord_api GET /api/reformations | jq ". | length")" -gt 0 ]'

# Check event structure
check "Reformation events have correct structure" '
    coord_api GET /api/reformations | jq -e ".[0].user_id" &&
    coord_api GET /api/reformations | jq -e ".[0].new_secondary" &&
    coord_api GET /api/reformations | jq -e ".[0].method"
'

# All reformation events should be successful
check "All reformation events successful" '
    failed=$(coord_api GET /api/reformations | jq "[.[] | select(.success == false)] | length")
    [ "$failed" -eq 0 ]
'

# Log the reformation details
echo ""
echo "  Reformation events:"
coord_api GET /api/reformations | jq -r '.[] | "    \(.user_id): \(.new_secondary) via \(.method) (\(.duration_ms)ms)"'

phase_result

# ══════════════════════════════════════════
# Phase 10: Cleanup
# ══════════════════════════════════════════
phase_start 10 "Cleanup"

# Clean up all machines
for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Cleanup $ip" 'machine_api "'"$ip"'" POST /cleanup'
done

# Verify clean state
for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
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

**Carried forward from Layers 4.1, 4.2, and 4.3:**

1. **DRBD config hostnames must match `hostname` output** — the DRBD resource file's `on <hostname>` blocks must match the system hostname. Deploy scripts set hostname to node ID (e.g., `fleet-1`).

2. **DRBD needs promote-before-sync** — the secondary won't sync until one side is Primary. Always promote the intended primary BEFORE waiting for sync.

3. **DRBD `--force` is required for initial promotion** — both sides start as Secondary. The first `drbdadm primary --force` is needed because there's no data to agree on yet. This same `--force` flag also works for failover promotion when the peer is disconnected.

4. **Container device-mount pattern** — containers get the DRBD device via `--device`, not bind mounts. The init script inside the container mounts the Btrfs subvolume. The host does NOT keep Btrfs mounted after provisioning.

5. **Host must NOT have Btrfs mounted** when container starts — `FormatBtrfs` unmounts after formatting. If the host still has the Btrfs filesystem mounted, the container's `mount` inside will fail with "device already mounted".

6. **Protocol A (async) replication** — DRBD is configured with `protocol A` for performance. Writes are acknowledged locally before reaching the peer. During failover, the last few seconds of writes may be lost. This is a known, accepted trade-off.

7. **Sparse files for disk efficiency** — images are created with `truncate`, not `dd`. Actual disk usage is proportional to data written, not apparent size.

8. **`vfs` storage driver for inner Docker** — not `overlay2`. DinD (or bare metal with kernel DRBD) uses the `vfs` storage driver for the nested Docker daemon.

9. **Private IPs for inter-machine communication** — coordinator talks to machine agents via 10.0.0.x addresses. Public IPs are only for SSH and the test harness.

10. **Failover must be idempotent** — if `failoverUser` is called twice for the same user+machine, the second call must be a no-op.

11. **Never promote a secondary that's already Primary** — the machine agent handles this gracefully (returns success with `already_existed`).

12. **Failover must not block the health checker** — each `failoverMachine` runs in its own goroutine.

13. **Dead machine resurrection must not auto-integrate** — if a dead machine's heartbeats resume, set status back to `"active"` but do NOT touch bipods or users. Bipod cleanup happens through the reformer.

14. **Container start requires DRBD Primary** — never attempt to start a container on a machine where DRBD is secondary.

15. **`hcloud server shutdown` for realistic testing** — simulates a graceful halt.

**New constraints for Layer 4.4:**

16. **Reformation must wait for stabilization** — do not attempt reformation within 30 seconds of a user becoming degraded. Prevents thrashing on transient issues.

17. **Configure new secondary BEFORE reconfiguring primary** — the new secondary's DRBD should be up and listening before the primary's `drbdadm adjust` tries to connect. DRBD auto-retries connections, but having the secondary ready first avoids unnecessary retries and is the correct operational order.

18. **Cleanup stale bipods before reformation** — when the reformer encounters a user with stale bipods on active machines, clean those up first to prevent resource conflicts.

19. **Reformation is single-threaded per user** — only one reformation should act on a user at a time. The `reforming` status prevents the reformer's next tick from acting on the same user.

20. **DRBD reconfigure fallback must handle container lifecycle** — if `drbdadm adjust` fails and the `down/up` fallback is needed, the coordinator must stop the container before and restart it after. The machine agent's reconfigure endpoint only handles DRBD, not containers.

21. **DRBD minor on new secondary may differ from old secondary** — DRBD minors are per-machine and allocated independently. The config file must reflect the correct minor for each machine.

22. **DRBD port is reused** — the same DRBD port allocated during initial provisioning is reused for the reformed bipod. No new port allocation needed.

23. **Cleanup of returned machines is best-effort** — `DRBDDestroy` and `DeleteUser` calls to a returned machine may fail if resources are already gone. These errors should be logged but not block reformation.

---

## Directory Structure After Layer 4.4

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
│       └── main.go                                  # MODIFIED: start reformer
│
├── internal/
│   ├── shared/
│   │   └── types.go                                 # MODIFIED: add DRBD disconnect/reconfigure types, reformation event
│   ├── machineagent/
│   │   ├── server.go                                # MODIFIED: add disconnect/reconfigure routes
│   │   ├── state.go                                 # unchanged
│   │   ├── images.go                                # unchanged
│   │   ├── drbd.go                                  # MODIFIED: add DRBDDisconnect, DRBDReconfigure
│   │   ├── btrfs.go                                 # unchanged
│   │   ├── containers.go                            # unchanged
│   │   ├── cleanup.go                               # unchanged
│   │   ├── exec.go                                  # unchanged
│   │   └── heartbeat.go                             # unchanged
│   └── coordinator/
│       ├── server.go                                # MODIFIED: add /api/reformations
│       ├── store.go                                 # MODIFIED: StatusChangedAt on User, reformation events, new methods
│       ├── fleet.go                                 # unchanged
│       ├── provisioner.go                           # unchanged
│       ├── machineapi.go                            # MODIFIED: add DRBDDisconnect, DRBDReconfigure client
│       ├── healthcheck.go                           # unchanged
│       └── reformer.go                              # NEW: reformation logic
│
├── container/
│   ├── Dockerfile                                   # unchanged
│   └── container-init.sh                            # unchanged
│
└── scripts/
    ├── layer-4.1/                                   # unchanged
    ├── layer-4.2/                                   # unchanged
    ├── layer-4.3/                                   # unchanged
    └── layer-4.4/                                   # NEW
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

1. Modify `internal/shared/types.go` — add `DRBDDisconnectResponse`, `DRBDReconfigureRequest`, `DRBDReconfigureResponse`, `ReformationEventResponse`.
2. Modify `internal/machineagent/drbd.go` — add `DRBDDisconnect` and `DRBDReconfigure` methods.
3. Modify `internal/machineagent/server.go` — add routes and handlers for disconnect and reconfigure.
4. Modify `internal/coordinator/machineapi.go` — add `DRBDDisconnect` and `DRBDReconfigure` client methods.
5. Modify `internal/coordinator/store.go` — add `StatusChangedAt` to User, add `ReformationEvent` struct, add `reformationEvents` to Store, add new methods (`GetDegradedUsers`, `GetStaleBipodsOnActiveMachines`, `RemoveBipod`, `SelectOneSecondary`, `RecordReformationEvent`, `GetReformationEvents`). Update `SetUserStatus` to set `StatusChangedAt`. Update `persist()` and `persistState`.
6. Modify `internal/coordinator/server.go` — add `GET /api/reformations` route and handler.
7. Modify `cmd/coordinator/main.go` — start reformer goroutine after health checker.
8. Create `internal/coordinator/reformer.go` — reformation logic.
9. Create all test scripts in `scripts/layer-4.4/`.

Make all scripts executable (`chmod +x`).

Report back when code is written. Do NOT run tests yet.

### Phase 2: Run Tests

When told "yes" / "ready":

```bash
cd scfuture/scripts/layer-4.4
./run.sh
```

This builds, creates infra, deploys, and runs the test suite.

**If tests fail:** Do NOT tear down infrastructure. Fix the issue (in Go code or test scripts), rebuild (`cd scfuture && make build`), redeploy if needed (`cd scripts/layer-4.4 && ./deploy.sh`), and re-run only the test suite (`./test_suite.sh`). Iterate until all checks pass.

**When all tests pass:** Tear down infrastructure (`./infra.sh down`).

### Phase 3: Update SESSION.md

Append a new section to the parent directory's `SESSION.md` documenting:
- What Layer 4.4 built
- Test results (checks passed/failed)
- Issues encountered and fixes (continue numbering from Layer 4.3)
- Which DRBD reconfiguration method worked (`adjust` vs `down/up`) — this is the key finding
- Any drift from this prompt
- Updated PoC progression plan

### Phase 4: Final Report

Provide a summary including:
- Total checks: passed / total
- Number of issues encountered
- Key technical finding: did `drbdadm adjust` work for live peer replacement, or was the `down/up` fallback needed?
- Reformation timing: how long from user becoming degraded to fully reformed?
- What Layer 4.5 will need to address

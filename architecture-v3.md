# Distributed Agent Platform — Architecture v3

## 1. Vision

An always-on AI agent platform where each user gets a persistent, replicated environment running 24/7. Users interact with their agent via messaging (Telegram, WhatsApp) and optional web dashboards. The platform provides orchestration, fault tolerance, continuous replication, cost tracking, and credential management. Each agent's entire state — memory, configs, databases, generated apps — lives on a replicated filesystem that survives machine failures and is continuously backed up. Infrastructure costs are ~€0.30-0.50/user/mo, enabling aggressive pricing and 95%+ gross margins.

### 1.1 Key Differentiators

Compared to existing agent hosting (OpenClaw ecosystem):

- **Zero memory loss** — agent state lives on replicated Btrfs; survives machine failures, restarts, and updates
- **True high availability** — bipod replication with automatic failover; agent back online in seconds after machine death
- **Cost control** — all LLM calls routed through credential proxy with spending limits; solves the "API Wallet Assassin" problem
- **Zero-setup onboarding** — user signs up, connects Telegram, gets a running agent; no VPS, Docker, SSH, or config files
- **Density** — ~200 agents per machine vs. 1 VPS per customer; 10-20x cost advantage

---

## 2. The Bipod: Core Primitive

The entire system is built on one concept: the **bipod**.

Every user's data exists as exactly 2 copies across 2 different fleet machines at all times. One copy is the **primary** (read-write, mounted, serving the agent). One copy is the **secondary** (read-only mirror, receiving writes in real-time via DRBD). Backblaze B2 provides the third tier of protection via continuous incremental backups.

```
    fleet-1              fleet-5
   (primary)  ── DRBD ── (secondary)
                  │
              Backblaze B2
            (continuous backup)
```

The bipod provides three things:

1. **Resilience** — either machine can die, one copy remains, zero data loss (minus Protocol A async gap of ~0-3 seconds)
2. **Availability** — if the primary dies, the secondary promotes and the agent is back in seconds
3. **Performance** — the active copy is always on local NVMe, full bare-metal speed

A user is assigned to a bipod at provisioning time and stays on that bipod permanently. The agent runs continuously. Changes push from primary to secondary in real-time.

---

## 3. Block Device Stack

### 3.1 The Image File

Each user's filesystem is a single sparse file formatted with Btrfs:

```bash
truncate -s 40G /data/images/alice.img   # 40GB apparent, ~0 actual
mkfs.btrfs -f /data/images/alice.img
```

The sparse file grows dynamically as the user's agent writes data. A typical agent consuming 2GB of actual data has a file that reports as 40GB but only occupies 2GB on disk. The 40GB limit is enforced naturally by the image size — Btrfs inside cannot exceed the block device size.

Why a file and not a raw partition or Btrfs subvolume on the host? Because a file is:
- **Portable** — can be copied, migrated, and backed up as a single unit
- **Self-contained** — one file = one user's entire state
- **Compatible with DRBD** — DRBD replicates the loop device backed by the file

### 3.2 Block Device Stack on the Primary Machine

```
Physical NVMe (/dev/nvme0n1)
  └─ XFS host filesystem
       └─ /data/images/alice.img (sparse file)
            └─ Loop device (/dev/loop0)
                 └─ DRBD primary (/dev/drbd100)
                      └─ Btrfs filesystem mounted at /mnt/users/alice
                           ├─ workspace/          (live subvolume)
                           └─ snapshots/
                               ├─ layer-000/       (read-only snapshot)
                               ├─ layer-001/
                               └─ tweak-entry/
```

### 3.3 Block Device Stack on the Secondary Machine

```
Physical NVMe
  └─ XFS host filesystem
       └─ /data/images/alice.img (sparse file, mirror of primary)
            └─ Loop device (/dev/loop0)
                 └─ DRBD secondary (/dev/drbd100)
                      └─ NOT MOUNTED (receives writes from primary)
```

---

## 4. DRBD Configuration

### 4.1 Protocol

**Protocol A (asynchronous)**. Writes are confirmed as soon as the primary has them in the TCP send buffer. This gives near-local-NVMe write performance. The risk of losing the last few seconds of writes in a catastrophic failure is acceptable for agent environments where the LLM maintains its own resumable context. During live migrations (Section 10), a deterministic flush ensures zero data loss at switchover.

### 4.2 Resource Configuration (Per User)

Each user gets a DRBD resource spanning their 2 bipod machines. The coordinator assigns a globally unique port per user.

```
resource user-alice {
    net {
        protocol A;
        max-buffers 8000;
        max-epoch-size 8000;
        sndbuf-size 0;        # auto-tune
        rcvbuf-size 0;
    }
    disk {
        on-io-error detach;    # don't crash on disk errors
    }
    on fleet-1 {
        device /dev/drbd0 minor 0;
        disk /data/images/alice.img;
        address 10.10.0.11:7942;
        meta-disk internal;
    }
    on fleet-5 {
        device /dev/drbd0 minor 12;
        disk /data/images/alice.img;
        address 10.10.0.15:7942;
        meta-disk internal;
    }
}
```

### 4.3 DRBD Lifecycle Per User

| User state | DRBD state |
|------------|------------|
| Provisioning | Create resources on 2 machines, initial sync (instant for template copies) |
| Running | Primary connected, secondary receiving writes in real-time |
| Live migration | 3rd machine joins temporarily as secondary, syncs, then one original member dropped. Back to 2. |
| Suspended | DRBD disconnected, images retained for potential reactivation |
| Evicted | DRBD destroyed, image files deleted on all fleet machines |

### 4.4 Port and Minor Number Allocation

The coordinator assigns each user a unique DRBD port (globally unique across the fleet). Both bipod machines use the same port for that user's resource.

Minor device numbers are local to each machine. The machine agent allocates them from its assigned range:

```
fleet-1: minors 0-499
fleet-2: minors 500-999
fleet-3: minors 1000-1499
fleet-4: minors 1500-1999
fleet-5: minors 2000-2499
```

In the prototype (shared kernel), this partitioning prevents collisions. In production (separate machines with separate kernels), minor numbers can start from 0 on every machine independently.

---

## 5. Btrfs Snapshots

### 5.1 Snapshot Architecture

Btrfs lives inside the image file. Snapshots are instant (COW, O(1)) and consume only the space of blocks that changed since the snapshot.

```
/mnt/users/alice/                     (Btrfs root, mounted from DRBD device)
├── workspace/                        (live subvolume — agent's working state)
│   ├── memory/                       (agent memory files, MEMORY.md, etc.)
│   ├── apps/                         (user's personal web apps)
│   ├── data/                         (agent databases, configs, state)
│   └── ...
└── snapshots/
    ├── layer-000/                    (read-only — initial setup)
    ├── layer-001/                    (read-only — after first config)
    ├── pre-update-20260224/          (read-only — before agent software update)
    └── tweak-entry/                  (read-only — tweak mode entry point)
```

### 5.2 Layer Snapshots

Configuration milestones and agent updates create permanent snapshots:

```bash
btrfs subvolume snapshot -r /mnt/users/alice/workspace \
                            /mnt/users/alice/snapshots/layer-003
```

### 5.3 Tweak Mode

```bash
# Enter tweak mode — snapshot current state
btrfs subvolume snapshot -r /mnt/users/alice/workspace \
                            /mnt/users/alice/snapshots/tweak-entry

# Agent experiments freely... adds logs, patches, explores...
# Agent writes a report of findings to /workspace/.tweak-report.md

# Exit tweak mode — revert everything
btrfs subvolume delete /mnt/users/alice/workspace
btrfs subvolume snapshot /mnt/users/alice/snapshots/tweak-entry \
                         /mnt/users/alice/workspace

# Clean up
btrfs subvolume delete /mnt/users/alice/snapshots/tweak-entry
```

Instant entry. Instant revert. The report survives (it's read before revert). All experimental changes are gone.

### 5.4 Rollback to Any Layer

```bash
btrfs subvolume delete /mnt/users/alice/workspace
btrfs subvolume snapshot /mnt/users/alice/snapshots/layer-001 \
                         /mnt/users/alice/workspace
```

### 5.5 Snapshot Storage Cost

Snapshots share unchanged data via COW. Typical overhead:
- Layer snapshot after a config change: 5-30MB delta
- Tweak mode session: 5-20MB delta
- 10 layer snapshots on a 2GB workspace: ~2.2-2.5GB total (not 20GB)

### 5.6 DRBD Replicates Snapshots Automatically

Snapshot operations are just block writes from DRBD's perspective. When you create a snapshot on the primary, the metadata block writes propagate through DRBD to the secondary. Both bipod copies have identical snapshot trees at all times. No separate snapshot replication process is needed.

---

## 6. User Lifecycle

### 6.1 New User — Provisioning

```
1. User signs up, connects Telegram account
2. Coordinator picks 2 least-loaded machines for the bipod
3. Each machine copies the base template image → alice.img
   (template is pre-distributed to all machines)
4. DRBD resources created on both, initial sync
   (images are identical copies — sync detects no differences, completes instantly)
5. Primary designated, DRBD promoted
6. Loop-mounts image, mounts Btrfs
7. Agent containers started with user's config
8. Agent is live — bipod is formed
9. User receives Telegram confirmation: "Your agent is ready"

USER STATE: running
```

### 6.2 Steady State — Always On

```
Agent containers run continuously on the primary machine.
DRBD replicates all writes to the secondary in real-time.
Machine agent monitors container health (see Section 9.2).
Backblaze backups run on schedule (see Section 14).

The user interacts with their agent via Telegram/WhatsApp.
The agent can serve personal web apps from the workspace.
All LLM API calls route through the credential proxy for cost tracking.

USER STATE: running (indefinitely)
```

### 6.3 User Suspends — Subscription Cancelled or Paused

```
1. Coordinator receives suspension trigger (subscription event)
2. Tells machine agent: stop all containers
3. Machine agent stops containers gracefully
4. Machine agent takes final Btrfs snapshot (auto-named with timestamp)
5. DRBD replicates final snapshot writes to secondary
6. If last_active > last_backup:
   → Machine agent uploads final incremental to Backblaze B2
7. DRBD stays connected (images warm for potential reactivation)
8. After 7 days with no reactivation:
   → DRBD disconnected, images retained on fleet for 30 more days
9. After 30 days:
   → Coordinator verifies Backblaze backup is complete
   → Images deleted from fleet machines
   → Bipod dissolved

USER STATE: suspended → evicted (after 30 days)
```

### 6.4 Reactivation — User Resumes Subscription

```
Case A — Images still on fleet (within 30-day retention):
  1. DRBD reconnected if disconnected (bitmap resync, usually instant)
  2. Primary promoted, Btrfs mounted
  3. Agent containers started
  4. User is live — instant reactivation

Case B — Images evicted, Backblaze only (cold start):
  1. Coordinator picks 2 least-loaded machines
  2. Machine-1 downloads latest snapshot chain from Backblaze:
     b2 download → zstd -d → btrfs receive (rebuilds the subvolume)
  3. Meanwhile, user sees "Restoring your agent..." (seconds to minutes)
  4. Once downloaded: image file exists, DRBD configured, bipod formed
  5. Agent containers started
  6. User is live

USER STATE: running
```

---

## 7. Failure Recovery

### 7.1 Primary Machine Dies — Agent Goes Down

```
fleet-1 stops heartbeating → marked offline after 30 seconds

For each user whose primary was fleet-1:
├─ Their bipod was: fleet-1 (primary), fleet-5 (secondary)
├─ fleet-1 is gone → fleet-5 has a copy
│
├─ Promote fleet-5's DRBD to primary
├─ Mount Btrfs on fleet-5
├─ Start agent containers on fleet-5
├─ Agent is back online (seconds)
│
├─ Asynchronously: coordinator picks a new machine (fleet-3)
│   → Create empty image on fleet-3
│   → Configure as DRBD secondary
│   → Initial sync from fleet-5 (new primary)
│   → Once synced: bipod is fully reformed (fleet-5 + fleet-3)
│
└─ User's Telegram messages queue during the brief outage.
   Agent catches up automatically on restart.

Data loss: ~0-3 seconds of writes (Protocol A async)
Process loss: in-flight agent work lost, but agent state is on disk, resumable
```

### 7.2 Secondary Machine Dies — No Immediate Impact

```
No impact on the running agent. Agent continues on primary.
Bipod is down to 1 member — single point of failure until reformed.

Coordinator immediately provisions a new secondary:
  → Pick a new machine
  → Create image, configure DRBD secondary
  → Initial sync from primary
  → Bipod reformed

Priority: HIGH. Single-copy state is risky.
Target: new secondary synced within 10 minutes.
```

### 7.3 Both Machines Die Simultaneously

```
Extremely rare (different physical machines, different racks ideally).
If it happens:
  → Cold restore from Backblaze B2
  → Maximum data loss: time since last backup (worst case ~30 minutes)
  → Coordinator provisions new bipod on 2 healthy machines
  → Downloads and restores from B2
  → Agent is back, with some data loss
```

### 7.4 Coordinator Dies

Active agents continue uninterrupted (containers running, DRBD replicating). New user provisioning pauses. Failure recovery and rebalancing pause. Fix: restart coordinator, it reads state from database and resumes.

### 7.5 Network Partition

DRBD Protocol A: primary continues writes locally. When network heals, DRBD resyncs automatically via bitmap. If split-brain occurs (shouldn't — only primary writes), the coordinator knows which machine has the running agent — that side wins.

---

## 8. Coordinator

### 8.1 Data Model

```sql
CREATE TABLE machines (
    machine_id      TEXT PRIMARY KEY,
    address         TEXT NOT NULL,              -- IP:port of machine agent
    status          TEXT NOT NULL DEFAULT 'active',
                    -- active | draining | offline
    disk_total_mb   INTEGER NOT NULL,
    disk_used_mb    INTEGER NOT NULL DEFAULT 0,
    ram_total_mb    INTEGER NOT NULL,
    ram_used_mb     INTEGER NOT NULL DEFAULT 0,
    cpu_load        REAL DEFAULT 0,
    active_agents   INTEGER DEFAULT 0,
    max_agents      INTEGER DEFAULT 200,
    drbd_port_start INTEGER NOT NULL,
    drbd_port_end   INTEGER NOT NULL,
    last_heartbeat  TIMESTAMP
);

CREATE TABLE users (
    user_id         TEXT PRIMARY KEY,
    status          TEXT NOT NULL DEFAULT 'provisioning',
                    -- provisioning: bipod being set up, agent not yet live
                    -- running: agent live, containers up, always on
                    -- suspended: subscription cancelled/paused, containers stopped,
                    --            images retained on fleet
                    -- evicted: no fleet presence, Backblaze only (cold)
    primary_machine TEXT REFERENCES machines(machine_id),
    telegram_id     TEXT UNIQUE,               -- linked Telegram account
    last_backup     TIMESTAMP,                 -- last successful Backblaze upload
    image_size_mb   INTEGER DEFAULT 0,
    image_quota_mb  INTEGER DEFAULT 40960,     -- 40GB
    tier            TEXT DEFAULT 'standard',
    drbd_port       INTEGER UNIQUE,            -- globally unique DRBD port
    monthly_token_budget_usd REAL DEFAULT 50,  -- spending limit
    tokens_used_this_month   INTEGER DEFAULT 0,
    spending_paused BOOLEAN DEFAULT FALSE,
    created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    suspended_at    TIMESTAMP                  -- when subscription was cancelled
);

CREATE TABLE bipods (
    user_id         TEXT NOT NULL REFERENCES users(user_id),
    machine_id      TEXT NOT NULL REFERENCES machines(machine_id),
    role            TEXT NOT NULL,
                    -- primary | secondary | syncing
    drbd_minor      INTEGER,                   -- local to this machine
    size_mb         INTEGER,
    last_synced     TIMESTAMP,
    created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, machine_id)
);

CREATE TABLE backups (
    user_id         TEXT NOT NULL REFERENCES users(user_id),
    snapshot_name   TEXT NOT NULL,
    backblaze_key   TEXT NOT NULL,
    parent_snapshot TEXT,
    size_bytes      INTEGER,
    uploaded_at     TIMESTAMP,
    PRIMARY KEY (user_id, snapshot_name)
);

CREATE TABLE operations (
    operation_id    TEXT PRIMARY KEY,
    type            TEXT NOT NULL,
                    -- live_migration | replica_creation | replica_removal
                    -- eviction | backup_upload | backup_restore
    user_id         TEXT NOT NULL REFERENCES users(user_id),
    source_machine  TEXT REFERENCES machines(machine_id),
    dest_machine    TEXT REFERENCES machines(machine_id),
    status          TEXT NOT NULL DEFAULT 'pending',
                    -- pending | in_progress | complete | cancelled | failed
    progress_pct    INTEGER DEFAULT 0,
    started_at      TIMESTAMP,
    completed_at    TIMESTAMP
);

CREATE TABLE events (
    event_id        INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp       TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    event_type      TEXT NOT NULL,
    machine_id      TEXT,
    user_id         TEXT,
    details         TEXT    -- JSON
);
```

### 8.2 Background Processes

| Process | Cadence | Function |
|---------|---------|----------|
| Heartbeat monitor | Continuous | Listens for machine heartbeats. Marks machines offline after 30s of silence. Triggers failure recovery (Section 7). |
| Bipod health reconciler | Every 30s | Ensures every running user has exactly 2 healthy bipod members. Creates new secondary if below 2 (e.g., after machine failure). Priority: HIGH for single-copy users. |
| Rebalancer | Every 60s | Compares agent density and disk usage across machines. Initiates live migrations (Section 10) to balance load. One migration at a time per machine. |
| Backblaze sync | Every 5min | Scans for users where data has changed since last backup. Triggers incremental backup to B2. See Section 14 for details. |
| Scale evaluator | Every 30s | Scale up if average agent density >70% of max. Scale down if <30% for 10+ minutes and above minimum fleet size. |
| Subscription lifecycle | On event | Handles subscription changes: new signup → provision, cancellation → suspend, reactivation → resume, payment failure → grace period → suspend. |

### 8.3 Coordinator API

```
# ── User management (called by external API gateway / billing system) ──

POST   /api/users                    → Create new user account
                                       Allocates DRBD port, creates DB entry
                                       Triggers bipod provisioning

POST   /api/users/{id}/provision     → Provision bipod and start agent
                                       Picks 2 machines, creates images, configures DRBD,
                                       starts agent containers
                                       Returns: { primary_machine, status }

POST   /api/users/{id}/suspend       → Suspend agent: stop containers, keep images
                                       Triggered by subscription cancellation
                                       Final snapshot + Backblaze backup

POST   /api/users/{id}/reactivate    → Reactivate suspended user
                                       Remounts, restarts containers (warm)
                                       or restores from B2 (cold)

DELETE /api/users/{id}               → Full deletion: evict, remove all data

# ── Machine agent → Coordinator (called by machine agents) ──

POST   /api/users/{id}/health        → Machine agent reports agent container health
                                       { status: "healthy" | "crashed" | "unresponsive" }
                                       Coordinator triggers restart or failover as needed

POST   /api/users/{id}/sync-confirm  → Ask primary to flush DRBD and confirm secondary synced
                                       Used during live migration (Section 10)

POST   /api/users/{id}/backup-complete → Machine agent confirms Backblaze upload finished
                                         Body: { snapshot_name, backblaze_key, size_bytes }
                                         Coordinator updates backups table

# ── Fleet management ──

GET    /api/fleet                     → All machines with stats
POST   /api/fleet/machines            → Add a new machine (scale up)
DELETE /api/fleet/machines/{id}       → Drain and remove (scale down)

# ── User data operations ──

GET    /api/users/{id}/bipod          → Bipod membership for a user
GET    /api/users/{id}/snapshots      → List of Btrfs snapshots
POST   /api/users/{id}/snapshot       → Create named snapshot
POST   /api/users/{id}/rollback       → Rollback to named snapshot
POST   /api/users/{id}/tweak/enter    → Enter tweak mode (snapshot + flag)
POST   /api/users/{id}/tweak/exit     → Exit tweak mode (revert)

# ── Operations ──

GET    /api/operations                → Active operations (migrations, syncs, backups)
DELETE /api/operations/{id}           → Cancel an operation

# ── Cost tracking ──

GET    /api/users/{id}/usage          → Token usage, spending, remaining budget
POST   /api/users/{id}/spending-limit → Update monthly spending limit
POST   /api/users/{id}/spending-pause → Pause/unpause LLM calls

# ── Dashboard ──

GET    /api/dashboard                 → Web UI
GET    /api/events                    → Event log stream (SSE or WebSocket)
```

---

## 9. Machine Agent

### 9.1 API

```
GET    /status                        → Machine health, disk/RAM/CPU, agent list

# ── Image lifecycle ──

POST   /images/{user_id}/create       → Create image (from template, B2, or empty for DRBD)
DELETE /images/{user_id}              → Delete image, DRBD, everything for this user

# ── DRBD management ──

POST   /images/{user_id}/drbd/create  → Configure DRBD resource with peer
POST   /images/{user_id}/drbd/promote → Promote to primary
POST   /images/{user_id}/drbd/demote  → Demote to secondary
POST   /images/{user_id}/drbd/connect → Connect to peer
POST   /images/{user_id}/drbd/disconnect → Disconnect from peer
DELETE /images/{user_id}/drbd         → Destroy DRBD resource
GET    /images/{user_id}/drbd/status  → Sync progress, role, connection state
POST   /images/{user_id}/drbd/flush-confirm → Flush DRBD send buffers, wait for
                                              secondary to confirm receipt

# ── Mount management ──

POST   /images/{user_id}/mount        → Mount Btrfs on local DRBD device
POST   /images/{user_id}/unmount      → Unmount Btrfs

# ── Snapshot management ──

POST   /images/{user_id}/snapshot     → Create Btrfs snapshot
DELETE /images/{user_id}/snapshot/{name} → Delete snapshot
POST   /images/{user_id}/rollback     → Rollback workspace to snapshot
GET    /images/{user_id}/snapshots    → List snapshots

# ── Backblaze backup (must run on primary — Btrfs must be mounted) ──

POST   /images/{user_id}/backup       → Take temporary snapshot if needed, btrfs send
                                        incremental from last backed-up snapshot, compress
                                        with zstd, upload to B2. On completion, calls
                                        coordinator: POST /api/users/{id}/backup-complete
POST   /images/{user_id}/restore      → Download snapshot chain from B2, decompress,
                                        btrfs receive in order, create workspace from latest

# ── Container management ──

POST   /containers/{user_id}/start    → Start agent containers
POST   /containers/{user_id}/stop     → Stop and remove containers
POST   /containers/{user_id}/pause    → docker pause (for live migration)
POST   /containers/{user_id}/unpause  → docker unpause
POST   /containers/{user_id}/restart  → Restart crashed container
GET    /containers/{user_id}/status   → Container list, health, resource usage
```

### 9.2 Agent Health Monitoring

The machine agent monitors each running agent for health. This runs as a background loop, once per minute per agent.

**Health signals**:

- Container running state (is the process alive?)
- Container responsiveness (can it respond to a health check endpoint?)
- Resource usage (memory, CPU — is it in a runaway loop?)
- Crash loop detection (restarted more than 3 times in 5 minutes?)

**Health monitoring flow:**

```
Machine agent health monitor (per running agent, every 60 seconds):

  Check container health
  │
  ├─ Container healthy?
  │   → Report to coordinator: POST /api/users/{id}/health { status: "healthy" }
  │   → Continue monitoring
  │
  ├─ Container crashed/stopped?
  │   → Restart container automatically
  │   → Report: POST /api/users/{id}/health { status: "restarted" }
  │   → If crash loops (>3 restarts in 5 min):
  │     → Report: POST /api/users/{id}/health { status: "crash_loop" }
  │     → Coordinator decides: notify user, pause agent, or investigate
  │
  └─ Container unresponsive (health check timeout)?
      → Kill and restart
      → Report: POST /api/users/{id}/health { status: "restarted" }
```

### 9.3 Heartbeat

Every 10 seconds, sends to coordinator:

```json
{
    "machine_id": "fleet-1",
    "disk_total_mb": 1000000,
    "disk_used_mb": 450000,
    "ram_total_mb": 65536,
    "ram_used_mb": 24000,
    "cpu_load": 2.3,
    "running_agents": ["alice", "bob", "charlie", "dave"],
    "drbd_resources": {
        "alice": { "role": "primary", "state": "UpToDate", "peer_state": "UpToDate" },
        "bob": { "role": "primary", "state": "UpToDate", "peer_state": "UpToDate" },
        "charlie": { "role": "secondary", "state": "UpToDate", "peer_state": "UpToDate" },
        "dave": { "role": "primary", "state": "UpToDate", "peer_state": "UpToDate" }
    }
}
```

---

## 10. Live Migration Protocol

Live migration moves a running agent from one machine to another with minimal downtime. This is used for rebalancing, planned maintenance, and machine draining. It replaces the v2 "square formation" with a simpler DRBD-only approach (no NBD, no dm-cache).

### 10.1 When It's Used

- **Rebalancing**: moving agents from overloaded to underloaded machines
- **Planned maintenance**: draining a machine before taking it offline
- **Bipod reshaping**: moving one side of a bipod to a better-placed machine

### 10.2 The Process

```
Starting state: bipod on fleet-1 (primary) + fleet-5 (secondary)
Target: move primary to fleet-3

Phase 1 — Add third copy (temporary tripod):
  1. Create empty image on fleet-3
  2. Configure DRBD: fleet-3 joins as additional secondary
  3. DRBD initial sync from fleet-1 to fleet-3
  4. Wait for sync to complete (fleet-3 now has full copy)

Phase 2 — Switchover (sub-second pause):
  5. docker pause all agent containers on fleet-1
  6. fsfreeze --freeze /mnt/users/alice
     (flushes pending Btrfs writes to DRBD device)
  7. Coordinator calls fleet-1: POST /images/alice/drbd/flush-confirm
     Fleet-1 flushes DRBD send buffers, waits until fleet-3
     confirms receipt. Returns 200.
     (milliseconds — data already on fleet-1, just draining async buffers)
  8. Unmount Btrfs on fleet-1
  9. Demote fleet-1 DRBD to secondary
  10. Promote fleet-3 DRBD to primary
  11. Mount Btrfs on fleet-3
  12. docker unpause containers on fleet-3
      (or start fresh containers if cross-machine)

Phase 3 — Cleanup:
  13. Remove fleet-1 from the bipod (DRBD disconnected, image deleted)
  14. Bipod is now: fleet-3 (primary) + fleet-5 (secondary)
```

### 10.3 Timing

| Step | Duration |
|------|----------|
| docker pause | <10ms |
| fsfreeze | <10ms |
| DRBD flush-confirm | 1-50ms (data already on primary, just draining buffers) |
| Unmount + demote + promote + mount | <100ms |
| Container start on new machine | 1-5 seconds (new containers, not unpause) |
| **Total agent downtime** | **~2-6 seconds** |

The agent's Telegram messages queue during this window. The agent catches up on restart.

### 10.4 Failure During Migration

| Failure point | Recovery |
|---------------|----------|
| Phase 1 (sync in progress) | Cancel sync, delete partial image on fleet-3. No impact on running agent. |
| Phase 2 steps 5-7 (before promotion) | Unpause containers on fleet-1, continue running there. Retry later. |
| Phase 2 steps 8-12 (after flush-confirm) | Data is safe on fleet-3. Complete promotion. If fleet-3 fails to start, fall back to fleet-5 (secondary has full copy). |

The key property: the agent's data is never at risk. Before flush-confirm, it's safe on fleet-1. After flush-confirm, it's safe on fleet-3 and fleet-5.

---

## 11. Rebalancing

### 11.1 Algorithm

```
Every 60 seconds:
  machines = all active machines
  avg_agent_density = average (running_agents / max_agents) across fleet
  avg_disk_pct = average disk usage across fleet

  overloaded = machines where density > avg + 20% OR disk_pct > avg + 15%
  underloaded = machines where density < avg - 20% OR disk_pct < avg - 15%

  if no overloaded machines: done

  for each overloaded machine (worst first):
    agents = agents on this machine, sorted by image size (smallest first)
    for each agent:
      if any operation in progress for this user: skip
      dest = underloaded machine that doesn't already have this image
      if no dest: continue
      start operation: type=live_migration, move primary to dest
      break  (one migration at a time per machine)
```

### 11.2 Rebalance Migration

A rebalance migration is a live migration (Section 10):

1. Add dest as temporary third DRBD node, sync
2. Once synced: switchover to dest (sub-second pause)
3. Remove source from bipod, delete image
4. Bipod has shifted — one endpoint moved

Since agents are always running, migrations always use the live migration protocol. There's no "wait for the user to disconnect" option.

---

## 12. Eviction

### 12.1 Eviction Context

In the always-on model, eviction only occurs for suspended users (subscription cancelled/paused). Active (running) agents are never evicted.

### 12.2 Eviction Tiers

| Tier | Condition | Action |
|------|-----------|--------|
| Protected | Running agent, or suspended < 7 days | Never evict |
| Warm retention | Suspended 7-30 days | DRBD disconnected, images retained on fleet |
| Full eviction | Suspended > 30 days, Backblaze backup confirmed | Remove all fleet copies |
| Space pressure | Fleet under disk pressure | Lower suspension thresholds for eviction |

### 12.3 Safety Rule

**Never delete the last fleet copy until Backblaze backup is verified.** If no backup exists, upload first, then evict.

---

## 13. Fleet Scaling

### 13.1 Scale Up

```
Trigger: avg(running_agents / max_agents) across fleet > 70%
   OR:   any machine disk > 85% and rebalancing can't resolve

Action: Provision new machine
  Prototype: docker run a new machine-node container
  Production: Hetzner API → new AX52

New machine registers via heartbeat, becomes available for placement.
Coordinator distributes the base template image to it.
```

### 13.2 Scale Down

```
Trigger: avg agent density < 30% for 10+ minutes
   AND:  fleet_size > MINIMUM_FLEET_SIZE (3)

Action: Drain least-loaded machine
  1. Mark as 'draining' — no new agents assigned
  2. For each agent on the machine:
     - Live migrate to another machine (Section 10)
  3. For suspended images:
     - If bipod has copy on another machine: just remove this one
     - If this is the only copy: migrate to another machine first
  4. Once empty: tear down
```

### 13.3 Minimum Fleet Size

Never drop below 3 machines. This ensures:
- Enough machines for 2x replication with placement flexibility
- Capacity for live migration (needs a free target)
- Headroom for sudden signups
- Baseline grows with user base growth

---

## 14. Backblaze Integration

### 14.1 Bucket Structure

```
b2://platform-backups/users/{user_id}/
  ├── layer-000.btrfs.zst             Full send of initial snapshot (base)
  ├── layer-001.btrfs.zst             Incremental from layer-000
  ├── auto-backup-20260224.btrfs.zst  Incremental from layer-001
  └── manifest.json                    Snapshot chain metadata + ordering
```

### 14.2 How Backups Work

Backups are always driven by the **coordinator** and executed by the **primary machine agent**. The primary is the only machine with the Btrfs filesystem mounted and access to the snapshot tree.

**Coordinator backup scan** (every 5 minutes):

```
For each running user:
  │
  ├─ Nothing changed since last backup?
  │   → Skip
  │
  ├─ (now - last_backup) > 30 minutes?
  │   → Time for a periodic backup
  │   → Tell primary: POST /images/{user_id}/backup
  │     Body: { "last_backed_up_snapshot": "layer-001" }
  │
  └─ About to evict last fleet copy?
      → MANDATORY backup before eviction
      → Wait for confirmation before proceeding
```

**Machine agent backup execution** (on the primary):

```
POST /images/{user_id}/backup received
│
├─ Take temporary read-only snapshot:
│   btrfs snapshot -r workspace → snapshots/auto-backup-20260224
├─ Send incremental from last backed-up snapshot:
│   btrfs send -p snapshots/layer-001 snapshots/auto-backup-20260224 \
│     | zstd | b2 upload
├─ Agent keeps running undisturbed — snapshot is frozen, reads don't
│   interfere with live writes
│
└─ On completion:
    Report to coordinator: POST /api/users/{id}/backup-complete
    Body: {
      "snapshot_name": "auto-backup-20260224",
      "parent_snapshot": "layer-001",
      "backblaze_key": "users/alice/auto-backup-20260224.btrfs.zst",
      "size_bytes": 4200000
    }
    Coordinator updates backups table
```

### 14.3 Why Backups Always Run From the Primary

The secondary has an exact block-level copy via DRBD, but does NOT have the Btrfs filesystem mounted. `btrfs send` requires a mounted filesystem. Only the primary has the mount.

This is fine because:
- `btrfs send` on a read-only snapshot doesn't interfere with live agent writes
- The I/O cost is minimal (reading a small delta, streaming to Backblaze)
- Network bandwidth to Backblaze is separate from inter-machine DRBD traffic

### 14.4 Restore Process (Cold Start from Backblaze)

When a fully evicted user reactivates:

```
1. Coordinator picks 2 least-loaded machines
2. Machine-1 creates a fresh empty image file:
   truncate -s 40G /data/images/alice.img
   mkfs.btrfs -f /data/images/alice.img
   mount -o loop /data/images/alice.img /mnt/users/alice
3. Download and apply snapshot chain in order:
   b2 download layer-000.btrfs.zst | zstd -d | btrfs receive /mnt/users/alice/snapshots/
   b2 download layer-001.btrfs.zst | zstd -d | btrfs receive /mnt/users/alice/snapshots/
   b2 download auto-backup-latest.btrfs.zst | zstd -d | btrfs receive /mnt/users/alice/snapshots/
4. Create workspace from latest snapshot:
   btrfs subvolume snapshot /mnt/users/alice/snapshots/auto-backup-latest \
                            /mnt/users/alice/workspace
5. Unmount, configure DRBD, form bipod with machine-2
6. DRBD initial sync from machine-1 to machine-2
7. Start agent containers on machine-1
8. Agent is live
```

### 14.5 Upload Cadence Summary

| Trigger | What gets uploaded |
|---------|--------------------|
| Running agent, every 30 minutes | Incremental from last backup snapshot to new auto-snapshot |
| User suspends (subscription event) | Incremental from last backup to final state |
| Layer snapshot created | Incremental from previous layer to new layer |
| Before evicting last fleet copy | Incremental to latest state (mandatory, must complete before eviction) |

### 14.6 Chain Maintenance

Long incremental chains make restores slow. To keep restores fast:

- Every 10 layer snapshots OR every month: upload a fresh **full send** (not incremental)
- This resets the chain — all older incrementals can be deleted
- Manifest.json tracks the chain: which snapshot is the base, which are incrementals, ordering

---

## 15. Agent Containers

### 15.1 Architecture Per User

Each running user gets containers on the primary machine:

```
alice-agent       — AI agent loop (the always-on assistant)
                    memory: 200MB reservation, 1GB limit
alice-apps        — Personal web app server (serves apps from workspace/apps/)
                    memory: 64MB reservation, 256MB limit
                    Only started if user has apps configured
```

All containers share a Docker network (`user-alice-net`). The workspace is bind-mounted from the Btrfs mount.

### 15.2 Container Configuration

The agent container receives:
- Bind mount to `/workspace` (the Btrfs subvolume)
- Environment variables for Telegram bot token, user ID
- Network access through the credential proxy (no direct LLM API access)
- No access to other users' containers or filesystems

### 15.3 Resource Sharing

Docker memory reservations are soft limits. Containers use what they need, up to the hard limit. On a 64GB machine with 4GB reserved for the host:

- At baseline (~200MB per agent): 300 agents could fit in memory
- Realistically with overhead: ~200 concurrent agents per machine
- Most agents are mostly idle (waiting for Telegram messages or cron triggers)

---

## 16. Docker Compose Prototype

### 16.1 Prerequisites

- MacBook Air M4, 24GB RAM
- VMware Fusion with Ubuntu 24.04 VM (12GB RAM, 4 cores)
- Kernel modules loaded on VM host:
  ```bash
  sudo apt install drbd-utils
  sudo modprobe drbd
  ```

### 16.2 Compose File

```yaml
version: "3.8"

networks:
  fleet-net:
    driver: bridge
    ipam:
      config:
        - subnet: 10.10.0.0/24

services:
  coordinator:
    build: ./coordinator
    hostname: coordinator
    ports:
      - "8080:8080"
    volumes:
      - ./data/coordinator:/data
    networks:
      fleet-net:
        ipv4_address: 10.10.0.10
    environment:
      - FLEET_MIN_SIZE=3
      - REPLICATION_FACTOR=2
      - REBALANCE_INTERVAL_SEC=60
      - HEARTBEAT_TIMEOUT_SEC=30

  fleet-1:
    build: ./machine-node
    privileged: true
    hostname: fleet-1
    volumes:
      - ./data/fleet-1:/var/lib/machine-data
    networks:
      fleet-net:
        ipv4_address: 10.10.0.11
    environment:
      - NODE_ID=fleet-1
      - NODE_ADDRESS=10.10.0.11
      - COORDINATOR_URL=http://10.10.0.10:8080
      - STORAGE_SIZE_GB=2
      - MAX_AGENTS=10
      - DRBD_MINOR_OFFSET=0
      - DRBD_PORT_START=7900

  fleet-2:
    build: ./machine-node
    privileged: true
    hostname: fleet-2
    volumes:
      - ./data/fleet-2:/var/lib/machine-data
    networks:
      fleet-net:
        ipv4_address: 10.10.0.12
    environment:
      - NODE_ID=fleet-2
      - NODE_ADDRESS=10.10.0.12
      - COORDINATOR_URL=http://10.10.0.10:8080
      - STORAGE_SIZE_GB=2
      - MAX_AGENTS=10
      - DRBD_MINOR_OFFSET=500
      - DRBD_PORT_START=7900

  fleet-3:
    build: ./machine-node
    privileged: true
    hostname: fleet-3
    volumes:
      - ./data/fleet-3:/var/lib/machine-data
    networks:
      fleet-net:
        ipv4_address: 10.10.0.13
    environment:
      - NODE_ID=fleet-3
      - NODE_ADDRESS=10.10.0.13
      - COORDINATOR_URL=http://10.10.0.10:8080
      - STORAGE_SIZE_GB=2
      - MAX_AGENTS=10
      - DRBD_MINOR_OFFSET=1000
      - DRBD_PORT_START=7900

  simulator:
    build: ./simulator
    hostname: simulator
    networks:
      fleet-net:
        ipv4_address: 10.10.0.100
    environment:
      - COORDINATOR_URL=http://10.10.0.10:8080
      - TOTAL_AGENTS=20
      - CHURN_RATE_PER_HOUR=2
      - FAILURE_INTERVAL_SEC=600
```

### 16.3 Machine Node Dockerfile

```dockerfile
FROM ubuntu:24.04

RUN apt-get update && apt-get install -y \
    drbd-utils \
    btrfs-progs \
    docker.io \
    openssh-server \
    dmsetup \
    xfsprogs \
    curl jq zstd \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /root/.ssh && \
    ssh-keygen -t ed25519 -f /root/.ssh/id_ed25519 -N "" && \
    cp /root/.ssh/id_ed25519.pub /root/.ssh/authorized_keys && \
    echo "StrictHostKeyChecking no" >> /root/.ssh/config

COPY machine-agent /usr/local/bin/machine-agent
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh /usr/local/bin/machine-agent

EXPOSE 8080 7900-7999
ENTRYPOINT ["/entrypoint.sh"]
```

### 16.4 Machine Node Entrypoint

```bash
#!/bin/bash
set -e

echo "[$(date)] Starting machine node: $NODE_ID"

# ── SSH daemon ──
mkdir -p /run/sshd
/usr/sbin/sshd

# ── Host storage setup (XFS formatted loop device) ──
STORAGE_FILE="/var/lib/machine-data/host-storage.img"
HOST_MOUNT="/data"
mkdir -p "$HOST_MOUNT"

if [ ! -f "$STORAGE_FILE" ]; then
    echo "[$(date)] Creating host storage: ${STORAGE_SIZE_GB}GB XFS"
    truncate -s "${STORAGE_SIZE_GB}G" "$STORAGE_FILE"
    mkfs.xfs "$STORAGE_FILE"
fi
mount -o loop "$STORAGE_FILE" "$HOST_MOUNT"

# ── Directory structure ──
mkdir -p /data/images /data/templates

# ── Base template image (Btrfs inside a file) ──
TEMPLATE="/data/templates/base.img"
if [ ! -f "$TEMPLATE" ]; then
    echo "[$(date)] Creating base template image"
    truncate -s 512M "$TEMPLATE"
    mkfs.btrfs -f "$TEMPLATE"

    TMPMNT=$(mktemp -d)
    mount -o loop "$TEMPLATE" "$TMPMNT"
    btrfs subvolume create "$TMPMNT/workspace"
    mkdir -p "$TMPMNT/workspace"/{memory,apps,data}
    mkdir -p "$TMPMNT/snapshots"
    echo '{}' > "$TMPMNT/workspace/data/config.json"
    btrfs subvolume snapshot -r "$TMPMNT/workspace" "$TMPMNT/snapshots/layer-000"
    umount "$TMPMNT"
    rmdir "$TMPMNT"
fi

# ── Docker daemon (DinD) ──
echo "[$(date)] Starting Docker daemon"
dockerd --host=unix:///var/run/docker.sock --storage-driver=overlay2 &
for i in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 1; done

# ── Start machine agent ──
echo "[$(date)] Starting machine agent on $NODE_ADDRESS"
exec machine-agent \
    --node-id="$NODE_ID" \
    --address="$NODE_ADDRESS" \
    --coordinator="$COORDINATOR_URL" \
    --data-dir="$HOST_MOUNT" \
    --max-agents="$MAX_AGENTS" \
    --drbd-minor-offset="$DRBD_MINOR_OFFSET" \
    --drbd-port-start="$DRBD_PORT_START"
```

---

## 17. Simulator

### 17.1 Behavior

The simulator manages a population of always-on agents and generates lifecycle events against the coordinator API.

```
Configuration:
  TOTAL_AGENTS: 20 simulated agents (all always-on)
  CHURN_RATE_PER_HOUR: 2 (signups and cancellations per hour)
  FAILURE_INTERVAL_SEC: 600 (simulate machine failure every 10 minutes)

Loop (every 10 seconds):
  1. If agent count < TOTAL_AGENTS and churn dice rolls signup:
     → POST /api/users (create) + POST /api/users/{id}/provision
  2. If churn dice rolls cancellation:
     → Pick random running agent → POST /api/users/{id}/suspend
  3. Every FAILURE_INTERVAL_SEC:
     → Pick random fleet machine → docker stop fleet-X
     → Verify: all agents on that machine recovered to other machines
     → docker start fleet-X after 60s (machine comes back)
  4. Log all events
```

### 17.2 Test Scenarios

| # | Scenario | Trigger | Verify |
|---|----------|---------|--------|
| 1 | New agent provisioning | Signup | Image created on 2 machines, DRBD synced, agent running |
| 2 | Machine failure (primary) | `docker stop fleet-1` | Affected agents promoted on secondary, back online in <30s |
| 3 | Machine failure (secondary) | `docker stop fleet-3` | No agent impact, new secondary created within 10 min |
| 4 | Agent suspension | Subscription cancel | Containers stopped, snapshot taken, B2 backup queued |
| 5 | Agent reactivation (warm) | Resubscribe within 30 days | Bipod remounted, agent live in <5s |
| 6 | Agent reactivation (cold) | Resubscribe after eviction | B2 download, bipod reformation, agent live |
| 7 | Live migration | Rebalancer triggers | Agent moves with <6s downtime, zero data loss |
| 8 | Scale up | Push agent count above 70% capacity | New machine provisioned |
| 9 | Scale down | Drop agent count below 30% capacity | Machine drained via live migrations, removed |
| 10 | Double failure | Two machines die | Agents with both bipod members down restored from B2 |

---

## 18. Dashboard

Web UI served by the coordinator at `http://localhost:8080/dashboard`.

**Fleet panel** — machine cards with disk/RAM bars, running agent count, status indicators, color-coded by health.

**Bipod viewer** — select a user, see their 2 machines, DRBD sync status, snapshot tree, Backblaze backup status, agent health.

**Operations panel** — active migrations, syncs, and backups with progress bars and cancel buttons.

**Event feed** — real-time scrolling log of all system events: provisions, suspensions, failovers, migrations, scale events.

**Stats** — total agents, running agents, fleet utilization, storage distribution, recommendation for scale up/down.

---

## 19. Technology Stack

| Component | Technology | Rationale |
|-----------|------------|-----------|
| Coordinator | Go | Fast, single binary, excellent concurrency, good HTTP/SQL libs |
| Machine Agent | Go | Same binary, different mode, or separate small binary |
| Database | SQLite (prototype) → Postgres (production) | Zero config for dev |
| Host filesystem | XFS | Best performance for large files and concurrent I/O |
| User filesystem | Btrfs (inside image files) | COW snapshots, instant rollback |
| Block replication | DRBD (Protocol A) | Battle-tested, bitmap resync |
| Container runtime | Docker with DinD (prototype), native Docker (production) | Standard |
| Cold storage | Backblaze B2 ($6/TB/mo) | Cheapest object storage |
| Inter-machine transport | SSH | Secure, reliable, universal |
| Credential proxy | Go (embedded in coordinator or standalone) | Routes LLM calls, tracks costs |
| Dashboard | Embedded in coordinator (Go templates or lightweight React) | Single deployment |

---

## 20. Production Migration Path

The prototype validates all orchestration logic. Moving to production:

| Prototype | Production | Change required |
|-----------|------------|-----------------|
| Machine = Docker container | Machine = Hetzner AX52 bare metal | Same agent binary, real hardware |
| Docker Compose network | Hetzner private vSwitch | Same IP-based communication |
| DinD (nested Docker) | Native Docker daemon | Simpler (no nesting) |
| Shared kernel DRBD | Per-machine kernel DRBD | Simpler (no minor partitioning) |
| 2GB storage per machine | 2TB NVMe per machine | Same code, bigger numbers |
| SQLite | Postgres | Connection string change |
| Manual scale (docker run) | Hetzner API provisioning | API call instead of docker run |
| No Backblaze | Backblaze B2 integration | Add B2 credentials and upload code |
| No Telegram gateway | Telegram Bot API gateway | Add bot routing layer |
| No credential proxy | LLM credential proxy with cost tracking | Add proxy service |

**No architectural changes.** Same coordinator, same machine agent API, same DRBD flow, same rebalancing algorithms. The prototype IS the production system, just running on smaller simulated hardware.

---

## 21. Cost Model

### 21.1 Infrastructure

| Fleet size | Monthly cost | Always-on agents | Notes |
|------------|-------------|-------------------|-------|
| 3x AX52 | €192 | ~400-500 | Minimum viable fleet |
| 5x AX52 | €320 | ~800-900 | Comfortable headroom |
| 10x AX52 | €640 | ~1,600-1,800 | |
| 30x AX52 | €1,920 | ~5,000-5,500 | |
| 100x AX52 | €6,400 | ~18,000-20,000 | |

### 21.2 Per-User Economics

```
Infrastructure cost per user:  ~€0.30-0.50/mo (2 copies × share of machine)
Backblaze storage per user:    ~€0.01-0.03/mo (5GB avg × $0.006/GB)
Total cost per user:           ~€0.35-0.55/mo

Revenue at $10/mo:             ~€9.20/mo per paying user
Gross margin:                  94-96%
```

### 21.3 Launch Scenario

```
Month 1-2 (free trial, 7 days):
  2,000 signups, ~300 peak always-on (trial users)
  Infrastructure: 3x AX52 = ~€192/mo
  Revenue: €0
  Total burn: ~€400

Month 3 (conversions):
  40% conversion = 800 paying users at $10/mo
  Revenue: ~€7,400/mo
  Infrastructure: ~€320/mo (5 machines)
  Profit: ~€7,080/mo
```

---

## 22. Open Questions

1. **DRBD Protocol A vs C**: Protocol A chosen for performance. With only 2 copies (no third to absorb failure), monitor actual data loss in machine failures carefully. Backblaze backup cadence (30 min) provides safety net.

2. **DRBD connection count at scale**: With 2,000 users and 2 copies each, that's 4,000 DRBD resources across the fleet, ~800-1,000 per machine. Each resource maintains 1 TCP connection to its peer. Need to benchmark DRBD with this many resources.

3. **Btrfs incremental chain to Backblaze**: Long chains make cold restores slow. Policy: upload full snapshot every 10 layers or every month. Delete old incrementals once superseded.

4. **Host filesystem choice**: XFS chosen for large file handling, but ext4 might be simpler and good enough. Benchmark both.

5. **Security**: DRBD traffic should be encrypted (DRBD supports TLS). Machine agent API needs mTLS. Agent containers need strict network isolation (no cross-user traffic). Credential proxy must be the only path to external APIs.

6. **Agent framework**: Which open-source agent loop to base the platform agent on. Needs to be lightweight, support persistent memory, cron scheduling, and Telegram integration. Likely a custom minimal loop rather than a heavy framework.

7. **Credential proxy design**: How exactly LLM API calls are intercepted, metered, and rate-limited. May use iptables + transparent proxy or explicit proxy configuration in agent containers.

8. **Telegram gateway routing**: Single bot handling messages for thousands of users. Need to handle rate limits (Telegram allows ~30 messages/sec per bot) and potentially multiple bot tokens for scale.

---

## 23. Changelog from v2

### Removed
- **Triangle (3-copy replication)** → replaced by bipod (2-copy)
- **Square formation** — no longer needed (always-on, no connect/disconnect)
- **NBD remote block access** — was only used during square formation
- **dm-cache block caching** — was only used during square formation
- **Deterministic switchover protocol** — replaced by simpler live migration
- **Connect/disconnect user lifecycle** — replaced by provision/suspend
- **Idle detection and grace periods** — agents are always on
- **User states: idle, offline** — replaced by provisioning, running, suspended, evicted
- **nbd-server/nbd-client packages** — no longer needed
- **thin-provisioning-tools** — no longer needed
- **5-machine minimum fleet** — reduced to 3
- **User workload simulator** — replaced by agent lifecycle simulator

### Added
- **Agent health monitoring** — crash detection, auto-restart, crash loop detection
- **Live migration protocol** — simplified DRBD-only approach for rebalancing
- **Subscription lifecycle management** — provision, suspend, reactivate, evict
- **Cost tracking fields** — monthly token budget, usage, spending pause
- **Credential proxy** (noted, to be detailed separately)
- **Telegram gateway** (noted, to be detailed separately)
- **Warm retention period** — 30-day grace before eviction of suspended users

### Modified
- **DRBD config** — 2 nodes instead of 3 per resource
- **Coordinator data model** — `triangles` → `bipods`, user states simplified, agent-centric fields added
- **Coordinator API** — connect/disconnect → provision/suspend/reactivate, added health and cost endpoints
- **Machine agent API** — removed NBD/dm-cache/switchover endpoints, added health monitoring and restart
- **Rebalancing** — always uses live migration (no waiting for idle users)
- **Eviction** — only for suspended users, not inactive users
- **Fleet scaling** — minimum 3 machines (was 5), terminology changed to agents
- **Container architecture** — simplified to agent + optional app server (was 4 containers)
- **Simulator** — lifecycle events instead of session simulation
- **Cost model** — no concurrency multiplier (all agents always on), lower per-user cost
- **Dashboard** — "Triangle viewer" → "Bipod viewer"
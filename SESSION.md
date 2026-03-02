# Session Log: Distributed Agent Platform

## Overview

This session covers the design and initial prototyping of an always-on AI agent platform — a personal computing environment in the cloud where each user gets a persistent, replicated world managed by an AI agent, accessible via Telegram/WhatsApp.

---

## 1. Architecture Evolution: v2 → v3

### Starting Point (v2)

The v2 architecture was designed for a **development environment platform** (like GitHub Codespaces) where users connect and disconnect. Key concepts:

- **Triangle (3-copy replication)**: Every user's data on 3 machines via DRBD
- **Square formation**: When a user lands on a machine outside their triangle, a 4th machine joins temporarily using NBD + dm-cache for instant access while DRBD syncs in the background
- **Connect/disconnect lifecycle**: Users come and go; idle detection triggers disconnection after grace period
- **User states**: active, idle, offline, evicted
- **Deterministic switchover protocol**: 10-step flush chain (docker pause → fsfreeze → dm-cache flush → DRBD flush-confirm → promote) for transitioning from NBD+dm-cache to local DRBD during square formation

### The Pivot

During a prior chat (exported and provided), the conversation pivoted from a dev environment platform to an **always-on agent platform** inspired by the OpenClaw hosting ecosystem. Key insight: the always-on agent model is simpler and a better market fit.

### v3 Architecture (Output: `architecture-v3.md`)

Produced via a TQE Mode 3 (automated three-question expansion) that identified all changes needed. Key changes:

**Core primitive: Triangle → Bipod**
- 2 copies instead of 3 (primary + secondary)
- Backblaze B2 as the third tier of protection
- Simpler DRBD config (2 nodes, 1 TCP connection per resource)

**Eliminated entirely:**
- Square formation (no connect/disconnect = no need for instant landing on arbitrary machines)
- NBD remote block access
- dm-cache block caching
- The full deterministic switchover protocol
- Connect/disconnect user lifecycle
- Idle detection and grace periods

**Simplified:**
- User states: `provisioning | running | suspended | evicted` (was 6 states, now 4)
- Minimum fleet size: 3 machines (was 5)
- Failure recovery: binary — secondary promotes, period

**Added:**
- Live migration protocol for rebalancing (simplified: add temp 3rd DRBD node → sync → pause → promote → cleanup; ~2-6 second agent downtime)
- Agent health monitoring (crash detection, auto-restart, crash loop detection)
- Subscription lifecycle management
- Cost tracking fields in the data model

**Retained intact:**
- Block device stack (sparse file → loop → DRBD → Btrfs)
- Btrfs snapshots (layers, tweak mode, rollback)
- Backblaze B2 integration (bucket structure, incremental send, chain maintenance)
- Go technology stack
- Coordinator/machine-agent architecture
- Heartbeat system
- Production migration path (Docker Compose prototype → Hetzner bare metal)

---

## 2. The Vision: Personal AI Platform

### Core Concept

A complete reversal of the current SaaS landscape. Instead of users connecting to narrow company-built systems, every user gets their own world in the cloud with an agent that builds, manages, and evolves their personal software ecosystem.

### What Each User Gets

- **Their own Docker container stack** running on the bipod
- **Their own PostgreSQL server** for data persistence
- **An always-on AI agent** accessible via Telegram/WhatsApp
- **Personal apps** built by the agent, served from their world
- **Built-in OAuth flows** for connecting to external services securely
- **A credential vault** where agents use API keys through a sandboxed system (never directly accessing the raw keys)
- **A memory/RAG retrieval system** so the agent remembers everything
- **A decision queue** on their phone — the agent surfaces choices when it needs human confirmation
- **A process manager view** on their phone — see running agents like a system manager
- **Snapshot-based safety** — everything is snapshottable, rollbackable

### The Marketplace

- **Apps as source code, not binaries.** When you install an app from the marketplace, you're copying a dev environment — the full source, built to work with the platform SDKs, using your Postgres, your auth system.
- **Every app is a starting point.** Don't like a feature? Tell your agent to modify it. Open it in staging, iterate, deploy when satisfied.
- **The staging/production split** makes customization safe. You're not editing what your friends are using live.
- **App creators sell blueprints** — zero marginal cost, no hosting burden. The buyer's agent handles deployment and maintenance.
- **Upstream updates** can be pulled like git upstream merges. The agent handles conflict resolution with user customizations.

### Open Source Model

The entire stack is open source. Agent loop, SDKs, primitives, container orchestration, Btrfs image format, DRBD replication, marketplace protocol — all of it. Anyone technical enough can self-host.

**The business is being the version that just works.** Sign up, connect Telegram, agent is live in 30 seconds on a replicated bipod with automatic failover, continuous backups, credential vault, cost controls, marketplace with one-tap installs.

This is the **WordPress.com vs WordPress.org model.** The open source project makes the hosted business credible and defensible. The exit door is always open, so almost nobody walks through it.

### The Bridge Between Desktop and Cloud

This is a new category of software that sits between desktop applications and cloud servers:
- Like a desktop: one user, one process, personal, lower complexity
- Like the cloud: always on, accessible anywhere, scalable resources
- Unlike either: managed by an agent, elastic resource usage, replicated and backed up

### OAuth as Bridge, Not Destination

OAuth connections to existing services (Gmail, Google Calendar, banks) are the onramp. Over time, the agent builds local replacements — email, calendar, notes, tasks, file storage — that are better because they're customized, integrated, and the user owns the data. External services gradually become unnecessary.

### Open Source Projects as Starting Points

The massive ecosystem of self-hosting projects (Mailcow, Nextcloud, Immich, Miniflux, Gitea, Actual Budget, etc.) become one-tap installs. The agent handles all the setup (DNS, SSL, DKIM, config). And because everything is source code, these too become starting points the agent can customize.

The missing piece that prevented self-hosting from going mainstream was operational burden. The agent eliminates that entirely.

---

## 3. Security Architecture

### World Isolation (Proven in PoC 1)

Every app is a **Btrfs subvolume** inside the user's image. Each subvolume mounts into its own Docker container with its own network namespace. From inside one container, other worlds literally don't exist — no filesystem path, no network path, no metadata leakage.

### Layered Security Model

1. **World isolation** — each app in its own Btrfs subvolume, own container, own network namespace
2. **Capability-based permissions** — SDK mediates all inter-world communication; each world declares what it needs; user approves
3. **Network whitelisting** — no outbound access by default; each domain individually approved through the decision queue; enforced by platform-level firewall outside the world
4. **Credential vault** — API keys and OAuth tokens never inside the world; accessed through SDK which makes calls on the world's behalf
5. **Continuous snapshots** — every 10 minutes, automatic, cheap; instant rollback of any compromised world without affecting others
6. **SDK guardrails** — security-critical code (auth, input validation, DB access) lives in platform library, not in agent-modifiable app code
7. **Bipod + Backblaze** — data replicated and backed up; recovery always possible

### Agent-Modified Code Risk

The biggest security risk: agents modifying source code of internet-facing services can introduce vulnerabilities. Mitigations:

- **Marketplace apps declare capabilities** at install time (network access, inter-world communication, etc.)
- **Network whitelisting with decision queue** — even if an attacker gets into a world, they can't phone home unless that domain was whitelisted
- **Staging gate** — automated security checks (static analysis, dependency scanning) before promotion to production
- **Modification-restricted zones** — security-critical infrastructure (email core, auth systems) has a locked core that agents can't modify; agents build on top via APIs and SDKs
- **Upstream tracking** — agent merges security patches from upstream projects automatically

### Attack Scenario Analysis

If a hacker exploits a vulnerability in a user's modified budgeting app:
1. They're inside the container → can see budgeting data only
2. They try to exfiltrate → network firewall blocks (only whitelisted domains)
3. They try to access other worlds → filesystem paths don't exist, no network path
4. They sit trapped → next 10-minute snapshot may roll them out
5. Even if they read data → blast radius is one world's data, not the user's whole life

### Metadata Isolation (Proven in PoC 1, Patch 1)

Containers don't use bind mounts. Instead, the block device is passed via `--device` and the subvolume is mounted from inside via an init script. `/proc/mounts` shows only the container's own subvolume name — no host paths, no other world names, no information leakage.

### Encryption (Discussed, Not Yet Implemented)

- LUKS above DRBD: `sparse file → loop → DRBD → LUKS/dm-crypt → Btrfs`
- DRBD replicates encrypted blocks — secondary never decrypts
- Per-user encryption keys stored in coordinator database (encrypted with master key)
- Protects against: physical disk theft, data center mishaps
- Does NOT protect against: platform operator access (same as every cloud provider)
- Honest framing: encryption raises the bar from casual access to deliberate targeted access

---

## 4. Pricing Model

### Usage-Based Metering (New Concept)

Traditional VPS pricing (fixed tier) doesn't fit because:
- Resources are shared across ~200 agents per machine
- Individual usage is bursty and unpredictable
- An agent can negotiate resource changes in real time
- Users shouldn't pay for idle reserved capacity

### Metering Layers

1. **Base rate** (~$1-2/month): cost of existing — image on two machines (bipod), DRBD running, Backblaze backups
2. **Compute metering**: CPU-seconds consumed, sampled per minute. Near-zero when idle, spikes during heavy tasks
3. **Memory metering**: baseline ~200MB, metered when agent requests more for heavy tasks
4. **Storage metering**: per GB actually used (not sparse apparent size)

### The Agent as Resource Negotiator

- Agent knows what resources a task needs, calculates cost, surfaces decision to user
- User can pre-authorize: "anything under $5, just do it"
- Running cost estimate always available: "you've spent $3.20 this month so far"
- Spending limits built in, managed through decision queue

### Two Parallel Metering Systems

1. **LLM tokens** (what the agent thinks costs) — routed through credential proxy
2. **Infrastructure** (what the agent does costs) — compute, memory, storage, network

Both flow through the agent, both have user-configurable limits, both surface through the decision queue.

### Psychology

- Light users feel respected ($2/month, not subsidizing others)
- Heavy users feel empowered (no ceiling, pay for what you consume)
- No bill shock: agent shows estimates, enforces limits
- Philosophy: "your world, you only pay for what you use, no limit to the power you might need"

---

## 5. Coordinator Design

### Approach: Start Simple, Design for HA

**Approach 1 (build now):** Single coordinator, fast restart. All state in Postgres. If it dies, restart it (5-10 seconds). Agents keep running, DRBD keeps replicating. New connections and failover decisions queue up.

**Approach 2 (design for):** Active-passive pair. Both connected to same Postgres. Leader election via Postgres advisory locks. Standby takes over in seconds if active dies. No split-brain risk — Postgres is single source of truth.

### Key Design Principle

Machine agents must be autonomous enough to keep users alive without the coordinator. Continue running containers, continue DRBD replication, buffer events. Coordinator is the brain; machine agents are the nervous system.

---

## 6. Proof of Concepts

---

### PoC 1: Btrfs World Isolation (`poc-btrfs/`)

#### Goal

Build a proof of concept demonstrating the foundational primitive for a distributed agent platform: a single Btrfs image file per user, divided into isolated subvolumes ("worlds"), each mounted into its own Docker container, with cheap instant snapshots and rollback. Everything runs inside Docker Desktop on macOS via a privileged Docker-in-Docker setup.

#### Architecture

```
macOS (Docker Desktop)
  └─ "machine" container (privileged, Ubuntu 24.04, DinD)
       └─ Btrfs image: /data/images/alice.img (sparse, 2GB)
            └─ Loop device → Btrfs mount at /mnt/users/alice/
                 ├─ subvol: core/          → container "alice-core"
                 ├─ subvol: app-email/     → container "alice-app-email"
                 ├─ subvol: app-budget/    → container "alice-app-budget"
                 └─ snapshots/
```

#### File Structure

```
poc-btrfs/
├── docker-compose.yml          # Single privileged "machine" service
├── machine/
│   ├── Dockerfile              # Ubuntu 24.04 + btrfs-progs + docker.io
│   ├── entrypoint.sh           # Starts dockerd, waits for ready, runs demo
│   └── demo.sh                 # Full 8-phase automated PoC
```

#### Issues Encountered & Fixes

##### Issue 1: overlay2 storage driver not supported in DinD

**Problem:** The entrypoint started dockerd with `--storage-driver=overlay2`. Inside the privileged container, overlay2 failed to mount:

```
level=error msg="failed to mount overlay: invalid argument" storage-driver=overlay2
failed to start daemon: error initializing graphdriver: driver not supported: overlay2
```

**Fix:** Switched to `--storage-driver=vfs`. The vfs driver has no kernel requirements — it works everywhere by doing full copies instead of COW layering. Slower and uses more space for image layers, but perfectly fine for a PoC where we only pull a small Alpine image.

##### Issue 2: /proc/mounts isolation check false positive

**Problem:** Phase 4 (Prove Isolation) included a check that grepped `/proc/mounts` inside each container for other world names. This failed because `/proc/mounts` shows the host-side mount source paths (e.g., `/mnt/users/alice/app-budget`), which naturally contain the subvolume names. The container can see the *string* in its mount metadata but cannot actually *access* those paths.

```
✗ FAIL: app-budget can see other worlds in /proc/mounts — isolation broken!
```

**Fix:** Replaced the `/proc/mounts` grep with actual filesystem access tests — trying to `ls` the other worlds' mount paths from inside the container. These paths don't exist inside the container's filesystem namespace, so the check correctly passes. The real isolation guarantee is filesystem access, not mount metadata strings.

#### Final Result

All 8 phases pass:

```
════════════════════════════════════════════
  PROOF OF CONCEPT: COMPLETE
════════════════════════════════════════════
  ✓ Sparse image file created (2GB apparent, 5.1M actual)
  ✓ Btrfs formatted and mounted
  ✓ 3 isolated worlds created as subvolumes
  ✓ 3 Docker containers, each seeing only its own world
  ✓ World isolation verified (no cross-world access)
  ✓ Agent work simulated (files written from container)
  ✓ Snapshot taken (instant, near-zero space)
  ✓ Disaster simulated (files corrupted/deleted)
  ✓ Rollback executed (instant restore from snapshot)
  ✓ Restored world verified (all good data back, bad data gone)
  ✓ Other worlds unaffected (isolation held during rollback)
  ✓ Disk efficiency confirmed (sparse + COW working)
════════════════════════════════════════════
```

Key numbers:
- **2GB apparent** image size vs **5.1MB actual** disk usage — sparse file + Btrfs COW means you only pay for data actually written
- 3 subvolumes + 1 snapshot + a full disaster/rollback cycle, all within that 5.1MB

#### How to Run

```bash
cd poc-btrfs
docker compose up --build
```

To explore interactively after the demo completes:
```bash
docker exec -it poc-btrfs-machine-1 bash
```

To clean up:
```bash
docker compose down -v
```

---

### PoC 1, Patch 1: Device Mount Isolation

#### Motivation

The original PoC used bind mounts (`-v /mnt/users/alice/app-budget:/workspace`) to give each container access to its subvolume. This worked for filesystem isolation, but leaked host-side mount paths into `/proc/mounts` inside the container:

```
/dev/loop0 /workspace btrfs rw,relatime,subvol=/app-budget ...
```

While the container couldn't access other paths, it could see the host mount structure in `/proc/mounts` — leaking information about the host. In production, no container should see any host path metadata.

#### Approach

Instead of bind mounts, pass the loop device into the container via `--device` and mount the specific Btrfs subvolume from inside using an init script. Each container gets:

1. Access to the loop device via `--device`
2. Minimal capabilities (dropped with `--cap-drop ALL`, then specific ones added back)
3. An init script (`container-init.sh`) that mounts the subvolume, then drops privileges by exec'ing as a non-root user

Production container launch pattern:
```bash
docker run --device /dev/loop0 \
  --cap-drop ALL --cap-add SYS_ADMIN --cap-add SETUID --cap-add SETGID \
  --network none \
  -e SUBVOL_NAME=app-budget -e BLOCK_DEVICE=/dev/loop0 \
  platform/app-container
```

Container init script: mount subvolume as root → drop to `appuser` via `su` → workload runs with zero capabilities as non-root.

#### New Files Created

- **`machine/container-init.sh`** — Init script that runs as root to `mount -o subvol=<name>`, creates an `appuser`, then `exec su` to drop privileges
- **`machine/app-container/Dockerfile`** — Custom Alpine image with btrfs-progs and the init script baked in
- **`machine/Dockerfile`** updated to copy the app-container build context into the machine

#### Changes to demo.sh

- **Phase 3:** Dynamically discovers loop device with `losetup -j`. Builds `platform/app-container` image using inner Docker daemon. Starts containers with `--device`, `--cap-drop ALL`, `--cap-add` for needed capabilities, and environment variables (`SUBVOL_NAME`, `LOOP_DEVICE`)
- **Phase 4:** Added `/proc/mounts` metadata isolation tests — verifies each container's `/proc/mounts` shows only its own `subvol=` entry, no other subvolume names, and no host paths like `/mnt/users/alice`
- **Phase 5, 6, 7:** All `docker exec` commands updated to use `-u root` since the workload now runs as `appuser`
- **Phase 6 rollback:** Restored container started with same device-mount pattern instead of bind mount
- **Summary:** Added "Metadata isolation verified (no host paths in /proc/mounts)" line

#### Issues Encountered

##### Issue 3: Containers crashing immediately — `--cap-drop ALL --cap-add SYS_ADMIN`

**Problem:** The patch prompt specified `--cap-drop ALL --cap-add SYS_ADMIN` as the capability pattern. Containers started but exited immediately with no output in `docker ps`.

**Debugging:** We ran the machine interactively and checked `docker logs` on the crashed container. The error was:

```
su: can't set groups: Operation not permitted
```

**Root cause:** The `su` command in `container-init.sh` needs `SETUID` and `SETGID` capabilities to switch users. `--cap-drop ALL` removes everything, and adding back only `SYS_ADMIN` isn't enough.

**Fix:** Added `--cap-add SETUID --cap-add SETGID` to all `docker run` commands. These capabilities are only used during the init script — once `exec su` replaces the process as `appuser`, all capabilities are gone (non-root users don't inherit capabilities by default in Linux).

##### Issue 4: Legacy Docker builder deprecation warning

**Problem:** The inner Docker daemon's `docker build` printed a noisy deprecation warning:

```
DEPRECATED: The legacy builder is deprecated and will be removed in a future release.
```

**Fix:** Added `docker-buildx` to the machine Dockerfile's apt packages. The inner Docker daemon now uses BuildKit by default, suppressing the warning.

##### Detour: Attempting to eliminate SETUID/SETGID capabilities

**Context:** We noticed that adding `SETUID`/`SETGID` went against the patch prompt's explicit statement that "only SYS_ADMIN" should be needed. We explored alternatives.

**Attempt 1 — `setpriv` instead of `su`:** Replaced the `su` call with `setpriv --reuid=<uid> --regid=<gid> --clear-groups --inh-caps=-all`. Added `util-linux` to the app container for `setpriv`. **Result:** Same failure — `setpriv` also needs SETUID/SETGID at the kernel level to change UID/GID. This is a kernel requirement, not a tool limitation.

**Attempt 2 — Stay as root, drop all caps:** Instead of switching users, used `setpriv --inh-caps=-all --ambient-caps=-all --bounding-set=-all` to drop every capability while remaining UID 0. A root process with zero capabilities is effectively unprivileged. **Result:** Technically worked, but felt wrong — running as root (even capability-less) is not as clean as actually switching to a non-root user. The user preferred the explicit user switch.

**Final decision:** Reverted to the `su`-based approach with `SETUID`/`SETGID` added. These capabilities exist only during the init script's brief execution and are gone once the workload process starts as `appuser`. The security posture is correct: the running workload has zero capabilities and runs as non-root. Also reverted the `util-linux` addition to the app container since it was only needed for `setpriv`.

**Key takeaway:** This is the production pattern. Swap loop device for DRBD device and it's identical.

#### Updated File Structure

```
poc-btrfs/
├── docker-compose.yml
├── machine/
│   ├── Dockerfile              # Now also includes docker-buildx
│   ├── entrypoint.sh
│   ├── demo.sh                 # Updated for device mounts + metadata tests
│   ├── container-init.sh       # New: mounts subvolume, drops to appuser
│   └── app-container/
│       └── Dockerfile          # New: Alpine + btrfs-progs + init script
```

#### Final Result

All 8 phases pass with the additional metadata isolation verification:

```
════════════════════════════════════════════
  PROOF OF CONCEPT: COMPLETE
════════════════════════════════════════════
  ✓ Sparse image file created (2GB apparent, ~5M actual)
  ✓ Btrfs formatted and mounted
  ✓ 3 isolated worlds created as subvolumes
  ✓ 3 Docker containers, each seeing only its own world
  ✓ World isolation verified (no cross-world access)
  ✓ Agent work simulated (files written from container)
  ✓ Snapshot taken (instant, near-zero space)
  ✓ Disaster simulated (files corrupted/deleted)
  ✓ Rollback executed (instant restore from snapshot)
  ✓ Restored world verified (all good data back, bad data gone)
  ✓ Other worlds unaffected (isolation held during rollback)
  ✓ Metadata isolation verified (no host paths in /proc/mounts)
  ✓ Disk efficiency confirmed (sparse + COW working)
════════════════════════════════════════════
```

---

### PoC 2: DRBD Bipod Replication (`poc-drbd/`)

#### Goal

Build a PoC demonstrating DRBD replication between two machines forming a bipod. Writes on the primary replicate to the secondary in real-time. When the primary dies, the secondary promotes, mounts the same Btrfs filesystem, starts containers, and all data survives. Built from `master-prompt2.md`.

#### Environment

**Hetzner bare-metal machine** running Ubuntu 24.04 LTS (kernel 6.8.0-90-generic, x86_64). Docker containers simulate fleet machines, but the kernel is real. Both containers are privileged and share the host kernel.

#### File Structure

```
poc-drbd/
├── docker-compose.yml            # Two machine services + drbd-net bridge (10.20.0.0/24)
├── machine/
│   ├── Dockerfile                # Ubuntu 24.04 + drbd-utils + btrfs-progs + docker + ssh + kmod
│   ├── entrypoint.sh             # Starts sshd + dockerd (vfs), builds app image, runs demo or waits
│   ├── demo.sh                   # Full 11-phase bipod test (phases 0-10)
│   ├── container-init.sh         # Mounts subvolume via BLOCK_DEVICE env var, drops to appuser
│   └── app-container/
│       └── Dockerfile            # Alpine + btrfs-progs + init script
```

#### Host Prerequisites Installed

The user had to manually install several things on the host, and we had to work around sudo and interactive prompt issues:

##### 1. DRBD kernel module

- `linux-modules-extra-6.8.0-90-generic` — Provides the in-tree DRBD 8.4 kernel module. Installed via `sudo apt install`.
- `drbd-dkms` from LINBIT PPA (`ppa:linbit/linbit-drbd9-stack`) — Provides DRBD **9.3.0** kernel module built via DKMS. This was necessary because `drbd-utils` 9.22 (the only version in Ubuntu 24.04 repos) is incompatible with the DRBD 8.4 kernel module.
- `linux-headers-6.8.0-90-generic` — Required for DKMS to build the module. Was already installed but needed verification.
- DKMS initially built for the wrong kernel (6.8.0-101). Had to run `sudo dkms install drbd/9.3.0-1ppa1~noble1 -k 6.8.0-90-generic` after installing headers.
- Final state: `modprobe drbd` loads DRBD 9.3.0 from `/lib/modules/6.8.0-90-generic/updates/dkms/drbd.ko.zst`

##### 2. drbd-utils

- Installed via `sudo apt install drbd-utils`. Version 9.22.0.
- **Blocked by interactive postfix dialog**: `drbd-utils` pulls in `bsd-mailx` → `postfix`, which has an interactive debconf prompt. User had to run:
  ```bash
  echo "postfix postfix/main_mailer_type select No configuration" | sudo debconf-set-selections
  echo "postfix postfix/mailname string localhost" | sudo debconf-set-selections
  sudo DEBIAN_FRONTEND=noninteractive apt install -y drbd-utils
  ```

##### 3. docker-compose-v2

- Not installed by default. Installed via `sudo apt install docker-compose-v2`.

##### 4. sudo access for Claude

- Claude user initially couldn't run `modprobe`, `kill`, or `add-apt-repository` due to missing NOPASSWD entries.
- User added `/usr/sbin/modprobe`, `/usr/bin/kill` to visudo, then temporarily gave full `ALL` privileges for the DRBD 9 installation steps.

#### Issues Encountered & Fixes — Docker Phase (Abandoned)

##### Issue 5: overlay2 fails in DinD (same as poc-btrfs)

**Problem:** `master-prompt2.md` specified `--storage-driver=overlay2` saying "overlay2 works on real Linux". While true for native Docker, the DinD containers have overlay2 rootfs, and overlay-on-overlay fails with `invalid argument`.

**Fix:** Changed `entrypoint.sh` to use `--storage-driver=vfs`, same as poc-btrfs.

##### Issue 6: DRBD kernel module not found in container

**Problem:** Phase 0 tried `modprobe drbd` but the container didn't have `kmod` installed. The module was loaded on the host but `modprobe` binary was missing.

**Fix:** Added `kmod` to the Dockerfile's apt packages. Also changed Phase 0 to check `/proc/drbd` first (since the module is loaded on the host and the container is privileged), falling back to `modprobe` only if needed.

##### Issue 7: DRBD 8.4 vs 9.x version mismatch

**Problem:** The host kernel had DRBD 8.4.11 (from `linux-modules-extra`), but containers had `drbd-utils` 9.22.0. When `drbdadm up alice` ran, it translated to DRBD 9 kernel commands (`drbdsetup new-resource`), but the 8.4 kernel module only understands 8.4 commands. Error:

```
invalid command
Command 'drbdsetup-84 new-resource alice' terminated with exit code 20
```

**Fix:** Installed DRBD 9.3.0 kernel module from LINBIT PPA via DKMS (see Host Prerequisites above). After `sudo rmmod drbd && sudo modprobe drbd`, the 9.3.0 module loads from the DKMS `updates/` directory.

##### Issue 8: `drbdadm create-md` failing silently

**Problem:** The original command `yes yes | drbdadm create-md alice 2>&1 | tail -1` showed "Operation canceled" and metadata wasn't actually created. DRBD 9 needs `--force` flag and different confirmation handling.

**Fix:** Changed to `yes | drbdadm create-md --force alice 2>&1` (removed `tail -1` for debugging, added `--force`). Metadata creation now shows "New drbd meta data block successfully created."

##### Issue 9: Stale DRBD kernel state between runs

**Problem:** DRBD resources and minors are kernel-global (shared between containers). After a failed run, `/dev/drbd0` persists in the kernel. Next run fails with:

```
sysfs node '/sys/devices/virtual/block/drbd0' (already? still?) exists
Minor or volume exists already (delete it first)
```

**Fix:** Added cleanup at start of Phase 0 using `drbdsetup down alice` (not `drbdadm down` which requires a config file). Also added loop device cleanup in Phase 1.

##### Issue 10: DRBD shared-kernel architecture conflict

**Problem:** Both containers share the host kernel, so DRBD resource names and minor numbers are kernel-global. When machine-1 runs `drbdadm up alice`, it creates the resource and minor 0 in the kernel. When machine-2 then runs `drbdadm up alice`, it fails because:
1. Resource `alice` already exists in the kernel
2. The minor it tries to create conflicts

Error: `Minor or volume exists already (delete it first)` for minor 1 on machine-2.

**Root cause:** `drbdadm` is designed for each host having its own kernel. It doesn't support two peers of the same resource on the same kernel.

**Attempted fix:** Replaced `drbdadm` with raw `drbdsetup`/`drbdmeta` commands, managing the entire DRBD setup from machine-1.

##### Issue 11: `drbdmeta` syntax differences

**Problem:** Multiple syntax errors with `drbdmeta` and `drbdsetup`:
- `drbdmeta create-md --max-peers 1` — `--max-peers` is not a flag, `max_peers` is a positional arg
- `drbdsetup new-resource alice --node-id 0` — `--node-id` is not a flag, `node_id` is positional
- `drbdsetup primary 0 --force` — `primary` takes a resource name, not a minor number
- `drbdsetup attach` needs 4 args: `{minor} {lower_dev} {meta_data_dev} {meta_data_index}`

**Fix:** Corrected all commands to match actual syntax:
```bash
drbdmeta --force 0 v09 /dev/loop0 internal create-md 1
drbdsetup new-resource alice 0
drbdsetup attach 0 /dev/loop0 /dev/loop0 internal
drbdsetup primary alice --force=yes
```

##### Issue 12: Loop device collision between containers

**Problem:** Both machine-1 and machine-2 got `/dev/loop0`. Phase 1 had cleanup code `losetup -j /data/images/alice.img | ... | losetup -d` that, when run on machine-2, detached machine-1's loop device because `losetup -j` queries the kernel's global loop device table.

**Fix:** Removed the `losetup -j` cleanup lines from Phase 1. Changed Phase 0 to clean up stale loop devices from prior runs instead.

##### Issue 13: FUNDAMENTAL — DRBD replication impossible with shared kernel

**Problem:** After fixing all the command syntax issues (Issues 10-12), we hit the fundamental architectural limitation: **DRBD replication between two containers on the same host cannot work** because:

1. **DRBD is a kernel module** — there is exactly ONE instance managing ALL DRBD state
2. **Replication requires TWO separate DRBD instances** talking over TCP — one on each host, each managing its own side of the resource
3. With shared kernel, you can create ONE resource with ONE node-id, but there's nobody on the other end of the TCP connection to replicate to
4. Even with `drbdsetup new-peer` and `drbdsetup new-path`, the peer at the remote IP has no DRBD instance listening — because it's the same kernel, and the DRBD module is already configured as node 0

This is not a configuration bug — it's a fundamental limitation of running DRBD between containers that share a kernel. DRBD's architecture **requires** two separate kernels.

**Attempted workaround:** Restructured demo.sh to use DRBD as a standalone block device layer (no peer connection) on machine-1, then `dd` the backing device to machine-2 to simulate replication. This proved the failover workflow but not actual real-time replication — which is the entire point of the DRBD PoC.

##### Issue 14: `mkfs.btrfs` on `/dev/drbd0` — device node visibility

**Problem:** After successfully setting up DRBD (Phase 2 showed `alice role:Primary disk:UpToDate`), `mkfs.btrfs -f /dev/drbd0` failed. The DRBD kernel module creates `/dev/drbd0` in devtmpfs, but the container may not see it if devtmpfs was mounted at container start time before DRBD was configured.

**Partial fix:** Added `mknod /dev/drbd0 b 147 0 2>/dev/null || true` to ensure the device node exists. Did not get to test this fix because we decided to switch approaches at this point.

#### Decision: Abandon Docker Approach

##### Why Docker fails for DRBD

The `poc-btrfs` PoC worked perfectly with Docker because Btrfs is a **filesystem** — it operates per-mount, and each container's mount is independent. DRBD is a **kernel module** with **global state** — resources, minors, and connections are shared across all containers on the same host. Docker containers share the host kernel by design.

The master-prompt2.md specification assumed containers would behave like separate machines with independent DRBD state. This is architecturally impossible with Docker on a single host.

##### What we tried (Docker approach)

1. **`drbdadm up alice`** on both containers → fails, resource already exists (Issue 10)
2. **Raw `drbdsetup`** managing both sides from machine-1 → no peer to replicate to (Issue 13)
3. **`dd`-based simulation** of replication → works but doesn't prove real DRBD replication
4. Multiple `drbdmeta`/`drbdsetup` syntax fixes along the way (Issues 11, 12)

##### QEMU/KVM attempt

Tried to use QEMU/KVM VMs on the current host (each VM would have its own kernel, solving the shared-kernel problem). Discovered:
- Current host is itself a VM (AMD EPYC with `hypervisor` flag, no `svm` flag)
- No nested virtualization support → `/dev/kvm` unavailable
- `sudo modprobe kvm_amd` → "Operation not supported"
- QEMU TCG (software emulation) would work but is 10-50x slower — impractical

Built an Alpine Linux base image and extracted kernel/initramfs, but abandoned this approach due to speed concerns.

#### Final Approach: Two Hetzner Cloud Servers

The real solution: use **two actual separate servers**, each with its own kernel. This is also the closest to the production environment.

Architecture:
```
Local machine (orchestration)
  │
  ├── hcloud CLI → Hetzner Cloud API
  │
  ├── machine-1 (CX23, Ubuntu 24.04, private IP 10.0.0.2)
  │    ├── DRBD primary (/dev/drbd0)
  │    ├── Btrfs mounted at /mnt/users/alice/
  │    └── Docker containers (native, not DinD)
  │
  ├── machine-2 (CX23, Ubuntu 24.04, private IP 10.0.0.3)
  │    ├── DRBD secondary (/dev/drbd0) — real TCP replication
  │    └── (standby until failover)
  │
  └── Private network: 10.0.0.0/24
```

Key advantages:
- **Separate kernels** → DRBD replication works natively
- **Standard `drbdadm`** → no raw drbdsetup hacks
- **Real network** → actual Protocol A async replication over TCP
- **Native Docker** → no DinD complexity
- **Production-realistic** → this IS the production topology
- **Automated lifecycle** → `hcloud` CLI creates/destroys servers; auto-teardown after demo (~€0.012/hr for both)

Scripts:
- `infra.sh` — Creates/destroys Hetzner infrastructure (servers, network, SSH keys)
- `run.sh` — Full orchestration: infra up → cloud-init wait → deploy scripts → run demo → teardown
- `cloud-init.yaml` — Installs DRBD 9 (LINBIT PPA + DKMS), Docker, btrfs-progs
- `scripts/demo.sh` — The 11-phase DRBD bipod demo (runs on machine-1, SSHes to machine-2)
- `scripts/container-init.sh` — Reused from Docker approach
- `scripts/app-container/Dockerfile` — Reused from Docker approach

#### Issues Encountered & Fixes — Hetzner Cloud Phase (Successful)

##### Issue 15: Hetzner server type deprecation

**Problem:** The plan specified `cx22` server type, but Hetzner deprecated the CX Gen2 and CPX Gen1 lines in EU locations as of Dec 31, 2025. `hcloud server create --type cx22` returned `server type not found`.

**Fix:** Queried the Hetzner API for available types at eu-central locations. The replacement options:
- `cpx11` (Gen1) — deprecated in EU, only available in US (ash/hil)
- `cpx12` (Gen2) — only available in Singapore
- `cx23` (Gen3, cost-optimized) — available at fsn1, nbg1, hel1
- `cpx22` (Gen2, regular) — available at fsn1, nbg1, hel1

Selected `cx23` (2 vCPU, 4GB RAM, 40GB disk, ~$0.007/hr).

##### Issue 16: fsn1 location temporarily disabled

**Problem:** With `cx23` at `fsn1`, server creation returned `server location disabled (resource_unavailable)`. Falkenstein was at capacity or under maintenance.

**Fix:** Changed location to `nbg1` (Nuremberg). Both locations are in the `eu-central` network zone, so the private network subnet worked without changes.

##### Issue 17: DRBD kernel module not compiled by DKMS

**Problem:** Cloud-init installed `drbd-dkms` and `linux-headers-generic`, but `modprobe drbd` failed:
```
modprobe: FATAL: Module drbd not found in directory /lib/modules/6.8.0-90-generic
```
The DRBD tools (`drbdadm`) worked fine — only the kernel module was missing. `drbdadm create-md` succeeded, but `drbdadm up` failed because `drbdsetup new-resource` needs the kernel module loaded.

**Root cause:** `linux-headers-generic` is a meta-package that installs headers for the latest kernel in the apt repo, which may not match the *running* kernel. Hetzner's Ubuntu 24.04 image boots with a specific kernel version (e.g., `6.8.0-90-generic`) that may lag behind the latest in the repo. DKMS built the module for the wrong kernel version.

**Fix:** Added two lines to cloud-init `runcmd` before `modprobe drbd`:
```yaml
runcmd:
  - apt-get install -y linux-headers-$(uname -r)
  - dkms autoinstall
  - modprobe drbd
```
This ensures headers match the running kernel, then rebuilds all DKMS modules.

#### Successful Run — 47/47 Checks Passed

```
═══ Phase 0: DRBD Module Check ═══
  ✓ DRBD module loaded (local)
  ✓ DRBD tools available (peer)

═══ Phase 1: Create Backing Storage ═══
  ✓ Local backing store: /data/images/alice.img → /dev/loop0
  ✓ Peer backing store: /data/images/alice.img → /dev/loop0

═══ Phase 2: Configure and Start DRBD ═══
  ✓ Config written locally: /etc/drbd.d/alice.res
  ✓ Config written on peer
  ✓ Metadata created (local)
  ✓ Metadata created (peer)
  ✓ DRBD resource up (local)
  ✓ DRBD resource up (peer)
  ✓ This node is now primary
  ✓ Sync complete: both nodes UpToDate

  DRBD status:
    alice role:Primary
      disk:UpToDate open:no
      drbd-machine-2 role:Secondary
        peer-disk:UpToDate

═══ Phase 3: Format Btrfs and Create Worlds ═══
  ✓ Btrfs filesystem created on /dev/drbd0
  ✓ Mounted at /mnt/users/alice
  ✓ Created 3 subvolumes: core, app-email, app-budget
  ✓ Seed data written to all 3 worlds

═══ Phase 4: Start Isolated Containers ═══
  ✓ Container alice-core started
  ✓ Container alice-app-email started
  ✓ Container alice-app-budget started
  ✓ All containers running with isolated world views

═══ Phase 5: Simulate Agent Work ═══
  ✓ alice-core: Profile updated
  ✓ alice-app-email: Draft created
  ✓ alice-app-budget: February budget added
  ✓ Isolation confirmed: email container cannot see budget data
  ✓ Isolation confirmed: budget container cannot see email drafts

═══ Phase 6: Take Pre-Failover Snapshot ═══
  ✓ Snapshots created
  ✓ Snapshot core: has updated profile (theme=dark)
  ✓ Snapshot email: has draft (subject=Meeting)
  ✓ Snapshot budget: has February data (income=5200)

═══ Phase 7: Simulate Primary Death ═══
  ✓ All containers stopped and removed
  ✓ Btrfs unmounted
  ✓ Demoted to secondary

═══ Phase 8: Failover — Promote Secondary ═══
  ✓ Machine-2 is now primary
  ✓ Btrfs mounted on machine-2
  ✓ App container image ready on machine-2
  ✓ Container alice-core started on machine-2
  ✓ Container alice-app-email started on machine-2
  ✓ Container alice-app-budget started on machine-2

═══ Phase 9: Verify Data Survived Failover ═══
  ✓ Core world intact: profile has theme=dark
  ✓ Email world intact: draft has subject=Meeting
  ✓ Budget world intact: February income=5200
  ✓ Pre-failover snapshot intact on machine-2
  ✓ Isolation intact on machine-2: email cannot see budget data

═══ Phase 10: Prove Rollback ═══
  ✓ Data corrupted successfully (simulating bad write)
  ✓ Core world restored from snapshot
  ✓ Rollback successful: profile restored (theme=dark)
  ✓ Machine-2 cleaned up

═══════════════════════════════════════
 DRBD Bipod Replication PoC — Results
═══════════════════════════════════════
  Passed: 47
  Failed: 0
  ALL CHECKS PASSED
```

Infrastructure auto-torn down after completion. No lingering Hetzner costs.

#### What Was Proven

1. **Real DRBD Protocol A replication** — actual async block-level replication over TCP between two independent servers
2. **Btrfs subvolumes as isolated container worlds** — each Docker container mounts only its own subvolume
3. **Automatic failover** — primary demotes, secondary promotes, mounts the same Btrfs filesystem
4. **Data integrity across failover** — all 3 worlds + snapshots survive intact on the new primary
5. **Container isolation survives failover** — email container still can't see budget data after promoting on a different machine
6. **Point-in-time rollback** — corrupt data, restore from snapshot, verify recovery
7. **Full lifecycle automation** — `./run.sh` handles everything: infra creation, provisioning, demo, teardown

#### How to Run

```bash
export HCLOUD_TOKEN=your-api-token
cd poc-drbd
./run.sh
```

The script:
1. Creates two cx23 servers at Hetzner Cloud (nbg1) with a private network
2. Waits for cloud-init to install DRBD 9, Docker, btrfs-progs (~3-4 min)
3. Sets up inter-machine SSH, deploys scripts, builds Docker images
4. Runs the 11-phase demo on machine-1
5. Auto-tears down all infrastructure on completion

To manage infrastructure manually:
```bash
./infra.sh up       # Create servers
./infra.sh status   # Check state
./infra.sh down     # Destroy everything
```

---

### PoC 3: Backblaze B2 Backup & Restore (`poc-backblaze/`)

#### Goal

Build a PoC demonstrating the third and final tier of data safety: cold backup and restore via Backblaze B2. This proves the complete recovery path: `btrfs send → zstd compress → B2 upload → [fleet copies deleted] → B2 download → zstd decompress → btrfs receive → workspace from snapshot → containers run → DRBD bipod formed`. This is the "both machines die" scenario and the "reactivation after eviction" scenario from the architecture.

After this PoC, every data safety scenario in the architecture has a proven recovery path.

#### Environment

Two Hetzner Cloud CX23 servers at `nbg1` (Nuremberg), Ubuntu 24.04, private network `10.0.0.0/24`. Same as PoC 2 but with additional B2 CLI tooling. Demo runs **locally on macOS** and SSHes into both machines (unlike PoC 2 where demo.sh ran on machine-1).

#### Architecture

```
machine-1 (CX23, Ubuntu 24.04, private IP 10.0.0.2)
  ├── Phase 1-7: alice.img → loop → Btrfs (creates world, backups to B2)
  ├── Phase 7: data destroyed (simulates fleet loss)
  └── Phase 8+: blank image → loop → DRBD secondary

machine-2 (CX23, Ubuntu 24.04, private IP 10.0.0.3)
  ├── Phase 8: blank image → loop → DRBD primary → mkfs.btrfs → btrfs receive from B2
  └── Phase 10: containers running on restored data

Backblaze B2 bucket: poc-backblaze-{random}/users/alice/
  ├── layer-000.btrfs.zst          (full send)
  ├── layer-001.btrfs.zst          (incremental from layer-000)
  ├── auto-backup-latest.btrfs.zst (incremental from layer-001)
  └── manifest.json                 (chain metadata)
```

#### File Structure

```
poc-backblaze/
├── run.sh                       # Main: validate env → infra up → demo → teardown
├── infra.sh                     # Hetzner lifecycle: up / down / status
├── cloud-init.yaml              # Server provisioning (DRBD 9, Docker, btrfs, zstd, b2)
├── scripts/
│   ├── demo.sh                  # 13-phase demo (phases 0-12), 65 checks
│   ├── container-init.sh        # Device-mount init script (from PoC 1/2)
│   └── app-container/
│       └── Dockerfile           # Alpine + btrfs-progs (from PoC 1/2)
```

#### Issues Encountered & Fixes

##### Issue 18: `du -b` reports apparent size, not disk usage

**Problem:** Phase 1 checked sparse file efficiency using `du -b` to get actual disk usage, but `du -b` (`--apparent-size --block-size=1`) reports the **apparent** file size (same as `stat -c%s`), not the actual on-disk blocks. The sparse check always failed because apparent == actual.

**Fix:** Changed to `du` (without `-b`) which reports actual disk blocks in KB, then multiplied by 1024 to get bytes. Now correctly shows apparent=2048MB vs actual=4MB.

##### Issue 19: B2 CLI v4 requires `b2://` URI for `b2 ls`

**Problem:** `b2 ls --recursive '$BUCKET_NAME'` failed with `error: argument B2_URI: Invalid B2 URI`. The B2 CLI v4 changed `b2 ls` to require a `b2://` prefixed URI.

**Fix:** Changed all `b2 ls` calls from `b2 ls --recursive '$BUCKET_NAME'` to `b2 ls --recursive 'b2://$BUCKET_NAME'`. Note: `b2 file upload` and `b2 file download` do NOT need this prefix — they take the bucket name as a positional argument. Only `b2 ls` and `b2 rm` use the URI format.

##### Issue 20: DRBD needs block devices, not regular files

**Problem:** The master-prompt3.md spec said to use `disk /data/images/alice.img` directly in the DRBD resource config. This failed: `'/data/images/alice.img' is not a block device!`. DRBD requires block devices for its backing storage.

**Root cause:** The master-prompt3.md was generated from a session that didn't have full context of the `poc-drbd` implementation details. In `poc-drbd`, loop devices are always used (`losetup --find --show`), and the DRBD config references the loop device (`/dev/loop0`), never the raw file.

**Fix:** Added `losetup` calls in Phase 10 to create loop devices on both machines before writing the DRBD config. The config now uses `$DRBD_LOOP1` / `$DRBD_LOOP2` paths.

##### Issue 21: `drbdadm create-md` fails on image with existing Btrfs data

**Problem:** On machine-2 (which has restored Btrfs data from the cold restore), `drbdadm create-md` detected existing data and prompted for confirmation. The `yes yes |` pipe wasn't sufficient because the interactive prompt behaves differently over SSH.

**Fix:** Added `--force` flag: `yes yes | drbdadm create-md --force alice`. In `poc-drbd`, both images were created fresh/empty so this never occurred.

##### Issue 22: DRBD internal metadata overwrites Btrfs superblock

**Problem:** With `meta-disk internal`, DRBD writes its metadata at the **end** of the backing device. Since the Btrfs filesystem was created using the full 2GB loop device, DRBD's `create-md --force` overwrites the last ~128KB of the Btrfs filesystem. After DRBD sync and mount, the Btrfs superblock was corrupted: `mount /dev/drbd0: wrong fs type, bad option, bad superblock on /dev/drbd0`.

**Root cause:** In `poc-drbd`, DRBD is set up on a blank device BEFORE `mkfs.btrfs` runs. So DRBD reserves the end-of-disk space for metadata first, and Btrfs is created on the (slightly smaller) `/dev/drbd0` device. In this PoC, the flow is reversed: Btrfs data exists FIRST (from cold restore), and DRBD is layered on top afterward. The internal metadata writes into space that Btrfs already uses.

**Original fix (v3.0):** Switched from `meta-disk internal` to **external metadata devices**. Each machine creates a separate 128MB image file (`/data/images/alice-drbd-meta.img`), loop-mounts it, and uses it as `meta-disk /dev/loop1` in the DRBD config. This keeps DRBD metadata completely separate from the Btrfs data.

**Proper fix (Patch 3.1):** Reorder the cold restore flow so DRBD is set up on blank devices FIRST (with `meta-disk internal`), then format, then receive. See Patch 3.1 below.

#### B2 Bucket Structure

```
b2://poc-backblaze-{random}/users/alice/
  ├── layer-000.btrfs.zst              (full send of base snapshot, ~1.2KB)
  ├── layer-001.btrfs.zst              (incremental from layer-000, ~838B)
  ├── auto-backup-latest.btrfs.zst     (incremental from layer-001)
  └── manifest.json                     (snapshot chain metadata)
```

#### Iterations (v3.0)

5 attempts to get all 64 checks passing:

| Attempt | Result | Issue |
|---------|--------|-------|
| 1 | 3 FAIL, exit at Phase 6 | `du -b` sparse check wrong; `b2 ls` missing `b2://` prefix; `set -e` exit |
| 2 | 55 PASS, exit at Phase 10 | Fixed sparse + b2 ls; DRBD config used raw file path instead of loop device |
| 3 | 55 PASS, exit at Phase 10 | Fixed loop devices; `create-md` failed on data-bearing image (no --force) |
| 4 | 57 PASS, exit at Phase 10 | Fixed --force; DRBD internal metadata overwrote Btrfs superblock |
| 5 | **64 PASS** | Fixed: external DRBD metadata device |

#### What Was Proven

1. **Full backup to B2** — `btrfs send → zstd compress → B2 file upload` of initial snapshot
2. **Incremental backups** — delta sends using parent snapshot; incremental smaller than full (838 < 1232 bytes)
3. **Manifest-tracked snapshot chain** — JSON manifest tracks chain ordering, parent relationships, file sizes
4. **Total data loss survival** — all data on machine-1 destroyed; only B2 copy remains
5. **Cold restore** — `B2 download → zstd decompress → btrfs receive` of full chain (3 layers applied in order)
6. **Complete data integrity** — ALL data from ALL phases survives: config.json (version 1.2), MEMORY.md (2 sessions), email inbox, budget ledger (2 transactions), budget alerts
7. **Historical snapshot access** — layer-000 snapshot accessible with original data (config version 1.0)
8. **Containers run on restored data** — Docker container starts, reads all restored files correctly
9. **DRBD bipod from restored data** — machine-2 (with restored data) becomes primary, machine-1 (empty) becomes secondary, full sync completes
10. **Btrfs on DRBD works post-restore** — new snapshots can be created on the DRBD-backed filesystem
11. **Chain ordering enforced** — out-of-order `btrfs receive` correctly fails without parent snapshot

#### How to Run

```bash
export HCLOUD_TOKEN="your-hetzner-api-token"
export B2_KEY_ID="your-backblaze-key-id"
export B2_APP_KEY="your-backblaze-application-key"

cd poc-backblaze
./run.sh
```

---

### Patch 3.1: Fix Cold Restore Ordering — DRBD Before Filesystem

#### Motivation

The original PoC 3 (v3.0) used a suboptimal flow for cold restore → bipod formation:

1. Phase 8: Restore Btrfs data onto a plain loop device (no DRBD)
2. Phase 9: Start containers on the raw loop device
3. Phase 10: Retrofit DRBD on top of existing data — requiring external metadata devices

This created two problems:
- **External metadata complexity** — 128MB `alice-drbd-meta.img` files, extra loop devices, `--force` on `create-md`
- **Different code path** — cold restore used a different block device stack than normal provisioning (external metadata vs. internal)

#### The Fix

Reorder so DRBD is set up on **blank devices FIRST** (with `meta-disk internal`, same as PoC 2), then format Btrfs on `/dev/drbd0`, then `btrfs receive` the snapshots. All `btrfs receive` writes replicate to machine-1 via DRBD in real-time.

New flow:
```
sparse file → loop device → DRBD (meta-disk internal) → /dev/drbd0 → mkfs.btrfs → btrfs receive
```

No external metadata files. No `--force` on `create-md`. No special cases. One architecture for all paths.

#### Phase Restructuring

| Phase | Before (v3.0) | After (v3.1) |
|-------|---------------|--------------|
| 8 | Cold restore (Btrfs only, machine-2) | Cold restore WITH DRBD (both machines) |
| 9 | Start containers (raw loop device) | Verify DRBD sync complete |
| 10 | Form bipod (retrofit DRBD, external metadata) | Start containers (on /dev/drbd0) |
| 11 | Verify bipod + data | Verify bipod + data (unchanged) |
| 12 | Negative test | Negative test (unchanged) |

#### New Phase 8 Flow (Merged Cold Restore + DRBD)

1. Create blank 2G images on **both** machines
2. Loop devices on both
3. DRBD config with `meta-disk internal` (same as poc-drbd)
4. `drbdadm create-md` — no `--force` needed (blank images)
5. `drbdadm up` on both, promote machine-2
6. `mkfs.btrfs -f /dev/drbd0`
7. Mount, download manifest, `btrfs receive` all 3 snapshots
8. Create workspace, verify data integrity
9. All writes replicate to machine-1 via DRBD Protocol A in real-time

#### Removed

- `alice-drbd-meta.img` files (128MB external metadata images)
- Extra loop devices for metadata
- `--force` flag on `drbdadm create-md`
- Unmount/re-mount dance between old Phases 9→10

#### Issue 23: DRBD status `peer-role` vs `role` format

**Problem:** Phase 9's DRBD role verification checked for `peer-role:Secondary` in the `drbdadm status` output. The actual output format shows the peer's role as `poc-b2-machine-1 role:Secondary` (under the peer name), not as `peer-role:Secondary`.

**Fix:** Changed grep pattern from `peer-role:Secondary` to `poc-b2-machine-1 role:Secondary`.

#### Iterations

2 attempts:

| Attempt | Result | Issue |
|---------|--------|-------|
| 1 | 64/65 PASS, 1 FAIL | `peer-role:Secondary` grep pattern wrong (Issue 23) |
| 2 | **65/65 PASS** | Fixed grep pattern |

#### Result — 65/65 Checks Passed

```
═══ Phase 0: Prerequisites ═══                     — 10 checks (DRBD, Docker, tools, B2)
═══ Phase 1: Create User World on Machine-1 ═══    — 5 checks
═══ Phase 2: Full Backup — layer-000 to B2 ═══     — 4 checks
═══ Phase 3: Simulate Agent Work + Create layer-001 — 3 checks
═══ Phase 4: Incremental Backup — layer-001 to B2  — 4 checks
═══ Phase 5: More Agent Work + Auto-Backup ═══     — 4 checks
═══ Phase 6: Verify B2 Bucket Contents ═══         — 5 checks
═══ Phase 7: Simulate Total Loss — Destroy Data ═══ — 3 checks
═══ Phase 8: Cold Restore with DRBD ═══            — 15 checks (DRBD setup + restore + integrity)
═══ Phase 9: Verify DRBD Sync ═══                  — 3 checks (sync, Primary, Secondary)
═══ Phase 10: Start Containers on Machine-2 ═══    — 3 checks
═══ Phase 11: Verify Bipod + Data Integrity ═══    — 4 checks
═══ Phase 12: Negative Test — Chain Ordering ═══   — 2 checks

  Passed: 65
  Failed: 0
  ALL CHECKS PASSED
```

#### Key Outcome

The cold restore path now uses the **identical block device stack** as normal provisioning:

```
Normal provisioning (poc-drbd):
  sparse → loop → DRBD (internal) → /dev/drbd0 → mkfs.btrfs → subvolumes

Cold restore (poc-backblaze v3.1):
  sparse → loop → DRBD (internal) → /dev/drbd0 → mkfs.btrfs → btrfs receive → subvolumes
```

One architecture. No special cases. Issue 22 (external metadata workaround) is no longer needed.

#### Resource Teardown Verified

After each run:
- `poc-b2-machine-1` — deleted
- `poc-b2-machine-2` — deleted
- `poc-backblaze-net` — deleted
- `b2-poc-key` — deleted
- B2 bucket — emptied and deleted
- Pre-existing `prethora-ttyd-bd7508a6` — untouched

---

### Layer 4.1: Machine Agent PoC (`poc-coordinator/`)

#### Goal

Build a Go HTTP server (machine agent) that wraps the proven block device stack behind idempotent API endpoints. First Go code in the project. Zero external dependencies — standard library only. A bash test harness on macOS drives the agent through the full lifecycle via HTTP calls, playing the role that a coordinator will play in Layer 4.2.

#### Architecture

```
macOS (test harness — bash scripts)
  │
  ├── HTTP → machine-1 (Hetzner CX23, public IP, port 8080) — Go machine agent
  └── HTTP → machine-2 (Hetzner CX23, public IP, port 8080) — Go machine agent

Each machine runs:
  machine-agent binary on :8080
  └── 13 HTTP endpoints wrapping: losetup, drbdadm, mkfs.btrfs, btrfs subvolume, docker

Hetzner private network (10.0.0.0/24):
  machine-1 ←──DRBD──→ machine-2 (per-user ports, 7900+)
```

#### File Structure

```
poc-coordinator/
├── go.mod                              (module: poc-coordinator, Go 1.22, zero deps)
├── Makefile                            (build/deploy/test/clean)
├── cmd/
│   └── machine-agent/
│       └── main.go                     # Entry point, env config, startup
├── internal/
│   └── machineagent/
│       ├── server.go                   # HTTP server, 13 routes, JSON helpers, system info
│       ├── images.go                   # Sparse image create, loop device attach, idempotent
│       ├── drbd.go                     # DRBD lifecycle + status parser (multi-format)
│       ├── btrfs.go                    # Format DRBD device, subvolume + snapshot creation
│       ├── containers.go              # Docker device-mount pattern start/stop/status
│       ├── state.go                    # In-memory state map, system discovery on startup
│       ├── cleanup.go                  # Per-user + full machine teardown
│       └── exec.go                     # Command execution with stdout/stderr capture
├── container/
│   ├── Dockerfile                      # Alpine + btrfs-progs + appuser
│   └── container-init.sh              # Mount subvol → drop to appuser → exec workload
├── scripts/
│   ├── run.sh                          # Full lifecycle: infra → deploy → test → teardown
│   ├── common.sh                       # IP discovery, SSH/API helpers, check framework
│   ├── infra.sh                        # Hetzner CX23 creation/deletion + private network
│   ├── deploy.sh                       # Cross-compile + SCP + hostname + container build
│   ├── test_suite.sh                  # 9 phases, 66 checks
│   └── cloud-init/
│       └── fleet.yaml                  # DRBD 9 + Docker + btrfs-progs + systemd unit
├── bin/
│   └── machine-agent                   # Cross-compiled Linux/amd64 binary
└── BUILD_PROMPT.md                     # Build prompt for Layer 4.1
```

#### API Endpoints (13 routes)

| Method | Path | Purpose |
|--------|------|---------|
| GET | /status | Machine health + per-user resource state |
| POST | /images/{user_id}/create | Sparse image + loop device |
| DELETE | /images/{user_id} | Full user teardown (reverse order) |
| POST | /images/{user_id}/drbd/create | Write config + create-md + up |
| POST | /images/{user_id}/drbd/promote | Promote to Primary (--force) |
| POST | /images/{user_id}/drbd/demote | Demote to Secondary |
| GET | /images/{user_id}/drbd/status | Parse DRBD status (multi-format) |
| DELETE | /images/{user_id}/drbd | Down + remove config |
| POST | /images/{user_id}/format-btrfs | mkfs.btrfs + workspace subvol + snapshot |
| POST | /containers/{user_id}/start | Device-mount container start |
| POST | /containers/{user_id}/stop | Container stop + rm |
| GET | /containers/{user_id}/status | Container running/exists |
| POST | /cleanup | Full machine cleanup |

#### Test Results — 66/66 Checks Passed

```
Phase 0: Prerequisites                          [8/8]
Phase 1: Single User Provisioning — Full Stack  [10/10]
Phase 2: Device-Mount Verification              [5/5]
Phase 3: Data Write + DRBD Replication          [4/4]
Phase 4: Failover via API                       [8/8]
Phase 5: Idempotency Tests                      [8/8]
Phase 6: Full Teardown                          [8/8]
Phase 7: Multi-User Density (3 users)           [9/9]
Phase 8: Status Endpoint Accuracy               [6/6]
═══════════════════════════════════════════════════
ALL PHASES COMPLETE: 66/66 checks passed
═══════════════════════════════════════════════════
```

#### Issues Encountered & Fixes

##### Issue 24: SSH key path (macOS)

**Problem:** `infra.sh` referenced `~/.ssh/id_ed25519.pub` which didn't exist.
**Fix:** Changed to `~/.ssh/id_rsa.pub` which is the available key.

##### Issue 25: DRBD status parser — extra tokens on status lines

**Problem:** DRBD 9 status output includes extra key:value pairs on the same line:
```
disk:UpToDate open:no
```
The initial parser treated the entire line as the disk state value, producing `"disk_state":"UpToDate open:no"`.

**Fix:** Rewrote parser to be token-based — split each line into space-separated tokens and extract known `key:value` prefixes individually. Now correctly yields `"disk_state":"UpToDate"`.

##### Issue 26: DRBD sync requires Primary first

**Problem:** Test harness waited for DRBD sync (UpToDate/UpToDate) BEFORE promoting either side. On a fresh DRBD resource, both sides start as Secondary/Inconsistent — sync never starts without a Primary.

**Fix:** Moved the promote call before the sync wait loop. After `drbdadm primary --force`, initial full sync begins and the wait loop succeeds.

##### Issue 27: AppArmor blocks mount inside containers

**Problem:** On real Hetzner Ubuntu 24.04 machines, `mount` inside the container fails with "Permission denied" even with `--cap-add SYS_ADMIN`. AppArmor's default Docker profile restricts mount syscalls. In PoC 1 (DinD), AppArmor was not active inside the privileged outer container, so this wasn't seen.

**Fix:** Added `--security-opt apparmor=unconfined` to the `docker run` command. The container already drops all caps except SYS_ADMIN (for mount) and drops to unprivileged user after init, so AppArmor confinement is redundant here.

**Impact:** New required flag for production container pattern.

##### Issue 28: macOS bash 3.2 doesn't support `declare -A`

**Problem:** macOS ships bash 3.2 which doesn't support associative arrays (`declare -A`). Phase 7's multi-user test used associative arrays to track loop devices per user.

**Fix:** Replaced associative arrays with simple variables.

##### Issue 29: `docker exec` runs as root, not PID 1's user

**Problem:** The "running as appuser" check used `docker exec alice-agent id -un` which returns `root` because `docker exec` defaults to root user, not the container's PID 1 user.

**Fix:** Changed check to use `docker top` which shows the host-visible process list. On the host, the user shows as UID `1000` (appuser's UID) rather than `root`, confirming the workload runs unprivileged.

##### Issue 30: Cleanup must handle DRBD holding loop devices

**Problem:** Cleanup failed to release loop devices because DRBD was still using them as backing devices. `losetup -d` fails when the loop device has holders (DRBD). The cleanup function in Go removed the DRBD config file before calling `drbdadm down`, which then couldn't find the resource.

**Fix:** Fixed ordering: `drbdadm down` BEFORE removing config; full dependency chain: stop container → unmount → DRBD down → remove config → detach loop → delete image.

#### Key Learnings from Layer 4.1

1. **Idempotent API Design Works** — Every endpoint was safe to call multiple times — the test harness proved this across images, DRBD, Btrfs format, containers, and cleanup. Critical for crash recovery in later layers.

2. **Device-Mount Pattern Works on Real Servers** — The production container isolation pattern (block device via `--device`, mount inside container, drop to unprivileged user) works correctly on real Hetzner servers with DRBD devices, not just loop devices in DinD.

3. **Failover via API Is Clean** — Stop container → demote DRBD → promote other side → start container. No host mount needed. Data survives. The device-mount pattern eliminates the need for host-side Btrfs mounts during normal operation.

4. **Multi-User Density Confirmed** — 3 users on the same machine pair with separate DRBD resources, ports, minors — no resource collisions. Stopping one user's container doesn't affect others.

#### Resource Teardown Verified

After the run:
- `poc41-machine-1` — deleted
- `poc41-machine-2` — deleted
- `poc41-net` — deleted
- `poc41` SSH key — deleted
- Pre-existing servers — untouched

---

### Layer 4.2: Coordinator Happy Path (`poc-coordinator/`)

#### Goal

Build a Go coordinator HTTP service that manages a fleet of machine agents and orchestrates user provisioning. The coordinator selects machines, drives the full provisioning state machine (image creation → DRBD → promote → sync → Btrfs format → container start), and tracks all state in memory. This is the "happy path" layer: provisioning works, fleet is healthy, no failures.

#### Architecture

```
macOS (test harness — bash scripts)
  │
  ├── HTTP → coordinator (Hetzner CX23, public IP, :8080) — Go coordinator binary
  │            └── Private IP: 10.0.0.2
  │
  ├── (coordinator calls) → fleet-1 (CX23, machine-agent :8080, private 10.0.0.11)
  ├── (coordinator calls) → fleet-2 (CX23, machine-agent :8080, private 10.0.0.12)
  └── (coordinator calls) → fleet-3 (CX23, machine-agent :8080, private 10.0.0.13)

Private network: 10.0.0.0/24
  - Coordinator ↔ fleet machines (HTTP API calls)
  - Fleet ↔ fleet (DRBD replication per user)
```

4 Hetzner Cloud CX23 servers at `nbg1`, all on the same private network.

#### New Code Written

**Coordinator (6 new files):**
- `cmd/coordinator/main.go` — Entry point, env config, startup
- `internal/coordinator/server.go` — HTTP routing (8 endpoints: fleet register/heartbeat/status, user CRUD, provision, bipod)
- `internal/coordinator/store.go` — In-memory state store (`sync.RWMutex`), placement algorithm, port/minor allocation, JSON persistence
- `internal/coordinator/provisioner.go` — Full provisioning state machine (8 steps, retry-once on failure)
- `internal/coordinator/fleet.go` — Fleet management delegation
- `internal/coordinator/machineapi.go` — HTTP client wrapping all 13 machine agent endpoints

**Machine Agent Additions (2 modified + 1 new):**
- `internal/machineagent/heartbeat.go` — Registration (retry until success) + 10-second heartbeat goroutine
- `cmd/machine-agent/main.go` — Added `COORDINATOR_URL`, `NODE_ADDRESS` env vars, heartbeat startup
- `internal/shared/types.go` — Added 10 new types for fleet and coordinator APIs

**Infrastructure (schema + scripts):**
- `schema.sql` — Postgres schema documentation (not used by code)
- `Makefile` — Added coordinator build target
- `scripts/layer-4.2/` — 7 files: run.sh, common.sh, infra.sh, deploy.sh, test_suite.sh, cloud-init/coordinator.yaml, cloud-init/fleet.yaml

#### Provisioning State Machine

```
REGISTERED → IMAGES_CREATED → DRBD_CONFIGURED → PRIMARY_PROMOTED → DRBD_SYNCED → BTRFS_FORMATTED → CONTAINER_STARTED → RUNNING
```

Key design: promote BEFORE sync wait (Issue #26 from Layer 4.1). Each step retries once on failure with 2-second backoff. DRBD sync polling every 2 seconds with 120-second timeout.

#### Placement Algorithm

1. Get all machines with status = "active"
2. Filter: disk_used < disk_total × 0.85
3. Sort by active_agents ascending
4. Pick top 2 (least loaded)
5. Increment active_agents under write lock to prevent double-placement

#### Test Results — 55/55 Checks Passed (First Attempt)

```
═══ Phase 0: Prerequisites ═══                    [11/11]
  ✓ Coordinator responding
  ✓ 3 fleet machines registered
  ✓ Machine agents responding (×3)
  ✓ DRBD module loaded (×3)
  ✓ Container image built (×3)

═══ Phase 1: Provision First User (alice) ═══     [9/9]
  ✓ Create, provision, reaches running
  ✓ Status, primary machine, DRBD port, bipod entries verified
  ✓ Container running on primary
  ✓ Data accessible in container

═══ Phase 2: Provision Second User (bob) ═══      [6/6]
  ✓ Create, provision, running
  ✓ Placed on fleet-2 (balanced)

═══ Phase 3: Provision Third User (charlie) ═══   [3/3]
  ✓ Running, placed on fleet-3

═══ Phase 4: Provision dave and eve ═══           [5/5]
  ✓ Both running, 5 total users

═══ Phase 5: Fleet Status Verification ═══        [10/10]
  ✓ 3 machines in fleet
  ✓ Balanced: no machine has >4 agents
  ✓ Machine status consistent (×3)
  ✓ All 5 users accessible via coordinator

═══ Phase 6: Data Isolation ═══                   [5/5]
  ✓ Write unique data to alice and bob
  ✓ Each reads only their own data
  ✓ DRBD replication healthy (UpToDate)

═══ Phase 7: Cleanup ═══                          [6/6]
  ✓ All 3 machines cleaned
  ✓ All 3 machines verified clean (0 users)

═══════════════════════════════════════════════════
 ALL PHASES COMPLETE: 55/55 checks passed
═══════════════════════════════════════════════════
```

#### Issues Encountered

None. All 55 checks passed on the first attempt. No code fixes were needed.

This is notable because:
- Layer 4.2 builds directly on the patterns and learnings from Layers 1-4.1
- The master-prompt4.2.md was comprehensive and incorporated all critical learnings (promote-before-sync, device-mount pattern, private IP addressing, DRBD-first ordering)
- The in-memory state store avoided any external dependency complexity

#### What Was Proven

1. **Coordinator → machine agent orchestration** — HTTP-based provisioning state machine drives the full block device stack across separate servers
2. **Multi-user provisioning** — 5 users provisioned sequentially, each with their own DRBD resource, port, and container
3. **Balanced placement** — Least-loaded algorithm distributes users across fleet: alice→fleet-1, bob→fleet-2, charlie→fleet-3, dave and eve spread across remaining capacity
4. **Fleet registration and heartbeats** — Machine agents self-register and send heartbeats; coordinator tracks fleet state
5. **Data isolation across users** — Each user's container sees only their own data; verified with unique writes and cross-reads
6. **DRBD replication active for all users** — All user DRBD resources report UpToDate peer disk state
7. **Clean teardown** — Machine agent cleanup endpoint releases all resources in correct dependency order

#### Resource Teardown Verified

After the run:
- `l42-coordinator` — deleted
- `l42-fleet-1` — deleted
- `l42-fleet-2` — deleted
- `l42-fleet-3` — deleted
- `l42-net` — deleted
- `l42-key` SSH key — deleted

#### Drift Analysis

No issues were encountered, so no drift analysis is needed. All established patterns were respected:
- Promote before sync (Issue #26)
- Device-mount container pattern (no host mounts at runtime)
- DRBD-first ordering (blank images → DRBD → format)
- AppArmor unconfined (already baked into machine agent)
- Token-based DRBD parsing (coordinator reads JSON responses from machine agent)
- Private IPs for coordinator↔machine communication
- Proper teardown dependency chain

#### Changes That Affect Future Layers

1. **Layer 4.3 (failure detection)** — The coordinator now has fleet heartbeats. Layer 4.3 needs to add: heartbeat timeout detection, automatic DRBD promotion on secondary, container restart on new primary.

2. **Layer 4.4 (bipod reformation)** — The coordinator tracks bipod state. Layer 4.4 needs to: detect single-copy state after failover, select a new secondary machine, test `drbdadm disconnect`/`adjust` for live peer replacement.

3. **Layer 4.6 (crash recovery)** — The in-memory store persists to `state.json` but has no crash recovery logic. Layer 4.6 will migrate to Supabase (hosted Postgres), add an `operations` table for crash-safe multi-step tracking, implement five-phase startup reconciliation, deterministic fault injection at 25-30 checkpoints, a ground-truth consistency checker, and chaos mode testing. See the detailed Layer 4.6 Preview section for the full extracted design from the original monolithic build prompt.

### Layer 4.3: Heartbeat Failure Detection & Automatic Failover

#### Goal

Add automatic failure detection and failover to the coordinator. When a fleet machine stops sending heartbeats, the coordinator detects it (active → suspect at 30s → dead at 60s) and automatically fails over affected users: promoting DRBD on the surviving secondary and starting containers on the new primary. Users whose secondary dies are marked degraded. Data integrity is preserved across failover.

#### Architecture

Same topology as Layer 4.2: 1 coordinator + 3 fleet machines on Hetzner Cloud (l43-* prefix). The test kills fleet-1 via `hcloud server shutdown`, waits for automatic detection and failover, then verifies data survived.

#### New Code Written

**Coordinator modifications (4 files modified):**
- `internal/coordinator/store.go` — Added `StatusChangedAt` to Machine, `FailoverEvent` struct, `failoverEvents` to Store, 6 new methods (`CheckMachineHealth`, `GetUsersOnMachine`, `GetSurvivingBipod`, `SetBipodRole`, `RecordFailoverEvent`, `GetFailoverEvents`), resurrection handling in `UpdateHeartbeat`, updated `persistState`/`persist()`
- `internal/coordinator/server.go` — Added `GET /api/failovers` route + handler, `GetStore()` accessor
- `internal/shared/types.go` — Added `FailoverEventResponse` type
- `cmd/coordinator/main.go` — Added `StartHealthChecker` call

**New files (1 Go + 7 scripts):**
- `internal/coordinator/healthcheck.go` — Health checker goroutine (10s tick, 30s suspect, 60s dead), `failoverMachine`, `failoverUser` with full promote + container start sequence
- `scripts/layer-4.3/` — run.sh, common.sh, infra.sh, deploy.sh, test_suite.sh, cloud-init/coordinator.yaml, cloud-init/fleet.yaml

#### Health Check State Machine

```
Machine: active ──(30s)──▶ suspect ──(60s)──▶ dead ──(heartbeat resumes)──▶ active
User:    running ──(primary dies)──▶ failing_over ──▶ running (on new primary)
         running ──(secondary dies)──▶ running_degraded
```

#### Failover Sequence (primary death)

1. Identify surviving bipod (the secondary on another machine)
2. Set user status → `failing_over`
3. Mark dead machine's bipod role → `stale`
4. POST `/images/{user_id}/drbd/promote` on surviving machine (uses `--force`, works with disconnected peer)
5. POST `/containers/{user_id}/start` on surviving machine
6. Update bipod roles and user's primary machine
7. Set user status → `running`
8. Record failover event

#### Test Results — 62/62 Checks Passed

```
═══ Phase 0: Prerequisites ═══                                [13/13]
  ✓ Coordinator responding, 3 fleet machines registered
  ✓ All machines active, DRBD loaded, container images built
  ✓ No failover events initially

═══ Phase 1: Provision Users (baseline) ═══                   [12/12]
  ✓ alice, bob, charlie provisioned and running
  ✓ Test data written to each user's container
  ✓ DRBD replication healthy (UpToDate) for all users

═══ Phase 2: Kill a Fleet Machine ═══                         [2/2]
  ✓ fleet-1 shutdown via hcloud
  ✓ fleet-1 unreachable via SSH

═══ Phase 3: Failure Detection ═══                            [3/3]
  ✓ fleet-1 detected as dead (within 90s)
  ✓ fleet-2 and fleet-3 still active

═══ Phase 4: Automatic Failover Verification ═══              [8/8]
  ✓ alice (primary on fleet-1) → running on fleet-2
  ✓ bob (secondary on fleet-1) → running_degraded
  ✓ charlie (no bipod on fleet-1) → running
  ✓ alice DRBD promoted and container running on new primary
  ✓ Failover events recorded with correct structure

═══ Phase 5: Data Integrity After Failover ═══                [9/9]
  ✓ Pre-failover data survived for all 3 users
  ✓ New data writable after failover
  ✓ config.json from initial provisioning intact

═══ Phase 6: Unaffected Users & Degraded State ═══            [5/5]
  ✓ bob marked running_degraded (secondary lost)
  ✓ bob primary unchanged, container still running
  ✓ Stale bipods correctly marked on fleet-1

═══ Phase 7: Coordinator State Consistency ═══                [6/6]
  ✓ Fleet shows fleet-1 dead, others active
  ✓ No user has fleet-1 as primary
  ✓ All users in valid state
  ✓ state.json persisted correctly

═══ Phase 8: Cleanup ═══                                      [4/4]
  ✓ Surviving machines cleaned and verified

═══════════════════════════════════════════════════
 ALL PHASES COMPLETE: 62/62 checks passed
═══════════════════════════════════════════════════
```

#### Issues Encountered

**Issue #31: Test script checked bipod on fleet-1 for users without one.**
The original test checked all 3 users for a 'stale' bipod on fleet-1, but placement is algorithmic — charlie's bipod was on fleet-2 and fleet-3, not fleet-1. Fixed by checking whether a user actually has a bipod on fleet-1 before asserting its role.

#### What Was Proven

1. **Automatic failure detection** — Coordinator's health checker goroutine detects machine death within 60 seconds via heartbeat timeout
2. **Automatic primary failover** — DRBD promote + container start on surviving secondary, user transitions from `running` → `failing_over` → `running` on new primary
3. **Secondary death handling** — Users whose secondary dies are correctly marked `running_degraded` with no disruption to the running primary
4. **Data integrity across failover** — All data written before the failure (test.txt, config.json) survives on the new primary. Protocol A async replication means the surviving copy was UpToDate before the failure.
5. **Failover event recording** — Each failover event captured with user, machines, type, success, duration
6. **Non-blocking failover** — Each dead machine's failover runs in its own goroutine, not blocking the health checker
7. **Idempotent promotion** — DRBD `--force` promote works on a secondary with a disconnected peer

#### Drift from Build Prompt

- **Phase 6 bipod check** — The prompt's test script checked all users for stale bipods on fleet-1. Fixed to only check users that actually have bipods on fleet-1 (Issue #31). This is a test improvement, not a code change.
- **No other drift** — All Go code, API endpoints, failover sequence, and state transitions match the build prompt exactly.

#### Changes That Affect Future Layers

1. **Layer 4.4 (bipod reformation)** — After failover, users are running on a single machine (no replication). Layer 4.4 must: detect single-copy state, select a new secondary, set up DRBD to the new peer, sync, and transition back to fully replicated.

2. **Layer 4.4 (dead machine return)** — When a dead machine's heartbeats resume, the coordinator marks it `active` but does NOT re-integrate bipods. Layer 4.4 must handle: cleaning up stale DRBD resources on the returned machine, potentially using it as a new secondary for users that need bipod reformation.

---

## 7. Technology Decisions Log

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Replication copies | 2 (bipod) + Backblaze | 3 copies unnecessary for always-on; Backblaze is third tier |
| DRBD protocol | Protocol A (async) | Near-local write speed; ~0-3s data loss on catastrophic failure acceptable |
| DRBD version | 9.3.0 (LINBIT PPA) | Ubuntu 24.04 ships DRBD 8.4 in-tree; drbd-utils 9.22 requires 9.x kernel module |
| DRBD metadata | `meta-disk internal`, always | DRBD set up before filesystem in all paths (provisioning + cold restore) |
| User filesystem | Btrfs inside sparse image file | COW snapshots, instant rollback, portable, self-contained |
| Host filesystem | XFS | Best for large files and concurrent I/O |
| Container isolation | Device mount + init script | No metadata leakage; production pattern |
| Container AppArmor | `--security-opt apparmor=unconfined` | Required on real Ubuntu 24.04 for mount inside container; discovered in Layer 4.1 |
| Capability model | SYS_ADMIN + SETUID + SETGID during init, zero at runtime | Minimum viable for mount + user switch |
| DRBD testing | Separate servers (not Docker containers) | DRBD requires separate kernels; shared-kernel approach is fundamentally impossible |
| PoC infrastructure | Hetzner Cloud CX23 + hcloud CLI | Cheap (~€0.012/hr), automated lifecycle, production-realistic topology |
| DinD storage driver | vfs (Docker Desktop), overlay2 (real Linux) | overlay2 doesn't work in nested Docker on Docker Desktop |
| Coordinator HA | Start single, design for active-passive | All state in Postgres; advisory lock for leader election |
| Pricing model | Usage-based metering | Fits bursty individual usage; agent negotiates resources |
| Coordinator state | In-memory + JSON persist | No external deps for happy path; Supabase (Postgres) when crash recovery matters (Layer 4.6) |
| Coordinator database | Supabase (hosted Postgres) | Managed Postgres avoids self-hosting burden; connection string via env var for tests |
| Coordinator placement | Least-loaded, mutex-protected | Prevents double-placement from concurrent provisioning |
| Fleet communication | Private IP HTTP | Coordinator uses 10.0.0.x addresses; test harness uses public IPs |
| Health check interval | 10s tick, 30s suspect, 60s dead | 3 missed heartbeats → suspect, 6 → dead; balances detection speed vs false positives |
| Failover concurrency | Per-machine goroutine | Each dead machine's failover runs independently; doesn't block health checker |
| Open source | Yes, entire stack | WordPress.com/org model; trust through transparency |
| Encryption | LUKS above DRBD (planned) | Protects data at rest; per-user keys in coordinator |
| Backup compression | zstd | Fast, good ratio, streaming-compatible |
| Backup transport | Temp file + B2 CLI upload | Reliable for PoC; production should use streaming for large deltas |
| Backup chain tracking | manifest.json in B2 bucket | Tracks chain ordering, parent relationships, file sizes |
| DRBD minor allocation | Per-machine from 0 (separate kernels) | No partitioned ranges needed; each kernel has its own minor namespace |

---

## 8. Key Files Produced

| File | Description |
|------|-------------|
| `architecture-v3.md` | Full architecture document for the always-on bipod model |
| `poc-btrfs/` | Working PoC: Btrfs world isolation with device-mount pattern |
| `poc-btrfs/BUILD_PROMPT.md` | Build prompt for PoC 1 |
| `poc-btrfs/UPDATE_PROMPT.md` | Patch 1: device mount isolation update |
| `poc-drbd/` | Working PoC: DRBD bipod replication on Hetzner Cloud |
| `poc-drbd/run.sh` | Full orchestration: infra up → provision → demo → teardown |
| `poc-drbd/infra.sh` | Hetzner infrastructure lifecycle (up/down/status) |
| `poc-drbd/cloud-init.yaml` | Server provisioning (DRBD 9, Docker, btrfs-progs) |
| `poc-drbd/scripts/demo.sh` | 11-phase DRBD bipod demo (47/47 checks) |
| `poc-drbd/scripts/container-init.sh` | Device-mount init script (reused from PoC 1) |
| `poc-drbd/scripts/app-container/Dockerfile` | Isolated world container image |
| `poc-backblaze/` | Working PoC: Backblaze B2 backup & cold restore |
| `poc-backblaze/run.sh` | Full orchestration: infra + bucket up → demo → teardown all |
| `poc-backblaze/infra.sh` | Hetzner lifecycle (up/down/status) |
| `poc-backblaze/cloud-init.yaml` | Server provisioning (DRBD 9, Docker, btrfs, zstd, b2 CLI) |
| `poc-backblaze/scripts/demo.sh` | 13-phase B2 backup & restore demo (65/65 checks) |
| `poc-backblaze/scripts/container-init.sh` | Device-mount init script (reused from PoC 1/2) |
| `poc-backblaze/scripts/app-container/Dockerfile` | Isolated world container image |
| `poc-coordinator/` | Layer 4.1: Machine agent Go HTTP server (66/66 checks) |
| `poc-coordinator/cmd/machine-agent/main.go` | Entry point, env config, startup |
| `poc-coordinator/internal/machineagent/` | 8 Go files: server, images, drbd, btrfs, containers, state, cleanup, exec |
| `poc-coordinator/container/` | Dockerfile + container-init.sh (device-mount pattern) |
| `poc-coordinator/scripts/` | run.sh, common.sh, infra.sh, deploy.sh, test_suite.sh, cloud-init/fleet.yaml |
| `poc-coordinator/BUILD_PROMPT.md` | Build prompt for Layer 4.1 |
| `poc-coordinator/cmd/coordinator/main.go` | Layer 4.2: Coordinator entry point |
| `poc-coordinator/internal/coordinator/` | 5 Go files: server, store, provisioner, fleet, machineapi |
| `poc-coordinator/internal/machineagent/heartbeat.go` | Registration + heartbeat goroutine |
| `poc-coordinator/schema.sql` | Postgres schema documentation (future migration) |
| `poc-coordinator/scripts/layer-4.2/` | run.sh, common.sh, infra.sh, deploy.sh, test_suite.sh, cloud-init/ |
| `master-prompt4.2.md` | Build prompt for Layer 4.2 |
| `poc-coordinator/internal/coordinator/healthcheck.go` | Layer 4.3: Health checker + failover logic |
| `poc-coordinator/scripts/layer-4.3/` | run.sh, common.sh, infra.sh, deploy.sh, test_suite.sh, cloud-init/ |
| `master-prompt4.3.md` | Build prompt for Layer 4.3 |

---

## 9. PoC Progression Plan

```
✅ Layer 1: Btrfs world isolation (poc-btrfs)
     Sparse images, subvolumes, containers, snapshots, rollback, isolation

✅ Layer 1 Patch: Device mount isolation
     No metadata leakage, production container pattern

✅ Layer 2: DRBD bipod replication (poc-drbd)
     Two real servers, Protocol A async replication, failover, data survival
     47/47 checks passed

✅ Layer 3: Backblaze B2 backup & restore (poc-backblaze)
     btrfs send/receive, incremental backups, cold restore from B2
     Full chain: backup → total loss → cold restore → bipod formation
     65/65 checks passed (after Patch 3.1: DRBD-first ordering)

✅ Layer 4.1: Machine agent (poc-coordinator)
     Go HTTP server wrapping full block device stack
     Device-mount containers on real servers, idempotent API, multi-user density
     66/66 checks passed

✅ Layer 4.2: Coordinator happy path (poc-coordinator)
     Go coordinator with in-memory state, provisioning state machine
     5 users across 3 fleet machines, balanced placement, data isolation
     55/55 checks passed (first attempt, zero issues)

✅ Layer 4.3: Heartbeat failure detection + automatic failover
     Health checker (10s tick, 30s suspect, 60s dead)
     Automatic DRBD promote + container start on surviving secondary
     Data integrity verified across failover
     62/62 checks passed (1 test fix: Issue #31)

✅ Layer 4.4: Bipod reformation + dead machine return — 91/91 checks
     Rebuild second copy after failover, zero-downtime DRBD peer replacement
     Test drbdadm disconnect/adjust for live reconfiguration
     Clean up orphaned resources on returning machines

✅ Layer 4.5: Suspension, reactivation, deletion — 63/63 checks
     Full user lifecycle management

✅ Layer 4.6: Reconciliation + crash hardening — 67/67 checks
     Migrate coordinator state from in-memory/JSON to Supabase (Postgres)
     Operations table for crash-safe multi-step operation tracking
     Coordinator advisory lock (pg_try_advisory_lock) for singleton enforcement
     Five-phase startup reconciliation (discover → reconcile → resume → cleanup → start)
     33 deterministic fault injection checkpoints across all operations
     Consistency checker (12 invariants, SSH ground truth vs DB)
     Chaos mode testing (5% random crash probability)
     Graceful shutdown (context cancel → HTTP drain → DB close)
     See detailed Layer 4.6 Preview section below for original design intelligence

🔲 Layer 5.1: Tripod primitive + manual migration
     Add 3rd DRBD node to running 2-node resource (temporary tripod)
     Coordinator-driven migration orchestration with operation tracking + crash recovery
     Test: manual API trigger, data integrity, bipod shift verification

🔲 Layer 5.2: Rebalancer + machine drain
     Rebalancer goroutine (detect imbalance, trigger migrations automatically)
     Machine drain API (migrate all users off a machine for planned maintenance)
     Test: provoke imbalance, verify auto-rebalancing, test drain scenario

🔲 Layer 6+: Agent integration, Telegram gateway, credential proxy,
     marketplace, SDK, metering, dashboard...
```

**All data safety primitives are now proven:**
- PoC 1: Data survives locally (snapshots, rollback)
- PoC 2: Data survives machine failure (DRBD failover)
- PoC 3: Data survives total fleet loss (B2 cold restore)

**Orchestration automation has begun:**
- Layer 4.1: Block device stack fully automated via Go HTTP API

### Why Layer 4 Was Broken Into Sub-Layers

The original Layer 4 build prompt attempted to go from "shell scripts on two machines" to "crash-resilient distributed orchestrator with chaos testing" in one step. This violated the layered learning approach that made Layers 1-3 successful — each layer teaches things that inform the next.

The sub-layered approach caught three design issues in the original build prompt before they were baked in:

1. **Container bind mounts instead of device-mount pattern** — the original Layer 4 build prompt used `-v /mnt/users/{id}/workspace:/workspace:rw`, which is exactly the pattern PoC 1 Patch 1 proved was wrong. Caught during pre-build review, corrected in the Layer 4.1 build prompt. Device-mount pattern used from day one.

2. **"PoC shortcut" thinking for bipod reformation** — the original prompt said "production should use drbdadm adjust" but planned a full down/up cycle for the PoC. This defeats the purpose of building PoCs to learn production patterns. Layer 4.4 will test the real `disconnect`/`adjust` approach.

3. **Unnecessary DRBD minor partitioning** — the original prompt used partitioned minor ranges (fleet-1: 0-99, fleet-2: 100-199) which was a workaround for shared-kernel testing. Since we're on separate Hetzner machines with separate kernels, minors start from 0 on each machine independently.

---

## 10. Critical Learnings

### DRBD Cannot Be Simulated with Docker Containers

DRBD is a kernel module with global state. Two containers on the same host share ONE kernel and therefore ONE DRBD instance. Replication requires two separate DRBD instances on two separate kernels talking over TCP. This is a fundamental architectural constraint, not a configuration issue.

**Impact on the project:** The Docker Compose prototype layout (architecture-v3.md Section 16) assumed DinD containers could simulate fleet machines with DRBD. This doesn't work. Options for prototype/integration testing:
- Multiple Hetzner Cloud servers (used in PoC 2, PoC 3, and Layer 4.1 — works perfectly, cheap)
- Multiple Hetzner bare-metal servers (production target)
- VMs with separate kernels on a host that supports nested virtualization

Btrfs-only testing (snapshots, subvolumes, isolation) still works fine with Docker containers since Btrfs operates at the filesystem level, not the kernel module level.

### DRBD 9 on Ubuntu 24.04 Requires LINBIT PPA

Ubuntu 24.04 ships DRBD 8.4 in-tree but `drbd-utils` 9.22 in the package repos. These are incompatible. The LINBIT PPA provides the DRBD 9.3.0 kernel module via DKMS. When provisioning machines, must ensure `linux-headers-$(uname -r)` matches the running kernel before DKMS build.

### DRBD Must Always Be Set Up Before the Filesystem

When using `meta-disk internal`, DRBD reserves space at the end of the backing device for metadata. If a filesystem already occupies the full device, DRBD's `create-md` overwrites the filesystem's tail (~128KB), corrupting it. Discovered in PoC 3 when attempting to retrofit DRBD onto a Btrfs image restored from B2.

**The rule:** Always set up DRBD on a blank device first, then format the filesystem on `/dev/drbdN`. This applies to all paths — normal provisioning, cold restore, migration. There are no exceptions. The cold restore flow is: blank image → loop → DRBD → promote → `mkfs.btrfs /dev/drbd0` → mount → `btrfs receive`.

### DRBD Initial Sync Requires a Primary First

A fresh DRBD resource starts with both sides Secondary/Inconsistent. No sync occurs until one side is promoted to Primary with `drbdadm primary --force`. The correct orchestration sequence is:

```
configure DRBD → drbdadm up → drbdadm primary --force → [sync starts] → wait for UpToDate/UpToDate
```

NOT: configure → up → wait for sync → promote. Sync never begins without a Primary. Discovered in Layer 4.1 (Issue 26). **This ordering is critical for Layer 4.2's provisioning state machine.**

### DRBD 9 Status Parsing Must Be Token-Based

DRBD 9 status lines contain multiple space-separated `key:value` tokens per line (e.g., `disk:UpToDate open:no`). Parsers must split on spaces and match by `key:value` prefix, not treat the whole line as a single value. The parser must also handle:
- Connected format (with peer section)
- Syncing format (with `done:` progress)
- Disconnected format (no peer section)
- Multi-resource output from `drbdadm status all`
- Peer role shown as `hostname role:Secondary` (hostname prefix)

### AppArmor on Real Ubuntu 24.04 Servers

Docker's default AppArmor profile restricts the `mount` syscall even with `CAP_SYS_ADMIN`. The device-mount container pattern requires `--security-opt apparmor=unconfined`. This was invisible in PoC 1 because DinD runs inside a privileged container where AppArmor is inactive. Discovered in Layer 4.1 (Issue 27).

The complete production container launch command is now:
```bash
docker run -d \
  --name {user_id}-agent \
  --device /dev/drbd{minor} \
  --cap-drop ALL --cap-add SYS_ADMIN --cap-add SETUID --cap-add SETGID \
  --security-opt apparmor=unconfined \
  --network none \
  --memory 64m \
  -e BLOCK_DEVICE=/dev/drbd{minor} \
  -e SUBVOL_NAME=workspace \
  platform/app-container
```

### Device-Mount Eliminates Host Mounts During Normal Operation

After the one-time `format-btrfs` provisioning step (which temporarily mounts on the host to create subvolumes, then unmounts), the host never mounts the user's Btrfs again. The container handles its own mount internally via the init script. This means:
- No host-side mount points active during normal operation
- Failover is simpler: promote DRBD → start device-mount container (no host mount step)
- Suspension is simpler: stop container (mount namespace dies with container, no separate unmount)

### Teardown Must Respect the Dependency Chain

Resources depend on each other in a stack. Teardown must proceed top-to-bottom:
```
container (holds mount namespace)
  → DRBD device (held by mount)
    → loop device (backing device for DRBD)
      → image file (backing file for loop device)
```

Specifically: stop container → unmount (if host-mounted) → `drbdadm down` (while config file still exists) → remove config → `losetup -d` → delete image file. Calling `drbdadm down` after removing the config file fails because `drbdadm` needs the config to identify the resource. Discovered in Layer 4.1 (Issue 30).

### The Production Block Device Stack Is Fully Validated

The complete chain — sparse file → loop device → DRBD (Protocol A, `meta-disk internal`) → Btrfs → subvolume-per-world → device-mount containers → snapshots → failover → rollback — is proven working end-to-end across two independent servers. This is the exact production topology.

Additionally, the complete backup and restore chain — `btrfs send → zstd → B2 upload → B2 download → zstd -d → btrfs receive` — is proven working, including incremental sends with parent tracking and manifest-based chain ordering.

As of Layer 4.1, this entire stack is also proven to work when driven by a Go HTTP API, with idempotent endpoints, multi-user density, and proper resource teardown ordering.

### btrfs send/receive Chain Ordering Is Mandatory

`btrfs send -p parent child` produces an incremental stream that describes only the delta between parent and child. On the receiving end, `btrfs receive` requires the parent subvolume to already exist. Applying incrementals out of order fails. The manifest.json tracks the chain so the restore process applies snapshots in the correct sequence.

Long chains make cold restores slow (each incremental must be downloaded and applied sequentially). Production should upload a fresh full send every 10 snapshots or monthly to reset the chain.

---

## 11. Drift Analysis: Layer 4.1

When Layer 4.1 was completed, each fix was reviewed against the established patterns from PoCs 1-3 and the architecture to ensure no architectural drift was introduced.

| Issue | Drift? | Assessment |
|-------|--------|------------|
| #24: SSH key path | No | Local environment detail |
| #25: Token-based DRBD parser | No | Better implementation of existing parsing need |
| #26: Promote before sync | No | **Corrects a bug in the build prompt**, restores PoC 2/3's proven ordering |
| #27: AppArmor unconfined | No | Real production requirement discovered — DinD hid this. Acceptable given other security layers |
| #28: Bash associative arrays | No | macOS compatibility, no architectural impact |
| #29: docker top vs docker exec | No | Test methodology improvement, container pattern unchanged |
| #30: Cleanup ordering | No | Correct implementation of dependency-aware teardown |

**No drift detected.** All fixes either correct errors in the build prompt, discover real production requirements, or improve robustness. The device-mount pattern, DRBD-first ordering, capability model, and idempotent API design are all preserved exactly as designed.

### Changes That Affect Future Layers

Three discoveries from Layer 4.1 must be carried forward:

1. **Layer 4.2 provisioning state machine** — must use promote → sync wait ordering (not sync wait → promote as in the original Layer 4 build prompt). The original Step 6 (DRBD_SYNCED) and Step 7 (PRIMARY_PROMOTED) must be swapped.

2. **All container launches everywhere** — must include `--security-opt apparmor=unconfined` alongside the existing `--cap-drop ALL --cap-add SYS_ADMIN --cap-add SETUID --cap-add SETGID`.

3. **All DRBD status parsing everywhere** — must use token-based parsing (split on spaces, match `key:value` prefixes), not line-based.

---

## 12. Open Questions / Future Decisions

1. **Agent framework**: Which open-source agent loop to base on. Lightweight, persistent memory, cron, Telegram support.
2. **Credential proxy design**: iptables + transparent proxy vs explicit proxy in containers.
3. **Telegram gateway**: Single bot for thousands of users; rate limit handling (30 msg/sec per bot).
4. **Inter-world communication protocol**: Message bus or API gateway mediated by SDK.
5. **Marketplace protocol**: How apps declare capabilities, how upstream updates merge with customizations.
6. **Modification-restricted zones**: Which app components agents can/cannot modify.
7. **DRBD at scale**: 2,000 users × 2 copies = 4,000 resources, ~800-1,000 per machine. Needs benchmarking.
8. **Host filesystem**: XFS vs ext4 — benchmark both.
9. **Domain management**: How users connect their domain, agent handles DNS/SSL/routing.
10. **Spending limit UX**: How the decision queue presents cost estimates and limit warnings.
11. **Prototype architecture revision**: Docker Compose layout in architecture-v3.md Section 16 needs updating to account for DRBD's separate-kernel requirement. Options: multi-server compose with real VMs, or split testing (Btrfs in Docker, DRBD on real servers).
12. **Architecture doc update**: Section 14.4 (cold restore) should explicitly state DRBD-first ordering — blank image → DRBD → mkfs → btrfs receive.
13. **Streaming B2 uploads**: Production should pipe `btrfs send | zstd | b2 upload-unbound-stream` directly instead of temp files. Temp files require disk space proportional to snapshot delta size.
14. **B2 chain maintenance**: Every 10 layers or monthly, upload fresh full send to reset the chain. Delete superseded incrementals.
15. **DRBD peer replacement strategy**: Layer 4.4 will test whether `drbdadm disconnect` + config update + `drbdadm adjust` can replace a dead peer without taking down the primary's DRBD resource. If this works, bipod reformation is zero-downtime. If DRBD 9 has quirks with adjust, we'll discover the real workaround.
16. **Custom AppArmor profile**: When internet-facing containers are added (with network access instead of `--network none`), a custom AppArmor profile that allows `mount` but restricts other operations may be warranted. For now, `apparmor=unconfined` is acceptable given cap-drop ALL + network none + unprivileged user.

---

## 13. Transition: PoC Directories → scfuture Repository

### The Problem with Horizontal PoC Directories

Layers 1-3 each lived in their own directory (`poc-btrfs/`, `poc-drbd/`, `poc-backblaze/`). This made sense — each PoC was a different technology stack with different scripts. But it created a structural problem: proven code was described in prose (SESSION.md) rather than carried forward as actual code. When the Layer 4 build prompt was written from prose descriptions, it regressed to bind mounts despite PoC 1 Patch 1 having already proven device-mount. The lesson existed as text, but the implementation wasn't inherited.

Layer 4.1 (`poc-coordinator/`) was the first Go code and the first layer that future layers would build directly on top of. Continuing to create new directories per layer would mean copying code between directories, re-describing proven patterns in each build prompt, and risking drift every time.

### The Move

After Layer 4.1 passed 66/66 checks, the proven machine agent code was reorganized into `scfuture/` — the permanent project repository. The Go module name changed from `poc-coordinator` to `scfuture`. No business logic was modified. The only structural change was extracting HTTP API types into a `shared` package (`internal/shared/types.go`) so that both the machine agent and the future coordinator import the same type definitions — making API contract drift a compile error rather than a runtime surprise.

Two untyped responses (`DRBDPromote` and `DRBDDemote` returning `map[string]interface{}`) were replaced with proper structs (`shared.DRBDPromoteResponse`, `shared.DRBDDemoteResponse`) with the same JSON keys, preserving backward compatibility with the test suite.

### Package Structure After the Move

```
scfuture/
├── cmd/machine-agent/          # Binary entry point (Layer 4.1)
├── internal/
│   ├── shared/                 # API contract — types both sides agree on
│   └── machineagent/           # Machine agent implementation (proven, 66/66)
├── container/                  # Dockerfile + init script (frozen from PoC 1 Patch 1)
└── scripts/                    # Test harness + infrastructure (frozen from Layer 4.1)
```

Future layers add to this structure:

```
cmd/coordinator/                # Added in Layer 4.2
internal/coordinator/           # Added in Layer 4.2
internal/shared/                # Extended as API surface grows
internal/machineagent/          # Small deltas (e.g., heartbeat endpoint in 4.3)
```

### How We Build From Now On

**Git commits replace directories.** Each completed layer is a git commit on the same codebase. To see the state after Layer 4.1, check out that commit. No more `poc-btrfs/`, `poc-drbd/`, `poc-backblaze/`, `poc-coordinator/` proliferation.

**Build prompts are delta documents.** Each layer's build prompt has three sections:
1. **Existing code (read-only reference)** — files that already exist, listed so the AI reads them. Not rewritten.
2. **Modifications to existing packages** — specific, scoped additions to proven code. "Add this endpoint to `machineagent/server.go`." Not "here's how containers work."
3. **New code** — new packages or files created from scratch.

**The shared package is the contract.** When the coordinator needs to call the machine agent, both sides import `scfuture/internal/shared`. Type agreement is enforced at compile time. If a field name changes in the machine agent's response, the coordinator won't compile until it's updated too.

**Proven code is inherited, not re-described.** The build prompt for Layer 4.2 won't explain the device-mount pattern, the DRBD status parser, or the AppArmor flag. It will say "read `internal/machineagent/containers.go` — this is how containers are started." The AI reads the actual code, not a prose summary that might drift.

**The machine agent package is mostly frozen.** Each layer may add small deltas (a new endpoint, an additional field in a response), but the core — image management, DRBD lifecycle, Btrfs formatting, container start/stop, cleanup — is proven and stable. The coordinator is the growing edge.

### What This Means for SESSION.md

This document continues to track learnings, issues, and drift analysis across layers. But it no longer needs to carry forward implementation details that are better expressed as code. "The production container launch command includes `--security-opt apparmor=unconfined`" is useful context here. But the authoritative source is now `internal/machineagent/containers.go`, not this document.

---

## 11. Layer 4.4 — Bipod Reformation & Dead Machine Re-integration

### What Layer 4.4 Built

Automatic bipod reformation: when a user loses their secondary (or has their primary fail over), the reformer restores 2-copy replication by provisioning a new secondary on a different fleet machine.

**New Go code:**
- `internal/coordinator/reformer.go` — Background goroutine (30s tick) that:
  1. Cleans stale bipods on machines that have returned to active
  2. Scans for `running_degraded` users past a 30s stabilization period
  3. For each degraded user: selects new secondary → creates image → configures DRBD → disconnects dead peer on primary → reconfigures primary to new peer → waits for sync
- `internal/machineagent/drbd.go` — Added `DRBDDisconnect` (idempotent disconnect from peer) and `DRBDReconfigure` (rewrite config + adjust, with down/up fallback)
- `internal/coordinator/store.go` — Added `StatusChangedAt` to User, `ReformationEvent` struct, and methods: `GetDegradedUsers`, `GetStaleBipodsOnActiveMachines`, `GetAllStaleBipodsOnActiveMachines`, `RemoveBipod`, `SelectOneSecondary`, `RecordReformationEvent`, `GetReformationEvents`
- `internal/coordinator/server.go` — Added `GET /api/reformations` endpoint
- `internal/coordinator/machineapi.go` — Added `DRBDDisconnect`, `DRBDReconfigure`, `DRBDDestroy` client methods
- `internal/coordinator/healthcheck.go` — Fixed primary-died failover to set `running_degraded` (not `running`)

**New test scripts:** `scripts/layer-4.4/` with full 10-phase integration test.

### Test Results

**91/91 checks passed** across 10 phases:
- Phase 0: Prerequisites (14/14)
- Phase 1: Provision Users baseline (15/15)
- Phase 2: Kill fleet machine (2/2)
- Phase 3: Failure Detection & Failover (6/6)
- Phase 4: Verify Degraded State (9/9)
- Phase 5: Wait for Bipod Reformation (9/9)
- Phase 6: DRBD Sync & Data Integrity (12/12)
- Phase 7: Dead Machine Return & Cleanup (7/7)
- Phase 8: Coordinator State Consistency (8/8)
- Phase 9: Reformation Events (3/3)
- Phase 10: Cleanup (6/6)

### Key Technical Finding: `drbdadm adjust` Works

**All 3 reformations used `drbdadm adjust` — zero downtime.** The down/up fallback was never needed.

When the primary's DRBD peer dies and goes StandAlone, the reconfigure flow:
1. Write new config file pointing to new peer
2. `drbdadm adjust <resource>` — DRBD reads the new config, sees the new peer address, and connects

The primary's container kept running throughout. DRBD handled the peer swap seamlessly. This confirms that peer replacement on a running primary is a zero-downtime operation.

Reformation timing: ~8.2 seconds per user (measured from "reforming" to "running"), dominated by DRBD sync of the 512MB image.

### Issues Encountered and Fixes

**Issue 24 (Layer 4.4): Primary-died failover set status to "running" instead of "running_degraded"**

The Layer 4.3 healthcheck's primary-died path set the user to `"running"` after successful failover. But the user only has 1 copy at that point — they should be `"running_degraded"` so the reformer picks them up. Fixed in `healthcheck.go`.

**Issue 25 (Layer 4.4): Machine-agent service not enabled for auto-start on reboot**

`deploy.sh` used `systemctl start` but not `systemctl enable`. When fleet-1 was powered off and back on, the machine-agent didn't auto-start, so it couldn't resume heartbeats. Fixed: `systemctl enable --now`.

**Issue 26 (Layer 4.4): Stale bipods not cleaned up after reformation**

The reformer's stale cleanup only ran during the `reformUser` flow for degraded users. After reformation completed (user → "running"), stale bipods on the dead machine persisted. When the dead machine returned to active, nobody cleaned them up. Fixed: added `cleanStaleBipodsOnActiveMachines()` that runs on every reformer tick, independent of user status.

**Issue 27 (Layer 4.4): Test assumed all users have bipods on fleet-1**

With 3 users and 3 machines, placement is non-deterministic. Some users may have no bipods on fleet-1 at all. Fixed test to be placement-aware: only check degraded state for users that actually had bipods on the killed machine.

### Drift from Build Prompt

1. **Added `cleanStaleBipodsOnActiveMachines()` to reformer** — the prompt only described stale cleanup as part of the `reformUser` flow (Step 0). This is insufficient because users transition to "running" after reformation, and the dead machine may return later. The separate cleanup pass handles this case.

2. **Added `GetAllStaleBipodsOnActiveMachines()` to store** — new method not in the prompt, needed for the cleanup pass.

3. **Added `DRBDDestroy` client method to machineapi.go** — the prompt's reformer code called `client.DRBDDestroy()` but it wasn't listed in the machineapi.go modifications. Added it.

4. **Changed healthcheck.go failover result from "running" to "running_degraded"** — this was a bug in Layer 4.3 code that prevented the reformer from working correctly. The prompt's reformation flow depends on users being in "running_degraded" state.

5. **`systemctl enable --now` instead of `systemctl start`** — not specified in the prompt's deploy.sh, but required for the dead machine return test to work.

6. **Private IPs auto-assigned (10.0.0.3-5) instead of prompt's 10.0.0.11-13** — Hetzner assigns IPs during `--network` server creation before `attach-to-network` with specific IPs can succeed. Functionally identical since code uses registered addresses.

### Architectural Drift Review: All Layer 4 Issues vs architecture-v3.md

After completing Layer 4.4, all issues across Layer 4 (Issues #24–#31 from L4.1, #31 from L4.3, #24–#27 from L4.4) were reviewed against architecture-v3.md to check for drift that might solve surface problems while contradicting the bigger picture.

#### No Drift (Confirmed Aligned)

| Issue | Assessment |
|-------|-----------|
| L4.1 #24: SSH key path | Env detail, no arch impact |
| L4.1 #25: DRBD parser tokens | Better implementation of existing need |
| L4.1 #26: Promote before sync | Corrects a sequencing bug; architecture Section 7.1 implies this ordering |
| L4.1 #28: Bash 3.2 compat | macOS compat, no arch impact |
| L4.1 #29: docker top vs exec | Test methodology |
| L4.1 #30: Cleanup ordering | Correct implementation of dependency teardown |
| L4.3 #31: Placement-aware tests | Test fix only |
| L4.4 #25: systemctl enable | Operational fix, no arch impact |
| L4.4 #27: Placement-aware tests | Test fix only |

#### Positive Drift (Better Than Architecture, Doc Needs Update)

**1. Device-mount pattern vs bind mounts.** Architecture-v3.md Section 15.2 says "Bind mount to `/workspace`." We use block device via `--device`, mount from inside the container. This is better — no host mount points during normal operation, no metadata leakage in `/proc/mounts`. Proven in PoC 1 Patch 1, carried through all layers. Architecture doc is stale.

**2. No host-side Btrfs mount during normal operation.** Architecture Section 3.2 shows `Btrfs filesystem mounted at /mnt/users/alice` as steady-state. Section 7.1 says "Mount Btrfs on fleet-5" during failover. In reality, the host NEVER mounts Btrfs during normal operation or failover — the container handles its own mount. Host mount only happens during the one-time `format-btrfs` provisioning step. Simplifies failover (promote DRBD → start container, no mount step) and eliminates host mount points as attack surface.

**3. DRBD backing device is loop, not raw file.** Architecture Section 4.2 config shows `disk /data/images/alice.img`. DRBD requires a block device — we use `losetup` to create a loop device. Corrected in PoC 2, carried forward everywhere.

#### Drift That Warrants Attention

**4. User states expanded beyond the architecture's 4-state model.**

Architecture Section 6 defines: `provisioning | running | suspended | evicted`. We added `running_degraded`, `failing_over`, and `reforming` as internal operational states. These are necessary — the reformer depends on `running_degraded` to find users needing bipod restoration.

**Hard requirement for Layer 6+:** The user-facing API layer (Telegram gateway, dashboard) MUST map `running_degraded`, `failing_over`, and `reforming` all to `running` from the end user's perspective. These internal states are operational metadata, not user-visible lifecycle states. If they ever leak to user-facing APIs, it contradicts the architecture's clean 4-state lifecycle.

**5. Failover timing: 60s vs architecture's 30s.**

Architecture Section 8.2 says "Marks machines offline after 30s." We use 30s to `suspect`, 60s to `dead`. This was deliberate — reduces false positives from transient network issues. Combined with the reformer's 30s stabilization period, a user whose primary dies may not have their bipod reformed until ~90–120s after the machine died. Still well within the architecture's "10 minute" reformation target (Section 7.2), but the detection window is doubled.

**6. Reformation triggered by polling, not by failover event.**

Architecture Section 7.1 says reformation happens "Asynchronously" after failover — implying it's triggered by the failover event itself. We use a 30s polling loop with 30s stabilization — up to 60s delay before reformation starts. The polling approach is simpler and more resilient (works after coordinator restart, catches missed events). For production, consider having the failover code directly trigger a reformation attempt to reduce single-copy exposure time, with the polling loop as a safety net.

**7. `--network none` vs architecture's per-user Docker network.**

Architecture Section 15.1 says containers share `user-alice-net`. We use `--network none`. Correct for now — the current container has no networking needs. But when Layer 6+ adds real agent containers with Telegram access and app servers, this must change to a per-user bridge network with the credential proxy as the only egress path. No risk now, but the architecture's multi-container model hasn't been tested.

#### Architecture Doc Sections That Need Updating

| Section | What's Wrong | Correct Value |
|---------|-------------|---------------|
| 3.2 | Shows host-side Btrfs mount as steady state | Host mount only during provisioning; container mounts internally |
| 4.2 | `disk /data/images/alice.img` | `disk /dev/loopN` (loop device) |
| 4.4 | Minor partitioning for shared-kernel | Per-machine from 0 (separate kernels) |
| 7.1 | "Mount Btrfs on fleet-5" during failover | No host mount; start device-mount container |
| 14.4 | `mount -o loop` then DRBD after | DRBD first on blank device, then format, then btrfs receive |
| 15.2 | "Bind mount to `/workspace`" | Device-mount pattern (block device via `--device`) |
| 16.x | Docker Compose prototype | Known non-viable for DRBD (shared kernel) |

#### Verdict

**No harmful drift.** Every fix either corrected an error in the build prompt, discovered a real production requirement, or added necessary operational states. The core architectural commitments — bipod replication, block device stack, coordinator/agent split, failure recovery, DRBD Protocol A — are all intact and proven. The main thing to enforce going forward: internal operational states (`running_degraded`, `failing_over`, `reforming`) must never surface to end users.

---

### Layer 4.5: Suspension, Reactivation & Deletion Lifecycle

#### Goal

Implement the full user lifecycle beyond "running": suspend users (stop containers, snapshot, backup to B2, demote DRBD), reactivate them via warm path (images still on fleet) or cold path (restore from B2 after eviction), evict them (delete all fleet resources), and enforce retention timers that automatically disconnect DRBD and evict suspended users over time. This completes the architecture's 4-state lifecycle (`provisioning → running → suspended → evicted`).

#### Architecture

Same topology: 1 coordinator + 3 fleet machines on Hetzner Cloud (l45-* prefix). Additionally uses a Backblaze B2 bucket (created/destroyed per test run) for backup storage. Fleet machines have `b2` CLI, `zstd`, and all existing DRBD/Btrfs/Docker tooling. Retention enforcer runs with accelerated timers (15s warm retention, 30s eviction) for testing.

#### New Code Written

**Machine agent — new file (1):**
- `internal/machineagent/backup.go` — `Backup` (btrfs send → zstd → b2 file upload + manifest), `Restore` (b2 file download → zstd -d → btrfs receive → workspace snapshot), `BackupStatus` (checks manifest.json in B2). All functions authorize b2 account before operations.

**Machine agent — modified files (3):**
- `internal/machineagent/btrfs.go` — Added `bare` parameter to `FormatBtrfs` (creates only snapshots dir, no workspace/seed data — used by cold restore). Added `Snapshot` method for creating read-only Btrfs snapshots.
- `internal/machineagent/drbd.go` — Added `DRBDConnect` method (runs `drbdadm connect` for reconnecting disconnected DRBD).
- `internal/machineagent/server.go` — Added 5 routes: snapshot, backup, restore, backup/status, drbd/connect. Modified `handleFormatBtrfs` to accept optional `FormatBtrfsRequest` body for bare mode.

**Coordinator — new files (2):**
- `internal/coordinator/lifecycle.go` — `suspendUser` (stop container → snapshot → B2 backup → demote DRBD → suspended), `reactivateUser` (routes warm/cold), `warmReactivate` (reconnect DRBD if needed → promote → start container), `coldReactivate` (select machines → create images → DRBD setup → format bare → restore from B2 → start container → reform bipod), `evictUser` (verify backup → disconnect/destroy DRBD → delete images → clear bipods).
- `internal/coordinator/retention.go` — `StartRetentionEnforcer` goroutine (60s tick), `enforceRetention` (scans suspended users), `disconnectSuspendedDRBD`. Configurable via `WARM_RETENTION_SECONDS` and `EVICTION_SECONDS` env vars.

**Coordinator — modified files (5):**
- `internal/coordinator/store.go` — Added `BackupExists`, `BackupPath`, `BackupBucket`, `BackupTimestamp`, `DRBDDisconnected` to User. Added `LifecycleEvent` struct. Added 6 methods: `SetUserBackup`, `SetUserDRBDDisconnected`, `ClearUserBipods`, `GetSuspendedUsers`, `RecordLifecycleEvent`, `GetLifecycleEvents`.
- `internal/coordinator/server.go` — Added `B2BucketName` to Coordinator. Changed `NewCoordinator` signature. Added 4 lifecycle routes (suspend, reactivate, evict, lifecycle-events) with async goroutine handlers.
- `internal/coordinator/machineapi.go` — Added 6 client methods: `Snapshot`, `Backup` (300s timeout), `Restore` (300s timeout), `BackupStatus`, `DRBDConnect`, `FormatBtrfsBare`.
- `internal/coordinator/healthcheck.go` — Added early return for suspended/evicted users in `failoverUser`.
- `internal/shared/types.go` — Added 11 new types for snapshot, backup, restore, lifecycle events.
- `cmd/coordinator/main.go` — Reads `B2_BUCKET_NAME` env var, starts retention enforcer.

**Test scripts (7 files):**
- `scripts/layer-4.5/` — run.sh (full lifecycle with B2 bucket management), common.sh, infra.sh, deploy.sh (deploys B2 credentials + retention timers), test_suite.sh (10 phases), cloud-init/coordinator.yaml, cloud-init/fleet.yaml (includes python3-pip, zstd, b2 CLI).

#### Lifecycle State Machine

```
running ──(suspend)──▶ suspended ──(warm retention expires)──▶ suspended+drbd_disconnected
                                  ──(eviction timer expires)──▶ evicted
suspended ──(reactivate, warm)──▶ running  (images on fleet, near-instant)
evicted ──(reactivate, cold)──▶ running    (restore from B2, ~30-60s)
suspended/evicted ──(evict)──▶ evicted     (delete fleet resources, keep B2)
```

#### Suspension Sequence

1. Stop container on primary machine
2. Create read-only Btrfs snapshot (`suspend-<timestamp>`)
3. Upload snapshot to B2 via `btrfs send | zstd | b2 file upload` + manifest.json
4. Demote DRBD to Secondary
5. Set user status → `suspended`, record lifecycle event

#### Cold Reactivation Sequence

1. Select 2 available machines with most free space
2. Create images (100MB loop files) on both machines
3. Set up DRBD between them (create metadata, attach, connect)
4. Format Btrfs in bare mode (snapshots dir only, no workspace)
5. Download snapshot from B2, decompress, `btrfs receive`, create workspace subvolume
6. Start container on primary
7. Wait for DRBD sync to complete (UpToDate)

#### Test Results — 63/63 Checks Passed

```
═══ Phase 0: Prerequisites ═══                                [16/16]
  ✓ Coordinator responding, 3 fleet machines registered
  ✓ All machines active, DRBD loaded, container images built
  ✓ B2 CLI available on all fleet machines
  ✓ No lifecycle events initially

═══ Phase 1: Provision Users (baseline) ═══                   [6/6]
  ✓ alice and bob provisioned and running
  ✓ Test data written to each user's container
  ✓ DRBD replication healthy (UpToDate) for both users

═══ Phase 2: Suspend alice ═══                                [5/5]
  ✓ alice status → suspended
  ✓ Container stopped, DRBD demoted to Secondary
  ✓ B2 backup exists (verified via coordinator)
  ✓ Suspension lifecycle event recorded

═══ Phase 3: Warm Reactivation (alice) ═══                    [5/5]
  ✓ alice status → running (near-instant)
  ✓ Container running, DRBD promoted to Primary
  ✓ Test data intact after warm reactivation

═══ Phase 4: Suspend alice again (pre-eviction) ═══           [3/3]
  ✓ More data written, alice suspended again
  ✓ B2 backup updated with new data

═══ Phase 5: Evict alice ═══                                  [4/4]
  ✓ alice status → evicted
  ✓ No bipods remain, images deleted on fleet
  ✓ Eviction lifecycle event recorded

═══ Phase 6: Cold Reactivation (alice from B2) ═══            [6/6]
  ✓ alice status → running after cold reactivation
  ✓ Container running with 2 healthy bipods
  ✓ Original data AND post-reactivation data survived cold restore

═══ Phase 7: Retention Enforcer — DRBD Disconnect (bob) ═══   [4/4]
  ✓ bob suspended, DRBD auto-disconnected after 15s warm retention
  ✓ DRBD connection state → StandAlone
  ✓ DRBD disconnect lifecycle event recorded

═══ Phase 8: Retention Enforcer — Auto Eviction (bob) ═══     [2/2]
  ✓ bob auto-evicted after 30s eviction timer
  ✓ No bipods remain

═══ Phase 9: Coordinator State Consistency ═══                [6/6]
  ✓ alice running, bob evicted — final state correct
  ✓ Both users have B2 backups
  ✓ ≥6 lifecycle events recorded, state.json persisted

═══ Phase 10: Cleanup ═══                                     [6/6]
  ✓ All fleet machines cleaned, 0 users remaining
```

#### Bug Fixed During Testing

- **B2 CLI authorization**: The `b2` CLI v4 on fleet machines requires explicit `b2 account authorize` before upload/download operations. The env vars `B2_KEY_ID`/`B2_APP_KEY` are not auto-detected by the CLI. Fixed by adding `b2 account authorize` calls in `backup.go` before all B2 operations (Backup, Restore, BackupStatus).

---

### Layer 4.6 Preview: Crash Recovery, Reconciliation & Postgres Migration

> **Origin of this material:** Before Layer 4 was broken into six sub-layers (4.1–4.6), we initially wrote a single monolithic build prompt (`master-prompt4.NOT_USED.md`) that attempted to cover all of Layer 4 in one shot — provisioning, failover, reformation, suspension, crash recovery, fault injection, and chaos testing. That prompt grew to ~2,500 lines and we realized the scope was too large for a single build pass. We'd lose sight of potential gaps, and the AI would struggle to hold the full context.
>
> So we broke it down: Layers 4.1 (machine agent), 4.2 (coordinator happy path), 4.3 (failover), 4.4 (reformation), 4.5 (lifecycle). Each was built, tested, and proven independently — a decision that proved excellent, as each layer uncovered its own bugs and design insights.
>
> But the original monolithic prompt contained extensive, already-deliberated intelligence about crash recovery, reconciliation, fault injection, and Postgres migration — material that was never used because we hadn't reached that layer yet. What follows is the full extraction of that intelligence, preserved as strong suggestions for the Layer 4.6 build prompt. Not everything will transfer directly (our implementation diverged from the original design in several ways), but the core ideas, the reconciliation algorithm, the fault injection methodology, and the consistency checker are deeply considered and form a solid foundation.

#### What Layer 4.6 Aims to Prove

The coordinator can be killed at any point during any multi-step operation (provisioning, failover, reformation, suspension, reactivation, eviction) and recover correctly on restart. This is the final reliability primitive — after this layer, the system is crash-safe.

From the original prompt: "Crash recovery from any partial state — coordinator can be killed at any point during any operation and recover correctly on restart."

#### 1. Database Migration: In-Memory → Supabase (Postgres)

**Why external Postgres, not local SQLite:**

The original prompt explicitly reasoned: "The coordinator must be testable for crash recovery. If the database is on the coordinator machine, killing the coordinator process risks database state. An external managed database eliminates this variable entirely. Supabase's free tier provides a production-grade Postgres instance at zero cost."

**Connection:** `DATABASE_URL` environment variable (Supabase Postgres connection string). Both the coordinator and the test harness use this same connection string. The database is external to all machines — it survives coordinator restarts and machine failures.

**Only external Go dependency:** `github.com/lib/pq` (Postgres driver). Everything else remains standard library.

**Coordinator advisory lock for singleton enforcement:**

```sql
SELECT pg_try_advisory_lock(12345)
```

On startup, the coordinator acquires this lock. If it returns false, another coordinator is already running — the new instance logs an error and exits. The lock auto-releases when the Postgres connection drops (i.e., when the coordinator process dies). This prevents split-brain and is a prerequisite for future active-passive HA.

#### 2. Database Schema (Original Design)

The original prompt designed a 5-table schema. Note: our current implementation uses different patterns (user-level status tracking vs. per-bipod state, no operations table), so this schema will need adaptation to match our actual codebase. Presented here as the original design with annotations on what has changed:

```sql
-- Machines in the fleet
CREATE TABLE machines (
    machine_id      TEXT PRIMARY KEY,
    address         TEXT NOT NULL,              -- private IP:port of machine agent
    status          TEXT NOT NULL DEFAULT 'active',  -- active | suspect | dead
                                                     -- (we added 'suspect' in Layer 4.3)
    disk_used_mb    INTEGER DEFAULT 0,
    ram_used_mb     INTEGER DEFAULT 0,
    active_agents   INTEGER DEFAULT 0,
    max_agents      INTEGER DEFAULT 10,
    last_heartbeat  TIMESTAMPTZ,
    status_changed_at TIMESTAMPTZ              -- added in Layer 4.4
);

-- User accounts
CREATE TABLE users (
    user_id         TEXT PRIMARY KEY,
    status          TEXT NOT NULL DEFAULT 'provisioning',
                    -- registered | provisioning | running | running_degraded |
                    -- failing_over | reforming | suspending | suspended |
                    -- reactivating | evicted | failed | unavailable
                    -- (expanded significantly from original's 4 statuses)
    primary_machine TEXT REFERENCES machines(machine_id),
    drbd_port       INTEGER UNIQUE,
    image_size_mb   INTEGER DEFAULT 100,
    error           TEXT,
    created_at      TIMESTAMPTZ DEFAULT NOW(),
    status_changed_at TIMESTAMPTZ,             -- added in Layer 4.4
    -- Layer 4.5 additions:
    backup_exists   BOOLEAN DEFAULT FALSE,
    backup_path     TEXT,
    backup_bucket   TEXT,
    backup_timestamp TIMESTAMPTZ,
    drbd_disconnected BOOLEAN DEFAULT FALSE
);

-- Bipod members (2 rows per user when healthy)
CREATE TABLE bipods (
    user_id         TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    machine_id      TEXT NOT NULL REFERENCES machines(machine_id),
    role            TEXT NOT NULL,              -- primary | secondary | stale
    drbd_minor      INTEGER,
    loop_device     TEXT,
    created_at      TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (user_id, machine_id)
);

-- Multi-step operations (KEY CRASH RECOVERY MECHANISM)
-- This table is the core of crash recovery. Every multi-step operation gets a row
-- with current_step tracking exactly where it was when the coordinator died.
-- On restart, reconciliation reads incomplete operations and resumes from the right step.
CREATE TABLE operations (
    operation_id    TEXT PRIMARY KEY,
    type            TEXT NOT NULL,              -- provision | failover | reform_bipod |
                                               -- suspend | reactivate | evict
    user_id         TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    status          TEXT NOT NULL DEFAULT 'pending',
                    -- pending | in_progress | complete | failed | cancelled
    current_step    TEXT,                       -- tracks progress through multi-step operation
    metadata        JSONB DEFAULT '{}',         -- operation-specific data (target machines, etc.)
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    error           TEXT
);

-- Event log (unified — replaces separate failoverEvents, reformationEvents, lifecycleEvents)
CREATE TABLE events (
    event_id        SERIAL PRIMARY KEY,
    timestamp       TIMESTAMPTZ DEFAULT NOW(),
    event_type      TEXT NOT NULL,             -- machine_offline, user_provisioned,
                                               -- failover_complete, bipod_reformed,
                                               -- user_suspended, user_reactivated,
                                               -- user_evicted, drbd_disconnected, etc.
    machine_id      TEXT,
    user_id         TEXT,
    operation_id    TEXT,
    details         JSONB
);

-- Indexes
CREATE INDEX idx_bipods_machine ON bipods(machine_id);
CREATE INDEX idx_operations_status ON operations(status);
CREATE INDEX idx_operations_user ON operations(user_id);
CREATE INDEX idx_events_type ON events(event_type);
CREATE INDEX idx_events_timestamp ON events(timestamp);
```

**Key design insight — the `operations` table:**

The original prompt's most important design contribution is the `operations` table. Currently, our multi-step operations (provisioning, failover, reformation, suspension, reactivation, eviction) are tracked only by user status transitions (`running → suspending → suspended`). If the coordinator dies mid-operation, we have no record of which step it was on. The `operations` table with its `current_step` field and `metadata` JSONB gives precise crash recovery: on restart, read the incomplete operation, determine the next step, and resume. Every step is idempotent (we already proved this), so re-executing the current step is safe.

**Adaptation note:** The original schema had fine-grained per-bipod state tracking (`pending → image_created → drbd_configured → drbd_synced → promoted → formatted → mounted → containers_running → ready`). Our current implementation doesn't track bipod state this granularly — we use user-level status only. The `operations` table approach may be better suited to our architecture than per-bipod state tracking, because it captures the orchestration intent (what the coordinator was trying to do) rather than just resource state.

#### 3. Five-Phase Startup Reconciliation

This is the most critical piece of the original design. It runs on coordinator startup BEFORE accepting any API requests or starting background goroutines.

**Phase 1: Discover Reality**

Probe ALL machines in the DB, regardless of their recorded status. This is critical — a machine marked `dead` by a previous coordinator run may have rebooted and be fully healthy. Only by probing can we know the actual current state.

```
For each machine in DB (ALL statuses, including 'dead'):
  Try: GET machine /status (timeout: 5 seconds)
  If reachable:
    Store full status response in memory
    Update last_heartbeat
    Set status to 'active' (even if it was 'dead' — it's back)
  If unreachable:
    Mark as 'dead' in DB
    Record: this machine is down
```

**Phase 2: Reconcile Database with Machine Reality**

```
For each user in DB:
  For each bipod member in DB:
    machine_status = reality[bipod.machine_id]

    If machine is dead:
      If bipod role not 'stale':
        Mark bipod as 'stale'
      Continue

    machine_user_info = machine_status.users[user_id]

    If machine_user_info is nil (machine doesn't know about this user):
      DB says resources exist, machine says they don't
      → Resources were lost, need to handle this

    Else (machine has resources for this user):
      Reconcile each layer:
        image_exists → image is present
        drbd exists and connected → DRBD is up
        drbd exists, primary → primary role confirmed
        container running → fully operational

      Update DB to match reality if DB is behind

  Check user-level consistency:
    If user.status == 'running' but no container is running on any machine:
      User needs repair
    If user.status == 'provisioning' but containers are running:
      User is actually running — update status

For each machine that is online:
  For each user the machine reports that DB doesn't know about:
    This is an orphan — queue cleanup (Phase 3b)
```

**Phase 3: Resume Interrupted Operations**

```
operations = SELECT * FROM operations WHERE status IN ('in_progress', 'pending')
             ORDER BY started_at

For each operation:
  Switch on operation.type:

    'provision':
      Check if required machines are online
      If not: try to adapt (swap offline machine for a new one)
               or mark failed and re-queue with fresh machines
      Read current_step from operation
      Determine next step based on current_step and actual state
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

    'evict':
      Read current_step
      Resume eviction from next step

Special case: user in 'provisioning' with NO active operation
  → Create a new provision operation and start from beginning
  → Handles the case where coordinator crashed between creating the user
    and creating the operation
```

**Phase 3b: Clean Up Orphans**

```
For each orphaned user resource discovered in Phase 2:
  DELETE /images/{user_id} on the machine (full teardown)
  Log: "Cleaned up orphaned resources for {user_id} on {machine_id}"
```

**Phase 4: Handle Offline Machines**

```
For machines discovered offline in Phase 1:
  Run standard failover logic for primary users on that machine
  Queue reformations for all affected users
```

**Phase 5: Start Normal Operation**

```
Start heartbeat monitor goroutine
Start bipod health checker goroutine
Start reformer goroutine
Start retention enforcer goroutine
Start HTTP server (begin accepting API requests)
Log: "Reconciliation complete. Processed {N} operations, {M} orphans, {K} offline machines."
```

#### 4. Fault Injection System

Two modes, controlled by environment variables:

**Deterministic mode** (`FAIL_AT` env var):

```go
func (c *Coordinator) checkFault(name string) {
    if c.failAt == name {
        log.Printf("FAULT INJECTION: crashing at checkpoint '%s'", name)
        os.Exit(1)  // Immediate, no cleanup
    }
}
```

The coordinator is started with `FAIL_AT=provision-primary-image-created`. When the provisioning code reaches that checkpoint, `os.Exit(1)` — immediate death, no graceful shutdown, no cleanup. Then the test restarts the coordinator without `FAIL_AT` and verifies it recovers correctly.

**Chaos mode** (`CHAOS_MODE` + `CHAOS_PROBABILITY` env vars):

```go
func (c *Coordinator) checkFault(name string) {
    // ... deterministic check first ...
    if c.chaosMode && rand.Float64() < c.chaosProbability {
        log.Printf("CHAOS: random crash at checkpoint '%s'", name)
        os.Exit(1)
    }
}
```

Every checkpoint has a probability of killing the coordinator. Used for stress testing.

`checkFault(name)` is called at every step transition in every multi-step operation. The function is a single line at the beginning of each step — trivial to add, impossible to forget.

#### 5. Eighteen Deterministic Crash Points (Original Design)

The original prompt identified 18 specific points where a crash could leave the system in a partial state. Each was designed to be tested individually: crash at that point, restart, verify recovery.

**Provisioning (10 checkpoints):**

| ID | Checkpoint | After this completes | Before this starts |
|----|-----------|---------------------|-------------------|
| F1 | `provision-user-created` | DB: user + bipod entries created | Image creation on primary |
| F2 | `provision-primary-image-created` | Image exists on primary machine | Image creation on secondary |
| F3 | `provision-secondary-image-created` | Images exist on both machines | DRBD configuration |
| F4 | `provision-primary-drbd-configured` | DRBD config on primary | DRBD config on secondary |
| F5 | `provision-secondary-drbd-configured` | DRBD config on both | DRBD sync wait |
| F6 | `provision-drbd-synced` | DRBD connected and synced | DRBD promote |
| F7 | `provision-primary-promoted` | DRBD promoted to Primary | Btrfs format |
| F8 | `provision-btrfs-formatted` | Btrfs formatted + workspace created | Container start |
| F9 | `provision-btrfs-mounted` | Btrfs mounted (original design) | Container start |
| F10 | `provision-containers-started` | Container running | DB finalization |

**Failover (4 checkpoints):**

| ID | Checkpoint | After | Before |
|----|-----------|-------|--------|
| F11 | `failover-detected` | Machine marked dead, bipod stale | DRBD promote |
| F12 | `failover-promoted` | DRBD promoted on survivor | Container start |
| F13 | `failover-mounted` | Btrfs mounted (original design) | Container start |
| F14 | `failover-containers-started` | Container running | DB finalization |

**Reformation (4 checkpoints):**

| ID | Checkpoint | After | Before |
|----|-----------|-------|--------|
| F15 | `reform-machine-picked` | New machine selected in DB | Image creation |
| F16 | `reform-image-created` | Image on new machine | DRBD config |
| F17 | `reform-drbd-configured` | DRBD configured | Sync wait |
| F18 | `reform-synced` | Fully synced | DB finalization |

**Adaptation note:** Our implementation differs from the original in several ways that affect checkpoints:
- We use device-mount pattern (no separate mount/unmount steps), so F9 and F13 don't apply directly
- We have 3 additional multi-step operations from Layer 4.5: suspension (5 steps), reactivation warm (4 steps), reactivation cold (10 steps), eviction (3 steps) — each needs its own crash points
- Reformation uses `drbdadm adjust` (zero downtime), not down/up/re-promote — different step sequence
- The total checkpoint count will likely be 25-30+ once Layer 4.5 operations are included

#### 6. Consistency Checker (Ground Truth Oracle)

The original prompt designed a standalone script that SSH's into every machine and verifies 8 system invariants by comparing database state against physical reality. This is the ultimate verification — not trusting the coordinator's view, but checking the actual machines.

**Invariant 1:** Every `running` user has containers on **exactly one** machine (not zero, not multiple)

**Invariant 2:** Every `running` user has **exactly 2** healthy bipod entries in the database

**Invariant 3:** DRBD roles on machines **match** what the database says (Primary on the recorded primary, Secondary on the recorded secondary)

**Invariant 4:** Primary has Btrfs mounted, secondary does **not** (original design — in our device-mount pattern, this translates to: container running on primary with device access, no container on secondary)

**Invariant 5:** No same-machine bipod pairs (both copies of a user are never on the same machine)

**Invariant 6:** No orphaned resources (no images on machines without matching bipod entries in the database)

**Invariant 7:** DRBD port uniqueness across all users

**Invariant 8:** DRBD minor uniqueness per machine

The consistency checker was designed to run after every test phase and after every crash recovery to verify the system returned to a consistent state. It queries Postgres for the "expected" state and SSH's into each machine for the "actual" state.

**Adaptation note:** For our device-mount pattern, Invariant 4 changes to "container running on primary, no container on secondary." We'd also want to add invariants for Layer 4.5 state (suspended users have no running containers, evicted users have no bipods, etc.).

#### 7. Chaos Mode Test Design

The original prompt designed a thorough chaos test:

```
Phase 9: Chaos Mode (50 iterations, 5% crash probability)

1. Start coordinator with CHAOS_MODE=true, CHAOS_PROBABILITY=0.05
2. For 50 iterations:
   - Create a user
   - Wait 3 seconds
   - Check if coordinator is alive
   - If dead: increment crash counter, restart, wait for reconciliation
3. After all iterations:
   - Restart coordinator WITHOUT chaos mode
   - Let reconciliation run (15 seconds)
   - Verify: all users are in valid final states (running/suspended/failed — not stuck
     in transient states like provisioning/failing_over/reforming)
   - Run full consistency checker
   - Verify: no orphaned resources
```

The key insight: after chaos, every user must be in a terminal state (running, suspended, evicted, failed), never stuck in a transient state (provisioning, failing_over, reforming, suspending, reactivating). The reconciliation must unstick everything.

#### 8. Graceful Shutdown

On SIGTERM (normal shutdown, not fault injection), the coordinator should:

1. Stop accepting new API requests
2. Wait for in-progress operations to reach a checkpoint (max 10 seconds)
3. Close database connections
4. Exit

This is for clean upgrades/restarts. Fault injection bypasses this entirely (`os.Exit(1)` — immediate death).

#### 9. What Has Changed Since the Original Design

Several aspects of the original prompt no longer match our implementation. These differences must be accounted for when writing the Layer 4.6 build prompt:

| Original Design | Our Actual Implementation | Impact on 4.6 |
|----------------|--------------------------|---------------|
| Host-mount pattern (separate `/mount` and `/unmount` endpoints) | Device-mount pattern (container gets raw block device via `--device`) | Fewer crash points (no mount step), consistency invariants change |
| Per-bipod state tracking (`pending → image_created → drbd_configured → ... → ready`) | User-level status tracking only (`registered → provisioning → running`) | Operations table `current_step` is more important — bipod state alone can't tell you where provisioning was |
| No operations table in current code | Multi-step operations tracked only by user status | Need to ADD the operations table — this is the core crash recovery mechanism |
| 4 user statuses (provisioning, running, suspended, failed) | 12 user statuses (registered, provisioning, running, running_degraded, failing_over, reforming, suspending, suspended, reactivating, evicted, failed, unavailable) | More transient states to unstick during reconciliation |
| B2 backups excluded | B2 backups integrated (Layer 4.5) | Suspension, cold reactivation, and eviction crash points need to account for B2 operations |
| Reformation uses down/up/re-promote (brief container downtime) | Reformation uses `drbdadm adjust` (zero downtime) | Different step sequence in reformation crash points |
| Fixed DRBD minor ranges per machine (0-99, 100-199, 200-299) | Sequential per-machine from 0 | Minor allocation query changes |
| Heartbeat threshold 15s | Heartbeat thresholds 30s suspect, 60s dead | Different timing in tests |
| `postgresql-client` on coordinator cloud-init | Currently nothing database-related | Need to add Postgres client/connection |
| 3 event lists (failover, reformation, lifecycle) | Same — 3 separate in-memory lists | Unify into single `events` table |

#### 10. What's Most Valuable for the Layer 4.6 Build Prompt

In priority order:

1. **The `operations` table with `current_step` + `metadata` JSONB** — This is the crash recovery mechanism. Every multi-step operation creates an operation row, updates `current_step` after each step, and reconciliation reads incomplete operations to resume them. Without this, crash recovery is just heuristic guessing.

2. **The five-phase reconciliation algorithm** — Discover reality → reconcile DB with machines → resume interrupted operations → clean up orphans → handle offline machines → start normal operation. This is the startup sequence.

3. **The 18+ fault injection checkpoints and test methodology** — Deterministic crash at each point, restart, verify recovery. This is how we prove crash safety. Needs expansion for Layer 4.5 operations.

4. **The consistency checker with 8 invariants** — Ground truth oracle that doesn't trust the coordinator's view. SSH into machines, compare against DB. Run after every crash recovery.

5. **The advisory lock for coordinator singleton** — Simple but important. Prevents split-brain.

6. **The chaos mode test** — 50 iterations, random crashes, verify everything ends up in valid states. The ultimate stress test.

7. **External Postgres (Supabase) rationale** — "Killing the coordinator process risks database state if the DB is local. External managed database eliminates this variable."

### Layer 4.6 Results — CLOSED

**Result: 67/67 checks passing (Run #12, 2026-03-01)**

| Phase | Description | Result |
|-------|------------|--------|
| 0 | Prerequisites | 9/9 |
| 1 | Happy Path — Provision & Verify Postgres | 10/10 |
| 2 | Provisioning Crash Tests (F1-F7) | 8/8 |
| 3 | Failover Crash Tests (F8-F10) | 3/3 |
| 4 | Suspension Crash Tests (F17-F20) | 5/5 |
| 5 | Warm Reactivation Crash Tests (F21-F23) | 4/4 |
| 6 | Eviction Crash Tests (F32-F33) | 3/3 |
| 7 | Cold Reactivation Crash Tests (F24-F31) | 9/9 |
| 8 | Reformation Crash Tests (F11-F16) | 6/6 |
| 9 | Chaos Mode — Random Crash Stress | 4/4 |
| 10 | Graceful Shutdown | 3/3 |
| 11 | Final Consistency & Cleanup | 3/3 |

**Key implementation details:**
- **Postgres migration**: All state moved from in-memory + JSON to Supabase Postgres via pgbouncer (port 6543). Write-through pattern: every mutation writes to Postgres first, then updates in-memory cache.
- **Advisory lock singleton**: `pg_try_advisory_lock(12345)` ensures only one coordinator runs. Stale locks from pgbouncer after crash are cleared via `pg_terminate_backend()`.
- **Five-phase startup reconciliation**: probe machines → reconcile DB → resume operations → clean orphans → handle offline machines. Runs BEFORE goroutines/HTTP server.
- **33 fault injection points**: Deterministic crash via `FAIL_AT` env var + `coord.step(opID, stepName)` → `coord.checkFault(stepName)` → `os.Exit(1)`.
- **Chaos mode**: Random 5% crash probability via `CHAOS_MODE=true` env var.
- **12 system invariants** verified by `check_consistency` function after each crash test phase.
- **One external dependency added**: `github.com/lib/pq` (Postgres driver). First `go.sum` in the project.

**Bugs found & fixed during testing:**
- Advisory lock stale after crash through pgbouncer → `pg_terminate_backend()` to forcibly release
- Container not restarted after interrupted suspension revert → added `ContainerStart` in `resumeSuspension`
- Evicted users retaining bipods from crashed cold reactivation → added cleanup in `resumeColdReactivation` and Phase 2 reconciliation
- Running users with destroyed resources after subsequent crash tests → Phase 2 marks running users with no live bipods as "failed"
- Auto-eviction interfering with crash tests → increased retention timers (600s/1200s)
- SSH drops killing test script → added retry loops to all SSH commands in `crash_test` and `deploy.sh`

---

### Layer 5 Preview: Live Migration

#### Context & Design Philosophy

Live migration moves a running user's world from one machine to another with minimal downtime. Use cases: rebalancing overloaded machines, planned maintenance (draining a machine before taking it offline), and bipod reshaping (moving one side of a bipod to a better-placed machine).

**Key constraint: we don't control what's running inside the container.** The user has an AI agent that builds their world — it may run HTTP servers, databases, open source projects, custom applications. Anything could be running at any time. We cannot expect arbitrary software to support a "prepare to migrate" signal or a freeze/thaw protocol. The platform contract is simpler: **handle SIGTERM gracefully, same as you would for a server reboot.**

This is a reasonable contract because:
- The agent already operates within a well-defined platform contract (SDKs, primitives, system knowledge)
- "Handle SIGTERM" is just another item in that contract
- Open source projects onboarded to the platform will be adapted to fit the system anyway (this is part of the onboarding process)
- Most well-written servers already handle SIGTERM; for those that don't, a wrapper script handles it
- Every cloud VM already makes this same promise (maintenance reboots happen)

#### Why Not `docker pause`?

The original v3 architecture designed migration around `docker pause` (cgroups freezer) + `fsfreeze` for sub-second downtime. We rejected this for several reasons:

1. **`docker pause` freezes processes mid-instruction** — no SIGTERM, no signal handlers, no graceful shutdown. In-memory state is frozen but cannot be transferred to the destination machine (that would require CRIU checkpoint/restore, which is fragile and not production-ready).
2. **`fsfreeze` doesn't work with device-mount** — our containers mount Btrfs internally via `--device /dev/drbdN`. The host has no mount to freeze. We'd need to enter the container's mount namespace, but the container is paused.
3. **After pause, we must start a fresh container on the destination anyway** — so processes restart regardless. The only question is whether they get a clean SIGTERM or a hard kill. SIGTERM is strictly better.

#### Migration Protocol (Device-Mount Pattern)

```
Starting state: bipod on fleet-1 (primary) + fleet-5 (secondary)
Target: move primary to fleet-3

Phase 1 — Pre-sync (transparent, no downtime):
  1. Create empty image on fleet-3
  2. Configure DRBD: add fleet-3 as 3rd node (temporary tripod)
  3. DRBD initial sync from fleet-1 to fleet-3
  4. Wait for sync to complete (fleet-3 now has full copy)
  User's container keeps running throughout. Sync is transparent.

Phase 2 — Switchover (~5-15s downtime):
  5. docker stop on fleet-1 (SIGTERM → grace period → SIGKILL)
     Processes get SIGTERM, finish current requests, close connections,
     flush buffers, shut down cleanly. Default 10s grace before SIGKILL.
  6. Demote DRBD on fleet-1 (secondary)
  7. Promote DRBD on fleet-3 (primary)
  8. docker start on fleet-3 (container starts with same /dev/drbdN, same data)
     All services restart from persisted state on the same filesystem.

Phase 3 — Cleanup:
  9. Remove fleet-1 from DRBD config (disconnect, destroy, delete image)
  10. Update bipod in store: fleet-1 → fleet-3
  Bipod is now: fleet-3 (primary) + fleet-5 (secondary)
```

**Downtime window:** SIGTERM grace period (up to 10s) + demote/promote (~100ms) + container start (~1-5s) = **~5-15 seconds**. During this window, the user's HTTP servers are unreachable and TCP connections break. On restart, everything comes back from the same Btrfs filesystem. Telegram/WhatsApp messages queue and the agent catches up.

**Data safety:** Before Phase 2, DRBD has fully synced fleet-3. After `docker stop`, all filesystem writes have been flushed (process shutdown + kernel flush). Btrfs COW guarantees on-disk consistency. The data exists on 3 machines at the moment of switchover. After cleanup, it's back to 2 (fleet-3 + fleet-5). At no point is data at risk.

#### The Key Unknown: Temporary Tripod (3-Node DRBD)

The one primitive we haven't built or tested: **adding a 3rd DRBD node to a running 2-node resource.**

DRBD 9 supports N nodes natively, but we've only ever configured 2-node resources. The critical questions:

1. **Can `drbdadm adjust` add a 3rd node to a running resource?** — This is the zero-downtime path. Rewrite the `.res` config file to include the 3rd node, run `drbdadm adjust`, DRBD picks up the new config and connects to the 3rd node. This is exactly how reformation works for *replacing* a node — but *adding* a node is different.
2. **If adjust doesn't work, what's the fallback?** — Possibly: disconnect, rewrite config, `drbdadm down`, `drbdadm up`, `drbdadm primary --force`, reconnect. This requires stopping the container briefly during reconfiguration (similar to the reformation fallback path). Acceptable but less elegant.
3. **Does the 3-node sync work correctly?** — Primary replicates to both secondaries simultaneously. Need to verify that sync progress is reported correctly and that `UpToDate` on the new node means a full copy.
4. **Removing a node from a 3-node resource** — After migration, we remove the old primary (now demoted to secondary). Does `drbdadm adjust` handle this cleanly? Or do we need disconnect + reconfigure?

**This is why Layer 5 is split into two sub-layers.** The tripod primitive is where the risk lives. If it works cleanly via `drbdadm adjust`, everything else is straightforward. If it doesn't, we need to find an alternative approach before building the rebalancer on top.

#### Layer 5.1 — Tripod Primitive & Manual Migration

**Goal:** Build and prove the live migration primitive, triggered manually via API.

**Machine agent changes:**
- DRBD config generation must support 3 nodes (currently hardcoded for 2)
- Possibly new endpoints or modifications to existing ones for adding/removing DRBD peers
- Test `drbdadm adjust` for adding a 3rd node to a running resource

**Coordinator changes:**
- New API endpoint: `POST /api/users/{id}/migrate` with `target_machine` parameter
- New multi-step operation type: `live_migration` in the operations table
- Migration orchestration: create image → add 3rd DRBD node → wait sync → stop container → demote/promote → start container → cleanup
- Crash recovery: `resumeMigration` handler in the reconciler

**Test suite should cover:**
- Provision users, trigger migration via API, verify:
  - Data written before migration survives on destination
  - Container running on destination machine
  - Bipod endpoints shifted correctly (new primary, same secondary)
  - Old machine cleaned up (no images, no DRBD config)
  - DRBD healthy (2-node, both UpToDate)
- Migration of primary vs migration of secondary
- Crash during each migration phase (fault injection via FAIL_AT)
- Consistency invariants hold after migration

#### Layer 5.2 — Rebalancer & Machine Drain

**Goal:** Automate migration decisions. Build on the proven migration primitive from 5.1.

**Rebalancer goroutine** (similar pattern to reformer):
- Ticks every 60 seconds
- Computes fleet-wide averages for agent density and disk usage
- Identifies overloaded machines (density or disk > avg + threshold)
- Identifies underloaded machines (density or disk < avg - threshold)
- For each overloaded machine: pick the smallest user, find a suitable destination, trigger migration
- One migration at a time per machine (avoid thundering herd)
- Stabilization period (don't rebalance a user that was just migrated)

**Machine drain API:**
- `POST /api/fleet/{machine_id}/drain` — marks machine as "draining", triggers sequential migration of all users off that machine
- Draining machine stops accepting new placements (excluded from SelectMachines)
- Once all users migrated, machine can be safely taken offline for maintenance
- `POST /api/fleet/{machine_id}/undrain` — cancels drain, machine returns to normal

**Test suite should cover:**
- Provision users unevenly, verify rebalancer triggers migrations
- Drain a machine, verify all users migrated off, verify machine is empty
- Undrain mid-drain, verify remaining users stay
- Rebalancer doesn't migrate during ongoing operations (suspend, failover, etc.)
- Crash recovery for in-progress rebalancer-triggered migrations

### Layer 5.1 Results — CLOSED

**Result: 73/73 checks passing (Run #18, 2026-03-02)**

| Phase | Description | Result |
|-------|------------|--------|
| 0 | Prerequisites | 9/9 |
| 1 | Baseline — Provision & Verify | 6/6 |
| 2 | Primary Migration — Happy Path | 10/10 |
| 3 | Secondary Migration — Happy Path | 8/8 |
| 4 | Validation & Edge Cases | 6/6 |
| 5 | Primary Migration Crash Tests (F34-F42) | 19/19 |
| 6 | Secondary Migration Crash Tests (F50-F54) | 6/6 |
| 7 | Post-Crash Verification | 7/7 |
| 8 | Final Consistency & Cleanup | 2/2 |

**Key implementation details:**
- **Tripod primitive proven**: DRBD 9 temporary 3-node config works. `drbdadm adjust` succeeds in most cases; force fallback (down/up) used when adjust fails (container briefly stopped during reconfig for primary migration).
- **Migration protocol**: 10-step orchestration — create image → DRBD tripod → sync → container stop → demote/promote → container start → cleanup → bipod update. Both primary and secondary migration types implemented.
- **`--max-peers 2` required**: DRBD resources must be created with `--max-peers 2` in initial metadata for later tripod expansion. This is set at provision time.
- **Stable DRBD node-ids**: Derived from hostname (fleet-1→0, fleet-2→1, fleet-3→2) to ensure consistency across reconfigures. Dynamic "first-available" minor allocation prevents collisions.
- **Crash recovery via 6-phase reconciler**: Extended from 5 phases (Layer 4.6) to 6. New phases handle migration resume/cancel and ensure running users have containers on their primary machines (Phase 5 safety net).
- **`containerStopped` fail handler**: If migration fails after the container is stopped, the fail handler restarts the container on the source machine, preventing orphaned "running" users with no container.
- **Systemd override.conf pattern**: Deploy uses idempotent `override.conf` drop-ins instead of fragile `sed`-based config, surviving transient SSH drops during deploy.

**Bugs found & fixed during testing:**
- DRBD `--max-peers` metadata incompatibility → All provisions now use `--max-peers 2`
- DRBD node-id inconsistency across reconfigures → Stable hostname-derived node-ids
- DRBD minor allocation collisions → First-available-minor algorithm
- Stale tripod bipods after crash tests → Phase 3c reconciliation cleans up 3-node leftovers
- Btrfs minimum size too small → Increased image size to 128MB
- `db_query` empty echo false positive → `[ -n "$result" ] && echo "$result"` prevents `wc -l` returning 1 for empty results
- Crash wait timeout too short for late-stage faults → Increased from 60s to 120s (128MB DRBD sync time)
- Container not restarted after mid-migration failure → `containerStopped` flag + explicit restart in fail handler + Phase 5 reconciler safety net
- `find_free_machine` failing with `set -e` → Added `|| true` to bare assignments
- Operations `in_progress` timing race → Added `wait_for_operations_settled` poll before consistency checks
- SSH transient drops during deploy → 5 retries with 10s sleep on all deploy SSH commands, `wait_cloud_init` decoupled from pipeline
- `exit` in `eval` killing test script → Wrapped retry loops in subshells

---

#### What Has Changed Since the Original v3 Design

| Original v3 Design | Our Implementation | Impact on Layer 5 |
|---|---|---|
| Host-mount pattern (fsfreeze, unmount/remount on host) | Device-mount pattern (container gets raw `/dev/drbdN`) | No fsfreeze step. Container stop/start instead of pause/unpause. Simpler but longer downtime (~5-15s vs ~2-6s) |
| `docker pause` + `fsfreeze` for sub-second downtime | `docker stop` (SIGTERM) for graceful shutdown | Processes get clean shutdown signal. In-memory state is lost but disk state is perfectly consistent |
| Assumes controlled workloads | Arbitrary user workloads (HTTP servers, databases, open source projects) | Cannot rely on application-level quiesce. Must use SIGTERM — it's the universal "shut down now" contract |
| DRBD flush-confirm step | Not needed with docker stop | `docker stop` ensures processes flush to disk before exiting. Btrfs COW handles consistency. No explicit DRBD flush needed |
| Container unpause on destination | Fresh container start on destination | Processes restart from persisted state. Equivalent to a fast reboot on a different machine |

---

### PoC Progression

```
Layer 4.1: Machine Agent PoC                    ✅ 66/66 checks
Layer 4.2: Coordinator Happy Path               ✅ 55/55 checks
Layer 4.3: Heartbeat Failure Detection           ✅ 62/62 checks
Layer 4.4: Bipod Reformation                     ✅ 91/91 checks
Layer 4.5: Suspension / Reactivation / Deletion  ✅ 63/63 checks
Layer 4.6: Crash Recovery / Reconciliation       ✅ 67/67 checks
Layer 5.1: Tripod Primitive & Manual Migration   ✅ 73/73 checks
Layer 5.2: Rebalancer & Machine Drain            ⬜ Next
```
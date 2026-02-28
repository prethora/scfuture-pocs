# Session Log: Btrfs World Isolation PoC

## Goal

Build a proof of concept demonstrating the foundational primitive for a distributed agent platform: a single Btrfs image file per user, divided into isolated subvolumes ("worlds"), each mounted into its own Docker container, with cheap instant snapshots and rollback. Everything runs inside Docker Desktop on macOS via a privileged Docker-in-Docker setup.

## What We Built

```
poc-btrfs/
├── docker-compose.yml          # Single privileged "machine" service
├── machine/
│   ├── Dockerfile              # Ubuntu 24.04 + btrfs-progs + docker.io
│   ├── entrypoint.sh           # Starts dockerd, waits for ready, runs demo
│   └── demo.sh                 # Full 8-phase automated PoC
```

### Architecture

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

## Issues Encountered & Fixes

### Issue 1: overlay2 storage driver not supported in DinD

**Problem:** The entrypoint started dockerd with `--storage-driver=overlay2`. Inside the privileged container, overlay2 failed to mount:

```
level=error msg="failed to mount overlay: invalid argument" storage-driver=overlay2
failed to start daemon: error initializing graphdriver: driver not supported: overlay2
```

**Fix:** Switched to `--storage-driver=vfs`. The vfs driver has no kernel requirements — it works everywhere by doing full copies instead of COW layering. Slower and uses more space for image layers, but perfectly fine for a PoC where we only pull a small Alpine image.

### Issue 2: /proc/mounts isolation check false positive

**Problem:** Phase 4 (Prove Isolation) included a check that grepped `/proc/mounts` inside each container for other world names. This failed because `/proc/mounts` shows the host-side mount source paths (e.g., `/mnt/users/alice/app-budget`), which naturally contain the subvolume names. The container can see the *string* in its mount metadata but cannot actually *access* those paths.

```
✗ FAIL: app-budget can see other worlds in /proc/mounts — isolation broken!
```

**Fix:** Replaced the `/proc/mounts` grep with actual filesystem access tests — trying to `ls` the other worlds' mount paths from inside the container. These paths don't exist inside the container's filesystem namespace, so the check correctly passes. The real isolation guarantee is filesystem access, not mount metadata strings.

## Final Result

All 8 phases pass. Full output concludes with:

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

---

## Patch 1: Device Mount Isolation (replacing bind mounts)

### Motivation

The original PoC used bind mounts (`-v /mnt/users/alice/app-budget:/workspace`) to give each container access to its subvolume. This worked for filesystem isolation, but leaked host-side mount paths into `/proc/mounts` inside the container:

```
/dev/loop0 /workspace btrfs rw,relatime,subvol=/app-budget ...
```

While the container couldn't access other paths, it could see the host mount structure in `/proc/mounts` — leaking information about the host. In production, no container should see any host path metadata.

### Approach

Instead of bind mounts, pass the loop device into the container via `--device` and mount the specific Btrfs subvolume from inside using an init script. Each container gets:

1. Access to the loop device via `--device`
2. Minimal capabilities (dropped with `--cap-drop ALL`, then specific ones added back)
3. An init script (`container-init.sh`) that mounts the subvolume, then drops privileges by exec'ing as a non-root user

### New Files Created

- **`machine/container-init.sh`** — Init script that runs as root to `mount -o subvol=<name>`, creates an `appuser`, then `exec su` to drop privileges
- **`machine/app-container/Dockerfile`** — Custom Alpine image with btrfs-progs and the init script baked in
- **`machine/Dockerfile`** updated to copy the app-container build context into the machine

### Changes to demo.sh

- **Phase 3:** Dynamically discovers loop device with `losetup -j`. Builds `platform/app-container` image using inner Docker daemon. Starts containers with `--device`, `--cap-drop ALL`, `--cap-add` for needed capabilities, and environment variables (`SUBVOL_NAME`, `LOOP_DEVICE`)
- **Phase 4:** Added `/proc/mounts` metadata isolation tests — verifies each container's `/proc/mounts` shows only its own `subvol=` entry, no other subvolume names, and no host paths like `/mnt/users/alice`
- **Phase 5, 6, 7:** All `docker exec` commands updated to use `-u root` since the workload now runs as `appuser`
- **Phase 6 rollback:** Restored container started with same device-mount pattern instead of bind mount
- **Summary:** Added "Metadata isolation verified (no host paths in /proc/mounts)" line

### Issues Encountered During This Patch

#### Issue 3: Containers crashing immediately — `--cap-drop ALL --cap-add SYS_ADMIN`

**Problem:** The patch prompt specified `--cap-drop ALL --cap-add SYS_ADMIN` as the capability pattern. Containers started but exited immediately with no output in `docker ps`.

**Debugging:** We ran the machine interactively and checked `docker logs` on the crashed container. The error was:

```
su: can't set groups: Operation not permitted
```

**Root cause:** The `su` command in `container-init.sh` needs `SETUID` and `SETGID` capabilities to switch users. `--cap-drop ALL` removes everything, and adding back only `SYS_ADMIN` isn't enough.

**Fix:** Added `--cap-add SETUID --cap-add SETGID` to all `docker run` commands. These capabilities are only used during the init script — once `exec su` replaces the process as `appuser`, all capabilities are gone (non-root users don't inherit capabilities by default in Linux).

#### Issue 4: Legacy Docker builder deprecation warning

**Problem:** The inner Docker daemon's `docker build` printed a noisy deprecation warning:

```
DEPRECATED: The legacy builder is deprecated and will be removed in a future release.
            Install the buildx component to build images with BuildKit:
            https://docs.docker.com/go/buildx/
```

**Fix:** Added `docker-buildx` to the machine Dockerfile's apt packages. The inner Docker daemon now uses BuildKit by default, suppressing the warning.

#### Detour: Attempting to eliminate SETUID/SETGID capabilities

**Context:** We noticed that adding `SETUID`/`SETGID` went against the patch prompt's explicit statement that "only SYS_ADMIN" should be needed. We explored alternatives.

**Attempt 1 — `setpriv` instead of `su`:** Replaced the `su` call with `setpriv --reuid=<uid> --regid=<gid> --clear-groups --inh-caps=-all`. Added `util-linux` to the app container for `setpriv`. **Result:** Same failure — `setpriv` also needs SETUID/SETGID at the kernel level to change UID/GID. This is a kernel requirement, not a tool limitation.

**Attempt 2 — Stay as root, drop all caps:** Instead of switching users, used `setpriv --inh-caps=-all --ambient-caps=-all --bounding-set=-all` to drop every capability while remaining UID 0. A root process with zero capabilities is effectively unprivileged. **Result:** Technically worked, but felt wrong — running as root (even capability-less) is not as clean as actually switching to a non-root user. The user preferred the explicit user switch.

**Final decision:** Reverted to the `su`-based approach with `SETUID`/`SETGID` added. These capabilities exist only during the init script's brief execution and are gone once the workload process starts as `appuser`. The security posture is correct: the running workload has zero capabilities and runs as non-root. Also reverted the `util-linux` addition to the app container since it was only needed for `setpriv`.

### Updated File Structure

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

### Final Result

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

## How to Run (poc-btrfs)

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

---

# PoC 2: DRBD Bipod Replication (`poc-drbd/`)

## Goal

Build a PoC demonstrating DRBD replication between two machines forming a bipod. Writes on the primary replicate to the secondary in real-time. When the primary dies, the secondary promotes, mounts the same Btrfs filesystem, starts containers, and all data survives. Built from `master-prompt2.md`.

## Environment

**Hetzner bare-metal machine** running Ubuntu 24.04 LTS (kernel 6.8.0-90-generic, x86_64). Docker containers simulate fleet machines, but the kernel is real. Both containers are privileged and share the host kernel.

## File Structure

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

## Host Prerequisites Installed

The user had to manually install several things on the host, and we had to work around sudo and interactive prompt issues:

### 1. DRBD kernel module

- `linux-modules-extra-6.8.0-90-generic` — Provides the in-tree DRBD 8.4 kernel module. Installed via `sudo apt install`.
- `drbd-dkms` from LINBIT PPA (`ppa:linbit/linbit-drbd9-stack`) — Provides DRBD **9.3.0** kernel module built via DKMS. This was necessary because `drbd-utils` 9.22 (the only version in Ubuntu 24.04 repos) is incompatible with the DRBD 8.4 kernel module.
- `linux-headers-6.8.0-90-generic` — Required for DKMS to build the module. Was already installed but needed verification.
- DKMS initially built for the wrong kernel (6.8.0-101). Had to run `sudo dkms install drbd/9.3.0-1ppa1~noble1 -k 6.8.0-90-generic` after installing headers.
- Final state: `modprobe drbd` loads DRBD 9.3.0 from `/lib/modules/6.8.0-90-generic/updates/dkms/drbd.ko.zst`

### 2. drbd-utils

- Installed via `sudo apt install drbd-utils`. Version 9.22.0.
- **Blocked by interactive postfix dialog**: `drbd-utils` pulls in `bsd-mailx` → `postfix`, which has an interactive debconf prompt. User had to run:
  ```bash
  echo "postfix postfix/main_mailer_type select No configuration" | sudo debconf-set-selections
  echo "postfix postfix/mailname string localhost" | sudo debconf-set-selections
  sudo DEBIAN_FRONTEND=noninteractive apt install -y drbd-utils
  ```

### 3. docker-compose-v2

- Not installed by default. Installed via `sudo apt install docker-compose-v2`.

### 4. sudo access for Claude

- Claude user initially couldn't run `modprobe`, `kill`, or `add-apt-repository` due to missing NOPASSWD entries.
- User added `/usr/sbin/modprobe`, `/usr/bin/kill` to visudo, then temporarily gave full `ALL` privileges for the DRBD 9 installation steps.

## Issues Encountered & Fixes During DRBD PoC

### Issue 5: overlay2 fails in DinD (same as poc-btrfs)

**Problem:** `master-prompt2.md` specified `--storage-driver=overlay2` saying "overlay2 works on real Linux". While true for native Docker, the DinD containers have overlay2 rootfs, and overlay-on-overlay fails with `invalid argument`.

**Fix:** Changed `entrypoint.sh` to use `--storage-driver=vfs`, same as poc-btrfs.

### Issue 6: DRBD kernel module not found in container

**Problem:** Phase 0 tried `modprobe drbd` but the container didn't have `kmod` installed. The module was loaded on the host but `modprobe` binary was missing.

**Fix:** Added `kmod` to the Dockerfile's apt packages. Also changed Phase 0 to check `/proc/drbd` first (since the module is loaded on the host and the container is privileged), falling back to `modprobe` only if needed.

### Issue 7: DRBD 8.4 vs 9.x version mismatch

**Problem:** The host kernel had DRBD 8.4.11 (from `linux-modules-extra`), but containers had `drbd-utils` 9.22.0. When `drbdadm up alice` ran, it translated to DRBD 9 kernel commands (`drbdsetup new-resource`), but the 8.4 kernel module only understands 8.4 commands. Error:

```
invalid command
Command 'drbdsetup-84 new-resource alice' terminated with exit code 20
```

**Fix:** Installed DRBD 9.3.0 kernel module from LINBIT PPA via DKMS (see Host Prerequisites above). After `sudo rmmod drbd && sudo modprobe drbd`, the 9.3.0 module loads from the DKMS `updates/` directory.

### Issue 8: `drbdadm create-md` failing silently

**Problem:** The original command `yes yes | drbdadm create-md alice 2>&1 | tail -1` showed "Operation canceled" and metadata wasn't actually created. DRBD 9 needs `--force` flag and different confirmation handling.

**Fix:** Changed to `yes | drbdadm create-md --force alice 2>&1` (removed `tail -1` for debugging, added `--force`). Metadata creation now shows "New drbd meta data block successfully created."

### Issue 9: Stale DRBD kernel state between runs

**Problem:** DRBD resources and minors are kernel-global (shared between containers). After a failed run, `/dev/drbd0` persists in the kernel. Next run fails with:

```
sysfs node '/sys/devices/virtual/block/drbd0' (already? still?) exists
Minor or volume exists already (delete it first)
```

**Fix:** Added cleanup at start of Phase 0 using `drbdsetup down alice` (not `drbdadm down` which requires a config file). Also added loop device cleanup in Phase 1.

### Issue 10: DRBD shared-kernel architecture conflict

**Problem:** Both containers share the host kernel, so DRBD resource names and minor numbers are kernel-global. When machine-1 runs `drbdadm up alice`, it creates the resource and minor 0 in the kernel. When machine-2 then runs `drbdadm up alice`, it fails because:
1. Resource `alice` already exists in the kernel
2. The minor it tries to create conflicts

Error: `Minor or volume exists already (delete it first)` for minor 1 on machine-2.

**Root cause:** `drbdadm` is designed for each host having its own kernel. It doesn't support two peers of the same resource on the same kernel.

**Attempted fix:** Replaced `drbdadm` with raw `drbdsetup`/`drbdmeta` commands, managing the entire DRBD setup from machine-1.

### Issue 11: `drbdmeta` syntax differences

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

### Issue 12: Loop device collision between containers

**Problem:** Both machine-1 and machine-2 got `/dev/loop0`. Phase 1 had cleanup code `losetup -j /data/images/alice.img | ... | losetup -d` that, when run on machine-2, detached machine-1's loop device because `losetup -j` queries the kernel's global loop device table.

**Fix:** Removed the `losetup -j` cleanup lines from Phase 1. Changed Phase 0 to clean up stale loop devices from prior runs instead.

### Issue 13: FUNDAMENTAL — DRBD replication impossible with shared kernel

**Problem:** After fixing all the command syntax issues (Issues 10-12), we hit the fundamental architectural limitation: **DRBD replication between two containers on the same host cannot work** because:

1. **DRBD is a kernel module** — there is exactly ONE instance managing ALL DRBD state
2. **Replication requires TWO separate DRBD instances** talking over TCP — one on each host, each managing its own side of the resource
3. With shared kernel, you can create ONE resource with ONE node-id, but there's nobody on the other end of the TCP connection to replicate to
4. Even with `drbdsetup new-peer` and `drbdsetup new-path`, the peer at the remote IP has no DRBD instance listening — because it's the same kernel, and the DRBD module is already configured as node 0

This is not a configuration bug — it's a fundamental limitation of running DRBD between containers that share a kernel. DRBD's architecture **requires** two separate kernels.

**Attempted workaround:** Restructured demo.sh to use DRBD as a standalone block device layer (no peer connection) on machine-1, then `dd` the backing device to machine-2 to simulate replication. This proved the failover workflow but not actual real-time replication — which is the entire point of the DRBD PoC.

### Issue 14: `mkfs.btrfs` on `/dev/drbd0` — device node visibility

**Problem:** After successfully setting up DRBD (Phase 2 showed `alice role:Primary disk:UpToDate`), `mkfs.btrfs -f /dev/drbd0` failed. The DRBD kernel module creates `/dev/drbd0` in devtmpfs, but the container may not see it if devtmpfs was mounted at container start time before DRBD was configured.

**Partial fix:** Added `mknod /dev/drbd0 b 147 0 2>/dev/null || true` to ensure the device node exists. Did not get to test this fix because we decided to switch approaches at this point.

## Decision: Abandon Docker Approach

### Why Docker fails for DRBD

The `poc-btrfs` PoC worked perfectly with Docker because Btrfs is a **filesystem** — it operates per-mount, and each container's mount is independent. DRBD is a **kernel module** with **global state** — resources, minors, and connections are shared across all containers on the same host. Docker containers share the host kernel by design.

The master-prompt2.md specification assumed containers would behave like separate machines with independent DRBD state. This is architecturally impossible with Docker on a single host.

### What we tried (Docker approach)

1. **`drbdadm up alice`** on both containers → fails, resource already exists (Issue 10)
2. **Raw `drbdsetup`** managing both sides from machine-1 → no peer to replicate to (Issue 13)
3. **`dd`-based simulation** of replication → works but doesn't prove real DRBD replication
4. Multiple `drbdmeta`/`drbdsetup` syntax fixes along the way (Issues 11, 12)

### QEMU/KVM attempt

Tried to use QEMU/KVM VMs on the current host (each VM would have its own kernel, solving the shared-kernel problem). Discovered:
- Current host is itself a VM (AMD EPYC with `hypervisor` flag, no `svm` flag)
- No nested virtualization support → `/dev/kvm` unavailable
- `sudo modprobe kvm_amd` → "Operation not supported"
- QEMU TCG (software emulation) would work but is 10-50x slower — impractical

Built an Alpine Linux base image and extracted kernel/initramfs, but abandoned this approach due to speed concerns.

### Final approach: Two Hetzner Cloud servers

The real solution: use **two actual separate servers**, each with its own kernel. This is also the closest to the production environment.

Architecture:
```
Local machine (orchestration)
  │
  ├── hcloud CLI → Hetzner Cloud API
  │
  ├── machine-1 (CX22, Ubuntu 24.04, private IP 10.0.0.2)
  │    ├── DRBD primary (/dev/drbd0)
  │    ├── Btrfs mounted at /mnt/users/alice/
  │    └── Docker containers (native, not DinD)
  │
  ├── machine-2 (CX22, Ubuntu 24.04, private IP 10.0.0.3)
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

## Hetzner Cloud Implementation

### Issues During Implementation

#### Issue 15: Hetzner server type deprecation

**Problem:** The plan specified `cx22` server type, but Hetzner deprecated the CX Gen2 and CPX Gen1 lines in EU locations as of Dec 31, 2025. `hcloud server create --type cx22` returned `server type not found`.

**Fix:** Queried the Hetzner API for available types at eu-central locations. The replacement options:
- `cpx11` (Gen1) — deprecated in EU, only available in US (ash/hil)
- `cpx12` (Gen2) — only available in Singapore
- `cx23` (Gen3, cost-optimized) — available at fsn1, nbg1, hel1
- `cpx22` (Gen2, regular) — available at fsn1, nbg1, hel1

Selected `cx23` (2 vCPU, 4GB RAM, 40GB disk, ~$0.007/hr).

#### Issue 16: fsn1 location temporarily disabled

**Problem:** With `cx23` at `fsn1`, server creation returned `server location disabled (resource_unavailable)`. Falkenstein was at capacity or under maintenance.

**Fix:** Changed location to `nbg1` (Nuremberg). Both locations are in the `eu-central` network zone, so the private network subnet worked without changes.

#### Issue 17: DRBD kernel module not compiled by DKMS

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

### Successful Run

After fixing all three issues, the full PoC ran end-to-end with **47/47 checks passed**:

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

### What Was Proven

1. **Real DRBD Protocol A replication** — actual async block-level replication over TCP between two independent servers
2. **Btrfs subvolumes as isolated container worlds** — each Docker container mounts only its own subvolume
3. **Automatic failover** — primary demotes, secondary promotes, mounts the same Btrfs filesystem
4. **Data integrity across failover** — all 3 worlds + snapshots survive intact on the new primary
5. **Container isolation survives failover** — email container still can't see budget data after promoting on a different machine
6. **Point-in-time rollback** — corrupt data, restore from snapshot, verify recovery
7. **Full lifecycle automation** — `./run.sh` handles everything: infra creation, provisioning, demo, teardown

### How to Run (poc-drbd)

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

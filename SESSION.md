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

## How to Run

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

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

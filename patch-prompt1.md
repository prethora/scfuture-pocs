# Update Prompt: Fix Container Isolation — Device Mount Instead of Bind Mount

## Working Directory

All file paths in this prompt are relative to `poc-btrfs/`. The project lives at `poc-btrfs/` in the current directory.

## Problem

The current PoC uses bind mounts to give each container access to its subvolume:

```bash
docker run -v /mnt/users/alice/app-budget:/workspace alpine ...
```

This leaks host-side mount paths into `/proc/mounts` inside the container. From inside the container:

```
/dev/loop0 /workspace btrfs rw,relatime,subvol=/app-budget ...
```

While the container can't *access* other paths, it can see the host mount structure in `/proc/mounts`, which leaks information. In production, we don't want any container seeing any host path metadata.

## Solution

Instead of bind mounts, pass the loop device into the container and mount the specific Btrfs subvolume from inside. Each container gets:

1. Access to the loop device via `--device`
2. `SYS_ADMIN` capability (needed for mount syscall)
3. An init script that mounts the subvolume, then **drops all extra capabilities** and execs the actual workload as a non-root user

This way `/proc/mounts` only shows:

```
/dev/loop0 on /workspace type btrfs (rw,subvol=app-budget)
```

No host paths. No other subvolume names. No information about the host filesystem structure.

## Changes Required

### 1. Create an init script for app containers

Create `poc-btrfs/machine/container-init.sh`:

```
#!/bin/sh
# This runs as root to perform the mount, then drops privileges.
#
# Environment variables:
#   SUBVOL_NAME  — which Btrfs subvolume to mount (e.g., "app-budget")
#   LOOP_DEVICE  — which loop device to mount from (e.g., "/dev/loop0")

set -e

# Mount the specific subvolume
mkdir -p /workspace
mount -o subvol=${SUBVOL_NAME} ${LOOP_DEVICE} /workspace

# Drop to a non-root user for the actual workload
# Create the user if it doesn't exist
adduser -D -h /workspace appuser 2>/dev/null || true

# Execute the actual command as non-root, with no extra capabilities
exec su -s /bin/sh appuser -c "exec tail -f /dev/null"
```

### 2. Update `poc-btrfs/machine/Dockerfile`

The container-init.sh needs to be available inside the app containers. The simplest approach: build a small custom image instead of using raw Alpine.

Create `poc-btrfs/machine/app-container/Dockerfile`:

```dockerfile
FROM alpine:3.19
COPY container-init.sh /container-init.sh
RUN chmod +x /container-init.sh
ENTRYPOINT ["/container-init.sh"]
```

Copy both `container-init.sh` and `app-container/Dockerfile` into the machine container so that `poc-btrfs/machine/demo.sh` can build the app image using the inner Docker daemon.

### 3. Update `poc-btrfs/machine/demo.sh` — Phase 3 (Start Isolated Containers)

Before starting containers, determine the loop device backing the Btrfs mount:

```bash
LOOP_DEV=$(losetup -j /data/images/alice.img | cut -d: -f1)
```

Then start containers like this (replacing the current `docker run` commands):

```bash
docker run -d --name alice-core \
  --device ${LOOP_DEV} \
  --cap-add SYS_ADMIN \
  --cap-drop ALL \
  --network none \
  -e SUBVOL_NAME=core \
  -e LOOP_DEVICE=${LOOP_DEV} \
  platform/app-container

docker run -d --name alice-app-email \
  --device ${LOOP_DEV} \
  --cap-add SYS_ADMIN \
  --cap-drop ALL \
  --network none \
  -e SUBVOL_NAME=app-email \
  -e LOOP_DEVICE=${LOOP_DEV} \
  platform/app-container

docker run -d --name alice-app-budget \
  --device ${LOOP_DEV} \
  --cap-add SYS_ADMIN \
  --cap-drop ALL \
  --network none \
  -e SUBVOL_NAME=app-budget \
  -e LOOP_DEVICE=${LOOP_DEV} \
  platform/app-container
```

Note: `--cap-drop ALL --cap-add SYS_ADMIN` gives only SYS_ADMIN. The init script uses it for mount, then execs as non-root (su drops the capability since the non-root user doesn't have it).

### 4. Update `poc-btrfs/machine/demo.sh` — Phase 4 (Prove Isolation)

Add a new explicit test: from inside each container, check `/proc/mounts` and verify:

1. The mount entry for `/workspace` shows `subvol=<own-subvolume-name>` — confirming the correct subvolume is mounted.
2. No other subvolume names appear anywhere in `/proc/mounts` (specifically, if we're in app-budget, the strings "app-email" and "core" should not appear in `/proc/mounts`; only "app-budget" should).
3. No host paths like `/mnt/users/alice` appear anywhere in `/proc/mounts`.

This replaces the current isolation checks. The filesystem access checks (trying to `ls` paths outside /workspace) should remain as well.

Also update the isolation tests to account for the fact that the workload now runs as `appuser` not root. Use `docker exec -u root` if any checks need root, or run checks that work as non-root.

### 5. Update `poc-btrfs/machine/demo.sh` — Phase 6 (Disaster + Rollback)

The rollback flow changes slightly. Since the container performs its own mount, after restoring the subvolume from snapshot, we just need to restart the container — it will re-run the init script and mount the restored subvolume automatically.

The stop/delete/restore/restart flow stays the same, but make sure the new container is started with the same `docker run` command (device + env vars + the custom image).

### 6. Update the summary

Add a line to the final summary:

```
  ✓ Metadata isolation verified (no host paths in /proc/mounts)
```

## Important Notes

- The loop device path (e.g., `/dev/loop0`) needs to be determined dynamically after mounting the image file, not hardcoded.
- The `--cap-drop ALL --cap-add SYS_ADMIN` pattern is correct — Docker's capability system lets you drop everything then add back specific ones.
- After `su` to `appuser`, the process no longer has SYS_ADMIN (non-root users don't inherit capabilities by default in Linux). So the mount capability is only available during init.
- In production, the DRBD device (`/dev/drbdXXX`) replaces the loop device, but the pattern is identical: pass the device in, mount the subvolume from inside.
- All existing tests should continue to pass. This is a strictly better isolation model.
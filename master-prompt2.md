# Build Prompt: DRBD Bipod Replication Proof of Concept

## Working Directory

All file paths are relative to `poc-drbd/` which lives alongside `poc-btrfs/` in the current directory.

## Environment

This runs on a **Hetzner bare-metal machine** running Ubuntu 24.04 LTS (kernel 6.8.0-90-generic, x86_64). This is the target production environment. Docker containers simulate fleet machines, but the kernel is real, DRBD runs natively, and everything behaves exactly as it will in production.

**Before running this PoC**, the following must be installed on the host (not inside containers):

```bash
sudo apt install -y drbd-utils drbd-dkms
sudo modprobe drbd
```

DRBD is a kernel module. The containers are privileged and share the host kernel, so DRBD loaded on the host is available inside all containers.

## What This Is

A proof of concept demonstrating the second foundational primitive: **DRBD replication between two machines forming a bipod**. Writes on the primary replicate to the secondary in real-time. When the primary dies, the secondary promotes, mounts the same Btrfs filesystem, starts the same isolated containers, and all data — including recent writes, subvolumes, and snapshots — survives.

This builds directly on the patterns proven in `poc-btrfs/`:
- Sparse image files with Btrfs
- Subvolumes as isolated worlds
- Device-mount pattern (containers mount their own subvolume via init script)
- Capability dropping (SYS_ADMIN + SETUID + SETGID during init, then zero)

The block device stack is now: **sparse file → loop device → DRBD → Btrfs mount**

## Architecture

```
Hetzner host (Ubuntu 24.04, kernel 6.8.0-90-generic)
  │
  ├─ machine-1 (privileged container, Ubuntu 24.04, DinD)
  │    └─ /data/images/alice.img → loop0 → DRBD primary (/dev/drbd0)
  │         └─ Btrfs mounted at /mnt/users/alice/
  │              ├─ subvol: core/         → container "alice-core"
  │              ├─ subvol: app-email/    → container "alice-app-email"
  │              ├─ subvol: app-budget/   → container "alice-app-budget"
  │              └─ snapshots/
  │
  ├─ machine-2 (privileged container, Ubuntu 24.04, DinD)
  │    └─ /data/images/alice.img → loop0 → DRBD secondary (/dev/drbd0)
  │         └─ NOT MOUNTED (receives writes from machine-1)
  │
  └─ DRBD replication (Protocol A, async) over Docker bridge network
```

After failover:

```
  machine-1: DEAD (stopped/disconnected)
  
  machine-2 (promoted to primary)
       └─ /data/images/alice.img → loop0 → DRBD primary (/dev/drbd0)
            └─ Btrfs mounted at /mnt/users/alice/
                 ├─ subvol: core/         → container "alice-core"
                 ├─ subvol: app-email/    → container "alice-app-email"
                 ├─ subvol: app-budget/   → container "alice-app-budget"
                 └─ snapshots/
                     └─ pre-failover/     (snapshot taken before machine-1 died)
```

## Directory Structure

```
poc-drbd/
├── docker-compose.yml            # Two machine services + network
├── machine/
│   ├── Dockerfile                # Ubuntu 24.04, drbd-utils, btrfs-progs, docker.io, openssh-server
│   ├── entrypoint.sh             # Starts sshd + dockerd, waits for ready, then runs demo or waits
│   ├── demo.sh                   # The full bipod test (runs on machine-1, SSHs to machine-2)
│   ├── container-init.sh         # Same as poc-btrfs: mounts subvolume, drops to appuser
│   └── app-container/
│       └── Dockerfile            # Same as poc-btrfs: Alpine + btrfs-progs + init script
```

## docker-compose.yml

Two services: `machine-1` and `machine-2`. Both privileged. Same Docker bridge network with static IPs.

```yaml
networks:
  drbd-net:
    driver: bridge
    ipam:
      config:
        - subnet: 10.20.0.0/24

services:
  machine-1:
    build: ./machine
    privileged: true
    hostname: machine-1
    volumes:
      - ./data/machine-1:/var/lib/machine-data
    networks:
      drbd-net:
        ipv4_address: 10.20.0.11
    environment:
      - NODE_ID=machine-1
      - NODE_IP=10.20.0.11
      - PEER_IP=10.20.0.12
      - ROLE=primary
      - RUN_DEMO=true

  machine-2:
    build: ./machine
    privileged: true
    hostname: machine-2
    volumes:
      - ./data/machine-2:/var/lib/machine-data
    networks:
      drbd-net:
        ipv4_address: 10.20.0.12
    environment:
      - NODE_ID=machine-2
      - NODE_IP=10.20.0.12
      - PEER_IP=10.20.0.11
      - ROLE=secondary
      - RUN_DEMO=false
```

## machine/Dockerfile

Based on Ubuntu 24.04. Install:
- `drbd-utils` (DRBD userspace tools — kernel module comes from the host)
- `btrfs-progs` (Btrfs filesystem tools)
- `docker.io` and `docker-buildx` (for DinD)
- `openssh-server` and `openssh-client` (for cross-machine coordination)
- `util-linux` (for losetup)
- `jq`, `curl`

Set up passwordless SSH between machines:
- Generate an SSH key pair during build
- Copy the public key to authorized_keys
- Disable strict host key checking

Copy all scripts and the app-container build context into the image.

## machine/entrypoint.sh

1. Start sshd (for cross-machine SSH).
2. Start dockerd in the background with `--storage-driver=overlay2` (works natively on a real Linux host).
3. Wait for Docker to be ready (poll `docker info`, timeout 30s).
4. Build the `platform/app-container` image using the inner Docker daemon.
5. If `RUN_DEMO=true` (machine-1): wait for machine-2 to be ready (poll SSH connectivity to PEER_IP), then run demo.sh.
6. If `RUN_DEMO=false` (machine-2): just keep the container alive (`tail -f /dev/null`). Machine-1 will SSH in to run commands.

## machine/container-init.sh

Same pattern as poc-btrfs but with env var renamed to `BLOCK_DEVICE` for clarity since it's now a DRBD device:

```sh
#!/bin/sh
set -e
mkdir -p /workspace
mount -o subvol=${SUBVOL_NAME} ${BLOCK_DEVICE} /workspace
adduser -D -h /workspace appuser 2>/dev/null || true
exec su -s /bin/sh appuser -c "exec tail -f /dev/null"
```

## machine/demo.sh — The Bipod Test

This script runs on machine-1 and uses SSH to execute commands on machine-2. Print clear section headers and pass/fail indicators with color. Use `set -e` to stop on failure.

Define a helper:

```bash
remote() {
    ssh -o StrictHostKeyChecking=no root@${PEER_IP} "$@"
}
```

### Phase 0: DRBD Module Check

1. Run `modprobe drbd`.
2. If it fails, print: "FATAL: DRBD kernel module not available. Run on host: sudo apt install drbd-utils drbd-dkms && sudo modprobe drbd" and exit 1.
3. Print `cat /proc/drbd` version info.
4. Print: "✓ DRBD kernel module loaded"

### Phase 1: Create Image Files on Both Machines

On machine-1 (locally):
1. `mkdir -p /data/images`
2. `truncate -s 2G /data/images/alice.img`
3. `mkfs.btrfs -f /data/images/alice.img`
4. Set up loop device: `losetup --find --show /data/images/alice.img` → capture as `$LOOP_DEV_LOCAL`
5. Print actual vs apparent size (sparse proof).

On machine-2 (via SSH):
1. Same steps: create directory, sparse image, format Btrfs, set up loop device.
2. Capture the remote loop device path.

### Phase 2: Configure and Start DRBD

**CRITICAL — DRBD minor numbers on shared kernel:** Both containers share the host kernel, so DRBD minor numbers are global. machine-1 uses minor 0 (`/dev/drbd0`), machine-2 uses minor 1 (`/dev/drbd1`).

Write to `/etc/drbd.d/alice.res` on both machines:

```
resource alice {
    net {
        protocol A;
    }
    on machine-1 {
        device /dev/drbd0 minor 0;
        disk <LOOP_DEV on machine-1>;
        address 10.20.0.11:7900;
        meta-disk internal;
    }
    on machine-2 {
        device /dev/drbd1 minor 1;
        disk <LOOP_DEV on machine-2>;
        address 10.20.0.12:7900;
        meta-disk internal;
    }
}
```

Steps on BOTH machines:
1. Write the resource config to `/etc/drbd.d/alice.res`
2. Create DRBD metadata: `yes yes | drbdadm create-md alice`
3. Bring up the resource: `drbdadm up alice`

Then on machine-1 only:
4. Force primary: `drbdadm primary --force alice`

Wait for sync:
5. Poll `drbdadm status alice` until both sides show `UpToDate`. Print progress. Timeout after 120 seconds.

Print:
```
✓ PASS: DRBD bipod established — machine-1 (Primary/UpToDate), machine-2 (Secondary/UpToDate)
```

### Phase 3: Mount Btrfs and Create Worlds on Primary

On machine-1:
1. `mkdir -p /mnt/users/alice`
2. Mount Btrfs on the DRBD device: `mount /dev/drbd0 /mnt/users/alice`
3. Create subvolumes: `core`, `app-email`, `app-budget`
4. `mkdir -p /mnt/users/alice/snapshots`
5. Seed worlds with content (same pattern as poc-btrfs):
   - core/: `config.json` with `{"agent_name": "alice-agent", "version": "1.0"}`, `memory/MEMORY.md` with sample agent memory
   - app-email/: `config.json` with `{"domain": "alice.example.com", "smtp_port": 587}`, `data/inbox.db` with sample messages
   - app-budget/: `data/transactions.db` with sample data, `src/app.py` with sample Python code
6. List subvolumes
7. Print: "✓ PASS: Worlds created on primary (machine-1)"

### Phase 4: Start Isolated Containers on Primary

On machine-1, using the device-mount pattern:
1. Start three containers with the inner Docker daemon:
   - Each with `--device /dev/drbd0`
   - Each with `--cap-drop ALL --cap-add SYS_ADMIN --cap-add SETUID --cap-add SETGID`
   - Each with `--network none`
   - Each with `SUBVOL_NAME=<name>` and `BLOCK_DEVICE=/dev/drbd0`
2. Verify all three running
3. Quick isolation spot-check on one container (we proved isolation thoroughly in poc-btrfs)
4. Print: "✓ PASS: 3 isolated containers running on primary"

### Phase 5: Simulate Agent Work

From inside the containers on machine-1 (via `docker exec -u root`):
1. `app-budget`: create `src/new-feature.py` with Python code, append new line to `data/transactions.db`
2. `app-email`: append a new message line to `data/inbox.db`
3. `core`: append a new entry to `memory/MEMORY.md`
4. Print what was written to each world
5. Check DRBD status: `drbdadm status alice` — should show Connected, UpToDate/UpToDate
6. Print: "✓ PASS: Agent work written to all 3 worlds, DRBD replicating"

### Phase 6: Take Pre-Failover Snapshot

1. `btrfs subvolume snapshot -r /mnt/users/alice/app-budget /mnt/users/alice/snapshots/pre-failover`
2. Verify: `btrfs subvolume list /mnt/users/alice`
3. Print: "✓ PASS: Pre-failover snapshot taken"

### Phase 7: Simulate Primary Death

Machine-1 "dies" — clean shutdown to prove replication completeness:
1. Stop all app containers: `docker stop alice-core alice-app-email alice-app-budget`
2. Unmount Btrfs: `umount /mnt/users/alice`
3. Demote DRBD: `drbdadm secondary alice`
4. Disconnect DRBD: `drbdadm disconnect alice`
5. Print: "⚠ machine-1 is DOWN — primary dead, DRBD disconnected"

### Phase 8: Failover — Promote Secondary

Via SSH to machine-2:
1. Disconnect from dead peer: `drbdadm disconnect alice` (may already be disconnected — don't fail if it errors)
2. Promote to primary: `drbdadm primary alice`
3. `mkdir -p /mnt/users/alice`
4. Mount Btrfs: `mount /dev/drbd1 /mnt/users/alice` (minor 1 on machine-2)
5. List subvolumes — should show all worlds + pre-failover snapshot
6. Start three app containers on machine-2's Docker daemon (via SSH):
   - Same pattern, but with `BLOCK_DEVICE=/dev/drbd1`
7. Print: "✓ PASS: machine-2 promoted to primary, containers running"

### Phase 9: Verify Data Survived Failover

Via SSH + docker exec on machine-2:

1. `app-budget`:
   - `src/new-feature.py` exists with correct content
   - `data/transactions.db` has the appended data from Phase 5
   - `src/app.py` exists (original seed)
   - Print: "✓ PASS: app-budget data survived failover"

2. `app-email`:
   - `data/inbox.db` has the new message from Phase 5
   - `config.json` intact
   - Print: "✓ PASS: app-email data survived failover"

3. `core`:
   - `memory/MEMORY.md` has the new entry from Phase 5
   - `config.json` intact
   - Print: "✓ PASS: core data survived failover"

4. Snapshot survived:
   - `btrfs subvolume list /mnt/users/alice` shows `pre-failover`
   - Print: "✓ PASS: pre-failover snapshot survived failover"

### Phase 10: Prove Rollback Works on New Primary

1. From `app-budget` on machine-2: delete `src/app.py`, overwrite `data/transactions.db` with garbage
2. Print the corrupted state
3. Stop budget container, delete subvolume, restore from `pre-failover` snapshot, restart container (with `BLOCK_DEVICE=/dev/drbd1`)
4. Verify: `src/app.py` back, correct data back, corruption gone, `src/new-feature.py` preserved
5. Print: "✓ PASS: Rollback works on new primary after failover"

### Final Summary

```
════════════════════════════════════════════
  PROOF OF CONCEPT: DRBD BIPOD — COMPLETE
════════════════════════════════════════════
  ✓ DRBD kernel module loaded
  ✓ Image files created on both machines (sparse, 2GB)
  ✓ DRBD bipod established (Protocol A, async replication)
  ✓ Btrfs mounted on primary with 3 isolated worlds
  ✓ Containers running with device-mount isolation
  ✓ Agent work written to all worlds
  ✓ Pre-failover snapshot taken
  ✓ Primary killed — DRBD disconnected
  ✓ Secondary promoted to primary
  ✓ All data verified on new primary (zero loss)
  ✓ Snapshots survived failover
  ✓ Rollback works on new primary
════════════════════════════════════════════
```

## Important Implementation Notes

- **DRBD minor numbers with shared kernel**: machine-1 uses minor 0 (`/dev/drbd0`), machine-2 uses minor 1 (`/dev/drbd1`). Both containers share the host kernel, so minor numbers are global. This is critical.
- **DRBD kernel module lives on the host**: Containers have `drbd-utils` for userspace tools (`drbdadm`), but the kernel module is loaded on the host. Containers access it because they're privileged.
- **SSH between containers**: Use the shared SSH key generated at build time. Test connectivity before starting the demo.
- **`yes yes | drbdadm create-md`**: Pipe confirmation to avoid interactive prompts.
- **overlay2 works on real Linux**: Unlike Docker Desktop, a real Linux host supports overlay2 for DinD. Use it.
- **BLOCK_DEVICE env var**: Renamed from LOOP_DEVICE (poc-btrfs) to BLOCK_DEVICE since it's now a DRBD device.
- **Color output**: Green for pass, red for fail, yellow for warnings.
- **set -e**: Stop on any failure with a clear error message.
- **Timeout on DRBD sync**: 120 seconds max. Print progress during sync.
- **Remote commands may need error tolerance**: When disconnecting DRBD on machine-2 during failover, the peer may already be gone. Use `|| true` where appropriate to avoid failing on expected errors.

## Host Prerequisites

Before running, ensure the host has DRBD and Docker:

```bash
sudo apt install -y drbd-utils drbd-dkms docker.io docker-compose-v2 btrfs-progs
sudo modprobe drbd
cat /proc/drbd   # Should show version info
```

## How to Run

```bash
cd poc-drbd
docker compose up --build
```

To explore interactively:
```bash
docker exec -it poc-drbd-machine-1-1 bash
docker exec -it poc-drbd-machine-2-1 bash
drbdadm status alice
cat /proc/drbd
```

To clean up:
```bash
docker compose down -v
rm -rf data/
```
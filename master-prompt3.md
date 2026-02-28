# BUILD PROMPT: PoC 3 — Backblaze B2 Backup & Restore

## Context

You are building Layer 3 of a distributed agent platform's proof-of-concept progression. This PoC proves the third and final tier of data safety: cold backup and restore via Backblaze B2.

**What's already proven (you don't need to re-prove these, but you build on top of them):**

- **PoC 1 (`poc-btrfs/`):** Btrfs world isolation — sparse image files, subvolumes as isolated container worlds, device-mount pattern (no metadata leakage), snapshots, rollback
- **PoC 2 (`poc-drbd/`):** DRBD bipod replication — Protocol A async replication between two Hetzner Cloud servers, failover (secondary promotes, mounts, runs containers), data + snapshot survival, rollback on new primary. 47/47 checks passed.

**What this PoC proves:**

The complete cold backup and restore path: `btrfs send → zstd compress → B2 upload → [fleet copies deleted] → B2 download → zstd decompress → btrfs receive → workspace from snapshot → containers run → DRBD bipod formed`. This is the "both machines die" scenario and the "reactivation after eviction" scenario from the architecture.

After this PoC, every data safety scenario in the architecture has a proven recovery path.

## Your working directory

You are running from the **parent directory** that contains `poc-btrfs/` and `poc-drbd/`. Create a new `poc-backblaze/` directory alongside them.

## Required environment variables

The user must provide these before running:

```bash
export HCLOUD_TOKEN="..."      # Hetzner Cloud API token
export B2_KEY_ID="..."         # Backblaze B2 application key ID
export B2_APP_KEY="..."        # Backblaze B2 application key
```

The `run.sh` script must validate all three are set before proceeding.

## Infrastructure: Hetzner Cloud via hcloud CLI

All infrastructure is orchestrated via the `hcloud` CLI. No manual server setup.

**Machines:**
- **machine-1:** Primary. Creates the user world, does backups to B2. Later: receives DRBD secondary copy after cold restore.
- **machine-2:** Starts empty. Used for cold restore — downloads from B2, restores snapshots, forms bipod with machine-1.

**Machine type:** `cx23` (this is the Gen3 replacement for the deprecated `cx22`; same as PoC 2)

**Location:** `nbg1` (Nuremberg — `fsn1` was temporarily unavailable during PoC 2; use `nbg1` for reliability)

**Private network:** `poc-backblaze-net`, subnet `10.0.0.0/24`, zone `eu-central`
- machine-1: `10.0.0.2`
- machine-2: `10.0.0.3`

**Cloud-init** provisions both machines with:
- DRBD 9 via LINBIT PPA (same as PoC 2 — `ppa:linbit/linbit-drbd9-stack`, `drbd-dkms`, `linux-headers-$(uname -r)`, `dkms autoinstall`, `modprobe drbd`)
- Pre-seed postfix debconf before installing `drbd-utils` (same as PoC 2)
- `btrfs-progs`
- `docker.io`
- `zstd`
- `jq`
- `curl`
- `pip3` and `pipx` — install the Backblaze B2 CLI via `pipx install b2` (pipx keeps it isolated). If `pipx` is unavailable, fall back to `pip3 install --break-system-packages b2[full]`
- Verify: `b2 version` or `b2 account --help` should work after install. Note: B2 CLI v4 uses subcommands like `b2 account authorize`, `b2 file upload`, `b2 file cat`, `b2 bucket create`, etc.

**IMPORTANT — DRBD on Ubuntu 24.04:** The in-tree kernel module is DRBD 8.4, which is incompatible with `drbd-utils` 9.22. You MUST install from the LINBIT PPA and ensure `linux-headers-$(uname -r)` matches the running kernel before DKMS build. See PoC 2's cloud-init for the exact sequence. After cloud-init, verify with `cat /proc/drbd` or `drbdadm --version` showing 9.x.

## File structure

```
poc-backblaze/
├── run.sh                       # Main: validate env → infra up → demo → teardown
├── infra.sh                     # Hetzner lifecycle: up / down / status
├── cloud-init.yaml              # Server provisioning
├── scripts/
│   ├── demo.sh                  # The phased demo (runs locally, SSHes into machines)
│   ├── container-init.sh        # Device-mount init script (from PoC 1/2 pattern)
│   └── app-container/
│       └── Dockerfile           # Isolated world container image (from PoC 1/2 pattern)
└── BUILD_PROMPT.md              # This file
```

## run.sh behavior

```bash
#!/usr/bin/env bash
set -euo pipefail

# 1. Validate HCLOUD_TOKEN, B2_KEY_ID, B2_APP_KEY are set
# 2. Check hcloud CLI is installed
# 3. Generate a random bucket name: poc-backblaze-${RANDOM_SUFFIX}
#    Store it in a state file (.bucket-name) for teardown
# 4. Call infra.sh up (create network + servers, wait for cloud-init)
# 5. SCP scripts/ to both machines
# 6. Run demo.sh (which SSHes into machines for each phase)
# 7. teardown (always, via trap):
#    - Delete all objects in B2 bucket, delete bucket
#    - Call infra.sh down (delete servers + network)
#    - Remove local state files
```

The trap must ensure teardown runs even on failure. B2 cleanup should be best-effort (bucket might not exist yet if failure was early).

## infra.sh behavior

```bash
# infra.sh up:
#   1. Create network: hcloud network create --name poc-backblaze-net --ip-range 10.0.0.0/8
#   2. Create subnet: hcloud network add-subnet poc-backblaze-net --type cloud --network-zone eu-central --ip-range 10.0.0.0/24
#   3. Create machine-1: hcloud server create --name poc-b2-machine-1 --type cx23 --image ubuntu-24.04 --location nbg1 --user-data-from-file cloud-init.yaml --network poc-backblaze-net
#   4. Create machine-2: same, --name poc-b2-machine-2
#   5. Attach private IPs (if not auto-assigned correctly):
#      hcloud server attach-to-network poc-b2-machine-1 --network poc-backblaze-net --ip 10.0.0.2
#      (hcloud may do this during create with --network; check behavior)
#   6. Wait for cloud-init to finish on both machines:
#      Loop SSH: "cloud-init status --wait" or check for /var/lib/cloud/instance/boot-finished
#   7. Verify DRBD, Docker, btrfs-progs, b2, zstd on both machines
#   8. Output public IPs for SSH access

# infra.sh down:
#   1. Delete servers: hcloud server delete poc-b2-machine-1, poc-b2-machine-2
#   2. Delete network: hcloud network delete poc-backblaze-net
#   Both with --quiet or || true to handle already-deleted

# infra.sh status:
#   Show server and network status
```

## B2 bucket structure (from architecture Section 14.1)

```
b2://{bucket-name}/users/alice/
  ├── layer-000.btrfs.zst              (full send of base snapshot)
  ├── layer-001.btrfs.zst              (incremental from layer-000)
  ├── auto-backup-latest.btrfs.zst     (incremental from layer-001)
  └── manifest.json                     (snapshot chain metadata)
```

**manifest.json format:**
```json
{
  "user_id": "alice",
  "chain": [
    {
      "snapshot": "layer-000",
      "type": "full",
      "parent": null,
      "key": "users/alice/layer-000.btrfs.zst",
      "size_bytes": 0
    },
    {
      "snapshot": "layer-001",
      "type": "incremental",
      "parent": "layer-000",
      "key": "users/alice/layer-001.btrfs.zst",
      "size_bytes": 0
    }
  ]
}
```

The `size_bytes` fields get filled in after upload.

## B2 CLI usage (v4 subcommand style)

```bash
# Authorize (run once per machine that talks to B2)
b2 account authorize "$B2_KEY_ID" "$B2_APP_KEY"

# Create bucket (allPublic is fine for a PoC; use allPrivate + app key in production)
b2 bucket create "$BUCKET_NAME" allPrivate

# Upload a file (use temp file approach for reliability)
btrfs send [-p parent_snap] snapshot | zstd > /tmp/backup.btrfs.zst
b2 file upload "$BUCKET_NAME" /tmp/backup.btrfs.zst "users/alice/layer-000.btrfs.zst"
rm /tmp/backup.btrfs.zst

# Upload manifest.json
b2 file upload "$BUCKET_NAME" /tmp/manifest.json "users/alice/manifest.json"

# Download a file
b2 file download "b2://$BUCKET_NAME/users/alice/layer-000.btrfs.zst" /tmp/layer-000.btrfs.zst
zstd -d /tmp/layer-000.btrfs.zst -o /tmp/layer-000.btrfs
btrfs receive /mnt/users/alice/snapshots/ < /tmp/layer-000.btrfs

# Alternative: b2 cat for streaming download (check if available)
# b2 cat "b2://$BUCKET_NAME/users/alice/layer-000.btrfs.zst" | zstd -d | btrfs receive /path/

# List bucket contents
b2 ls "$BUCKET_NAME" --recursive

# Delete all files in bucket (for teardown)
b2 rm --recursive --no-progress "b2://$BUCKET_NAME"

# Delete bucket
b2 bucket delete "$BUCKET_NAME"
```

**IMPORTANT:** The `b2` CLI v4 changed command names. The old `b2 authorize-account` is now `b2 account authorize`. The old `b2 upload-file` is now `b2 file upload`. The old `b2 download-file-by-name` is now `b2 file download`. Check `b2 --help` and adapt if needed. The script should handle both old and new CLI versions gracefully, or just target v4.

**IMPORTANT:** Use temp files for upload/download rather than streaming pipes. This is more reliable for a PoC. The temp file approach also lets us capture and verify file sizes. Note in the output that production should use streaming for large deltas.

## Demo phases

The demo script runs locally and SSHes into the machines. Use a helper function for SSH commands, same pattern as PoC 2.

Each check should be explicitly numbered and output `[CHECK NN] PASS: description` or `[CHECK NN] FAIL: description`. Track total pass/fail count. Exit non-zero if any check fails.

### Phase 0: Prerequisites (both machines)

Verify on both machines:
- DRBD module loaded (`lsmod | grep drbd` or `cat /proc/drbd`)
- Docker running (`docker info`)
- `btrfs` command available
- `zstd` command available
- `jq` command available
- `b2` CLI available (`b2 version`)

On machine-1 only (the one doing backups):
- Authorize B2: `b2 account authorize "$B2_KEY_ID" "$B2_APP_KEY"`
- Create bucket: `b2 bucket create "$BUCKET_NAME" allPrivate`
- Verify bucket exists: `b2 bucket list | grep "$BUCKET_NAME"`

### Phase 1: Create user world on machine-1

This follows the same pattern as PoC 1/2:

```bash
# On machine-1:
truncate -s 2G /data/images/alice.img
LOOP=$(losetup --find --show /data/images/alice.img)
mkfs.btrfs -f "$LOOP"
mkdir -p /mnt/users/alice
mount "$LOOP" /mnt/users/alice

# Create workspace subvolume
btrfs subvolume create /mnt/users/alice/workspace

# Create world subvolumes inside workspace
# (For this PoC, worlds are directories inside workspace, not separate subvolumes,
#  to keep btrfs send/receive simple — subvolume-per-world is a container isolation
#  concern proven in PoC 1/2)
mkdir -p /mnt/users/alice/workspace/{memory,apps,data}
mkdir -p /mnt/users/alice/workspace/apps/{core,email,budget}

# Seed data
echo '{"agent": "alice", "version": "1.0"}' > /mnt/users/alice/workspace/data/config.json
echo "# Agent Memory\n\nInitial setup complete." > /mnt/users/alice/workspace/memory/MEMORY.md
echo "Welcome to your core world." > /mnt/users/alice/workspace/apps/core/index.html
echo "Email app placeholder." > /mnt/users/alice/workspace/apps/email/index.html
echo "Budget app placeholder." > /mnt/users/alice/workspace/apps/budget/index.html

# Create snapshots directory
mkdir -p /mnt/users/alice/snapshots

# Take layer-000 snapshot (initial state)
btrfs subvolume snapshot -r /mnt/users/alice/workspace /mnt/users/alice/snapshots/layer-000
```

**Checks:**
- Image file exists and is sparse (apparent size 2G, actual < 10MB)
- Loop device attached
- Btrfs mounted
- Workspace subvolume exists
- All seed data files present
- `layer-000` snapshot exists and is read-only

### Phase 2: Full backup — layer-000 to B2

```bash
# On machine-1:
# Full send (no parent)
btrfs send /mnt/users/alice/snapshots/layer-000 | zstd > /tmp/layer-000.btrfs.zst

# Upload to B2
b2 file upload "$BUCKET_NAME" /tmp/layer-000.btrfs.zst "users/alice/layer-000.btrfs.zst"

# Record size
LAYER0_SIZE=$(stat -c%s /tmp/layer-000.btrfs.zst)

# Initialize manifest
cat > /tmp/manifest.json << EOF
{
  "user_id": "alice",
  "chain": [
    {
      "snapshot": "layer-000",
      "type": "full",
      "parent": null,
      "key": "users/alice/layer-000.btrfs.zst",
      "size_bytes": $LAYER0_SIZE
    }
  ]
}
EOF

b2 file upload "$BUCKET_NAME" /tmp/manifest.json "users/alice/manifest.json"

rm /tmp/layer-000.btrfs.zst /tmp/manifest.json
```

**Checks:**
- `btrfs send` succeeded (exit code 0)
- zstd compressed file created
- B2 upload succeeded
- File exists in B2 bucket (`b2 ls` shows it)
- Size > 0
- manifest.json uploaded

### Phase 3: Simulate agent work + create layer-001

```bash
# On machine-1: simulate agent activity
echo "## Session 1\n\nUser asked about weather. Responded with forecast." >> /mnt/users/alice/workspace/memory/MEMORY.md
echo "<h1>Inbox</h1><p>From: boss@work.com — Q3 report due Friday</p>" > /mnt/users/alice/workspace/apps/email/inbox.html
echo '{"transactions": [{"date": "2026-02-28", "amount": -42.50, "desc": "Groceries"}]}' > /mnt/users/alice/workspace/apps/budget/ledger.json
echo '{"agent": "alice", "version": "1.1", "last_active": "2026-02-28T10:00:00Z"}' > /mnt/users/alice/workspace/data/config.json

# Take layer-001 snapshot
btrfs subvolume snapshot -r /mnt/users/alice/workspace /mnt/users/alice/snapshots/layer-001
```

**Checks:**
- New data present in workspace
- `layer-001` snapshot exists and is read-only
- Snapshot contains the new data (verify by reading from snapshot path)

### Phase 4: Incremental backup — layer-001 to B2

```bash
# On machine-1:
# Incremental send (layer-000 is parent)
btrfs send -p /mnt/users/alice/snapshots/layer-000 \
              /mnt/users/alice/snapshots/layer-001 | zstd > /tmp/layer-001.btrfs.zst

LAYER1_SIZE=$(stat -c%s /tmp/layer-001.btrfs.zst)

b2 file upload "$BUCKET_NAME" /tmp/layer-001.btrfs.zst "users/alice/layer-001.btrfs.zst"

# Update manifest
# Download current, add entry, re-upload
b2 file download "b2://$BUCKET_NAME/users/alice/manifest.json" /tmp/manifest.json
# Use jq to append to chain array
jq --argjson size "$LAYER1_SIZE" '.chain += [{
  "snapshot": "layer-001",
  "type": "incremental",
  "parent": "layer-000",
  "key": "users/alice/layer-001.btrfs.zst",
  "size_bytes": $size
}]' /tmp/manifest.json > /tmp/manifest-updated.json

b2 file upload "$BUCKET_NAME" /tmp/manifest-updated.json "users/alice/manifest.json"

rm /tmp/layer-001.btrfs.zst /tmp/manifest.json /tmp/manifest-updated.json
```

**Checks:**
- Incremental send succeeded
- Incremental file size < full file size (should be significantly smaller since only delta)
- Upload succeeded
- B2 has both layer-000 and layer-001 files
- manifest.json has 2 entries in chain

### Phase 5: More agent work + auto-backup

```bash
# On machine-1: more agent activity
echo "## Session 2\n\nUser set up budget alerts for >$100 transactions." >> /mnt/users/alice/workspace/memory/MEMORY.md
echo '{"transactions": [{"date": "2026-02-28", "amount": -42.50, "desc": "Groceries"}, {"date": "2026-02-28", "amount": -150.00, "desc": "Electric bill"}]}' > /mnt/users/alice/workspace/apps/budget/ledger.json
echo '{"alerts": [{"type": "threshold", "amount": 100}]}' > /mnt/users/alice/workspace/apps/budget/alerts.json
echo '{"agent": "alice", "version": "1.2", "last_active": "2026-02-28T14:00:00Z"}' > /mnt/users/alice/workspace/data/config.json

# Take auto-backup snapshot
btrfs subvolume snapshot -r /mnt/users/alice/workspace /mnt/users/alice/snapshots/auto-backup-latest

# Incremental from layer-001
btrfs send -p /mnt/users/alice/snapshots/layer-001 \
              /mnt/users/alice/snapshots/auto-backup-latest | zstd > /tmp/auto-backup-latest.btrfs.zst

AUTO_SIZE=$(stat -c%s /tmp/auto-backup-latest.btrfs.zst)

b2 file upload "$BUCKET_NAME" /tmp/auto-backup-latest.btrfs.zst "users/alice/auto-backup-latest.btrfs.zst"

# Update manifest
b2 file download "b2://$BUCKET_NAME/users/alice/manifest.json" /tmp/manifest.json
jq --argjson size "$AUTO_SIZE" '.chain += [{
  "snapshot": "auto-backup-latest",
  "type": "incremental",
  "parent": "layer-001",
  "key": "users/alice/auto-backup-latest.btrfs.zst",
  "size_bytes": $size
}]' /tmp/manifest.json > /tmp/manifest-updated.json

b2 file upload "$BUCKET_NAME" /tmp/manifest-updated.json "users/alice/manifest.json"

rm /tmp/auto-backup-latest.btrfs.zst /tmp/manifest.json /tmp/manifest-updated.json
```

**Checks:**
- Auto-backup snapshot created
- Incremental upload succeeded
- manifest.json now has 3 entries in chain
- Chain ordering is correct: layer-000 (full) → layer-001 (incr, parent=layer-000) → auto-backup-latest (incr, parent=layer-001)

### Phase 6: Verify B2 bucket contents

```bash
# On machine-1:
# List all files
b2 ls --recursive "$BUCKET_NAME"

# Download and verify manifest
b2 file download "b2://$BUCKET_NAME/users/alice/manifest.json" /tmp/verify-manifest.json
cat /tmp/verify-manifest.json | jq .
```

**Checks:**
- Bucket contains exactly 4 files: layer-000.btrfs.zst, layer-001.btrfs.zst, auto-backup-latest.btrfs.zst, manifest.json
- manifest.json is valid JSON
- manifest.json chain has 3 entries
- All file keys in manifest match actual B2 objects
- All size_bytes > 0

### Phase 7: Simulate total loss — destroy machine-1's data

This simulates the eviction scenario — all fleet copies are gone, only B2 remains.

```bash
# On machine-1:
umount /mnt/users/alice
losetup -d "$LOOP"   # detach loop device
rm -f /data/images/alice.img
rmdir /mnt/users/alice 2>/dev/null || true
```

**Checks:**
- `/mnt/users/alice` is not mounted
- `/data/images/alice.img` does not exist
- Data is irrecoverable on machine-1

### Phase 8: Cold restore on machine-2

This follows architecture Section 14.4 exactly.

```bash
# On machine-2:
# Authorize B2
b2 account authorize "$B2_KEY_ID" "$B2_APP_KEY"

# Step 1: Create fresh empty image
truncate -s 2G /data/images/alice.img
LOOP=$(losetup --find --show /data/images/alice.img)
mkfs.btrfs -f "$LOOP"
mkdir -p /mnt/users/alice
mount "$LOOP" /mnt/users/alice

# Create snapshots directory for receiving
mkdir -p /mnt/users/alice/snapshots

# Step 2: Download manifest
b2 file download "b2://$BUCKET_NAME/users/alice/manifest.json" /tmp/manifest.json

# Step 3: Download and apply snapshot chain IN ORDER
# Parse manifest with jq, iterate chain entries in order

# Apply layer-000 (full)
b2 file download "b2://$BUCKET_NAME/users/alice/layer-000.btrfs.zst" /tmp/layer-000.btrfs.zst
zstd -d /tmp/layer-000.btrfs.zst -o /tmp/layer-000.btrfs
btrfs receive /mnt/users/alice/snapshots/ < /tmp/layer-000.btrfs
rm /tmp/layer-000.btrfs.zst /tmp/layer-000.btrfs

# Apply layer-001 (incremental, requires layer-000 present)
b2 file download "b2://$BUCKET_NAME/users/alice/layer-001.btrfs.zst" /tmp/layer-001.btrfs.zst
zstd -d /tmp/layer-001.btrfs.zst -o /tmp/layer-001.btrfs
btrfs receive /mnt/users/alice/snapshots/ < /tmp/layer-001.btrfs
rm /tmp/layer-001.btrfs.zst /tmp/layer-001.btrfs

# Apply auto-backup-latest (incremental, requires layer-001 present)
b2 file download "b2://$BUCKET_NAME/users/alice/auto-backup-latest.btrfs.zst" /tmp/auto-backup-latest.btrfs.zst
zstd -d /tmp/auto-backup-latest.btrfs.zst -o /tmp/auto-backup-latest.btrfs
btrfs receive /mnt/users/alice/snapshots/ < /tmp/auto-backup-latest.btrfs
rm /tmp/auto-backup-latest.btrfs.zst /tmp/auto-backup-latest.btrfs

# Step 4: Create workspace from latest snapshot
btrfs subvolume snapshot /mnt/users/alice/snapshots/auto-backup-latest \
                         /mnt/users/alice/workspace
```

**Checks:**
- Fresh image created on machine-2
- All 3 snapshots received successfully (each `btrfs receive` exit code 0)
- Snapshots exist: `btrfs subvolume list /mnt/users/alice` shows all 3 snapshots + workspace
- Workspace created from latest snapshot
- **Data integrity — ALL data from ALL phases survives:**
  - `config.json` has version 1.2, last_active timestamp
  - `MEMORY.md` contains Session 1 and Session 2 entries
  - `apps/core/index.html` exists with original content
  - `apps/email/inbox.html` has the boss's email
  - `apps/budget/ledger.json` has both transactions
  - `apps/budget/alerts.json` has the threshold alert
- Layer-000 data is also accessible (verify by reading snapshot)

### Phase 9: Start containers on machine-2

Build and run the isolated world containers using the device-mount pattern from PoC 1/2.

```bash
# On machine-2:
# Build the app container image
docker build -t platform/app-container /path/to/app-container/

# Start 3 containers (core, email, budget)
# Each sees only its portion of the workspace
docker run -d --name alice-core \
  --device "$LOOP" \
  --cap-drop ALL --cap-add SYS_ADMIN --cap-add SETUID --cap-add SETGID \
  --network none \
  -e SUBVOL_NAME=workspace -e BLOCK_DEVICE="$LOOP" -e APP_DIR=apps/core \
  platform/app-container

# ... similar for email and budget
```

**Note on container isolation approach:** In PoC 1/2, each world was a separate Btrfs subvolume mounted independently. For this PoC, the key thing being tested is B2 backup/restore, not container isolation (already proven). So it's acceptable to mount the workspace subvolume and use directory-level separation, OR to replicate the full subvolume-per-world pattern from PoC 1/2. Choose whichever is simpler. The important thing is that containers start and can access the restored data.

**Simplified alternative:** If the full device-mount pattern adds too much complexity to this PoC, it's acceptable to simply verify data access by running commands inside a basic container with a bind mount. The device-mount pattern is already proven. The critical check here is that the *restored data* is usable, not that the container isolation still works (it does — PoC 1/2 proved it).

**Checks:**
- At least one container starts successfully
- Container can read the restored data (config.json, MEMORY.md, etc.)
- Data matches what was written in phases 3 and 5

### Phase 10: Form bipod from restored data

Now we prove the full cold restore path ends with a properly replicated bipod.

```bash
# On machine-1: create a fresh empty image (it was destroyed in phase 7)
truncate -s 2G /data/images/alice.img
LOOP1=$(losetup --find --show /data/images/alice.img)

# Configure DRBD on both machines
# machine-2 has the data (primary), machine-1 is the new secondary

# On both machines: write DRBD resource config
# /etc/drbd.d/alice.res:
cat > /etc/drbd.d/alice.res << 'EOF'
resource alice {
    net {
        protocol A;
    }
    disk {
        on-io-error detach;
    }
    on poc-b2-machine-1 {
        device /dev/drbd0 minor 0;
        disk /data/images/alice.img;
        address 10.0.0.2:7900;
        meta-disk internal;
    }
    on poc-b2-machine-2 {
        device /dev/drbd0 minor 0;
        disk /data/images/alice.img;
        address 10.0.0.3:7900;
        meta-disk internal;
    }
}
EOF

# On machine-2 (has data): stop containers first, unmount Btrfs
docker stop alice-core  # (and any other containers)
umount /mnt/users/alice
losetup -d "$LOOP2"     # detach the current loop device

# On both machines: create DRBD metadata
drbdadm create-md --force alice

# On both machines: bring up DRBD
drbdadm up alice

# On machine-2: promote to primary (it has the real data)
# Use --force for initial promotion when both sides are Inconsistent
drbdadm primary --force alice

# Wait for initial sync (machine-2 → machine-1)
# Monitor: drbdadm status alice
# Wait until both are UpToDate

# On machine-2: mount Btrfs on DRBD device
mount /dev/drbd0 /mnt/users/alice
```

**IMPORTANT:** When unmounting Btrfs on machine-2 to set up DRBD, the loop device must be detached and the image file must be used directly as DRBD's backing disk. DRBD needs the raw image file (it manages its own access). After DRBD is configured and machine-2 is promoted to primary, mount Btrfs on `/dev/drbd0` (not the loop device).

**Checks:**
- DRBD resource configured on both machines
- DRBD metadata created on both
- DRBD connected (both show Connected)
- machine-2 is Primary, machine-1 is Secondary
- Initial sync completes (both UpToDate)
- Btrfs mounted on machine-2 via `/dev/drbd0`

### Phase 11: Verify bipod + data integrity

```bash
# On machine-2 (primary):
# Verify data still intact after DRBD layer insertion
cat /mnt/users/alice/workspace/data/config.json
cat /mnt/users/alice/workspace/memory/MEMORY.md

# Take a new snapshot to verify Btrfs operations work on DRBD
btrfs subvolume snapshot -r /mnt/users/alice/workspace \
                            /mnt/users/alice/snapshots/post-restore-001

# Verify DRBD status
drbdadm status alice
```

**Checks:**
- All data intact on the DRBD-mounted filesystem
- New snapshot creation succeeds (Btrfs operates normally on DRBD device)
- DRBD shows both nodes UpToDate
- `btrfs subvolume list` shows all original snapshots + workspace + new snapshot

### Phase 12: Negative test — chain ordering matters

This proves that the manifest ordering is critical and that out-of-order receive fails.

```bash
# On machine-2 (or a temp mount):
# Create a fresh image to test out-of-order receive
truncate -s 1G /tmp/test-order.img
mkfs.btrfs -f /tmp/test-order.img
mkdir -p /tmp/test-order-mnt
mount -o loop /tmp/test-order.img /tmp/test-order-mnt
mkdir -p /tmp/test-order-mnt/snapshots

# Download layer-001 (incremental) WITHOUT first applying layer-000
b2 file download "b2://$BUCKET_NAME/users/alice/layer-001.btrfs.zst" /tmp/layer-001.btrfs.zst
zstd -d /tmp/layer-001.btrfs.zst -o /tmp/layer-001.btrfs

# Attempt to receive — THIS SHOULD FAIL
btrfs receive /tmp/test-order-mnt/snapshots/ < /tmp/layer-001.btrfs  # Expected: error

# Clean up
umount /tmp/test-order-mnt
rm -f /tmp/test-order.img /tmp/layer-001.btrfs.zst /tmp/layer-001.btrfs
```

**Checks:**
- `btrfs receive` of incremental without parent FAILS (non-zero exit code)
- Error message mentions missing parent subvolume
- This confirms chain ordering in manifest.json is mandatory

## Summary of expected checks

| Phase | Description | Approx checks |
|-------|-------------|---------------|
| 0 | Prerequisites | 8 |
| 1 | Create user world | 6 |
| 2 | Full backup to B2 | 5 |
| 3 | Agent work + layer-001 | 3 |
| 4 | Incremental backup | 4 |
| 5 | More work + auto-backup | 4 |
| 6 | Verify B2 contents | 5 |
| 7 | Destroy machine-1 data | 3 |
| 8 | Cold restore on machine-2 | 8 |
| 9 | Start containers | 3 |
| 10 | Form bipod | 6 |
| 11 | Verify bipod + integrity | 4 |
| 12 | Negative test (ordering) | 2 |
| **Total** | | **~61 checks** |

## Container files (reuse from PoC 1/2)

### container-init.sh

```bash
#!/bin/bash
set -e
# Mount the subvolume from the block device
mount -t btrfs -o subvol="$SUBVOL_NAME" "$BLOCK_DEVICE" /workspace
# Drop to non-root user
exec su -s /bin/bash appuser -c "cd /workspace && exec $@"
```

Adapt as needed — the exact init script depends on whether you're using subvolume-per-world or directory-per-world for this PoC. The critical pattern is: mount from block device → drop privileges → run workload.

### app-container/Dockerfile

```dockerfile
FROM ubuntu:24.04
RUN apt-get update && apt-get install -y btrfs-progs && rm -rf /var/lib/apt/lists/*
RUN useradd -m appuser
RUN mkdir /workspace
COPY container-init.sh /init.sh
RUN chmod +x /init.sh
ENTRYPOINT ["/init.sh"]
CMD ["bash", "-c", "echo 'Container running'; sleep infinity"]
```

## Production notes (include in PoC output)

After the PoC passes, note these for production:

1. **Streaming uploads:** Production should pipe `btrfs send | zstd | b2 upload-unbound-stream` directly instead of using temp files. Temp files require disk space proportional to the snapshot delta size.
2. **Parallel restore:** For long chains, snapshots could be downloaded in parallel (but must be applied sequentially). Download the next one while applying the current one.
3. **Chain maintenance:** Every 10 layer snapshots or every month, upload a fresh full send. This resets the chain and keeps cold restores fast. Delete old incrementals that are superseded by the new full.
4. **Encryption:** B2 supports server-side encryption. Production should enable SSE-B2 or use client-side encryption before upload.
5. **Retry logic:** All B2 operations should have exponential backoff retry in production.
6. **Bandwidth:** `btrfs send` on a read-only snapshot doesn't interfere with live agent writes. The I/O cost is minimal. Network bandwidth to B2 is separate from inter-machine DRBD traffic.

## How to run

```bash
export HCLOUD_TOKEN="your-hetzner-api-token"
export B2_KEY_ID="your-backblaze-key-id"
export B2_APP_KEY="your-backblaze-application-key"

cd poc-backblaze
./run.sh
```

Expected runtime: ~10-15 minutes (most time is cloud-init and DRBD sync). Cost: ~€0.02 for Hetzner (2 CX23 servers for ~15 min) + negligible B2 storage (a few MB for minutes).

The script creates everything, runs all tests, and tears everything down. If you want to inspect state between phases, edit `run.sh` to pause or comment out teardown.
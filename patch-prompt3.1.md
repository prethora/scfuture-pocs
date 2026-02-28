# PATCH PROMPT: PoC 3 — Fix Cold Restore Ordering (DRBD Before Filesystem)

## Context

You are patching `poc-backblaze/` which currently has 64/64 checks passing. The PoC works but uses a suboptimal approach for the cold restore → bipod formation path.

**The problem:** Phases 8 and 10 are currently split — Phase 8 restores Btrfs data onto a plain loop device (no DRBD), then Phase 10 retrofits DRBD on top of existing data. This required external metadata devices (`meta-disk /dev/loopN` with a separate 128MB image) to avoid DRBD's internal metadata overwriting the Btrfs superblock. This is unnecessary complexity and creates a different code path from normal provisioning.

**The fix:** Reorder so that DRBD is set up on blank devices FIRST (with `meta-disk internal`, same as normal provisioning), THEN format Btrfs on `/dev/drbd0`, THEN `btrfs receive` the snapshots. This makes cold restore use the exact same block device stack as normal provisioning:

```
sparse file → loop device → DRBD (meta-disk internal) → /dev/drbd0 → Btrfs → btrfs receive
```

No external metadata files. No special cases. One architecture for all paths.

## What to change

The current flow across Phases 8 and 10 is:

```
Phase 8 (machine-2 only):
  1. Create image → loop device → mkfs.btrfs → mount
  2. btrfs receive all snapshots
  3. Create workspace from latest snapshot
  4. Verify data integrity

Phase 9:
  Start containers on machine-2 (using loop device)

Phase 10 (both machines):
  1. Stop containers on machine-2
  2. Unmount Btrfs on machine-2
  3. Create external metadata images on both machines (128MB each)
  4. Loop devices for metadata
  5. Loop device for machine-1's blank image
  6. DRBD config with meta-disk /dev/loopN (external)
  7. create-md --force on both (force needed because machine-2 has data)
  8. Promote machine-2, mount /dev/drbd0, sync
```

The new flow should merge Phases 8 and 10 into a single sequence:

```
New Phase 8 — Cold Restore with DRBD (both machines):
  1. Create blank image on BOTH machines (2G sparse each)
  2. Loop device on both
  3. Write DRBD config on both — meta-disk internal (same as poc-drbd)
  4. drbdadm create-md alice (both — images are blank, no --force needed)
  5. drbdadm up alice (both)
  6. Promote machine-2 to primary (arbitrary choice — both are blank and identical)
  7. mkfs.btrfs on /dev/drbd0 (machine-2)
  8. Mount /dev/drbd0 on machine-2
  9. mkdir snapshots directory
  10. btrfs receive all 3 snapshots in chain order (from B2, same download logic as before)
  11. Create workspace from latest snapshot
  12. Verify all data integrity (same checks as current Phase 8)
  -- DRBD is already replicating everything to machine-1 as the receives happen --

New Phase 9 — Verify Bipod is Synced:
  Wait for DRBD sync to complete (both UpToDate)
  -- All the btrfs receive writes have been replicated to machine-1 --

New Phase 10 — Start Containers:
  Start containers on machine-2 (using /dev/drbd0 mount, not raw loop)
  Same container checks as current Phase 9

New Phase 11 — Verify Bipod + Data Integrity:
  Same as current Phase 11, but no need to re-mount — already mounted on /dev/drbd0
  Verify data, take new snapshot, verify subvolume count

New Phase 12 — Negative Test (chain ordering):
  Same as current — unchanged
```

## Specific changes needed

### 1. Remove all external metadata logic

Delete from `demo.sh`:
- Creation of `alice-drbd-meta.img` files (128MB metadata images)
- Loop device setup for metadata images
- Any `meta-disk /dev/loop*` references that point to metadata devices
- The `--force` flag on `drbdadm create-md` (no longer needed — images are blank)

### 2. Rewrite Phase 8

Phase 8 now does DRBD setup + Btrfs format + cold restore all in one sequence.

On **both** machines:
```bash
# Create blank image
mkdir -p /data/images
truncate -s 2G /data/images/alice.img
LOOP=$(losetup --find --show /data/images/alice.img)
```

On **both** machines, write the DRBD config — same format as poc-drbd, using `meta-disk internal`:
```
resource alice {
    net { protocol A; }
    disk { on-io-error detach; }
    on poc-b2-machine-1 {
        device /dev/drbd0 minor 0;
        disk $LOOP1_PATH;
        address 10.0.0.2:7900;
        meta-disk internal;
    }
    on poc-b2-machine-2 {
        device /dev/drbd0 minor 0;
        disk $LOOP2_PATH;
        address 10.0.0.3:7900;
        meta-disk internal;
    }
}
```

On **both** machines:
```bash
drbdadm create-md alice          # No --force needed — blank images
drbdadm up alice
```

On **machine-2** (will be primary):
```bash
drbdadm primary --force alice    # --force for initial promotion when both Inconsistent
mkfs.btrfs -f /dev/drbd0
mkdir -p /mnt/users/alice
mount /dev/drbd0 /mnt/users/alice
mkdir -p /mnt/users/alice/snapshots
```

Then the B2 download + `btrfs receive` chain — same logic as the current Phase 8, but now writing to a DRBD-backed mount:
```bash
# Download and apply each snapshot in chain order from manifest.json
# layer-000 (full), layer-001 (incremental), auto-backup-latest (incremental)
# Each: b2 download → zstd decompress → btrfs receive /mnt/users/alice/snapshots/
```

Then:
```bash
btrfs subvolume snapshot /mnt/users/alice/snapshots/auto-backup-latest \
                         /mnt/users/alice/workspace
```

All the data integrity checks remain the same.

### 3. New Phase 9 — Verify DRBD Sync

Wait for machine-1 (secondary) to finish syncing. All the `btrfs receive` writes on machine-2 were replicated via DRBD Protocol A in real-time.

```bash
# Poll drbdadm status alice until both UpToDate
# Same pattern as poc-drbd's sync wait
```

Checks:
- DRBD connected
- Both nodes UpToDate
- machine-2 is Primary, machine-1 is Secondary

### 4. Renumber phases

The old flow was:
```
Phase 0-7:  unchanged (prerequisites, create world, backups, destroy data)
Phase 8:    cold restore (Btrfs only)
Phase 9:    start containers
Phase 10:   form bipod (retrofit DRBD)
Phase 11:   verify bipod
Phase 12:   negative test
```

The new flow:
```
Phase 0-7:  unchanged
Phase 8:    cold restore WITH DRBD (blank images → DRBD → Btrfs → receive → workspace)
Phase 9:    verify DRBD sync complete
Phase 10:   start containers (on /dev/drbd0 mount)
Phase 11:   verify bipod + data integrity
Phase 12:   negative test (chain ordering)
```

Still 13 phases (0-12). The check count may change slightly (external metadata checks removed, DRBD sync checks added in a different spot). Target should still be roughly 60+ checks.

### 5. Update container startup

Containers in the new Phase 10 should use the DRBD-backed mount. The mount is already at `/mnt/users/alice` from Phase 8. The block device for the device-mount pattern is `/dev/drbd0`, not the raw loop device.

If the current Phase 9 uses `--device $LOOP` for containers, change it to `--device /dev/drbd0`.

### 6. Remove Issue 22 workaround

The external metadata approach (Issue 22) is no longer needed. Remove any comments or code referencing it. The `--force` flag on `create-md` (Issue 21) is also no longer needed since both images are blank when DRBD metadata is created.

Issue 20 (DRBD needs block devices) is still relevant — loop devices are still required. That fix stays.

## What does NOT change

- Phases 0-7 (prerequisites, world creation, backup chain, B2 verification, data destruction)
- B2 upload/download logic
- manifest.json structure and chain ordering
- Container init script and Dockerfile
- infra.sh (same 2 machines, same network)
- cloud-init.yaml
- run.sh orchestration and teardown
- Phase 12 negative test (chain ordering)
- The DRBD resource config format (just uses `meta-disk internal` instead of external)

## Expected result

All checks pass with `meta-disk internal` on blank devices. The cold restore path now uses the identical block device stack as normal provisioning. No external metadata files. No `--force` on create-md. One architecture for all paths.

## Architecture doc update needed

After this patch, update `architecture-v3.md` Section 14.4 to make the ordering explicit:

> When restoring from Backblaze, set up DRBD on blank images FIRST (same as normal provisioning), then format Btrfs on /dev/drbd0, then `btrfs receive` the snapshot chain. This ensures the cold restore path uses the identical block device stack as normal provisioning — no special cases.

This should be noted in the PoC's session log but the actual architecture doc update can be done separately.
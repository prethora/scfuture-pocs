# Build Prompt: Btrfs World Isolation Proof of Concept

## What This Is

A proof of concept that demonstrates the foundational primitive for a distributed agent platform. The primitive: a single Btrfs image file per user, divided into isolated subvolumes ("worlds"), each mounted into its own Docker container, with cheap instant snapshots and rollback.

This runs entirely inside Docker Desktop on macOS. One privileged container simulates a fleet machine. Inside it, Docker-in-Docker runs app containers.

## Architecture

```
macOS (Docker Desktop)
  └─ "machine" container (privileged, Ubuntu 24.04, DinD)
       └─ Btrfs image file: /data/images/alice.img (sparse, 2GB)
            └─ Loop device → Btrfs mount at /mnt/users/alice/
                 ├─ subvol: core/          → mounted into container "alice-core"
                 ├─ subvol: app-email/     → mounted into container "alice-app-email"
                 ├─ subvol: app-budget/    → mounted into container "alice-app-budget"
                 └─ snapshots/
                      └─ (created during test)
```

## Directory Structure to Create

```
poc-btrfs/
├── docker-compose.yml          # Single service: the machine container
├── machine/
│   ├── Dockerfile              # Ubuntu 24.04, btrfs-progs, docker.io
│   ├── entrypoint.sh           # Starts dockerd, then runs the demo
│   └── demo.sh                 # The actual proof of concept test script
```

## docker-compose.yml

Single service called `machine`. Privileged. Runs with Docker-in-Docker.

## machine/Dockerfile

Based on Ubuntu 24.04. Install: btrfs-progs, docker.io, util-linux (for losetup), jq, curl. Copy entrypoint.sh and demo.sh. Make them executable.

## machine/entrypoint.sh

1. Start dockerd in the background (with overlay2 storage driver).
2. Wait for Docker to be ready (poll `docker info` in a loop, timeout 30 seconds).
3. Run demo.sh.
4. When demo.sh finishes, keep the container alive (tail -f /dev/null) so the user can shell in to explore if they want.

## machine/demo.sh — The Proof of Concept

This is the main script. It should print clear, readable output for every step, using section headers and pass/fail indicators. It should exit on any failure with a clear error message. Here's exactly what it should do:

### Phase 1: Create the User Image

1. Create directory /data/images/
2. Create a 2GB sparse image file: `truncate -s 2G /data/images/alice.img`
3. Print the actual disk usage (should be ~0 bytes, proving sparseness): `du -h /data/images/alice.img`
4. Print the apparent size (should be 2GB): `du -h --apparent-size /data/images/alice.img`
5. Format with Btrfs: `mkfs.btrfs -f /data/images/alice.img`
6. Create mount point and mount via loop: `mount -o loop /data/images/alice.img /mnt/users/alice`
7. Verify mount worked: `mount | grep alice`

### Phase 2: Create Subvolumes (Worlds)

1. Create three subvolumes:
   - `btrfs subvolume create /mnt/users/alice/core`
   - `btrfs subvolume create /mnt/users/alice/app-email`
   - `btrfs subvolume create /mnt/users/alice/app-budget`
2. Create a snapshots directory: `mkdir -p /mnt/users/alice/snapshots`
3. List subvolumes: `btrfs subvolume list /mnt/users/alice`
4. Seed each world with some initial content:
   - core/: create `config.json` with `{"agent_name": "alice-agent", "version": "1.0"}` and a `memory/` directory with a `MEMORY.md` file containing a few lines of sample agent memory.
   - app-email/: create a `data/` dir with an `inbox.db` file (just some text simulating a database) and a `config.json` with `{"domain": "alice.example.com", "smtp_port": 587}`.
   - app-budget/: create a `data/` dir with a `transactions.db` file (some text simulating data) and `src/` dir with a simple `app.py` file containing a few lines of Python (doesn't need to run, just represent code the agent might modify).

### Phase 3: Start Isolated Containers

1. Build or pull a simple Alpine-based image for the app containers. Alpine is fine — we just need a shell.
2. Start three containers using the machine's Docker daemon:
   - `alice-core`: mount only `/mnt/users/alice/core` as `/workspace` inside the container. Run as read-only filesystem except for /workspace. Use `tail -f /dev/null` to keep it alive.
   - `alice-app-email`: mount only `/mnt/users/alice/app-email` as `/workspace`. Same pattern.
   - `alice-app-budget`: mount only `/mnt/users/alice/app-budget` as `/workspace`. Same pattern.
3. Each container should be on its own isolated Docker network (or no network at all — `--network none` is fine for this POC).
4. Verify all three containers are running: `docker ps`

### Phase 4: Prove Isolation

This is the most important test. From inside each container, prove it can only see its own world.

1. From inside `alice-app-budget`:
   - Run `ls /workspace/` — should show the budget app's files (data/, src/, app.py).
   - Run `ls /workspace/src/app.py` — should succeed.
   - Try to access email world: `ls /mnt/` — should show nothing or fail (path doesn't exist inside this container).
   - Try common escape paths: `ls /data/`, `ls /mnt/users/`, `cat /proc/mounts | grep alice` — none should reveal other worlds.
   - Print: "PASS: app-budget can only see its own world"

2. From inside `alice-app-email`:
   - Run `ls /workspace/` — should show inbox.db, config.json.
   - Verify no access to budget files: try to find anything containing "transactions" or "app.py" — should find nothing.
   - Print: "PASS: app-email can only see its own world"

3. From inside `alice-core`:
   - Run `ls /workspace/` — should show config.json, memory/.
   - Verify no access to other worlds.
   - Print: "PASS: core can only see its own world"

Use `docker exec` to run commands inside each container. All assertions should be explicit with PASS/FAIL output.

### Phase 5: Simulate Agent Work + Snapshot

1. From inside `alice-app-budget`, write new files simulating the agent modifying the app:
   - Create `src/custom_feature.py` with some Python code.
   - Append a line to `data/transactions.db` simulating new data.
   - Create `src/dashboard.html` with some HTML.
2. Print the current state: list all files in the budget world.
3. Take a snapshot of the budget world:
   `btrfs subvolume snapshot -r /mnt/users/alice/app-budget /mnt/users/alice/snapshots/app-budget-checkpoint-1`
4. Print: snapshot created, list snapshots.
5. Show that the snapshot took essentially no additional disk space: run `btrfs filesystem du -s /mnt/users/alice/snapshots/app-budget-checkpoint-1` and compare to the live subvolume.

### Phase 6: Simulate Disaster + Rollback

1. From inside `alice-app-budget`, simulate something going catastrophically wrong:
   - Delete `src/app.py` (the main application file).
   - Overwrite `data/transactions.db` with garbage data.
   - Create a suspicious file: `src/.backdoor.sh` with some content.
2. Print the current (broken) state: list all files, show the corrupted data.
3. Print: "DISASTER: app-budget has been compromised/corrupted"
4. Now roll back:
   - Stop the `alice-app-budget` container: `docker stop alice-app-budget && docker rm alice-app-budget`
   - Delete the corrupted subvolume: `btrfs subvolume delete /mnt/users/alice/app-budget`
   - Restore from snapshot: `btrfs subvolume snapshot /mnt/users/alice/snapshots/app-budget-checkpoint-1 /mnt/users/alice/app-budget`
   - Restart the container with the same mount.
5. From inside the restored container, verify:
   - `src/app.py` is back.
   - `data/transactions.db` has the correct data (including the agent's additions from Phase 5).
   - `src/.backdoor.sh` does not exist.
   - `src/custom_feature.py` exists (it was in the snapshot).
   - `src/dashboard.html` exists (it was in the snapshot).
6. Print: "PASS: rollback successful, budget world restored to checkpoint-1"

### Phase 7: Prove Other Worlds Were Unaffected

1. From inside `alice-app-email`:
   - Verify all files are intact and unchanged.
   - Print: "PASS: email world was completely unaffected by budget disaster + rollback"
2. From inside `alice-core`:
   - Verify all files are intact and unchanged.
   - Print: "PASS: core world was completely unaffected by budget disaster + rollback"

### Phase 8: Show Disk Efficiency

1. Print overall Btrfs filesystem usage: `btrfs filesystem usage /mnt/users/alice`
2. Print per-subvolume space usage.
3. Print the actual disk usage of the sparse image file: `du -h /data/images/alice.img`
4. Print the apparent size: `du -h --apparent-size /data/images/alice.img`
5. Show that even with 3 worlds, a snapshot, and a rollback, actual disk usage is tiny compared to the 2GB apparent size.

### Final Summary

Print a clear summary:

```
════════════════════════════════════════════
  PROOF OF CONCEPT: COMPLETE
════════════════════════════════════════════
  ✓ Sparse image file created (2GB apparent, ~Xmb actual)
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

## Important Implementation Notes

- The machine container MUST be privileged (for loop mounts, Btrfs, and DinD).
- Use `set -e` in scripts so any failure stops execution with a clear error.
- Every test assertion should print explicitly what it's checking and whether it passed or failed.
- Use color in terminal output if possible (green for pass, red for fail) to make it scannable.
- The demo should be completely automated — no user interaction required. Run docker-compose up and watch it work.
- If any test fails, stop immediately and print the failure clearly. Don't continue past a failure.
- Keep the container alive after the demo so the user can `docker exec -it machine bash` to explore.

## How to Run

```bash
cd poc-btrfs
docker compose up --build
```

That's it. Watch the output. Everything should pass.

To explore interactively after:
```bash
docker exec -it poc-btrfs-machine-1 bash
# Now you're inside the "fleet machine"
# Look around:
ls /mnt/users/alice/
btrfs subvolume list /mnt/users/alice
docker ps
docker exec -it alice-app-budget sh
```

To clean up:
```bash
docker compose down -v
```
#!/bin/bash
set -e

# ─── Colors ───
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

pass() { echo -e "${GREEN}✓ PASS: $1${NC}"; }
fail() { echo -e "${RED}✗ FAIL: $1${NC}"; exit 1; }
warn() { echo -e "${YELLOW}⚠ $1${NC}"; }
info() { echo -e "${CYAN}$1${NC}"; }
phase() { echo -e "\n${BOLD}════════════════════════════════════════════${NC}"; echo -e "${BOLD}  Phase $1: $2${NC}"; echo -e "${BOLD}════════════════════════════════════════════${NC}\n"; }

remote() {
    ssh -o StrictHostKeyChecking=no root@${PEER_IP} "$@"
}

# ═══════════════════════════════════════════════════════
#  Phase 0: DRBD Module Check + Cleanup
# ═══════════════════════════════════════════════════════
phase 0 "DRBD Module Check"

if [ -f /proc/drbd ]; then
    info "DRBD module already loaded on host kernel"
elif modprobe drbd 2>/dev/null; then
    info "DRBD module loaded via modprobe"
else
    echo -e "${RED}FATAL: DRBD kernel module not available.${NC}"
    echo "Run on host: sudo apt install linux-modules-extra-\$(uname -r) drbd-utils && sudo modprobe drbd"
    exit 1
fi

# Clean up any stale DRBD resources from prior runs (kernel state is shared)
info "Cleaning up stale DRBD state..."
drbdsetup down alice 2>/dev/null || true

# Clean up stale loop devices from prior runs
info "Cleaning up stale loop devices..."
losetup -a 2>/dev/null | grep alice.img | cut -d: -f1 | while read LOOP; do
    losetup -d "$LOOP" 2>/dev/null || true
done

info "DRBD version info:"
cat /proc/drbd
pass "DRBD kernel module loaded"

# ═══════════════════════════════════════════════════════
#  Phase 1: Create Image Files on Both Machines
# ═══════════════════════════════════════════════════════
phase 1 "Create Image Files on Both Machines"

info "Creating sparse image on machine-1..."
mkdir -p /data/images
truncate -s 2G /data/images/alice.img
LOOP_DEV_LOCAL=$(losetup --find --show /data/images/alice.img)
echo "  machine-1: loop device = $LOOP_DEV_LOCAL"
echo "  Apparent size: $(du --apparent-size -h /data/images/alice.img | cut -f1)"
echo "  Actual size:   $(du -h /data/images/alice.img | cut -f1)"

info "Creating sparse image on machine-2..."
LOOP_DEV_REMOTE=$(remote "
    mkdir -p /data/images
    truncate -s 2G /data/images/alice.img
    losetup --find --show /data/images/alice.img
")
LOOP_DEV_REMOTE=$(echo "$LOOP_DEV_REMOTE" | tr -d '[:space:]')
echo "  machine-2: loop device = $LOOP_DEV_REMOTE"

pass "Image files created on both machines"

# ═══════════════════════════════════════════════════════
#  Phase 2: Configure and Start DRBD
# ═══════════════════════════════════════════════════════
phase 2 "Configure and Start DRBD"

info "Setting up DRBD on machine-1's backing device..."
info "(Both containers share the host kernel. DRBD manages the block device layer;"
info " in production with separate hosts, DRBD also replicates over TCP.)"

# Ensure transport module is loaded
modprobe drbd_transport_tcp 2>/dev/null || true

# Write metadata on machine-1's backing device
info "Creating DRBD metadata on ${LOOP_DEV_LOCAL}..."
yes | drbdmeta --force 0 v09 ${LOOP_DEV_LOCAL} internal create-md 1 2>&1

# Create resource and minor
info "Creating DRBD resource (alice, minor 0)..."
drbdsetup new-resource alice 0
drbdsetup new-minor alice 0 0

# Attach backing device
info "Attaching backing device..."
drbdsetup attach 0 ${LOOP_DEV_LOCAL} ${LOOP_DEV_LOCAL} internal

# Force primary (no peer connection in shared-kernel environment)
info "Promoting to primary..."
drbdsetup primary alice --force=yes

info "DRBD status:"
drbdsetup status alice

pass "DRBD configured — /dev/drbd0 is Primary/UpToDate"

# ═══════════════════════════════════════════════════════
#  Phase 3: Mount Btrfs and Create Worlds on Primary
# ═══════════════════════════════════════════════════════
phase 3 "Mount Btrfs and Create Worlds on Primary"

info "Formatting Btrfs on DRBD device (/dev/drbd0)..."
ls -la /dev/drbd0 2>&1 || true
mkfs.btrfs -f /dev/drbd0

info "Mounting Btrfs..."
mkdir -p /mnt/users/alice
mount /dev/drbd0 /mnt/users/alice

info "Creating subvolumes..."
btrfs subvolume create /mnt/users/alice/core
btrfs subvolume create /mnt/users/alice/app-email
btrfs subvolume create /mnt/users/alice/app-budget
mkdir -p /mnt/users/alice/snapshots

info "Seeding worlds with content..."

# core/
mkdir -p /mnt/users/alice/core/memory
cat > /mnt/users/alice/core/config.json << 'EOF'
{"agent_name": "alice-agent", "version": "1.0"}
EOF
cat > /mnt/users/alice/core/memory/MEMORY.md << 'EOF'
# Agent Memory
- Initialized on first boot
- User preference: dark mode
EOF

# app-email/
mkdir -p /mnt/users/alice/app-email/data
cat > /mnt/users/alice/app-email/config.json << 'EOF'
{"domain": "alice.example.com", "smtp_port": 587}
EOF
cat > /mnt/users/alice/app-email/data/inbox.db << 'EOF'
id|from|subject|date
1|bob@example.com|Meeting tomorrow|2025-01-15
2|carol@example.com|Project update|2025-01-16
EOF

# app-budget/
mkdir -p /mnt/users/alice/app-budget/data /mnt/users/alice/app-budget/src
cat > /mnt/users/alice/app-budget/data/transactions.db << 'EOF'
id|amount|category|date
1|+5000.00|salary|2025-01-01
2|-45.99|groceries|2025-01-03
3|-120.00|utilities|2025-01-05
EOF
cat > /mnt/users/alice/app-budget/src/app.py << 'PYEOF'
#!/usr/bin/env python3
"""Budget tracking application."""

def get_balance(transactions):
    return sum(float(t.split('|')[1]) for t in transactions[1:])

if __name__ == "__main__":
    with open("data/transactions.db") as f:
        lines = f.readlines()
    print(f"Balance: ${get_balance(lines):.2f}")
PYEOF

info "Subvolumes:"
btrfs subvolume list /mnt/users/alice

pass "Worlds created on primary (machine-1)"

# ═══════════════════════════════════════════════════════
#  Phase 4: Start Isolated Containers on Primary
# ═══════════════════════════════════════════════════════
phase 4 "Start Isolated Containers on Primary"

# Ensure /dev/drbd0 exists as a device node
mknod /dev/drbd0 b 147 0 2>/dev/null || true

info "Starting containers with device-mount isolation..."

for WORLD in core app-email app-budget; do
    docker run -d \
        --name "alice-${WORLD}" \
        --device /dev/drbd0 \
        --cap-drop ALL \
        --cap-add SYS_ADMIN \
        --cap-add SETUID \
        --cap-add SETGID \
        --network none \
        -e SUBVOL_NAME="${WORLD}" \
        -e BLOCK_DEVICE=/dev/drbd0 \
        platform/app-container > /dev/null
    echo "  Started alice-${WORLD}"
done

sleep 2

info "Running containers:"
docker ps --format "  {{.Names}} — {{.Status}}"

# Quick isolation check on one container
info "Isolation spot-check (alice-app-budget):"
if docker exec -u root alice-app-budget ls /mnt/users/alice 2>/dev/null; then
    fail "Container can see host mount — isolation broken!"
else
    echo "  Cannot access /mnt/users/alice from inside container — isolated ✓"
fi

pass "3 isolated containers running on primary"

# ═══════════════════════════════════════════════════════
#  Phase 5: Simulate Agent Work
# ═══════════════════════════════════════════════════════
phase 5 "Simulate Agent Work"

info "Writing to app-budget..."
docker exec -u root alice-app-budget sh -c 'cat > /workspace/src/new-feature.py << "EOF"
#!/usr/bin/env python3
"""New budget forecasting feature."""

def forecast(transactions, months=3):
    avg = sum(float(t.split("|")[1]) for t in transactions[1:]) / len(transactions[1:])
    return [avg * i for i in range(1, months + 1)]

if __name__ == "__main__":
    print("Forecasting module loaded")
EOF'
docker exec -u root alice-app-budget sh -c 'echo "4|+3200.00|freelance|2025-01-20" >> /workspace/data/transactions.db'
echo "  Created src/new-feature.py"
echo "  Appended transaction to data/transactions.db"

info "Writing to app-email..."
docker exec -u root alice-app-email sh -c 'echo "3|dave@example.com|Lunch Friday?|2025-01-17" >> /workspace/data/inbox.db'
echo "  Appended message to data/inbox.db"

info "Writing to core..."
docker exec -u root alice-core sh -c 'echo "- Learned: budget forecasting is useful" >> /workspace/memory/MEMORY.md'
echo "  Appended entry to memory/MEMORY.md"

info "DRBD status after writes:"
drbdsetup status alice

pass "Agent work written to all 3 worlds"

# ═══════════════════════════════════════════════════════
#  Phase 6: Take Pre-Failover Snapshot
# ═══════════════════════════════════════════════════════
phase 6 "Take Pre-Failover Snapshot"

btrfs subvolume snapshot -r /mnt/users/alice/app-budget /mnt/users/alice/snapshots/pre-failover

info "Subvolumes after snapshot:"
btrfs subvolume list /mnt/users/alice

pass "Pre-failover snapshot taken"

# ═══════════════════════════════════════════════════════
#  Phase 7: Simulate Primary Death
# ═══════════════════════════════════════════════════════
phase 7 "Simulate Primary Death"

info "Stopping app containers..."
docker stop alice-core alice-app-email alice-app-budget > /dev/null 2>&1
docker rm alice-core alice-app-email alice-app-budget > /dev/null 2>&1

info "Syncing filesystem..."
sync

info "Unmounting Btrfs..."
umount /mnt/users/alice

info "Tearing down DRBD on machine-1..."
drbdsetup secondary alice
drbdsetup detach 0
drbdsetup del-minor 0
drbdsetup del-resource alice

warn "machine-1 is DOWN — DRBD torn down"

info "Performing block-level replication to machine-2..."
info "(In production, DRBD replicates every write in real-time over TCP."
info " Since both containers share the host kernel, we perform the"
info " equivalent block-level copy now to prove the failover workflow.)"

dd if=${LOOP_DEV_LOCAL} of=${LOOP_DEV_REMOTE} bs=4M status=none

pass "Block-level replication complete — machine-2 has exact replica"

# ═══════════════════════════════════════════════════════
#  Phase 8: Failover — Promote Secondary
# ═══════════════════════════════════════════════════════
phase 8 "Failover — Promote Secondary"

info "Setting up DRBD on machine-2's replica (minor 1)..."
yes | drbdmeta --force 1 v09 ${LOOP_DEV_REMOTE} internal create-md 1 2>&1

drbdsetup new-resource alice 0
drbdsetup new-minor alice 1 0

info "Attaching backing device..."
drbdsetup attach 1 ${LOOP_DEV_REMOTE} ${LOOP_DEV_REMOTE} internal

info "Promoting machine-2 to primary..."
drbdsetup primary alice --force=yes

info "DRBD status on new primary:"
drbdsetup status alice

# Ensure /dev/drbd1 device node exists on machine-2
remote "mknod /dev/drbd1 b 147 1 2>/dev/null || true"

info "Mounting Btrfs on machine-2..."
remote "mkdir -p /mnt/users/alice && mount /dev/drbd1 /mnt/users/alice"

info "Subvolumes on new primary:"
remote "btrfs subvolume list /mnt/users/alice"

info "Starting containers on machine-2..."
for WORLD in core app-email app-budget; do
    remote "docker run -d \
        --name alice-${WORLD} \
        --device /dev/drbd1 \
        --cap-drop ALL \
        --cap-add SYS_ADMIN \
        --cap-add SETUID \
        --cap-add SETGID \
        --network none \
        -e SUBVOL_NAME=${WORLD} \
        -e BLOCK_DEVICE=/dev/drbd1 \
        platform/app-container" > /dev/null
    echo "  Started alice-${WORLD} on machine-2"
done

sleep 2

info "Running containers on machine-2:"
remote "docker ps --format '  {{.Names}} — {{.Status}}'"

pass "machine-2 promoted to primary, containers running"

# ═══════════════════════════════════════════════════════
#  Phase 9: Verify Data Survived Failover
# ═══════════════════════════════════════════════════════
phase 9 "Verify Data Survived Failover"

# app-budget
info "Checking app-budget..."
FEATURE_PY=$(remote "docker exec -u root alice-app-budget cat /workspace/src/new-feature.py" 2>&1)
if echo "$FEATURE_PY" | grep -q "forecast"; then
    echo "  src/new-feature.py — exists with correct content ✓"
else
    fail "app-budget: src/new-feature.py missing or wrong content"
fi

TX_DATA=$(remote "docker exec -u root alice-app-budget cat /workspace/data/transactions.db" 2>&1)
if echo "$TX_DATA" | grep -q "freelance"; then
    echo "  data/transactions.db — has appended data ✓"
else
    fail "app-budget: data/transactions.db missing appended data"
fi

if remote "docker exec -u root alice-app-budget test -f /workspace/src/app.py" 2>/dev/null; then
    echo "  src/app.py — original seed intact ✓"
else
    fail "app-budget: src/app.py missing"
fi
pass "app-budget data survived failover"

# app-email
info "Checking app-email..."
INBOX=$(remote "docker exec -u root alice-app-email cat /workspace/data/inbox.db" 2>&1)
if echo "$INBOX" | grep -q "dave@example.com"; then
    echo "  data/inbox.db — has new message ✓"
else
    fail "app-email: data/inbox.db missing new message"
fi

if remote "docker exec -u root alice-app-email test -f /workspace/config.json" 2>/dev/null; then
    echo "  config.json — intact ✓"
else
    fail "app-email: config.json missing"
fi
pass "app-email data survived failover"

# core
info "Checking core..."
MEMORY=$(remote "docker exec -u root alice-core cat /workspace/memory/MEMORY.md" 2>&1)
if echo "$MEMORY" | grep -q "budget forecasting"; then
    echo "  memory/MEMORY.md — has new entry ✓"
else
    fail "core: memory/MEMORY.md missing new entry"
fi

if remote "docker exec -u root alice-core test -f /workspace/config.json" 2>/dev/null; then
    echo "  config.json — intact ✓"
else
    fail "core: config.json missing"
fi
pass "core data survived failover"

# snapshot
info "Checking snapshot survived..."
SUBVOLS=$(remote "btrfs subvolume list /mnt/users/alice" 2>&1)
if echo "$SUBVOLS" | grep -q "pre-failover"; then
    echo "  pre-failover snapshot — present ✓"
else
    fail "pre-failover snapshot missing after failover"
fi
pass "pre-failover snapshot survived failover"

# ═══════════════════════════════════════════════════════
#  Phase 10: Prove Rollback Works on New Primary
# ═══════════════════════════════════════════════════════
phase 10 "Prove Rollback Works on New Primary"

info "Simulating disaster on app-budget (machine-2)..."
remote "docker exec -u root alice-app-budget rm /workspace/src/app.py"
remote "docker exec -u root alice-app-budget sh -c 'echo CORRUPTED > /workspace/data/transactions.db'"
echo "  Deleted src/app.py"
echo "  Corrupted data/transactions.db"

info "Corrupted state:"
remote "docker exec -u root alice-app-budget ls /workspace/src/" 2>&1 | sed 's/^/  /'
echo "  transactions.db content: $(remote "docker exec -u root alice-app-budget cat /workspace/data/transactions.db")"

info "Performing rollback..."
remote "docker stop alice-app-budget > /dev/null 2>&1 && docker rm alice-app-budget > /dev/null 2>&1"
remote "umount /mnt/users/alice/app-budget 2>/dev/null || true"
remote "btrfs subvolume delete /mnt/users/alice/app-budget"
remote "btrfs subvolume snapshot /mnt/users/alice/snapshots/pre-failover /mnt/users/alice/app-budget"

info "Restarting app-budget container..."
remote "docker run -d \
    --name alice-app-budget \
    --device /dev/drbd1 \
    --cap-drop ALL \
    --cap-add SYS_ADMIN \
    --cap-add SETUID \
    --cap-add SETGID \
    --network none \
    -e SUBVOL_NAME=app-budget \
    -e BLOCK_DEVICE=/dev/drbd1 \
    platform/app-container" > /dev/null

sleep 2

info "Verifying rollback..."

if remote "docker exec -u root alice-app-budget test -f /workspace/src/app.py" 2>/dev/null; then
    echo "  src/app.py — restored ✓"
else
    fail "Rollback: src/app.py not restored"
fi

RESTORED_TX=$(remote "docker exec -u root alice-app-budget cat /workspace/data/transactions.db" 2>&1)
if echo "$RESTORED_TX" | grep -q "freelance" && ! echo "$RESTORED_TX" | grep -q "CORRUPTED"; then
    echo "  data/transactions.db — correct data restored, corruption gone ✓"
else
    fail "Rollback: data/transactions.db not properly restored"
fi

if remote "docker exec -u root alice-app-budget test -f /workspace/src/new-feature.py" 2>/dev/null; then
    echo "  src/new-feature.py — preserved ✓"
else
    fail "Rollback: src/new-feature.py not preserved"
fi

pass "Rollback works on new primary after failover"

# ═══════════════════════════════════════════════════════
#  Final Summary
# ═══════════════════════════════════════════════════════
echo ""
echo -e "${BOLD}════════════════════════════════════════════${NC}"
echo -e "${BOLD}  PROOF OF CONCEPT: DRBD BIPOD — COMPLETE${NC}"
echo -e "${BOLD}════════════════════════════════════════════${NC}"
echo -e "${GREEN}  ✓ DRBD kernel module loaded${NC}"
echo -e "${GREEN}  ✓ Image files created on both machines (sparse, 2GB)${NC}"
echo -e "${GREEN}  ✓ DRBD block device layer active on primary${NC}"
echo -e "${GREEN}  ✓ Btrfs formatted on /dev/drbd0 with 3 isolated worlds${NC}"
echo -e "${GREEN}  ✓ Containers running with device-mount isolation${NC}"
echo -e "${GREEN}  ✓ Agent work written to all worlds${NC}"
echo -e "${GREEN}  ✓ Pre-failover snapshot taken${NC}"
echo -e "${GREEN}  ✓ Primary killed — DRBD torn down${NC}"
echo -e "${GREEN}  ✓ Block-level replication to secondary${NC}"
echo -e "${GREEN}  ✓ Secondary promoted to primary via DRBD${NC}"
echo -e "${GREEN}  ✓ All data verified on new primary (zero loss)${NC}"
echo -e "${GREEN}  ✓ Snapshots survived failover${NC}"
echo -e "${GREEN}  ✓ Rollback works on new primary${NC}"
echo -e "${BOLD}════════════════════════════════════════════${NC}"

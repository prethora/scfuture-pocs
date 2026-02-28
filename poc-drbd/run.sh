#!/bin/bash
set -e

# ─── DRBD Bipod PoC — Full Orchestration ───
# Creates infrastructure, provisions servers, runs the demo, tears down.
# Usage: ./run.sh

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
STATE_DIR="$SCRIPT_DIR/.state"
INFRA_ENV="$STATE_DIR/infra.env"
SCRIPTS_DIR="$SCRIPT_DIR/scripts"

GREEN='\033[0;32m'
CYAN='\033[0;36m'
RED='\033[0;31m'
YELLOW='\033[0;33m'
BOLD='\033[1m'
NC='\033[0m'

info()  { echo -e "${CYAN}[run] $1${NC}"; }
pass()  { echo -e "${GREEN}[run] $1${NC}"; }
fail()  { echo -e "${RED}[run] $1${NC}"; exit 1; }
warn()  { echo -e "${YELLOW}[run] $1${NC}"; }

# ─── Trap: Auto-teardown on error/interrupt ───
TEARDOWN_ON_EXIT=true
cleanup() {
    if $TEARDOWN_ON_EXIT && [ -f "$INFRA_ENV" ]; then
        echo ""
        warn "Cleaning up infrastructure (auto-teardown)..."
        "$SCRIPT_DIR/infra.sh" down || true
    fi
}
trap cleanup EXIT

# SSH helper
ssh_cmd() {
    local ip="$1"
    shift
    ssh -i "$STATE_DIR/ssh-key" $SSH_OPTS root@"$ip" "$@"
}
scp_cmd() {
    scp -i "$STATE_DIR/ssh-key" $SSH_OPTS "$@"
}
SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"

# ─── Step 1: Infrastructure ───
echo -e "\n${BOLD}${CYAN}╔══════════════════════════════════════════╗${NC}"
echo -e "${BOLD}${CYAN}║   DRBD Bipod Replication PoC             ║${NC}"
echo -e "${BOLD}${CYAN}║   Two Hetzner Cloud Servers + DRBD 9     ║${NC}"
echo -e "${BOLD}${CYAN}╚══════════════════════════════════════════╝${NC}\n"

if [ ! -f "$INFRA_ENV" ]; then
    info "Creating infrastructure..."
    "$SCRIPT_DIR/infra.sh" up
else
    info "Infrastructure already exists, reusing..."
fi

# Load state
source "$INFRA_ENV"

info "Machine 1: $MACHINE1_PUBLIC_IP (private: $MACHINE1_PRIVATE_IP)"
info "Machine 2: $MACHINE2_PUBLIC_IP (private: $MACHINE2_PRIVATE_IP)"

# ─── Step 2: Wait for SSH access ───
info "Waiting for SSH access..."

for name_ip in "machine-1:$MACHINE1_PUBLIC_IP" "machine-2:$MACHINE2_PUBLIC_IP"; do
    name="${name_ip%%:*}"
    ip="${name_ip##*:}"
    TIMEOUT=120
    ELAPSED=0
    while ! ssh_cmd "$ip" "echo ready" >/dev/null 2>&1; do
        sleep 2
        ELAPSED=$((ELAPSED + 2))
        if [ $ELAPSED -ge $TIMEOUT ]; then
            fail "$name: SSH not available after ${TIMEOUT}s"
        fi
        if [ $((ELAPSED % 10)) -eq 0 ]; then
            info "Waiting for $name SSH... (${ELAPSED}s)"
        fi
    done
    pass "$name: SSH accessible"
done

# ─── Step 3: Wait for cloud-init to complete ───
info "Waiting for cloud-init provisioning to complete..."

for name_ip in "machine-1:$MACHINE1_PUBLIC_IP" "machine-2:$MACHINE2_PUBLIC_IP"; do
    name="${name_ip%%:*}"
    ip="${name_ip##*:}"
    info "Waiting for $name cloud-init (this may take 2-3 minutes)..."
    ssh_cmd "$ip" "cloud-init status --wait" 2>/dev/null || true
    # Verify key packages are installed
    if ssh_cmd "$ip" "which drbdadm && which docker && which mkfs.btrfs" >/dev/null 2>&1; then
        pass "$name: All packages installed"
    else
        fail "$name: Package installation incomplete. Check cloud-init log:
  ssh -i .state/ssh-key root@$ip 'cat /var/log/cloud-init-output.log | tail -50'"
    fi
done

# ─── Step 4: Set up inter-machine SSH ───
info "Setting up inter-machine SSH (machine-1 → machine-2)..."

# Copy SSH private key to machine-1 so it can SSH to machine-2
scp_cmd "$STATE_DIR/ssh-key" root@"$MACHINE1_PUBLIC_IP":/root/.ssh/id_ed25519
ssh_cmd "$MACHINE1_PUBLIC_IP" "chmod 600 /root/.ssh/id_ed25519"

# Set up SSH config on machine-1 for accessing machine-2 via private IP
ssh_cmd "$MACHINE1_PUBLIC_IP" "cat > /root/.ssh/config" << EOF
Host *
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null
    LogLevel ERROR
EOF
ssh_cmd "$MACHINE1_PUBLIC_IP" "chmod 600 /root/.ssh/config"

# Verify machine-1 can SSH to machine-2 via private IP
if ssh_cmd "$MACHINE1_PUBLIC_IP" "ssh root@$MACHINE2_PRIVATE_IP 'echo ready'" >/dev/null 2>&1; then
    pass "Inter-machine SSH working (machine-1 → machine-2 via $MACHINE2_PRIVATE_IP)"
else
    fail "Inter-machine SSH failed. machine-1 cannot reach machine-2 at $MACHINE2_PRIVATE_IP"
fi

# ─── Step 5: Copy scripts to both machines ───
info "Deploying scripts to servers..."

# Copy scripts to machine-1
scp_cmd -r "$SCRIPTS_DIR" root@"$MACHINE1_PUBLIC_IP":/opt/scripts
pass "Scripts deployed to machine-1"

# Copy scripts to machine-2 (for container image build)
scp_cmd -r "$SCRIPTS_DIR" root@"$MACHINE2_PUBLIC_IP":/opt/scripts
pass "Scripts deployed to machine-2"

# ─── Step 6: Build Docker image on both machines ───
info "Building app-container Docker image on both machines..."

# Copy container-init.sh into the Docker build context
for ip in "$MACHINE1_PUBLIC_IP" "$MACHINE2_PUBLIC_IP"; do
    ssh_cmd "$ip" "cp /opt/scripts/container-init.sh /opt/scripts/app-container/container-init.sh"
    ssh_cmd "$ip" "docker build -t platform/app-container /opt/scripts/app-container/" >/dev/null 2>&1
done
pass "Docker image built on both machines"

# ─── Step 7: Set hostnames ───
info "Setting hostnames..."
ssh_cmd "$MACHINE1_PUBLIC_IP" "hostnamectl set-hostname drbd-machine-1"
ssh_cmd "$MACHINE2_PUBLIC_IP" "hostnamectl set-hostname drbd-machine-2"

# Also ensure /etc/drbd.d/ exists and DRBD module is loaded
for ip in "$MACHINE1_PUBLIC_IP" "$MACHINE2_PUBLIC_IP"; do
    ssh_cmd "$ip" "mkdir -p /etc/drbd.d && modprobe drbd" 2>/dev/null || true
done
pass "Hostnames set and DRBD module loaded"

# ─── Step 8: Run the demo ───
echo ""
echo -e "${BOLD}${CYAN}─── Starting DRBD Demo ───${NC}"
echo ""

DEMO_EXIT=0
ssh_cmd "$MACHINE1_PUBLIC_IP" "NODE_IP=$MACHINE1_PRIVATE_IP PEER_IP=$MACHINE2_PRIVATE_IP bash /opt/scripts/demo.sh" || DEMO_EXIT=$?

# ─── Step 9: Results ───
echo ""
if [ $DEMO_EXIT -eq 0 ]; then
    echo -e "${GREEN}${BOLD}Demo completed successfully!${NC}"
else
    echo -e "${RED}${BOLD}Demo exited with code $DEMO_EXIT${NC}"
fi

# ─── Step 10: Teardown ───
echo ""
info "Auto-teardown: destroying infrastructure..."
# The EXIT trap will handle this, but we set the flag so it runs
TEARDOWN_ON_EXIT=true

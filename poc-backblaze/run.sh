#!/bin/bash
set -euo pipefail

# ─── Backblaze B2 Backup & Restore PoC — Full Orchestration ───
# Creates infrastructure, provisions servers, runs the demo, tears down.
# Usage: ./run.sh

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
STATE_DIR="$SCRIPT_DIR/.state"
INFRA_ENV="$STATE_DIR/infra.env"
SCRIPTS_DIR="$SCRIPT_DIR/scripts"
BUCKET_FILE="$STATE_DIR/.bucket-name"

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

SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"

ssh_cmd() {
    local ip="$1"; shift
    ssh -i "$STATE_DIR/ssh-key" $SSH_OPTS root@"$ip" "$@"
}
scp_cmd() {
    scp -i "$STATE_DIR/ssh-key" $SSH_OPTS "$@"
}

# ─── Validate environment ───
echo -e "\n${BOLD}${CYAN}╔══════════════════════════════════════════════╗${NC}"
echo -e "${BOLD}${CYAN}║   Backblaze B2 Backup & Restore PoC          ║${NC}"
echo -e "${BOLD}${CYAN}║   Two Hetzner Servers + B2 + DRBD 9          ║${NC}"
echo -e "${BOLD}${CYAN}╚══════════════════════════════════════════════╝${NC}\n"

MISSING=""
[ -z "${HCLOUD_TOKEN:-}" ] && MISSING="${MISSING}  HCLOUD_TOKEN\n"
[ -z "${B2_KEY_ID:-}" ] && MISSING="${MISSING}  B2_KEY_ID\n"
[ -z "${B2_APP_KEY:-}" ] && MISSING="${MISSING}  B2_APP_KEY\n"

if [ -n "$MISSING" ]; then
    echo -e "${RED}Missing required environment variables:${NC}"
    echo -e "${RED}${MISSING}${NC}"
    echo "Set them with:"
    echo '  export HCLOUD_TOKEN="..."'
    echo '  export B2_KEY_ID="..."'
    echo '  export B2_APP_KEY="..."'
    exit 1
fi

if ! command -v hcloud >/dev/null 2>&1; then
    fail "hcloud CLI not found. The infra.sh script will install it if needed."
fi

pass "All environment variables set"

# ─── Generate bucket name ───
mkdir -p "$STATE_DIR"
if [ -f "$BUCKET_FILE" ]; then
    BUCKET_NAME=$(cat "$BUCKET_FILE")
    info "Reusing bucket name: $BUCKET_NAME"
else
    RANDOM_SUFFIX=$(head -c 6 /dev/urandom | xxd -p | head -c 12)
    BUCKET_NAME="poc-backblaze-${RANDOM_SUFFIX}"
    echo "$BUCKET_NAME" > "$BUCKET_FILE"
    pass "Generated bucket name: $BUCKET_NAME"
fi

# ─── Trap: Auto-teardown on exit ───
cleanup() {
    local exit_code=$?
    echo ""
    warn "Running teardown (auto-cleanup)..."

    # B2 cleanup (best-effort)
    if [ -f "$BUCKET_FILE" ]; then
        local bucket=$(cat "$BUCKET_FILE")
        info "Cleaning up B2 bucket: $bucket"
        if [ -f "$INFRA_ENV" ]; then
            source "$INFRA_ENV"
            # Try to delete bucket contents and bucket via machine-1
            ssh_cmd "$MACHINE1_PUBLIC_IP" "export PATH=\$PATH:/root/.local/bin; b2 rm --recursive --no-progress 'b2://$bucket'" 2>/dev/null || true
            ssh_cmd "$MACHINE1_PUBLIC_IP" "export PATH=\$PATH:/root/.local/bin; b2 bucket delete '$bucket'" 2>/dev/null || true
            pass "B2 bucket cleanup attempted"
        else
            warn "No infra state — cannot clean B2 bucket remotely"
            # Try locally if b2 is installed
            if command -v b2 >/dev/null 2>&1; then
                b2 account authorize "$B2_KEY_ID" "$B2_APP_KEY" >/dev/null 2>&1 || true
                b2 rm --recursive --no-progress "b2://$bucket" 2>/dev/null || true
                b2 bucket delete "$bucket" 2>/dev/null || true
            fi
        fi
    fi

    # Hetzner cleanup
    if [ -f "$INFRA_ENV" ]; then
        info "Tearing down Hetzner infrastructure..."
        "$SCRIPT_DIR/infra.sh" down || true
    fi

    # Remove local state
    rm -f "$BUCKET_FILE"

    if [ $exit_code -eq 0 ]; then
        pass "Teardown complete. No lingering resources."
    else
        warn "Teardown complete (script exited with code $exit_code)."
    fi
}
trap cleanup EXIT

# ─── Step 1: Infrastructure ───
if [ ! -f "$INFRA_ENV" ]; then
    info "Creating infrastructure..."
    "$SCRIPT_DIR/infra.sh" up
else
    info "Infrastructure already exists, reusing..."
fi

source "$INFRA_ENV"
info "Machine 1: $MACHINE1_PUBLIC_IP (private: $MACHINE1_PRIVATE_IP)"
info "Machine 2: $MACHINE2_PUBLIC_IP (private: $MACHINE2_PRIVATE_IP)"

# ─── Step 2: Wait for SSH ───
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

# ─── Step 3: Wait for cloud-init ───
info "Waiting for cloud-init provisioning to complete..."

for name_ip in "machine-1:$MACHINE1_PUBLIC_IP" "machine-2:$MACHINE2_PUBLIC_IP"; do
    name="${name_ip%%:*}"
    ip="${name_ip##*:}"
    info "Waiting for $name cloud-init (this may take 3-4 minutes)..."
    ssh_cmd "$ip" "cloud-init status --wait" 2>/dev/null || true

    if ssh_cmd "$ip" "which drbdadm && which docker && which mkfs.btrfs && which zstd && which jq" >/dev/null 2>&1; then
        pass "$name: All packages installed"
    else
        fail "$name: Package installation incomplete. Check:
  ssh -i .state/ssh-key root@$ip 'cat /var/log/cloud-init-output.log | tail -50'"
    fi

    # Verify b2 CLI
    if ssh_cmd "$ip" "export PATH=\$PATH:/root/.local/bin; b2 version" >/dev/null 2>&1; then
        pass "$name: B2 CLI available"
    else
        warn "$name: B2 CLI not found via pipx, trying pip fallback..."
        ssh_cmd "$ip" "pip3 install --break-system-packages b2[full]" >/dev/null 2>&1 || true
        if ssh_cmd "$ip" "b2 version" >/dev/null 2>&1; then
            pass "$name: B2 CLI installed via pip3 fallback"
        else
            fail "$name: B2 CLI installation failed"
        fi
    fi
done

# ─── Step 4: Deploy scripts ───
info "Deploying scripts to servers..."

for ip in "$MACHINE1_PUBLIC_IP" "$MACHINE2_PUBLIC_IP"; do
    scp_cmd -r "$SCRIPTS_DIR" root@"$ip":/opt/scripts
done
pass "Scripts deployed to both machines"

# ─── Step 5: Set hostnames and load DRBD ───
info "Setting hostnames and loading DRBD module..."
ssh_cmd "$MACHINE1_PUBLIC_IP" "hostnamectl set-hostname poc-b2-machine-1"
ssh_cmd "$MACHINE2_PUBLIC_IP" "hostnamectl set-hostname poc-b2-machine-2"

for ip in "$MACHINE1_PUBLIC_IP" "$MACHINE2_PUBLIC_IP"; do
    ssh_cmd "$ip" "mkdir -p /etc/drbd.d && modprobe drbd" 2>/dev/null || true
done
pass "Hostnames set and DRBD module loaded"

# ─── Step 6: Run the demo ───
echo ""
echo -e "${BOLD}${CYAN}─── Starting Backblaze B2 Demo ───${NC}"
echo ""

DEMO_EXIT=0
MACHINE1_IP="$MACHINE1_PUBLIC_IP" \
MACHINE2_IP="$MACHINE2_PUBLIC_IP" \
MACHINE1_PRIV="$MACHINE1_PRIVATE_IP" \
MACHINE2_PRIV="$MACHINE2_PRIVATE_IP" \
B2_KEY_ID="$B2_KEY_ID" \
B2_APP_KEY="$B2_APP_KEY" \
BUCKET_NAME="$BUCKET_NAME" \
SSH_KEY="$STATE_DIR/ssh-key" \
"$SCRIPTS_DIR/demo.sh" || DEMO_EXIT=$?

# The EXIT trap handles teardown
echo ""
if [ $DEMO_EXIT -eq 0 ]; then
    echo -e "${GREEN}${BOLD}Demo completed successfully!${NC}"
else
    echo -e "${RED}${BOLD}Demo exited with $DEMO_EXIT failure(s)${NC}"
fi
echo ""
info "Auto-teardown: destroying infrastructure and B2 bucket..."

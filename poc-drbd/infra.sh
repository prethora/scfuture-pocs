#!/bin/bash
set -e

# ─── DRBD Bipod PoC — Hetzner Cloud Infrastructure ───
# Manages two CX22 servers with a private network for DRBD replication.
# Usage: ./infra.sh up | down | status

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
STATE_DIR="$SCRIPT_DIR/.state"
INFRA_ENV="$STATE_DIR/infra.env"
CLOUD_INIT="$SCRIPT_DIR/cloud-init.yaml"

# Resource names
SSH_KEY_NAME="drbd-poc-key"
NETWORK_NAME="drbd-net"
SERVER1_NAME="drbd-machine-1"
SERVER2_NAME="drbd-machine-2"
SERVER_TYPE="cx23"
SERVER_IMAGE="ubuntu-24.04"
SERVER_LOCATION="nbg1"

# Private network IPs
NETWORK_RANGE="10.0.0.0/16"
SUBNET_RANGE="10.0.0.0/24"
MACHINE1_PRIVATE_IP="10.0.0.2"
MACHINE2_PRIVATE_IP="10.0.0.3"

GREEN='\033[0;32m'
CYAN='\033[0;36m'
RED='\033[0;31m'
YELLOW='\033[0;33m'
NC='\033[0m'

info()  { echo -e "${CYAN}[infra] $1${NC}"; }
pass()  { echo -e "${GREEN}[infra] $1${NC}"; }
fail()  { echo -e "${RED}[infra] $1${NC}"; exit 1; }
warn()  { echo -e "${YELLOW}[infra] $1${NC}"; }

# ─── Ensure hcloud CLI is available ───
ensure_hcloud() {
    if command -v hcloud >/dev/null 2>&1; then
        return
    fi

    info "Installing hcloud CLI..."
    local version="1.49.0"
    local url="https://github.com/hetznercloud/cli/releases/download/v${version}/hcloud-linux-amd64.tar.gz"
    local tmp=$(mktemp -d)
    curl -fsSL "$url" | tar xz -C "$tmp"
    sudo mv "$tmp/hcloud" /usr/local/bin/hcloud
    rm -rf "$tmp"
    chmod +x /usr/local/bin/hcloud
    pass "Installed hcloud CLI v${version}"
}

# ─── Check prerequisites ───
check_prereqs() {
    if [ -z "$HCLOUD_TOKEN" ]; then
        fail "HCLOUD_TOKEN environment variable is not set.
Set it with: export HCLOUD_TOKEN=your-api-token
Get a token from: https://console.hetzner.cloud/ → Project → Security → API Tokens"
    fi
    ensure_hcloud
}

# ─── infra.sh up ───
cmd_up() {
    check_prereqs

    if [ -f "$INFRA_ENV" ]; then
        warn "Infrastructure already exists (.state/infra.env found)"
        warn "Run './infra.sh down' first, or './infra.sh status' to check"
        return 1
    fi

    mkdir -p "$STATE_DIR"

    # 1. Generate SSH key pair
    info "Generating SSH key pair..."
    ssh-keygen -t ed25519 -f "$STATE_DIR/ssh-key" -N "" -q
    pass "SSH key pair generated"

    # 2. Upload public key to Hetzner
    info "Uploading SSH public key to Hetzner..."
    # Delete stale key if it exists from a previous failed run
    hcloud ssh-key delete "$SSH_KEY_NAME" 2>/dev/null || true
    hcloud ssh-key create --name "$SSH_KEY_NAME" --public-key-from-file "$STATE_DIR/ssh-key.pub"
    pass "SSH key uploaded: $SSH_KEY_NAME"

    # 3. Create private network
    info "Creating private network..."
    hcloud network delete "$NETWORK_NAME" 2>/dev/null || true
    hcloud network create --name "$NETWORK_NAME" --ip-range "$NETWORK_RANGE"
    hcloud network add-subnet "$NETWORK_NAME" --type cloud --network-zone eu-central --ip-range "$SUBNET_RANGE"
    pass "Private network created: $NETWORK_NAME ($SUBNET_RANGE)"

    # 4. Create servers
    info "Creating $SERVER1_NAME..."
    hcloud server create \
        --name "$SERVER1_NAME" \
        --type "$SERVER_TYPE" \
        --image "$SERVER_IMAGE" \
        --ssh-key "$SSH_KEY_NAME" \
        --location "$SERVER_LOCATION" \
        --user-data-from-file "$CLOUD_INIT" \
        --network "$NETWORK_NAME" \
        > /dev/null
    pass "Created $SERVER1_NAME"

    info "Creating $SERVER2_NAME..."
    hcloud server create \
        --name "$SERVER2_NAME" \
        --type "$SERVER_TYPE" \
        --image "$SERVER_IMAGE" \
        --ssh-key "$SSH_KEY_NAME" \
        --location "$SERVER_LOCATION" \
        --user-data-from-file "$CLOUD_INIT" \
        --network "$NETWORK_NAME" \
        > /dev/null
    pass "Created $SERVER2_NAME"

    # 5. Get server details
    local m1_public_ip=$(hcloud server ip "$SERVER1_NAME")
    local m2_public_ip=$(hcloud server ip "$SERVER2_NAME")
    local m1_id=$(hcloud server describe "$SERVER1_NAME" -o format='{{.ID}}')
    local m2_id=$(hcloud server describe "$SERVER2_NAME" -o format='{{.ID}}')

    # 6. Save state
    cat > "$INFRA_ENV" << EOF
# DRBD Bipod PoC — Infrastructure State
# Generated at $(date -u +"%Y-%m-%dT%H:%M:%SZ")
MACHINE1_PUBLIC_IP=$m1_public_ip
MACHINE2_PUBLIC_IP=$m2_public_ip
MACHINE1_PRIVATE_IP=$MACHINE1_PRIVATE_IP
MACHINE2_PRIVATE_IP=$MACHINE2_PRIVATE_IP
MACHINE1_ID=$m1_id
MACHINE2_ID=$m2_id
SSH_KEY=$STATE_DIR/ssh-key
EOF

    echo ""
    pass "Infrastructure is up!"
    echo ""
    info "Machine 1: $m1_public_ip (private: $MACHINE1_PRIVATE_IP)"
    info "Machine 2: $m2_public_ip (private: $MACHINE2_PRIVATE_IP)"
    echo ""
    info "Cloud-init is provisioning packages. This takes ~2-3 minutes."
    info "Run './run.sh' to start the demo (it will wait for provisioning)."
}

# ─── infra.sh down ───
cmd_down() {
    check_prereqs

    info "Tearing down infrastructure..."

    # Delete servers (ignore errors if they don't exist)
    info "Deleting servers..."
    hcloud server delete "$SERVER1_NAME" 2>/dev/null && pass "Deleted $SERVER1_NAME" || warn "$SERVER1_NAME not found"
    hcloud server delete "$SERVER2_NAME" 2>/dev/null && pass "Deleted $SERVER2_NAME" || warn "$SERVER2_NAME not found"

    # Delete network
    info "Deleting network..."
    hcloud network delete "$NETWORK_NAME" 2>/dev/null && pass "Deleted $NETWORK_NAME" || warn "$NETWORK_NAME not found"

    # Delete SSH key
    info "Deleting SSH key..."
    hcloud ssh-key delete "$SSH_KEY_NAME" 2>/dev/null && pass "Deleted $SSH_KEY_NAME" || warn "$SSH_KEY_NAME not found"

    # Remove state directory
    if [ -d "$STATE_DIR" ]; then
        rm -rf "$STATE_DIR"
        pass "Removed .state/"
    fi

    echo ""
    pass "Infrastructure torn down. No lingering resources."
}

# ─── infra.sh status ───
cmd_status() {
    check_prereqs

    echo ""
    info "=== Hetzner Cloud Resources ==="
    echo ""

    # Check servers
    for name in "$SERVER1_NAME" "$SERVER2_NAME"; do
        local status=$(hcloud server describe "$name" -o format='{{.Status}}' 2>/dev/null || echo "not found")
        local ip=$(hcloud server ip "$name" 2>/dev/null || echo "n/a")
        if [ "$status" = "not found" ]; then
            warn "$name: not found"
        else
            info "$name: $status (IP: $ip)"
        fi
    done

    # Check network
    local net_status=$(hcloud network describe "$NETWORK_NAME" -o format='{{.ID}}' 2>/dev/null || echo "not found")
    if [ "$net_status" = "not found" ]; then
        warn "Network $NETWORK_NAME: not found"
    else
        info "Network $NETWORK_NAME: exists (ID: $net_status)"
    fi

    # Check local state
    if [ -f "$INFRA_ENV" ]; then
        echo ""
        info "Local state (.state/infra.env):"
        cat "$INFRA_ENV" | grep -v '^#' | grep -v '^$' | while read line; do
            info "  $line"
        done
    else
        warn "No local state file (.state/infra.env)"
    fi
    echo ""
}

# ─── Main ───
case "${1:-}" in
    up)     cmd_up ;;
    down)   cmd_down ;;
    status) cmd_status ;;
    *)
        echo "Usage: $0 {up|down|status}"
        echo ""
        echo "  up      Create two Hetzner Cloud servers with private network"
        echo "  down    Delete all resources and clean up"
        echo "  status  Show current infrastructure state"
        exit 1
        ;;
esac

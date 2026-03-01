#!/bin/bash
# common.sh — shared functions sourced by all scripts

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
IP_FILE="$SCRIPT_DIR/.ips"

NETWORK_NAME="poc41-net"
SSH_KEY_NAME="poc41"
LOCATION="nbg1"
SERVER_TYPE="cx23"
IMAGE="ubuntu-24.04"

# Check counters
TOTAL_CHECKS=0
TOTAL_PASSED=0
PHASE_CHECKS=0
PHASE_PASSED=0

save_ips() {
    echo "Discovering machine IPs..."
    cat > "$IP_FILE" << EOF
MACHINE1_IP=$(hcloud server ip poc41-machine-1)
MACHINE1_PRIV=$(hcloud server describe poc41-machine-1 -o json | jq -r '.private_net[0].ip')
MACHINE2_IP=$(hcloud server ip poc41-machine-2)
MACHINE2_PRIV=$(hcloud server describe poc41-machine-2 -o json | jq -r '.private_net[0].ip')
EOF
    echo "IPs saved to $IP_FILE"
    cat "$IP_FILE"
}

load_ips() {
    if [ ! -f "$IP_FILE" ]; then
        echo "ERROR: IP file not found. Run infra.sh up first."
        exit 1
    fi
    source "$IP_FILE"
    export MACHINE1_IP MACHINE1_PRIV MACHINE2_IP MACHINE2_PRIV
}

wait_for_cloud_init() {
    load_ips
    echo "Waiting for cloud-init to complete..."
    for ip in $MACHINE1_IP $MACHINE2_IP; do
        for attempt in $(seq 1 60); do
            if ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 root@$ip \
                "cloud-init status --wait" 2>/dev/null | grep -q "done"; then
                echo "  $ip: cloud-init complete"
                break
            fi
            if [ "$attempt" -eq 60 ]; then
                echo "ERROR: cloud-init timeout on $ip"
                exit 1
            fi
            sleep 5
        done
    done
}

# Test framework
check() {
    local description="$1"
    local test_cmd="$2"
    TOTAL_CHECKS=$((TOTAL_CHECKS + 1))
    PHASE_CHECKS=$((PHASE_CHECKS + 1))
    if eval "$test_cmd" 2>/dev/null; then
        echo "  ✓ $description"
        TOTAL_PASSED=$((TOTAL_PASSED + 1))
        PHASE_PASSED=$((PHASE_PASSED + 1))
    else
        echo "  ✗ FAILED: $description"
    fi
}

phase_start() {
    PHASE_CHECKS=0
    PHASE_PASSED=0
    echo ""
    echo "Phase $1: $2"
}

phase_result() {
    echo "  [$PHASE_PASSED/$PHASE_CHECKS checks passed]"
}

final_result() {
    echo ""
    echo "═══════════════════════════════════════════════════"
    echo "ALL PHASES COMPLETE: $TOTAL_PASSED/$TOTAL_CHECKS checks passed"
    echo "═══════════════════════════════════════════════════"
    if [ "$TOTAL_PASSED" -ne "$TOTAL_CHECKS" ]; then
        exit 1
    fi
}

# API helpers
api() {
    local machine_ip="$1"
    local method="$2"
    local path="$3"
    local body="${4:-}"
    if [ -n "$body" ]; then
        curl -sf -X "$method" "http://$machine_ip:8080$path" \
            -H "Content-Type: application/json" \
            -d "$body"
    else
        curl -sf -X "$method" "http://$machine_ip:8080$path"
    fi
}

ssh_cmd() {
    local ip="$1"
    shift
    ssh -o StrictHostKeyChecking=no root@"$ip" "$@"
}

docker_exec() {
    local ip="$1"
    local container="$2"
    shift 2
    ssh_cmd "$ip" "docker exec $container $*"
}

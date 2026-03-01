#!/bin/bash
# common.sh — shared functions for Layer 4.4 scripts

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCFUTURE_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
IP_FILE="$SCRIPT_DIR/.ips"

NETWORK_NAME="l44-net"
SSH_KEY_NAME="l44-key"
LOCATION="nbg1"
SERVER_TYPE="cx23"
IMAGE="ubuntu-24.04"

# ── IP management ──

save_ips() {
    echo "Discovering machine IPs..."
    cat > "$IP_FILE" << EOF
COORD_PUB_IP=$(hcloud server ip l44-coordinator)
COORD_PRIV_IP=$(hcloud server describe l44-coordinator -o json | jq -r '.private_net[0].ip')
FLEET1_PUB_IP=$(hcloud server ip l44-fleet-1)
FLEET1_PRIV_IP=$(hcloud server describe l44-fleet-1 -o json | jq -r '.private_net[0].ip')
FLEET2_PUB_IP=$(hcloud server ip l44-fleet-2)
FLEET2_PRIV_IP=$(hcloud server describe l44-fleet-2 -o json | jq -r '.private_net[0].ip')
FLEET3_PUB_IP=$(hcloud server ip l44-fleet-3)
FLEET3_PRIV_IP=$(hcloud server describe l44-fleet-3 -o json | jq -r '.private_net[0].ip')
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
    export COORD_PUB_IP COORD_PRIV_IP
    export FLEET1_PUB_IP FLEET1_PRIV_IP
    export FLEET2_PUB_IP FLEET2_PRIV_IP
    export FLEET3_PUB_IP FLEET3_PRIV_IP
}

# Map machine_id → public IP
get_public_ip() {
    local machine_id="$1"
    case "$machine_id" in
        fleet-1) echo "$FLEET1_PUB_IP" ;;
        fleet-2) echo "$FLEET2_PUB_IP" ;;
        fleet-3) echo "$FLEET3_PUB_IP" ;;
        *) echo "" ;;
    esac
}

# ── SSH / API helpers ──

ssh_cmd() {
    local ip="$1"; shift
    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=5 root@"$ip" "$@" 2>/dev/null
}

docker_exec() {
    local ip="$1" container="$2"; shift 2
    ssh_cmd "$ip" "docker exec $container $*"
}

coord_api() {
    local method="$1" path="$2" body="${3:-}"
    if [ -n "$body" ]; then
        curl -sf -X "$method" -H "Content-Type: application/json" \
            -d "$body" "http://${COORD_PUB_IP}:8080${path}"
    else
        curl -sf -X "$method" "http://${COORD_PUB_IP}:8080${path}"
    fi
}

machine_api() {
    local ip="$1" method="$2" path="$3" body="${4:-}"
    if [ -n "$body" ]; then
        curl -sf -X "$method" -H "Content-Type: application/json" \
            -d "$body" "http://${ip}:8080${path}"
    else
        curl -sf -X "$method" "http://${ip}:8080${path}"
    fi
}

# ── Test framework ──

TOTAL_PASS=0
TOTAL_FAIL=0
PHASE_PASS=0
PHASE_FAIL=0

check() {
    local desc="$1"; shift
    if eval "$@" >/dev/null 2>&1; then
        echo "  ✓ $desc"
        PHASE_PASS=$((PHASE_PASS + 1))
        TOTAL_PASS=$((TOTAL_PASS + 1))
    else
        echo "  ✗ FAIL: $desc"
        PHASE_FAIL=$((PHASE_FAIL + 1))
        TOTAL_FAIL=$((TOTAL_FAIL + 1))
    fi
}

phase_start() {
    PHASE_PASS=0
    PHASE_FAIL=0
    echo ""
    echo "═══ Phase $1: $2 ═══"
}

phase_result() {
    echo "  Phase result: ${PHASE_PASS} passed, ${PHASE_FAIL} failed"
}

final_result() {
    echo ""
    echo "═══════════════════════════════════════════════════"
    if [ "$TOTAL_FAIL" -eq 0 ]; then
        echo " ALL PHASES COMPLETE: ${TOTAL_PASS}/${TOTAL_PASS} checks passed"
    else
        echo " FAILURES: ${TOTAL_PASS} passed, ${TOTAL_FAIL} failed"
    fi
    echo "═══════════════════════════════════════════════════"
    [ "$TOTAL_FAIL" -eq 0 ]
}

# ── Poll helpers ──

wait_for_user_status() {
    local user_id="$1" target_status="$2" timeout="${3:-120}"
    local elapsed=0
    local status=""
    while [ "$elapsed" -lt "$timeout" ]; do
        status=$(coord_api GET "/api/users/${user_id}" | jq -r '.status // empty')
        if [ "$status" = "$target_status" ]; then
            return 0
        fi
        if [ "$status" = "failed" ]; then
            echo "  ✗ User $user_id provisioning FAILED:"
            coord_api GET "/api/users/${user_id}" | jq -r '.error // "unknown error"'
            return 1
        fi
        sleep 2
        elapsed=$((elapsed + 2))
    done
    echo "  ✗ Timeout waiting for $user_id to reach $target_status (stuck at $status)"
    return 1
}

# Wait for a machine to reach a specific status in the coordinator
wait_for_machine_status() {
    local machine_id="$1" target_status="$2" timeout="${3:-90}"
    local elapsed=0
    local status=""
    while [ "$elapsed" -lt "$timeout" ]; do
        status=$(coord_api GET /api/fleet | jq -r ".machines[] | select(.machine_id == \"$machine_id\") | .status // empty")
        if [ "$status" = "$target_status" ]; then
            return 0
        fi
        sleep 2
        elapsed=$((elapsed + 2))
    done
    echo "  ✗ Timeout waiting for machine $machine_id to reach $target_status (stuck at $status)"
    return 1
}

# Wait for a user to have N bipods with a specific role
wait_for_user_bipod_count() {
    local user_id="$1" min_count="$2" timeout="${3:-300}"
    local elapsed=0
    local count=0
    while [ "$elapsed" -lt "$timeout" ]; do
        count=$(coord_api GET "/api/users/${user_id}/bipod" | jq '[.[] | select(.role != "stale")] | length')
        if [ "$count" -ge "$min_count" ]; then
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    echo "  ✗ Timeout waiting for $user_id to have $min_count live bipods (has $count)"
    return 1
}

# Wait for user to reach one of multiple target statuses
wait_for_user_status_multi() {
    local user_id="$1" timeout="${2:-120}"
    shift 2
    local targets=("$@")
    local elapsed=0
    local status=""
    while [ "$elapsed" -lt "$timeout" ]; do
        status=$(coord_api GET "/api/users/${user_id}" | jq -r '.status // empty')
        for target in "${targets[@]}"; do
            if [ "$status" = "$target" ]; then
                return 0
            fi
        done
        if [ "$status" = "failed" ]; then
            echo "  ✗ User $user_id is FAILED:"
            coord_api GET "/api/users/${user_id}" | jq -r '.error // "unknown error"'
            return 1
        fi
        sleep 2
        elapsed=$((elapsed + 2))
    done
    echo "  ✗ Timeout waiting for $user_id to reach [${targets[*]}] (stuck at $status)"
    return 1
}

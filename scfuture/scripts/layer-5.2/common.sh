#!/bin/bash
# common.sh — shared functions for Layer 5.2 scripts

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCFUTURE_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
IP_FILE="$SCRIPT_DIR/.ips"

NETWORK_NAME="l52-net"
SSH_KEY_NAME="l52-key"
LOCATION="nbg1"
SERVER_TYPE="cx23"
IMAGE="ubuntu-24.04"

# ── IP management ──

save_ips() {
    echo "Discovering machine IPs..."
    cat > "$IP_FILE" << EOF
COORD_PUB_IP=$(hcloud server ip l52-coordinator)
COORD_PRIV_IP=$(hcloud server describe l52-coordinator -o json | jq -r '.private_net[0].ip')
FLEET1_PUB_IP=$(hcloud server ip l52-fleet-1)
FLEET1_PRIV_IP=$(hcloud server describe l52-fleet-1 -o json | jq -r '.private_net[0].ip')
FLEET2_PUB_IP=$(hcloud server ip l52-fleet-2)
FLEET2_PRIV_IP=$(hcloud server describe l52-fleet-2 -o json | jq -r '.private_net[0].ip')
FLEET3_PUB_IP=$(hcloud server ip l52-fleet-3)
FLEET3_PRIV_IP=$(hcloud server describe l52-fleet-3 -o json | jq -r '.private_net[0].ip')
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
    local output
    if output=$(eval "$@" 2>&1); then
        echo "  ✓ $desc"
        PHASE_PASS=$((PHASE_PASS + 1))
        TOTAL_PASS=$((TOTAL_PASS + 1))
    else
        echo "  ✗ FAIL: $desc"
        if [ -n "$output" ]; then
            echo "$output" | head -5 | sed 's/^/    /'
        fi
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

# ── Layer 5.2 additions ──

# Database query helper (fallback — prefer API-based helpers below)
db_query() {
    local sql="$1"
    for _attempt in 1 2 3; do
        local result
        if result=$(ssh_cmd "$COORD_PUB_IP" "psql '$DATABASE_URL' -t -A -c \"$sql\"" | tr -d '\0' | tr -d '\r'); then
            result=$(echo "$result" | tr -d ' ')
            [ -n "$result" ] && echo "$result"
            return 0
        fi
        sleep 2
    done
    ssh_cmd "$COORD_PUB_IP" "psql '$DATABASE_URL' -t -A -c \"$sql\"" | tr -d '\0' | tr -d '\r'
}

# ── Event-log query helpers (API-based, no SSH needed) ──

# Query events via API. Pass query params as argument.
# Usage: query_events "type=migration&trigger=rebalancer&since=2024-01-01T00:00:00Z"
query_events() {
    coord_api GET "/api/events/query?$1"
}

# Count events via API. Returns integer count.
# Usage: count_events "type=migration&trigger=drain"
count_events() {
    local resp
    resp=$(coord_api GET "/api/events/count?$1" 2>/dev/null) || echo "0"
    echo "$resp" | jq -r '.count // 0'
}

# Mark current time for "since" queries (UTC ISO8601)
mark_time() {
    date -u +"%Y-%m-%dT%H:%M:%SZ"
}

# Wait for event to appear. Returns 0 on success, 1 on timeout.
# Usage: wait_for_event "type=drain_completed&machine_id=fleet-3" 120
wait_for_event() {
    local query="$1"
    local timeout="${2:-120}"
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local c
        c=$(count_events "$query")
        if [ "${c:-0}" -gt 0 ]; then
            return 0
        fi
        sleep 3
        elapsed=$((elapsed + 3))
    done
    return 1
}

# Wait for system stability via API (no SSH needed)
wait_stable() {
    local timeout="${1:-300}"
    local elapsed=0
    local stable_count=0
    while [ $elapsed -lt $timeout ]; do
        local resp
        resp=$(coord_api GET "/api/system/stable" 2>/dev/null) || resp='{}'
        local is_stable
        is_stable=$(echo "$resp" | jq -r '.stable // false')
        if [ "$is_stable" = "true" ]; then
            stable_count=$((stable_count + 1))
            # Require 3 consecutive stable checks (15s) to avoid false positives
            if [ $stable_count -ge 3 ]; then
                return 0
            fi
        else
            stable_count=0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    echo "  WARNING: system not stable after ${timeout}s"
    return 0  # don't fail the test, just warn
}

# Wait for all users on a machine to be migrated off (drain completion)
wait_for_machine_empty() {
    local machine_id=$1
    local timeout=${2:-300}
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local bipod_count
        bipod_count=$(db_query "SELECT COUNT(*) FROM bipods WHERE machine_id='$machine_id' AND role != 'stale'" 2>/dev/null | tr -d ' ')
        if [ "${bipod_count:-1}" = "0" ] 2>/dev/null; then
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    return 1
}

# Crash test helper
crash_test() {
    local fail_at="$1"
    local setup_cmd="$2"
    local trigger_cmd="$3"
    local description="$4"

    echo "    Crash test: $description (FAIL_AT=$fail_at)"

    for _attempt in 1 2 3; do
        if ssh_cmd "$COORD_PUB_IP" "
            mkdir -p /etc/systemd/system/coordinator.service.d
            cat > /etc/systemd/system/coordinator.service.d/fault.conf << 'EOF'
[Service]
Environment=FAIL_AT=$fail_at
EOF
            systemctl daemon-reload
            systemctl reset-failed coordinator 2>/dev/null || true
            systemctl restart coordinator
        "; then
            break
        fi
        echo "      Fault injection SSH failed (attempt $_attempt), retrying in 3s..."
        sleep 3
    done

    if wait_for_coordinator 45; then
        if [ -n "$setup_cmd" ]; then
            eval "$setup_cmd"
        fi
        eval "$trigger_cmd"

        for i in $(seq 1 120); do
            if ! ssh_cmd "$COORD_PUB_IP" "systemctl is-active coordinator" 2>/dev/null | grep -q "^active"; then
                break
            fi
            sleep 1
        done

        if ssh_cmd "$COORD_PUB_IP" "systemctl is-active coordinator" 2>/dev/null | grep -q "^active"; then
            echo "      INFO: Coordinator survived checkpoint (operation may have failed before reaching it)"
        else
            echo "      Coordinator crashed as expected"
        fi
    else
        echo "      Coordinator crashed during startup/reconciliation (crash loop at $fail_at)"
    fi

    for _attempt in 1 2 3; do
        if ssh_cmd "$COORD_PUB_IP" "
            rm -f /etc/systemd/system/coordinator.service.d/fault.conf
            systemctl daemon-reload
            systemctl reset-failed coordinator 2>/dev/null || true
            systemctl restart coordinator
        "; then
            break
        fi
        echo "      Recovery SSH failed (attempt $_attempt), retrying in 3s..."
        sleep 3
    done

    wait_for_coordinator 60 || true
    sleep 15
}

# Consistency checker — verifies system invariants
check_consistency() {
    local label="${1:-consistency}"
    local resp
    resp=$(coord_api GET "/api/system/consistency" 2>/dev/null) || {
        echo "    ✗ Consistency check API call failed ($label)"
        return 1
    }

    local pass
    pass=$(echo "$resp" | jq -r '.pass // false')

    if [ "$pass" = "true" ]; then
        echo "    ✓ All consistency invariants passed ($label)"
        return 0
    else
        # Print failing checks
        echo "$resp" | jq -r '.checks[] | select(.pass == false) | "    ✗ \(.name): \(.detail)"'
        local fail_count
        fail_count=$(echo "$resp" | jq '[.checks[] | select(.pass == false)] | length')
        echo "    ✗ $fail_count consistency invariant(s) FAILED ($label)"
        return 1
    fi
}

# Wait for coordinator HTTP to respond
wait_for_coordinator() {
    local timeout="${1:-30}"
    for i in $(seq 1 "$timeout"); do
        if coord_api GET /api/fleet >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    echo "WARNING: coordinator not ready after ${timeout}s"
    return 1
}

# Wait for all operations to settle (no in_progress operations)
wait_for_operations_settled() {
    local timeout="${1:-30}"
    for _i in $(seq 1 "$timeout"); do
        local in_progress
        in_progress=$(db_query "SELECT COUNT(*) FROM operations WHERE status = 'in_progress'") || true
        if [ "${in_progress:-0}" = "0" ]; then
            return 0
        fi
        sleep 1
    done
    echo "  WARNING: operations still in_progress after ${timeout}s"
    return 0
}

# Wait for system to be fully stable (no in_progress ops AND no users in transient states)
wait_for_system_stable() {
    local timeout="${1:-300}"
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        local in_progress
        in_progress=$(db_query "SELECT COUNT(*) FROM operations WHERE status = 'in_progress'" 2>/dev/null) || true
        local transient
        transient=$(db_query "SELECT COUNT(*) FROM users WHERE status IN ('provisioning','failing_over','reforming','suspending','reactivating','evicting','migrating')" 2>/dev/null) || true
        if [ "${in_progress:-1}" = "0" ] && [ "${transient:-1}" = "0" ]; then
            # Wait 15s to ensure rebalancer doesn't re-trigger (interval is 10s)
            local stable=true
            for _check in 1 2 3; do
                sleep 5
                in_progress=$(db_query "SELECT COUNT(*) FROM operations WHERE status = 'in_progress'" 2>/dev/null) || true
                transient=$(db_query "SELECT COUNT(*) FROM users WHERE status IN ('provisioning','failing_over','reforming','suspending','reactivating','evicting','migrating')" 2>/dev/null) || true
                if [ "${in_progress:-1}" != "0" ] || [ "${transient:-1}" != "0" ]; then
                    stable=false
                    break
                fi
            done
            if [ "$stable" = "true" ]; then
                return 0
            fi
        fi
        sleep 3
        elapsed=$((elapsed + 3))
    done
    echo "  WARNING: system not stable after ${timeout}s (ops=${in_progress:-?}, transient=${transient:-?})"
    return 0
}

# Find the free machine (not in a user's bipod)
find_free_machine() {
    local user_id="$1"
    local bipod_machines
    bipod_machines=$(coord_api GET "/api/users/${user_id}/bipod" | jq -r '[.[] | select(.role != "stale")] | .[].machine_id')
    for m in fleet-1 fleet-2 fleet-3; do
        if ! echo "$bipod_machines" | grep -q "^${m}$"; then
            echo "$m"
            return 0
        fi
    done
    echo ""
    return 1
}

# Get count of non-stale bipods on a machine
get_bipod_count_on_machine() {
    local machine_id="$1"
    db_query "SELECT COUNT(*) FROM bipods WHERE machine_id='$machine_id' AND role != 'stale'" | tr -d ' '
}

# Get the machine that has a user's bipod of a specific role
get_user_machine_by_role() {
    local user_id="$1" role="$2"
    coord_api GET "/api/users/${user_id}/bipod" | jq -r ".[] | select(.role == \"$role\") | .machine_id"
}

# Get active_agents count for a machine
get_machine_agents() {
    local machine_id="$1"
    coord_api GET /api/fleet | jq -r ".machines[] | select(.machine_id == \"$machine_id\") | .active_agents"
}

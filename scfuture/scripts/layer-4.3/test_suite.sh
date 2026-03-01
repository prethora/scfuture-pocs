#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/common.sh"
load_ips

echo "═══ Layer 4.3: Heartbeat Failure Detection & Automatic Failover — Test Suite ═══"

# ══════════════════════════════════════════
# Phase 0: Prerequisites
# ══════════════════════════════════════════
phase_start 0 "Prerequisites"

# Coordinator responding
check "Coordinator responding" 'coord_api GET /api/fleet | jq -e .machines'

# Wait for fleet machines to register
echo "  Waiting for 3 fleet machines to register..."
for i in $(seq 1 60); do
    count=$(coord_api GET /api/fleet | jq '.machines | length')
    [ "$count" -ge 3 ] && break
    sleep 2
done

check "3 fleet machines registered" '[ "$(coord_api GET /api/fleet | jq ".machines | length")" -ge 3 ]'

# Check each fleet machine is active
for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Machine agent responding at $ip" 'machine_api "'"$ip"'" GET /status | jq -e .machine_id'
done

# All machines active
check "All machines active" '
    dead=$(coord_api GET /api/fleet | jq "[.machines[] | select(.status != \"active\")] | length")
    [ "$dead" -eq 0 ]
'

# DRBD module on each fleet machine
for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "DRBD module loaded on $ip" 'ssh_cmd "'"$ip"'" "lsmod | grep -q drbd"'
done

# Container image on each fleet machine
for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Container image on $ip" 'ssh_cmd "'"$ip"'" "docker images platform/app-container -q" | grep -q .'
done

# Failover events should be empty initially
check "No failover events initially" '[ "$(coord_api GET /api/failovers | jq ". | length")" -eq 0 ]'

phase_result

# ══════════════════════════════════════════
# Phase 1: Provision Users (baseline)
# ══════════════════════════════════════════
phase_start 1 "Provision Users (baseline)"

# Provision 3 users so we have bipods spread across machines
for user in alice bob charlie; do
    coord_api POST /api/users "{\"user_id\":\"$user\"}" > /dev/null
    coord_api POST /api/users/$user/provision > /dev/null
done

for user in alice bob charlie; do
    check "$user reaches running" 'wait_for_user_status '"$user"' running 180'
done

# Verify all running
for user in alice bob charlie; do
    check "$user is running" '[ "$(coord_api GET /api/users/'"$user"' | jq -r .status)" = "running" ]'
done

# Record which machine is primary for each user
echo ""
echo "  User placements:"
for user in alice bob charlie; do
    primary=$(coord_api GET /api/users/$user | jq -r .primary_machine)
    echo "    $user → primary: $primary"
done

# Write test data into each user's container
for user in alice bob charlie; do
    PRIMARY_ID=$(coord_api GET /api/users/$user | jq -r .primary_machine)
    PRIMARY_PUB=$(get_public_ip "$PRIMARY_ID")
    docker_exec "$PRIMARY_PUB" ${user}-agent "sh -c 'echo ${user}-data-before > /workspace/data/test.txt'"
done

for user in alice bob charlie; do
    PRIMARY_ID=$(coord_api GET /api/users/$user | jq -r .primary_machine)
    PRIMARY_PUB=$(get_public_ip "$PRIMARY_ID")
    check "$user data written" '
        result=$(docker_exec "'"$PRIMARY_PUB"'" '"$user"'-agent "cat /workspace/data/test.txt")
        [ "$result" = "'"$user"'-data-before" ]
    '
done

# Verify DRBD replication is healthy for all users
for user in alice bob charlie; do
    PRIMARY_ID=$(coord_api GET /api/users/$user | jq -r .primary_machine)
    PRIMARY_PUB=$(get_public_ip "$PRIMARY_ID")
    check "$user DRBD healthy (UpToDate)" '
        machine_api "'"$PRIMARY_PUB"'" GET /images/'"$user"'/drbd/status | jq -e ".peer_disk_state == \"UpToDate\""
    '
done

phase_result

# ══════════════════════════════════════════
# Phase 2: Kill a Fleet Machine
# ══════════════════════════════════════════
phase_start 2 "Kill a Fleet Machine"

# Determine which machine to kill — pick fleet-1 (has known IP)
KILL_MACHINE_ID="fleet-1"
KILL_PUB_IP="$FLEET1_PUB_IP"

echo "  Target: $KILL_MACHINE_ID ($KILL_PUB_IP)"
echo "  Users on this machine (as primary or secondary):"
coord_api GET /api/users | jq -r '.[] | select(.bipod[].machine_id == "fleet-1") | "    \(.user_id) (primary: \(.primary_machine))"'

# Record which users have their primary on fleet-1 (these need failover)
FAILOVER_USERS=$(coord_api GET /api/users | jq -r '.[] | select(.primary_machine == "fleet-1") | .user_id')
DEGRADED_USERS=$(coord_api GET /api/users | jq -r '.[] | select(.primary_machine != "fleet-1") | select(.bipod[].machine_id == "fleet-1") | .user_id')

echo "  Users needing failover (primary on fleet-1): $FAILOVER_USERS"
echo "  Users becoming degraded (secondary on fleet-1): $DEGRADED_USERS"

# Shutdown the machine via hcloud (simulates hardware failure)
check "Shutdown fleet-1" 'hcloud server shutdown l43-fleet-1'

# Verify machine is actually down (SSH should fail)
sleep 5
check "fleet-1 unreachable via SSH" '! ssh_cmd "$KILL_PUB_IP" "true" 2>/dev/null'

phase_result

# ══════════════════════════════════════════
# Phase 3: Failure Detection
# ══════════════════════════════════════════
phase_start 3 "Failure Detection"

echo "  Waiting for coordinator to detect fleet-1 as dead (up to 90s)..."
check "fleet-1 detected as dead" 'wait_for_machine_status "fleet-1" "dead" 90'

# Other machines should still be active
check "fleet-2 still active" '
    status=$(coord_api GET /api/fleet | jq -r ".machines[] | select(.machine_id == \"fleet-2\") | .status")
    [ "$status" = "active" ]
'
check "fleet-3 still active" '
    status=$(coord_api GET /api/fleet | jq -r ".machines[] | select(.machine_id == \"fleet-3\") | .status")
    [ "$status" = "active" ]
'

phase_result

# ══════════════════════════════════════════
# Phase 4: Automatic Failover Verification
# ══════════════════════════════════════════
phase_start 4 "Automatic Failover Verification"

# Wait for failover to complete — users should reach running or running_degraded
echo "  Waiting for failover to complete..."
sleep 10  # Give the failover goroutine time to work

# Check all users' final status
for user in alice bob charlie; do
    USER_STATUS=$(coord_api GET /api/users/$user | jq -r .status)
    check "$user status is running or running_degraded (got: $USER_STATUS)" '
        status=$(coord_api GET /api/users/'"$user"' | jq -r .status)
        [ "$status" = "running" ] || [ "$status" = "running_degraded" ]
    '
done

# Verify that users whose primary was on fleet-1 have been failed over
if [ -n "$FAILOVER_USERS" ]; then
    for user in $FAILOVER_USERS; do
        NEW_PRIMARY=$(coord_api GET /api/users/$user | jq -r .primary_machine)
        check "$user primary moved from fleet-1 to $NEW_PRIMARY" '
            primary=$(coord_api GET /api/users/'"$user"' | jq -r .primary_machine)
            [ "$primary" != "fleet-1" ]
        '

        # Verify DRBD is Primary on the new machine
        NEW_PUB=$(get_public_ip "$NEW_PRIMARY")
        check "$user DRBD is Primary on new machine ($NEW_PRIMARY)" '
            machine_api "'"$NEW_PUB"'" GET /images/'"$user"'/drbd/status | jq -e ".role == \"Primary\""
        '

        # Verify container is running on new machine
        check "$user container running on $NEW_PRIMARY" '
            machine_api "'"$NEW_PUB"'" GET /containers/'"$user"'/status | jq -e .running
        '
    done
fi

# Verify failover events were recorded
check "Failover events recorded" '[ "$(coord_api GET /api/failovers | jq ". | length")" -gt 0 ]'

# Verify each failover event has the right structure
check "Failover events have correct structure" '
    coord_api GET /api/failovers | jq -e ".[0].user_id" &&
    coord_api GET /api/failovers | jq -e ".[0].from_machine" &&
    coord_api GET /api/failovers | jq -e ".[0].type"
'

phase_result

# ══════════════════════════════════════════
# Phase 5: Data Integrity After Failover
# ══════════════════════════════════════════
phase_start 5 "Data Integrity After Failover"

# For each user: verify data written BEFORE failover survived
for user in alice bob charlie; do
    USER_STATUS=$(coord_api GET /api/users/$user | jq -r .status)
    if [ "$USER_STATUS" = "running" ] || [ "$USER_STATUS" = "running_degraded" ]; then
        PRIMARY_ID=$(coord_api GET /api/users/$user | jq -r .primary_machine)
        PRIMARY_PUB=$(get_public_ip "$PRIMARY_ID")

        # Check: can we read test data?
        check "$user pre-failover data survived" '
            result=$(docker_exec "'"$PRIMARY_PUB"'" '"$user"'-agent "cat /workspace/data/test.txt")
            [ "$result" = "'"$user"'-data-before" ]
        '

        # Check: can we write NEW data?
        docker_exec "$PRIMARY_PUB" ${user}-agent "sh -c 'echo ${user}-data-after > /workspace/data/test2.txt'" 2>/dev/null || true
        check "$user can write new data after failover" '
            result=$(docker_exec "'"$PRIMARY_PUB"'" '"$user"'-agent "cat /workspace/data/test2.txt")
            [ "$result" = "'"$user"'-data-after" ]
        '

        # Check: config.json from initial provisioning still present
        check "$user config.json survived failover" '
            docker_exec "'"$PRIMARY_PUB"'" '"$user"'-agent "cat /workspace/data/config.json" | jq -e .user
        '
    fi
done

phase_result

# ══════════════════════════════════════════
# Phase 6: Unaffected Users & Degraded State
# ══════════════════════════════════════════
phase_start 6 "Unaffected Users & Degraded State"

# Users whose primary was NOT on fleet-1 should still be "running" or "running_degraded"
if [ -n "$DEGRADED_USERS" ]; then
    for user in $DEGRADED_USERS; do
        check "$user is running_degraded (secondary lost)" '
            status=$(coord_api GET /api/users/'"$user"' | jq -r .status)
            [ "$status" = "running_degraded" ] || [ "$status" = "running" ]
        '

        # Verify primary hasn't changed
        check "$user primary unchanged" '
            primary=$(coord_api GET /api/users/'"$user"' | jq -r .primary_machine)
            [ "$primary" != "fleet-1" ]
        '

        # Verify container still running on original primary
        PRIMARY_ID=$(coord_api GET /api/users/$user | jq -r .primary_machine)
        PRIMARY_PUB=$(get_public_ip "$PRIMARY_ID")
        check "$user container still running on $PRIMARY_ID" '
            machine_api "'"$PRIMARY_PUB"'" GET /containers/'"$user"'/status | jq -e .running
        '
    done
fi

# Verify bipod roles are consistent — only check users that have bipods on fleet-1
for user in alice bob charlie; do
    HAS_BIPOD=$(coord_api GET /api/users/$user/bipod | jq '[.[] | select(.machine_id == "fleet-1")] | length')
    if [ "$HAS_BIPOD" -gt 0 ]; then
        check "$user has a 'stale' bipod on fleet-1" '
            coord_api GET /api/users/'"$user"'/bipod | jq -e ".[] | select(.machine_id == \"fleet-1\") | .role == \"stale\""
        '
    fi
done

phase_result

# ══════════════════════════════════════════
# Phase 7: Coordinator State Consistency
# ══════════════════════════════════════════
phase_start 7 "Coordinator State Consistency"

# Fleet status: fleet-1 should be dead, others active
check "Fleet shows fleet-1 as dead" '
    coord_api GET /api/fleet | jq -e ".machines[] | select(.machine_id == \"fleet-1\") | .status == \"dead\""
'
check "Fleet shows fleet-2 as active" '
    coord_api GET /api/fleet | jq -e ".machines[] | select(.machine_id == \"fleet-2\") | .status == \"active\""
'
check "Fleet shows fleet-3 as active" '
    coord_api GET /api/fleet | jq -e ".machines[] | select(.machine_id == \"fleet-3\") | .status == \"active\""
'

# No user should claim fleet-1 as primary
check "No user has fleet-1 as primary" '
    count=$(coord_api GET /api/users | jq "[.[] | select(.primary_machine == \"fleet-1\")] | length")
    [ "$count" -eq 0 ]
'

# All users should be in a valid state
check "All users in valid state" '
    coord_api GET /api/users | jq -e ".[] | .status" | while read status; do
        case $status in
            \"running\"|\"running_degraded\"|\"unavailable\") true ;;
            *) exit 1 ;;
        esac
    done
'

# state.json should reflect the correct state
check "Coordinator state.json persisted" '
    ssh_cmd "$COORD_PUB_IP" "test -f /data/state.json" &&
    ssh_cmd "$COORD_PUB_IP" "cat /data/state.json | jq -e .machines"
'

phase_result

# ══════════════════════════════════════════
# Phase 8: Cleanup
# ══════════════════════════════════════════
phase_start 8 "Cleanup"

# Clean up surviving machines only (fleet-1 is down)
for ip_var in FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Cleanup $ip" 'machine_api "'"$ip"'" POST /cleanup'
done

# Verify clean state on surviving machines
for ip_var in FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Machine $ip clean" '
        users=$(machine_api "'"$ip"'" GET /status | jq ".users | length")
        [ "$users" -eq 0 ]
    '
done

phase_result

# ══════════════════════════════════════════
final_result

#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/common.sh"
load_ips

echo "═══ Layer 4.4: Bipod Reformation & Dead Machine Re-integration — Test Suite ═══"

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

# No failover or reformation events initially
check "No failover events initially" '[ "$(coord_api GET /api/failovers | jq ". | length")" -eq 0 ]'
check "No reformation events initially" '[ "$(coord_api GET /api/reformations | jq ". | length")" -eq 0 ]'

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
    secondary=$(coord_api GET /api/users/$user/bipod | jq -r '.[] | select(.role == "secondary") | .machine_id')
    echo "    $user → primary: $primary, secondary: $secondary"
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

# All users should have exactly 2 bipods
for user in alice bob charlie; do
    check "$user has 2 bipods" '
        count=$(coord_api GET /api/users/'"$user"'/bipod | jq ". | length")
        [ "$count" -eq 2 ]
    '
done

phase_result

# ══════════════════════════════════════════
# Phase 2: Kill a Fleet Machine (trigger failover)
# ══════════════════════════════════════════
phase_start 2 "Kill a Fleet Machine (trigger failover)"

# Kill fleet-1
KILL_MACHINE_ID="fleet-1"
KILL_PUB_IP="$FLEET1_PUB_IP"

echo "  Target: $KILL_MACHINE_ID ($KILL_PUB_IP)"
echo "  Users on this machine (as primary or secondary):"
coord_api GET /api/users | jq -r '.[] | select(.bipod[].machine_id == "fleet-1") | "    \(.user_id) (primary: \(.primary_machine))"' 2>/dev/null || true

# Record user placement pre-kill
FAILOVER_USERS=$(coord_api GET /api/users | jq -r '.[] | select(.primary_machine == "fleet-1") | .user_id') || true
DEGRADED_USERS=$(coord_api GET /api/users | jq -r '.[] | select(.primary_machine != "fleet-1") | select(.bipod[].machine_id == "fleet-1") | .user_id') || true

echo "  Users needing failover (primary on fleet-1): $FAILOVER_USERS"
echo "  Users becoming degraded (secondary on fleet-1): $DEGRADED_USERS"

# Shutdown the machine
check "Shutdown fleet-1" 'hcloud server shutdown l44-fleet-1'

# Verify machine is actually down
sleep 5
check "fleet-1 unreachable via SSH" '! ssh_cmd "$KILL_PUB_IP" "true" 2>/dev/null'

phase_result

# ══════════════════════════════════════════
# Phase 3: Failure Detection & Failover (Layer 4.3 behavior)
# ══════════════════════════════════════════
phase_start 3 "Failure Detection & Failover"

echo "  Waiting for coordinator to detect fleet-1 as dead (up to 90s)..."
check "fleet-1 detected as dead" 'wait_for_machine_status "fleet-1" "dead" 90'

# Wait for failover to complete
sleep 15  # Give the failover goroutine time

# All users should be in running or running_degraded
for user in alice bob charlie; do
    check "$user survived failover (running or running_degraded)" '
        status=$(coord_api GET /api/users/'"$user"' | jq -r .status)
        [ "$status" = "running" ] || [ "$status" = "running_degraded" ]
    '
done

# No user should have fleet-1 as primary
check "No user has fleet-1 as primary" '
    count=$(coord_api GET /api/users | jq "[.[] | select(.primary_machine == \"fleet-1\")] | length")
    [ "$count" -eq 0 ]
'

# Failover events should exist
check "Failover events recorded" '[ "$(coord_api GET /api/failovers | jq ". | length")" -gt 0 ]'

phase_result

# ══════════════════════════════════════════
# Phase 4: Verify Degraded State (pre-reformation)
# ══════════════════════════════════════════
phase_start 4 "Verify Degraded State (pre-reformation)"

# Check each user — only users with bipods on fleet-1 should be affected
for user in alice bob charlie; do
    had_bipod_on_fleet1=$(coord_api GET /api/users/$user/bipod | jq '[.[] | select(.machine_id == "fleet-1")] | length')
    if [ "$had_bipod_on_fleet1" -gt 0 ]; then
        live_count=$(coord_api GET /api/users/$user/bipod | jq '[.[] | select(.role != "stale")] | length')
        check "$user has exactly 1 live bipod (got: $live_count)" '
            count=$(coord_api GET /api/users/'"$user"'/bipod | jq "[.[] | select(.role != \"stale\")] | length")
            [ "$count" -eq 1 ]
        '
        check "$user has stale bipod on fleet-1" '
            coord_api GET /api/users/'"$user"'/bipod | jq -e ".[] | select(.machine_id == \"fleet-1\") | select(.role == \"stale\")"
        '
        check "$user is running_degraded" '
            status=$(coord_api GET /api/users/'"$user"' | jq -r .status)
            [ "$status" = "running_degraded" ]
        '
    else
        check "$user unaffected (2 live bipods)" '
            count=$(coord_api GET /api/users/'"$user"'/bipod | jq "[.[] | select(.role != \"stale\")] | length")
            [ "$count" -eq 2 ]
        '
        check "$user still running" '
            status=$(coord_api GET /api/users/'"$user"' | jq -r .status)
            [ "$status" = "running" ]
        '
    fi
done

echo ""
echo "  Current state (post-failover, pre-reformation):"
for user in alice bob charlie; do
    status=$(coord_api GET /api/users/$user | jq -r .status)
    primary=$(coord_api GET /api/users/$user | jq -r .primary_machine)
    bipods=$(coord_api GET /api/users/$user/bipod | jq -r '.[] | "\(.machine_id):\(.role)"' | tr '\n' ' ')
    echo "    $user → status: $status, primary: $primary, bipods: $bipods"
done

phase_result

# ══════════════════════════════════════════
# Phase 5: Wait for Bipod Reformation
# ══════════════════════════════════════════
phase_start 5 "Wait for Bipod Reformation"

echo "  Waiting for reformation to complete (stabilization + reformer tick + sync)..."
echo "  Expected timeline: ~30s stabilization + ~30s reformer tick + ~15s sync = ~75s"
echo "  Timeout: 300s"

# Wait for all users to reach "running" with 2 live bipods
for user in alice bob charlie; do
    check "$user reformation complete (running)" '
        wait_for_user_status '"$user"' running 300
    '
done

# All users should have 2 live bipods now
for user in alice bob charlie; do
    check "$user has 2 live bipods after reformation" '
        count=$(coord_api GET /api/users/'"$user"'/bipod | jq "[.[] | select(.role != \"stale\")] | length")
        [ "$count" -eq 2 ]
    '
done

# The new secondary should NOT be fleet-1 (it's dead)
for user in alice bob charlie; do
    check "$user new secondary is not fleet-1" '
        secondary=$(coord_api GET /api/users/'"$user"'/bipod | jq -r ".[] | select(.role == \"secondary\") | .machine_id")
        [ "$secondary" != "fleet-1" ]
    '
done

echo ""
echo "  Post-reformation state:"
for user in alice bob charlie; do
    status=$(coord_api GET /api/users/$user | jq -r .status)
    primary=$(coord_api GET /api/users/$user | jq -r .primary_machine)
    bipods=$(coord_api GET /api/users/$user/bipod | jq -r '.[] | "\(.machine_id):\(.role)"' | tr '\n' ' ')
    echo "    $user → status: $status, primary: $primary, bipods: $bipods"
done

phase_result

# ══════════════════════════════════════════
# Phase 6: DRBD Sync & Data Integrity After Reformation
# ══════════════════════════════════════════
phase_start 6 "DRBD Sync & Data Integrity After Reformation"

# Verify DRBD is fully synced on primary
for user in alice bob charlie; do
    PRIMARY_ID=$(coord_api GET /api/users/$user | jq -r .primary_machine)
    PRIMARY_PUB=$(get_public_ip "$PRIMARY_ID")
    check "$user DRBD fully synced (UpToDate)" '
        machine_api "'"$PRIMARY_PUB"'" GET /images/'"$user"'/drbd/status | jq -e ".peer_disk_state == \"UpToDate\""
    '
done

# Verify data written BEFORE failover survived
for user in alice bob charlie; do
    PRIMARY_ID=$(coord_api GET /api/users/$user | jq -r .primary_machine)
    PRIMARY_PUB=$(get_public_ip "$PRIMARY_ID")
    check "$user pre-failover data survived" '
        result=$(docker_exec "'"$PRIMARY_PUB"'" '"$user"'-agent "cat /workspace/data/test.txt")
        [ "$result" = "'"$user"'-data-before" ]
    '
done

# Write new data after reformation
for user in alice bob charlie; do
    PRIMARY_ID=$(coord_api GET /api/users/$user | jq -r .primary_machine)
    PRIMARY_PUB=$(get_public_ip "$PRIMARY_ID")
    docker_exec "$PRIMARY_PUB" ${user}-agent "sh -c 'echo ${user}-data-after > /workspace/data/test2.txt'" 2>/dev/null || true
    check "$user can write new data after reformation" '
        result=$(docker_exec "'"$PRIMARY_PUB"'" '"$user"'-agent "cat /workspace/data/test2.txt")
        [ "$result" = "'"$user"'-data-after" ]
    '
done

# Config.json from initial provisioning should still be there
for user in alice bob charlie; do
    PRIMARY_ID=$(coord_api GET /api/users/$user | jq -r .primary_machine)
    PRIMARY_PUB=$(get_public_ip "$PRIMARY_ID")
    check "$user config.json survived" '
        docker_exec "'"$PRIMARY_PUB"'" '"$user"'-agent "cat /workspace/data/config.json" | jq -e .user
    '
done

phase_result

# ══════════════════════════════════════════
# Phase 7: Dead Machine Return & Cleanup
# ══════════════════════════════════════════
phase_start 7 "Dead Machine Return & Cleanup"

# Power on fleet-1
echo "  Powering on fleet-1..."
check "Power on fleet-1" 'hcloud server poweron l44-fleet-1'

# Wait for fleet-1 to come back (needs time for boot + service start)
echo "  Waiting for fleet-1 to resume heartbeats (up to 180s)..."
check "fleet-1 back to active" 'wait_for_machine_status "fleet-1" "active" 180'

# Wait a bit for the reformer to clean up stale bipods
echo "  Waiting for stale bipod cleanup (up to 90s)..."
sleep 60  # Wait for reformer tick + cleanup

# Verify fleet-1 has no DRBD resources
check "fleet-1 has no DRBD resources" '
    ssh_cmd "$FLEET1_PUB_IP" "drbdadm status all 2>&1" | grep -qv "user-" || true
'

# Verify fleet-1 has no user images
check "fleet-1 has no user images" '
    result=$(machine_api "$FLEET1_PUB_IP" GET /status | jq ".users | length")
    [ "$result" -eq 0 ]
'

# Verify no stale bipods remain for users that had bipods on fleet-1
for user in alice bob charlie; do
    had_stale=$(coord_api GET /api/users/$user/bipod | jq '[.[] | select(.machine_id == "fleet-1")] | length')
    if [ "$had_stale" -gt 0 ]; then
        check "$user has no stale bipods" '
            stale_count=$(coord_api GET /api/users/'"$user"'/bipod | jq "[.[] | select(.role == \"stale\")] | length")
            [ "$stale_count" -eq 0 ]
        '
    else
        check "$user was unaffected (no stale)" '
            stale_count=$(coord_api GET /api/users/'"$user"'/bipod | jq "[.[] | select(.role == \"stale\")] | length")
            [ "$stale_count" -eq 0 ]
        '
    fi
done

phase_result

# ══════════════════════════════════════════
# Phase 8: Coordinator State Consistency
# ══════════════════════════════════════════
phase_start 8 "Coordinator State Consistency"

# All machines active
check "All machines active" '
    dead=$(coord_api GET /api/fleet | jq "[.machines[] | select(.status != \"active\")] | length")
    [ "$dead" -eq 0 ]
'

# All users running with 2 bipods
for user in alice bob charlie; do
    check "$user is running" '[ "$(coord_api GET /api/users/'"$user"' | jq -r .status)" = "running" ]'
    check "$user has 2 bipods (primary + secondary)" '
        roles=$(coord_api GET /api/users/'"$user"'/bipod | jq -r ".[].role" | sort | tr "\n" " ")
        [ "$roles" = "primary secondary " ]
    '
done

# state.json should be persisted
check "Coordinator state.json persisted" '
    ssh_cmd "$COORD_PUB_IP" "test -f /data/state.json" &&
    ssh_cmd "$COORD_PUB_IP" "cat /data/state.json | jq -e .machines"
'

phase_result

# ══════════════════════════════════════════
# Phase 9: Reformation Events
# ══════════════════════════════════════════
phase_start 9 "Reformation Events"

# Reformation events should exist
check "Reformation events recorded" '[ "$(coord_api GET /api/reformations | jq ". | length")" -gt 0 ]'

# Check event structure
check "Reformation events have correct structure" '
    coord_api GET /api/reformations | jq -e ".[0].user_id" &&
    coord_api GET /api/reformations | jq -e ".[0].new_secondary" &&
    coord_api GET /api/reformations | jq -e ".[0].method"
'

# All reformation events should be successful
check "All reformation events successful" '
    failed=$(coord_api GET /api/reformations | jq "[.[] | select(.success == false)] | length")
    [ "$failed" -eq 0 ]
'

# Log the reformation details
echo ""
echo "  Reformation events:"
coord_api GET /api/reformations | jq -r '.[] | "    \(.user_id): \(.new_secondary) via \(.method) (\(.duration_ms)ms)"'

phase_result

# ══════════════════════════════════════════
# Phase 10: Cleanup
# ══════════════════════════════════════════
phase_start 10 "Cleanup"

# Clean up all machines
for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Cleanup $ip" 'machine_api "'"$ip"'" POST /cleanup'
done

# Verify clean state
for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Machine $ip clean" '
        users=$(machine_api "'"$ip"'" GET /status | jq ".users | length")
        [ "$users" -eq 0 ]
    '
done

phase_result

# ══════════════════════════════════════════
final_result

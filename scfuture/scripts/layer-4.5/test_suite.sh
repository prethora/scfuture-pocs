#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/common.sh"
load_ips

echo "═══ Layer 4.5: Suspension, Reactivation & Deletion Lifecycle — Test Suite ═══"

# ══════════════════════════════════════════
# Phase 0: Prerequisites
# ══════════════════════════════════════════
phase_start 0 "Prerequisites"

check "Coordinator responding" 'coord_api GET /api/fleet | jq -e .machines'

echo "  Waiting for 3 fleet machines to register..."
for i in $(seq 1 60); do
    count=$(coord_api GET /api/fleet | jq '.machines | length')
    [ "$count" -ge 3 ] && break
    sleep 2
done

check "3 fleet machines registered" '[ "$(coord_api GET /api/fleet | jq ".machines | length")" -ge 3 ]'

for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Machine agent responding at $ip" 'machine_api "'"$ip"'" GET /status | jq -e .machine_id'
done

check "All machines active" '
    dead=$(coord_api GET /api/fleet | jq "[.machines[] | select(.status != \"active\")] | length")
    [ "$dead" -eq 0 ]
'

for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "DRBD module loaded on $ip" 'ssh_cmd "'"$ip"'" "lsmod | grep -q drbd"'
done

for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Container image on $ip" 'ssh_cmd "'"$ip"'" "docker images platform/app-container -q" | grep -q .'
done

for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "B2 CLI available on $ip" 'ssh_cmd "'"$ip"'" "which b2"'
done

check "No lifecycle events initially" '[ "$(coord_api GET /api/lifecycle-events | jq ". | length")" -eq 0 ]'

phase_result

# ══════════════════════════════════════════
# Phase 1: Provision Users (baseline)
# ══════════════════════════════════════════
phase_start 1 "Provision Users (baseline)"

for user in alice bob; do
    coord_api POST /api/users "{\"user_id\":\"$user\"}" > /dev/null
    coord_api POST /api/users/$user/provision > /dev/null
done

for user in alice bob; do
    wait_for_user_status "$user" "running" 120
    check "$user is running" '[ "$(coord_api GET /api/users/'"$user"' | jq -r .status)" = "running" ]'
done

# Write test data
ALICE_PRIMARY=$(coord_api GET /api/users/alice | jq -r .primary_machine)
ALICE_PRIMARY_IP=$(get_public_ip "$ALICE_PRIMARY")
check "Write test data to alice" '
    docker_exec "'"$ALICE_PRIMARY_IP"'" alice-agent "sh -c \"echo hello-alice > /workspace/data/test.txt\""
'

BOB_PRIMARY=$(coord_api GET /api/users/bob | jq -r .primary_machine)
BOB_PRIMARY_IP=$(get_public_ip "$BOB_PRIMARY")
check "Write test data to bob" '
    docker_exec "'"$BOB_PRIMARY_IP"'" bob-agent "sh -c \"echo hello-bob > /workspace/data/test.txt\""
'

# Verify DRBD healthy
check "alice DRBD UpToDate" '
    machine_api "'"$ALICE_PRIMARY_IP"'" GET /images/alice/drbd/status | jq -e "select(.peer_disk_state == \"UpToDate\")"
'
check "bob DRBD UpToDate" '
    machine_api "'"$BOB_PRIMARY_IP"'" GET /images/bob/drbd/status | jq -e "select(.peer_disk_state == \"UpToDate\")"
'

phase_result

# ══════════════════════════════════════════
# Phase 2: Suspend alice
# ══════════════════════════════════════════
phase_start 2 "Suspend alice"

coord_api POST /api/users/alice/suspend > /dev/null

wait_for_user_status "alice" "suspended" 120
check "alice is suspended" '[ "$(coord_api GET /api/users/alice | jq -r .status)" = "suspended" ]'

# Container should be stopped
check "alice container stopped" '
    machine_api "'"$ALICE_PRIMARY_IP"'" GET /containers/alice/status | jq -e "select(.running == false)"
'

# DRBD should be Secondary (demoted)
check "alice DRBD role is Secondary" '
    machine_api "'"$ALICE_PRIMARY_IP"'" GET /images/alice/drbd/status | jq -e "select(.role == \"Secondary\")"
'

# B2 backup should exist
check "alice has B2 backup" '[ "$(coord_api GET /api/users/alice | jq -r .backup_exists)" = "true" ]'

# Lifecycle event recorded
check "Suspension event recorded" '
    coord_api GET /api/lifecycle-events | jq -e "[.[] | select(.user_id == \"alice\" and .type == \"suspension\" and .success == true)] | length > 0"
'

phase_result

# ══════════════════════════════════════════
# Phase 3: Warm Reactivation
# ══════════════════════════════════════════
phase_start 3 "Warm Reactivation (alice)"

coord_api POST /api/users/alice/reactivate > /dev/null

wait_for_user_status "alice" "running" 120
check "alice is running again" '[ "$(coord_api GET /api/users/alice | jq -r .status)" = "running" ]'

# Container should be running
ALICE_PRIMARY=$(coord_api GET /api/users/alice | jq -r .primary_machine)
ALICE_PRIMARY_IP=$(get_public_ip "$ALICE_PRIMARY")

check "alice container running" '
    machine_api "'"$ALICE_PRIMARY_IP"'" GET /containers/alice/status | jq -e "select(.running == true)"
'

# Data should be intact
check "alice test data intact" '
    docker_exec "'"$ALICE_PRIMARY_IP"'" alice-agent "cat /workspace/data/test.txt" | grep -q "hello-alice"
'

# DRBD should be Primary
check "alice DRBD is Primary" '
    machine_api "'"$ALICE_PRIMARY_IP"'" GET /images/alice/drbd/status | jq -e "select(.role == \"Primary\")"
'

check "Warm reactivation event recorded" '
    coord_api GET /api/lifecycle-events | jq -e "[.[] | select(.user_id == \"alice\" and .type == \"reactivation_warm\" and .success == true)] | length > 0"
'

phase_result

# ══════════════════════════════════════════
# Phase 4: Suspend alice again (for eviction test)
# ══════════════════════════════════════════
phase_start 4 "Suspend alice again (pre-eviction)"

# Write more data first
check "Write more data to alice" '
    docker_exec "'"$ALICE_PRIMARY_IP"'" alice-agent "sh -c \"echo post-reactivation-data > /workspace/data/test2.txt\""
'

coord_api POST /api/users/alice/suspend > /dev/null

wait_for_user_status "alice" "suspended" 120
check "alice suspended again" '[ "$(coord_api GET /api/users/alice | jq -r .status)" = "suspended" ]'
check "alice B2 backup updated" '[ "$(coord_api GET /api/users/alice | jq -r .backup_exists)" = "true" ]'

phase_result

# ══════════════════════════════════════════
# Phase 5: Evict alice
# ══════════════════════════════════════════
phase_start 5 "Evict alice"

coord_api POST /api/users/alice/evict > /dev/null

wait_for_user_status "alice" "evicted" 120
check "alice is evicted" '[ "$(coord_api GET /api/users/alice | jq -r .status)" = "evicted" ]'

# No bipods should remain
check "alice has no bipods" '[ "$(coord_api GET /api/users/alice/bipod | jq ". | length")" -eq 0 ]'

# Images should be deleted on fleet machines
check "alice image deleted on primary" '
    machine_api "'"$ALICE_PRIMARY_IP"'" GET /status | jq -e "select(.users.alice == null)"
'

check "Eviction event recorded" '
    coord_api GET /api/lifecycle-events | jq -e "[.[] | select(.user_id == \"alice\" and .type == \"eviction\" and .success == true)] | length > 0"
'

phase_result

# ══════════════════════════════════════════
# Phase 6: Cold Reactivation from B2
# ══════════════════════════════════════════
phase_start 6 "Cold Reactivation (alice from B2)"

coord_api POST /api/users/alice/reactivate > /dev/null

wait_for_user_status "alice" "running" 300
check "alice is running after cold reactivation" '[ "$(coord_api GET /api/users/alice | jq -r .status)" = "running" ]'

ALICE_PRIMARY=$(coord_api GET /api/users/alice | jq -r .primary_machine)
ALICE_PRIMARY_IP=$(get_public_ip "$ALICE_PRIMARY")

check "alice container running" '
    machine_api "'"$ALICE_PRIMARY_IP"'" GET /containers/alice/status | jq -e "select(.running == true)"
'

# Data should be intact — both the original and post-reactivation data
check "alice original data survived cold restore" '
    docker_exec "'"$ALICE_PRIMARY_IP"'" alice-agent "cat /workspace/data/test.txt" | grep -q "hello-alice"
'
check "alice post-reactivation data survived cold restore" '
    docker_exec "'"$ALICE_PRIMARY_IP"'" alice-agent "cat /workspace/data/test2.txt" | grep -q "post-reactivation-data"
'

# Should have 2 bipods (fully replicated)
check "alice has 2 bipods" '[ "$(coord_api GET /api/users/alice/bipod | jq "[.[] | select(.role != \"stale\")] | length")" -eq 2 ]'

check "Cold reactivation event recorded" '
    coord_api GET /api/lifecycle-events | jq -e "[.[] | select(.user_id == \"alice\" and .type == \"reactivation_cold\" and .success == true)] | length > 0"
'

phase_result

# ══════════════════════════════════════════
# Phase 7: Retention Enforcer — DRBD Disconnect
# ══════════════════════════════════════════
phase_start 7 "Retention Enforcer — DRBD Disconnect (bob)"

# Suspend bob
coord_api POST /api/users/bob/suspend > /dev/null
wait_for_user_status "bob" "suspended" 120
check "bob is suspended" '[ "$(coord_api GET /api/users/bob | jq -r .status)" = "suspended" ]'

# Wait for retention enforcer to disconnect DRBD (WARM_RETENTION_SECONDS=15)
echo "  Waiting for retention enforcer to disconnect DRBD (~15-75s)..."
for i in $(seq 1 90); do
    disconnected=$(coord_api GET /api/users/bob | jq -r '.drbd_disconnected // false')
    if [ "$disconnected" = "true" ]; then
        break
    fi
    sleep 1
done

check "bob DRBD disconnected by retention enforcer" '[ "$(coord_api GET /api/users/bob | jq -r .drbd_disconnected)" = "true" ]'

check "DRBD disconnect event recorded" '
    coord_api GET /api/lifecycle-events | jq -e "[.[] | select(.user_id == \"bob\" and .type == \"drbd_disconnect\" and .success == true)] | length > 0"
'

# Verify DRBD is actually StandAlone on the machine
BOB_PRIMARY=$(coord_api GET /api/users/bob | jq -r .primary_machine)
BOB_PRIMARY_IP=$(get_public_ip "$BOB_PRIMARY")
check "bob DRBD is StandAlone" '
    machine_api "'"$BOB_PRIMARY_IP"'" GET /images/bob/drbd/status | jq -e "select(.connection_state == \"StandAlone\")"
'

phase_result

# ══════════════════════════════════════════
# Phase 8: Retention Enforcer — Auto Eviction
# ══════════════════════════════════════════
phase_start 8 "Retention Enforcer — Auto Eviction (bob)"

# Wait for retention enforcer to auto-evict bob (EVICTION_SECONDS=30)
echo "  Waiting for retention enforcer to auto-evict bob (~30-90s from suspension)..."
wait_for_user_status "bob" "evicted" 120

check "bob is auto-evicted" '[ "$(coord_api GET /api/users/bob | jq -r .status)" = "evicted" ]'
check "bob has no bipods" '[ "$(coord_api GET /api/users/bob/bipod | jq ". | length")" -eq 0 ]'

phase_result

# ══════════════════════════════════════════
# Phase 9: Coordinator State Consistency
# ══════════════════════════════════════════
phase_start 9 "Coordinator State Consistency"

check "alice is running" '[ "$(coord_api GET /api/users/alice | jq -r .status)" = "running" ]'
check "bob is evicted" '[ "$(coord_api GET /api/users/bob | jq -r .status)" = "evicted" ]'

check "alice has backup" '[ "$(coord_api GET /api/users/alice | jq -r .backup_exists)" = "true" ]'
check "bob has backup" '[ "$(coord_api GET /api/users/bob | jq -r .backup_exists)" = "true" ]'

check "Lifecycle events count >= 6" '[ "$(coord_api GET /api/lifecycle-events | jq ". | length")" -ge 6 ]'

# state.json should exist and be valid
check "state.json persisted" 'ssh_cmd "$COORD_PUB_IP" "cat /data/state.json" | jq -e .users'

phase_result

# ══════════════════════════════════════════
# Phase 10: Cleanup
# ══════════════════════════════════════════
phase_start 10 "Cleanup"

for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Cleanup $ip" 'machine_api "'"$ip"'" POST /cleanup'
done

for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Verify clean $ip" '
        user_count=$(machine_api "'"$ip"'" GET /status | jq ".users | length")
        [ "$user_count" -eq 0 ]
    '
done

phase_result

# ══════════════════════════════════════════
# Final Result
# ══════════════════════════════════════════
final_result

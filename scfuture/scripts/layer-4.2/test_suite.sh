#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/common.sh"
load_ips

echo "═══ Layer 4.2: Coordinator Happy Path — Test Suite ═══"

# ══════════════════════════════════════════
# Phase 0: Prerequisites
# ══════════════════════════════════════════
phase_start 0 "Prerequisites"

# Coordinator responding
check "Coordinator responding" 'coord_api GET /api/fleet | jq -e .machines'

# Wait for fleet machines to register (they register on startup)
echo "  Waiting for 3 fleet machines to register..."
for i in $(seq 1 60); do
    count=$(coord_api GET /api/fleet | jq '.machines | length')
    [ "$count" -ge 3 ] && break
    sleep 2
done

check "3 fleet machines registered" '[ "$(coord_api GET /api/fleet | jq ".machines | length")" -ge 3 ]'

# Check each fleet machine
for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Machine agent responding at $ip" 'machine_api "'"$ip"'" GET /status | jq -e .machine_id'
done

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

phase_result

# ══════════════════════════════════════════
# Phase 1: Provision First User (alice)
# ══════════════════════════════════════════
phase_start 1 "Provision First User (alice)"

check "Create user alice" 'coord_api POST /api/users "{\"user_id\":\"alice\"}" | jq -e ".status == \"registered\""'
check "Provision alice" 'coord_api POST /api/users/alice/provision | jq -e ".status == \"provisioning\""'
check "Alice reaches running" 'wait_for_user_status alice running 180'

# Verify through coordinator API
check "Alice status is running" '[ "$(coord_api GET /api/users/alice | jq -r .status)" = "running" ]'
check "Alice has primary machine" '[ -n "$(coord_api GET /api/users/alice | jq -r .primary_machine)" ]'
check "Alice has DRBD port" '[ "$(coord_api GET /api/users/alice | jq -r .drbd_port)" -ge 7900 ]'
check "Alice has 2 bipod entries" '[ "$(coord_api GET /api/users/alice | jq ".bipod | length")" -eq 2 ]'

# Verify on actual machines
ALICE_PRIMARY_ID=$(coord_api GET /api/users/alice | jq -r .primary_machine)
ALICE_PRIMARY_PUB=$(get_public_ip "$ALICE_PRIMARY_ID")

check "Container running on primary" 'machine_api "'"$ALICE_PRIMARY_PUB"'" GET /containers/alice/status | jq -e .running'

# Verify data accessible inside container
check "Data accessible in container" 'docker_exec "'"$ALICE_PRIMARY_PUB"'" alice-agent "cat /workspace/data/config.json" | jq -e .user'

phase_result

# ══════════════════════════════════════════
# Phase 2: Provision Second User (bob)
# ══════════════════════════════════════════
phase_start 2 "Provision Second User (bob)"

check "Create user bob" 'coord_api POST /api/users "{\"user_id\":\"bob\"}" | jq -e ".status == \"registered\""'
check "Provision bob" 'coord_api POST /api/users/bob/provision | jq -e ".status == \"provisioning\""'
check "Bob reaches running" 'wait_for_user_status bob running 180'
check "Bob status is running" '[ "$(coord_api GET /api/users/bob | jq -r .status)" = "running" ]'

BOB_PRIMARY_ID=$(coord_api GET /api/users/bob | jq -r .primary_machine)
check "Bob placed (primary: $BOB_PRIMARY_ID)" 'true'

BOB_PUB=$(get_public_ip "$BOB_PRIMARY_ID")
check "Bob container running" 'machine_api "'"$BOB_PUB"'" GET /containers/bob/status | jq -e .running'

phase_result

# ══════════════════════════════════════════
# Phase 3: Provision Third User (charlie)
# ══════════════════════════════════════════
phase_start 3 "Provision Third User (charlie)"

coord_api POST /api/users '{"user_id":"charlie"}' > /dev/null
coord_api POST /api/users/charlie/provision > /dev/null
check "Charlie reaches running" 'wait_for_user_status charlie running 180'
check "Charlie is running" '[ "$(coord_api GET /api/users/charlie | jq -r .status)" = "running" ]'

CHARLIE_PRIMARY_ID=$(coord_api GET /api/users/charlie | jq -r .primary_machine)
check "Charlie placed (primary: $CHARLIE_PRIMARY_ID)" 'true'

phase_result

# ══════════════════════════════════════════
# Phase 4: Provision More Users (dave, eve)
# ══════════════════════════════════════════
phase_start 4 "Provision Users dave and eve"

for user in dave eve; do
    coord_api POST /api/users "{\"user_id\":\"$user\"}" > /dev/null
    coord_api POST /api/users/$user/provision > /dev/null
    check "$user reaches running" 'wait_for_user_status '"$user"' running 180'
    check "$user is running" '[ "$(coord_api GET /api/users/'"$user"' | jq -r .status)" = "running" ]'
done

check "5 users total in coordinator" '[ "$(coord_api GET /api/users | jq ". | length")" -eq 5 ]'

phase_result

# ══════════════════════════════════════════
# Phase 5: Fleet Status Verification
# ══════════════════════════════════════════
phase_start 5 "Fleet Status Verification"

check "Fleet shows 3 machines" '[ "$(coord_api GET /api/fleet | jq ".machines | length")" -eq 3 ]'

# Verify balanced placement — no machine should have all agents
check "Balanced: no machine has >4 agents" '
    max=$(coord_api GET /api/fleet | jq "[.machines[].active_agents] | max")
    [ "$max" -le 4 ]
'

# Verify coordinator view matches machine agent reality
for ip_var in FLEET1_PUB_IP FLEET2_PUB_IP FLEET3_PUB_IP; do
    ip="${!ip_var}"
    check "Machine $ip status consistent" 'machine_api "'"$ip"'" GET /status | jq -e .machine_id'
done

# All users accessible
for user in alice bob charlie dave eve; do
    check "User $user accessible via coordinator" '[ "$(coord_api GET /api/users/'"$user"' | jq -r .status)" = "running" ]'
done

phase_result

# ══════════════════════════════════════════
# Phase 6: Data Isolation
# ══════════════════════════════════════════
phase_start 6 "Data Isolation"

# Write unique data to alice and bob
ALICE_PUB=$(get_public_ip "$(coord_api GET /api/users/alice | jq -r .primary_machine)")
BOB_PUB=$(get_public_ip "$(coord_api GET /api/users/bob | jq -r .primary_machine)")

check "Write data to alice" 'docker_exec "'"$ALICE_PUB"'" alice-agent "sh -c \"echo alice-secret > /workspace/data/secret.txt\""'
check "Write data to bob" 'docker_exec "'"$BOB_PUB"'" bob-agent "sh -c \"echo bob-secret > /workspace/data/secret.txt\""'
check "Alice reads her own data" '
    result=$(docker_exec "'"$ALICE_PUB"'" alice-agent "cat /workspace/data/secret.txt")
    [ "$result" = "alice-secret" ]
'
check "Bob reads his own data" '
    result=$(docker_exec "'"$BOB_PUB"'" bob-agent "cat /workspace/data/secret.txt")
    [ "$result" = "bob-secret" ]
'

# Verify DRBD replication status for a user
check "Alice DRBD healthy" '
    machine_api "'"$ALICE_PUB"'" GET /images/alice/drbd/status | jq -e ".peer_disk_state == \"UpToDate\""
'

phase_result

# ══════════════════════════════════════════
# Phase 7: Cleanup
# ══════════════════════════════════════════
phase_start 7 "Cleanup"

# Clean up all users via machine agent cleanup endpoints
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

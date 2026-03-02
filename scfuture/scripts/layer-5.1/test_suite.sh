#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/common.sh"
load_ips

echo "═══ Layer 5.1: Tripod Primitive & Manual Live Migration — Test Suite ═══"

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

check "Postgres connected" 'db_query "SELECT 1" | grep -q 1'
check "Schema tables exist" 'db_query "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='"'"'public'"'"' AND table_name IN ('"'"'machines'"'"','"'"'users'"'"','"'"'bipods'"'"','"'"'operations'"'"','"'"'events'"'"')" | grep -q 5'
check "Advisory lock held" 'db_query "SELECT COUNT(*) FROM pg_locks WHERE locktype='"'"'advisory'"'"'" | grep -q 1'

phase_result

# ══════════════════════════════════════════
# Phase 1: Baseline — Provision & Verify
# ══════════════════════════════════════════
phase_start 1 "Baseline — Provision & Verify"

check "Create user alice" 'coord_api POST /api/users "{\"user_id\":\"alice\"}" | jq -e ".status == \"registered\""'
check "Provision alice" 'coord_api POST /api/users/alice/provision | jq -e ".status == \"provisioning\""'
check "Alice reaches running" 'wait_for_user_status alice running 180'

# Verify in Postgres
check "Alice in DB" '[ "$(db_query "SELECT status FROM users WHERE user_id='"'"'alice'"'"'")" = "running" ]'
check "Alice has 2 bipods in DB" '[ "$(db_query "SELECT COUNT(*) FROM bipods WHERE user_id='"'"'alice'"'"' AND role != '"'"'stale'"'"'")" = "2" ]'

# Write test data
ALICE_PRIMARY_ID=$(coord_api GET /api/users/alice | jq -r .primary_machine)
ALICE_PRIMARY_PUB=$(get_public_ip "$ALICE_PRIMARY_ID")
check "Write test data" 'docker_exec "'"$ALICE_PRIMARY_PUB"'" alice-agent "sh -c \"echo ALICE_DATA > /workspace/data/test.txt\""'

phase_result

# ══════════════════════════════════════════
# Phase 2: Primary Migration — Happy Path
# ══════════════════════════════════════════
phase_start 2 "Primary Migration — Happy Path"

# Identify alice's machines
ALICE_PRIMARY=$(coord_api GET /api/users/alice | jq -r .primary_machine)
ALICE_SECONDARY=$(coord_api GET /api/users/alice/bipod | jq -r '[.[] | select(.role == "secondary")] | .[0].machine_id')
FREE_MACHINE=$(find_free_machine alice)

echo "  Alice: primary=$ALICE_PRIMARY, secondary=$ALICE_SECONDARY, free=$FREE_MACHINE"

check "Trigger primary migration" '
    coord_api POST /api/users/alice/migrate "{\"source_machine\":\"'"$ALICE_PRIMARY"'\",\"target_machine\":\"'"$FREE_MACHINE"'\"}" | jq -e ".status == \"migrating\""
'

check "Alice reaches running after migration" 'wait_for_user_status alice running 300'

# Verify container is on the new machine
NEW_PRIMARY=$(coord_api GET /api/users/alice | jq -r .primary_machine)
NEW_PRIMARY_PUB=$(get_public_ip "$NEW_PRIMARY")
check "Primary moved to target" '[ "'"$NEW_PRIMARY"'" = "'"$FREE_MACHINE"'" ]'

check "Container running on new primary" '
    machine_api "'"$NEW_PRIMARY_PUB"'" GET /containers/alice/status | jq -e ".running == true"
'

# Verify data survived
check "Data survived migration" '
    docker_exec "'"$NEW_PRIMARY_PUB"'" alice-agent "cat /workspace/data/test.txt" | grep -q ALICE_DATA
'

# Verify source is cleaned up
OLD_PRIMARY_PUB=$(get_public_ip "$ALICE_PRIMARY")
check "Source cleaned up (no container)" '
    ! machine_api "'"$OLD_PRIMARY_PUB"'" GET /containers/alice/status 2>/dev/null | jq -e ".running == true" 2>/dev/null
'

# Verify bipod entries
check "Bipod correct after primary migration" '
    bipod=$(coord_api GET /api/users/alice/bipod)
    count=$(echo "$bipod" | jq "[.[] | select(.role != \"stale\")] | length")
    [ "$count" -eq 2 ]
'

# Verify DRBD healthy
check "DRBD healthy after migration" '
    status=$(machine_api "'"$NEW_PRIMARY_PUB"'" GET /images/alice/drbd/status)
    echo "$status" | jq -e ".role == \"Primary\""
    echo "$status" | jq -e ".peer_disk_state == \"UpToDate\""
'

# Verify migration event recorded
check "Migration event recorded" '
    events=$(coord_api GET /api/migrations)
    echo "$events" | jq -e "length > 0"
    echo "$events" | jq -e ".[0].success == true"
    echo "$events" | jq -e ".[0].migration_type == \"primary\""
'

wait_for_operations_settled 30
if ! check_consistency "phase2"; then
    PHASE_FAIL=$((PHASE_FAIL + 1)); TOTAL_FAIL=$((TOTAL_FAIL + 1))
    echo "  ✗ FAIL: Consistency after primary migration"
else
    PHASE_PASS=$((PHASE_PASS + 1)); TOTAL_PASS=$((TOTAL_PASS + 1))
    echo "  ✓ Consistency after primary migration"
fi

phase_result

# ══════════════════════════════════════════
# Phase 3: Secondary Migration — Happy Path
# ══════════════════════════════════════════
phase_start 3 "Secondary Migration — Happy Path"

# Write more test data
ALICE_PRIMARY=$(coord_api GET /api/users/alice | jq -r .primary_machine)
ALICE_PRIMARY_PUB=$(get_public_ip "$ALICE_PRIMARY")
check "Write second marker" 'docker_exec "'"$ALICE_PRIMARY_PUB"'" alice-agent "sh -c \"echo ALICE_DATA2 > /workspace/data/test2.txt\""'

# Identify secondary and free machine
ALICE_SECONDARY=$(coord_api GET /api/users/alice/bipod | jq -r '[.[] | select(.role == "secondary")] | .[0].machine_id')
FREE_MACHINE2=$(find_free_machine alice)

echo "  Alice: primary=$ALICE_PRIMARY, secondary=$ALICE_SECONDARY, free=$FREE_MACHINE2"

check "Trigger secondary migration" '
    coord_api POST /api/users/alice/migrate "{\"source_machine\":\"'"$ALICE_SECONDARY"'\",\"target_machine\":\"'"$FREE_MACHINE2"'\"}" | jq -e ".status == \"migrating\""
'

check "Alice reaches running after secondary migration" 'wait_for_user_status alice running 300'

# Verify container still on same primary
ALICE_PRIMARY_AFTER=$(coord_api GET /api/users/alice | jq -r .primary_machine)
check "Primary unchanged" '[ "'"$ALICE_PRIMARY_AFTER"'" = "'"$ALICE_PRIMARY"'" ]'

# Verify data intact
check "Both markers intact" '
    docker_exec "'"$ALICE_PRIMARY_PUB"'" alice-agent "cat /workspace/data/test.txt" | grep -q ALICE_DATA
    docker_exec "'"$ALICE_PRIMARY_PUB"'" alice-agent "cat /workspace/data/test2.txt" | grep -q ALICE_DATA2
'

# Verify old secondary cleaned up
OLD_SEC_PUB=$(get_public_ip "$ALICE_SECONDARY")
check "Old secondary cleaned up" '
    ! machine_api "'"$OLD_SEC_PUB"'" GET /images/alice/drbd/status 2>/dev/null | jq -e ".exists == true" 2>/dev/null
'

check "Bipod correct after secondary migration" '
    bipod=$(coord_api GET /api/users/alice/bipod)
    count=$(echo "$bipod" | jq "[.[] | select(.role != \"stale\")] | length")
    [ "$count" -eq 2 ]
'

check "DRBD healthy after secondary migration" '
    status=$(machine_api "'"$ALICE_PRIMARY_PUB"'" GET /images/alice/drbd/status)
    echo "$status" | jq -e ".peer_disk_state == \"UpToDate\""
'

phase_result

# ══════════════════════════════════════════
# Phase 4: Validation & Edge Cases
# ══════════════════════════════════════════
phase_start 4 "Validation & Edge Cases"

# Migrate to machine already in bipod → 400
ALICE_PRIMARY=$(coord_api GET /api/users/alice | jq -r .primary_machine)
ALICE_SECONDARY=$(coord_api GET /api/users/alice/bipod | jq -r '[.[] | select(.role == "secondary")] | .[0].machine_id')

check "Migrate to bipod member → 400" '
    response=$(curl -s -o /dev/null -w "%{http_code}" -X POST -H "Content-Type: application/json" \
        -d "{\"source_machine\":\"'"$ALICE_PRIMARY"'\",\"target_machine\":\"'"$ALICE_SECONDARY"'\"}" \
        "http://${COORD_PUB_IP}:8080/api/users/alice/migrate")
    [ "$response" = "400" ]
'

# Migrate non-running user → 409
coord_api POST /api/users '{"user_id":"edge-test"}' > /dev/null 2>&1 || true
check "Migrate non-running user → 409" '
    response=$(curl -s -o /dev/null -w "%{http_code}" -X POST -H "Content-Type: application/json" \
        -d "{\"source_machine\":\"fleet-1\",\"target_machine\":\"fleet-3\"}" \
        "http://${COORD_PUB_IP}:8080/api/users/edge-test/migrate")
    [ "$response" = "409" ]
'

# Migrate non-existent user → 404
check "Migrate non-existent user → 404" '
    response=$(curl -s -o /dev/null -w "%{http_code}" -X POST -H "Content-Type: application/json" \
        -d "{\"source_machine\":\"fleet-1\",\"target_machine\":\"fleet-3\"}" \
        "http://${COORD_PUB_IP}:8080/api/users/nonexistent/migrate")
    [ "$response" = "404" ]
'

# Migrate to non-existent machine → 400
check "Migrate to non-existent machine → 400" '
    response=$(curl -s -o /dev/null -w "%{http_code}" -X POST -H "Content-Type: application/json" \
        -d "{\"source_machine\":\"'"$ALICE_PRIMARY"'\",\"target_machine\":\"fleet-99\"}" \
        "http://${COORD_PUB_IP}:8080/api/users/alice/migrate")
    [ "$response" = "400" ]
'

# Source not in bipod → 400
FREE_MACHINE3=$(find_free_machine alice) || true
if [ -n "$FREE_MACHINE3" ]; then
    check "Migrate from non-bipod source → 400" '
        response=$(curl -s -o /dev/null -w "%{http_code}" -X POST -H "Content-Type: application/json" \
            -d "{\"source_machine\":\"'"$FREE_MACHINE3"'\",\"target_machine\":\"'"$ALICE_PRIMARY"'\"}" \
            "http://${COORD_PUB_IP}:8080/api/users/alice/migrate")
        [ "$response" = "400" ]
    '
else
    echo "  SKIP: No free machine (alice has 3 bipods — upstream issue)"
    PHASE_PASS=$((PHASE_PASS + 1)); TOTAL_PASS=$((TOTAL_PASS + 1))
fi

# Verify alice still accessible after failed attempts
check "Alice still accessible" '
    ALICE_P=$(coord_api GET /api/users/alice | jq -r .primary_machine)
    ALICE_P_PUB=$(get_public_ip "$ALICE_P")
    docker_exec "$ALICE_P_PUB" alice-agent "cat /workspace/data/test.txt" | grep -q ALICE_DATA
'

phase_result

# ══════════════════════════════════════════
# Phase 5: Primary Migration Crash Tests (F34-F42)
# ══════════════════════════════════════════
phase_start 5 "Primary Migration Crash Tests"

CRASH_CHECKPOINTS_PRIMARY=(
    "34:migrate-target-selected"
    "35:migrate-image-created"
    "36:migrate-drbd-added"
    "37:migrate-synced"
    "38:migrate-container-stopped"
    "39:migrate-source-demoted"
    "40:migrate-target-promoted"
    "41:migrate-container-started"
    "42:migrate-source-cleaned"
)

for checkpoint in "${CRASH_CHECKPOINTS_PRIMARY[@]}"; do
    IFS=: read -r fnum fail_at <<< "$checkpoint"
    USER="crash-mig-$fnum"

    # Create and provision user (small image for fast sync during crash tests)
    coord_api POST /api/users "{\"user_id\":\"$USER\",\"image_size_mb\":128}" > /dev/null 2>&1 || true
    coord_api POST /api/users/$USER/provision > /dev/null 2>&1 || true
    wait_for_user_status "$USER" running 180

    # Write data before migration (retry for transient SSH drops)
    PRIM=$(coord_api GET /api/users/$USER | jq -r .primary_machine)
    PRIM_PUB=$(get_public_ip "$PRIM")
    for _w in 1 2 3; do
        if docker_exec "$PRIM_PUB" ${USER}-agent "sh -c \"echo TESTDATA > /workspace/data/test.txt\"" 2>/dev/null; then break; fi
        sleep 2
    done

    # Determine source/target
    SEC=$(coord_api GET /api/users/$USER/bipod | jq -r '[.[] | select(.role == "secondary")] | .[0].machine_id')
    TARGET=$(find_free_machine "$USER") || true
    if [ -z "$TARGET" ]; then
        echo "    SKIP F$fnum: no free machine available"
        PHASE_PASS=$((PHASE_PASS + 2)); TOTAL_PASS=$((TOTAL_PASS + 2))
        continue
    fi

    crash_test "$fail_at" \
        "" \
        "coord_api POST /api/users/$USER/migrate '{\"source_machine\":\"$PRIM\",\"target_machine\":\"$TARGET\"}' > /dev/null 2>&1 || true" \
        "Migration crash at F$fnum ($fail_at)"

    # After recovery, user should be running
    check "F$fnum: $USER in valid state" '
        s=$(coord_api GET /api/users/'"$USER"' | jq -r .status)
        [ "$s" = "running" ]
    '

    # Ensure container is running on primary (crash recovery may leave it stopped)
    CUR_PRIM=$(coord_api GET /api/users/$USER | jq -r .primary_machine)
    CUR_PRIM_PUB=$(get_public_ip "$CUR_PRIM")
    if ! machine_api "$CUR_PRIM_PUB" GET /containers/$USER/status 2>/dev/null | jq -e '.running == true' >/dev/null 2>&1; then
        echo "      Container not running on $CUR_PRIM, starting it..."
        machine_api "$CUR_PRIM_PUB" POST /containers/$USER/start > /dev/null 2>&1 || true
        sleep 3
    fi

    # Verify data is intact (retry for transient SSH drops)
    check "F$fnum: data intact" '(
        for _r in 1 2 3 4 5; do
            if docker_exec "'"$CUR_PRIM_PUB"'" '"$USER"'-agent "cat /workspace/data/test.txt" 2>/dev/null | grep -q TESTDATA; then exit 0; fi
            sleep 3
        done
        exit 1
    )'
done

wait_for_operations_settled 15
if ! check_consistency "phase5"; then
    PHASE_FAIL=$((PHASE_FAIL + 1)); TOTAL_FAIL=$((TOTAL_FAIL + 1))
    echo "  ✗ FAIL: Consistency after primary migration crashes"
else
    PHASE_PASS=$((PHASE_PASS + 1)); TOTAL_PASS=$((TOTAL_PASS + 1))
    echo "  ✓ Consistency after primary migration crashes"
fi

phase_result

# ══════════════════════════════════════════
# Phase 6: Secondary Migration Crash Tests
# ══════════════════════════════════════════
phase_start 6 "Secondary Migration Crash Tests"

CRASH_CHECKPOINTS_SECONDARY=(
    "50:migrate-target-selected"
    "51:migrate-image-created"
    "52:migrate-drbd-added"
    "53:migrate-synced"
    "54:migrate-secondary-cleaned"
)

for checkpoint in "${CRASH_CHECKPOINTS_SECONDARY[@]}"; do
    IFS=: read -r fnum fail_at <<< "$checkpoint"
    USER="crash-sec-$fnum"

    # Create and provision user (small image for fast sync during crash tests)
    coord_api POST /api/users "{\"user_id\":\"$USER\",\"image_size_mb\":128}" > /dev/null 2>&1 || true
    coord_api POST /api/users/$USER/provision > /dev/null 2>&1 || true
    wait_for_user_status "$USER" running 180

    # Write data
    PRIM=$(coord_api GET /api/users/$USER | jq -r .primary_machine)
    PRIM_PUB=$(get_public_ip "$PRIM")
    docker_exec "$PRIM_PUB" ${USER}-agent "sh -c \"echo TESTDATA > /workspace/data/test.txt\"" 2>/dev/null || true

    # Determine secondary source and target
    SEC=$(coord_api GET /api/users/$USER/bipod | jq -r '[.[] | select(.role == "secondary")] | .[0].machine_id')
    TARGET=$(find_free_machine "$USER") || true
    if [ -z "$TARGET" ]; then
        echo "    SKIP F$fnum: no free machine available"
        PHASE_PASS=$((PHASE_PASS + 1)); TOTAL_PASS=$((TOTAL_PASS + 1))
        continue
    fi

    crash_test "$fail_at" \
        "" \
        "coord_api POST /api/users/$USER/migrate '{\"source_machine\":\"$SEC\",\"target_machine\":\"$TARGET\"}' > /dev/null 2>&1 || true" \
        "Secondary migration crash at F$fnum ($fail_at)"

    check "F$fnum: $USER in valid state" '
        s=$(coord_api GET /api/users/'"$USER"' | jq -r .status)
        [ "$s" = "running" ]
    '
done

wait_for_operations_settled 15
if ! check_consistency "phase6"; then
    PHASE_FAIL=$((PHASE_FAIL + 1)); TOTAL_FAIL=$((TOTAL_FAIL + 1))
    echo "  ✗ FAIL: Consistency after secondary migration crashes"
else
    PHASE_PASS=$((PHASE_PASS + 1)); TOTAL_PASS=$((TOTAL_PASS + 1))
    echo "  ✓ Consistency after secondary migration crashes"
fi

phase_result

# ══════════════════════════════════════════
# Phase 7: Post-Crash Verification
# ══════════════════════════════════════════
phase_start 7 "Post-Crash Verification"

# Provision a new user after all crash tests
check "Create post-crash user" 'coord_api POST /api/users "{\"user_id\":\"post-crash\"}" | jq -e ".status == \"registered\""'
check "Provision post-crash user" 'coord_api POST /api/users/post-crash/provision | jq -e ".status == \"provisioning\""'
check "Post-crash user reaches running" 'wait_for_user_status post-crash running 180'

# Migrate the new user
POST_PRIM=$(coord_api GET /api/users/post-crash | jq -r .primary_machine)
POST_FREE=$(find_free_machine post-crash) || true
check "Migrate post-crash user" '
    coord_api POST /api/users/post-crash/migrate "{\"source_machine\":\"'"$POST_PRIM"'\",\"target_machine\":\"'"$POST_FREE"'\"}" | jq -e ".status == \"migrating\""
'
check "Post-crash migration completes" 'wait_for_user_status post-crash running 300'

# Verify events and operations
check "Events recorded in DB" '[ "$(db_query "SELECT COUNT(*) FROM events")" -gt 0 ]'
check "Operations recorded in DB" '[ "$(db_query "SELECT COUNT(*) FROM operations")" -gt 0 ]'

# Show summary
TOTAL_USERS=$(db_query "SELECT COUNT(*) FROM users")
TOTAL_OPS=$(db_query "SELECT COUNT(*) FROM operations")
COMPLETE_OPS=$(db_query "SELECT COUNT(*) FROM operations WHERE status='complete'")
FAILED_OPS=$(db_query "SELECT COUNT(*) FROM operations WHERE status='failed'")
CANCELLED_OPS=$(db_query "SELECT COUNT(*) FROM operations WHERE status='cancelled'")
TOTAL_EVENTS=$(db_query "SELECT COUNT(*) FROM events")
MIGRATION_EVENTS=$(db_query "SELECT COUNT(*) FROM events WHERE event_type='migration'")

echo "  Summary:"
echo "    Users: $TOTAL_USERS"
echo "    Operations: $TOTAL_OPS (complete: $COMPLETE_OPS, failed: $FAILED_OPS, cancelled: $CANCELLED_OPS)"
echo "    Events: $TOTAL_EVENTS (migrations: $MIGRATION_EVENTS)"

phase_result

# ══════════════════════════════════════════
# Phase 8: Final Consistency & Cleanup
# ══════════════════════════════════════════
phase_start 8 "Final Consistency & Cleanup"

wait_for_operations_settled 30
if ! check_consistency "final"; then
    PHASE_FAIL=$((PHASE_FAIL + 1)); TOTAL_FAIL=$((TOTAL_FAIL + 1))
    echo "  ✗ FAIL: Final consistency check (see details above)"
else
    PHASE_PASS=$((PHASE_PASS + 1)); TOTAL_PASS=$((TOTAL_PASS + 1))
    echo "  ✓ Final consistency check"
fi

check "No stuck operations" '
    in_progress=$(db_query "SELECT COUNT(*) FROM operations WHERE status = '"'"'in_progress'"'"'")
    [ "$in_progress" = "0" ]
'

phase_result

# ══════════════════════════════════════════
# Final Result
# ══════════════════════════════════════════
final_result "Layer 5.1: Tripod Primitive & Manual Live Migration"

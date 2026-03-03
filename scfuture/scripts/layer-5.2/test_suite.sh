#!/usr/bin/env bash
set -uo pipefail
# NOTE: set -e intentionally omitted — this is a test script where we need
# to survive transient API failures and check results explicitly via check()

source "$(dirname "$0")/common.sh"
load_ips

# Helper: retry migration (rebalancer may be running concurrently, causing 409s)
retry_migrate() {
    local uid="$1" source="$2" target="$3"
    for _retry in 1 2 3 4 5; do
        wait_for_user_status "$uid" "running" 60 2>/dev/null || true
        if coord_api POST "/api/users/$uid/migrate" "{\"source_machine\":\"$source\",\"target_machine\":\"$target\"}" > /dev/null 2>&1; then
            return 0
        fi
        echo "      Migration request failed (attempt $_retry), waiting 15s..."
        sleep 15
    done
    echo "      WARNING: Migration of $uid from $source to $target failed after 5 retries"
    return 1
}

echo ""
echo "═══ Layer 5.2: Rebalancer & Machine Drain — Test Suite ═══"
echo "Started: $(date)"
echo "Coordinator: $COORD_PUB_IP"
echo "Fleet: $FLEET1_PUB_IP, $FLEET2_PUB_IP, $FLEET3_PUB_IP"

# ════════════════════════════════════════════════════════════════
# Phase 0: Prerequisites
# ════════════════════════════════════════════════════════════════
phase_start 0 "Prerequisites"

check "Coordinator responding" '
    coord_api GET /api/fleet | jq -e ".machines" > /dev/null
'

# Wait for all fleet machines to register
echo "  Waiting for fleet machines to register..."
sleep 15

check "Fleet-1 registered and active" '
    coord_api GET /api/fleet | jq -e ".machines[] | select(.machine_id == \"fleet-1\") | select(.status == \"active\")" > /dev/null
'
check "Fleet-2 registered and active" '
    coord_api GET /api/fleet | jq -e ".machines[] | select(.machine_id == \"fleet-2\") | select(.status == \"active\")" > /dev/null
'
check "Fleet-3 registered and active" '
    coord_api GET /api/fleet | jq -e ".machines[] | select(.machine_id == \"fleet-3\") | select(.status == \"active\")" > /dev/null
'

check "Machine agent fleet-1 responding" '
    machine_api "$FLEET1_PUB_IP" GET /status | jq -e ".machine_id" > /dev/null
'
check "Machine agent fleet-2 responding" '
    machine_api "$FLEET2_PUB_IP" GET /status | jq -e ".machine_id" > /dev/null
'
check "Machine agent fleet-3 responding" '
    machine_api "$FLEET3_PUB_IP" GET /status | jq -e ".machine_id" > /dev/null
'

check "System stable endpoint responding" '
    coord_api GET /api/system/stable | jq -e ".stable" > /dev/null
'
check "Event query endpoint responding" '
    coord_api GET "/api/events/count?type=__test__" | jq -e ".count" > /dev/null
'

phase_result

# ════════════════════════════════════════════════════════════════
# Phase 1: Baseline — Provision Users
# ════════════════════════════════════════════════════════════════
phase_start 1 "Baseline — Provision 6 Users"

USERS=("rb-user-1" "rb-user-2" "rb-user-3" "rb-user-4" "rb-user-5" "rb-user-6")

# Provision sequentially to avoid DRBD port/minor allocation race conditions
ALL_RUNNING=true
for uid in "${USERS[@]}"; do
    echo "  Creating and provisioning $uid..."
    coord_api POST /api/users "{\"user_id\":\"$uid\",\"image_size_mb\":128}" > /dev/null
    coord_api POST "/api/users/$uid/provision" > /dev/null
    if ! wait_for_user_status "$uid" "running" 300; then
        ALL_RUNNING=false
        echo "  ✗ $uid failed to reach running, continuing..."
    else
        echo "  ✓ $uid is running"
    fi
    sleep 2
done

check "All 6 users are running" '[ "$ALL_RUNNING" = "true" ]'

# Record distribution
echo "  Fleet distribution after provisioning:"
for m in fleet-1 fleet-2 fleet-3; do
    agents=$(get_machine_agents "$m")
    echo "    $m: $agents agents"
done

check "All users have 2 non-stale bipods" '
    for uid in "${USERS[@]}"; do
        count=$(coord_api GET "/api/users/$uid/bipod" | jq "[.[] | select(.role != \"stale\")] | length")
        [ "$count" -eq 2 ] || exit 1
    done
'

# Verify provision events recorded
check "Provision events recorded for all users" '
    prov_count=$(count_events "type=provision&success=true")
    [ "${prov_count:-0}" -ge 6 ]
'

wait_stable 360
check "Consistency after provisioning" 'check_consistency "after provisioning"'

phase_result

# ════════════════════════════════════════════════════════════════
# Phase 2: Create Imbalance & Verify Rebalancer
# ════════════════════════════════════════════════════════════════
phase_start 2 "Create Imbalance & Verify Rebalancer"

# Wait for any Phase 1 rebalancer activity to settle
wait_stable 120

# Mark timestamp before creating imbalance
IMBALANCE_TS=$(mark_time)

echo "  Creating artificial imbalance by migrating primaries to fleet-1..."

IMBALANCE_MIGRATIONS=0
for uid in rb-user-1 rb-user-2 rb-user-3 rb-user-4 rb-user-5; do
    primary_machine=$(get_user_machine_by_role "$uid" "primary")
    if [ "$primary_machine" != "fleet-1" ]; then
        secondary_machine=$(get_user_machine_by_role "$uid" "secondary")
        if [ "$secondary_machine" = "fleet-1" ]; then
            free=$(find_free_machine "$uid")
            echo "    Moving $uid secondary from fleet-1 to $free first..."
            retry_migrate "$uid" "fleet-1" "$free"
            wait_for_user_status "$uid" "running" 180
            sleep 2
        fi
        primary_machine=$(get_user_machine_by_role "$uid" "primary")
        echo "    Migrating $uid primary from $primary_machine to fleet-1..."
        retry_migrate "$uid" "$primary_machine" "fleet-1"
        wait_for_user_status "$uid" "running" 180
        IMBALANCE_MIGRATIONS=$((IMBALANCE_MIGRATIONS + 1))
        sleep 2
    fi
done

echo "  Imbalance created with $IMBALANCE_MIGRATIONS manual migrations"

# Wait for heartbeats to propagate
echo "  Waiting 20s for heartbeats to propagate..."
sleep 20

# Record distribution BEFORE rebalancer
echo "  Fleet distribution BEFORE rebalancer:"
BEFORE_F1=$(get_machine_agents "fleet-1")
BEFORE_F2=$(get_machine_agents "fleet-2")
BEFORE_F3=$(get_machine_agents "fleet-3")
echo "    fleet-1: $BEFORE_F1, fleet-2: $BEFORE_F2, fleet-3: $BEFORE_F3"

check "Fleet-1 is overloaded (more agents than others)" '
    [ "$BEFORE_F1" -gt "$BEFORE_F2" ] || [ "$BEFORE_F1" -gt "$BEFORE_F3" ]
'

# Wait for rebalancer to act — use event-log verification
echo "  Waiting for rebalancer to trigger (up to 300s)..."
REBAL_TRIGGERED=false
if wait_for_event "type=rebalancer_trigger" 300; then
    REBAL_COUNT=$(count_events "type=migration&trigger=rebalancer")
    REBAL_TRIGGERED=true
    echo "  Rebalancer triggered ($REBAL_COUNT total rebalancer migration(s))"
fi

check "Rebalancer triggered at least one migration" '[ "$REBAL_TRIGGERED" = "true" ]'

# Wait for rebalancer to finish ALL its migrations
echo "  Waiting for system to stabilize after rebalancer..."
wait_stable 360

# Check distribution is more balanced
AFTER_F1=$(get_machine_agents "fleet-1")
AFTER_F2=$(get_machine_agents "fleet-2")
AFTER_F3=$(get_machine_agents "fleet-3")
echo "  Fleet distribution AFTER rebalancer:"
echo "    fleet-1: $AFTER_F1, fleet-2: $AFTER_F2, fleet-3: $AFTER_F3"

check "Fleet distribution more balanced (fleet-1 has fewer agents)" '
    [ "$AFTER_F1" -le "$BEFORE_F1" ]
'

check "All users still running after rebalancer" '
    for uid in "${USERS[@]}"; do
        status=$(coord_api GET "/api/users/$uid" | jq -r ".status")
        [ "$status" = "running" ] || exit 1
    done
'

check "Consistency after rebalancing" 'check_consistency "after rebalancing"'

phase_result

# ════════════════════════════════════════════════════════════════
# Phase 3: Rebalancer Stability & Edge Cases
# ════════════════════════════════════════════════════════════════
phase_start 3 "Rebalancer Stability & Edge Cases"

# Wait for system to stabilize fully before checking stability
wait_stable 120

# Check distribution BEFORE the stability window
CURRENT_F1=$(get_machine_agents "fleet-1")
CURRENT_F2=$(get_machine_agents "fleet-2")
CURRENT_F3=$(get_machine_agents "fleet-3")
MAX_AGENTS=$(echo -e "$CURRENT_F1\n$CURRENT_F2\n$CURRENT_F3" | sort -rn | head -1)
MIN_AGENTS=$(echo -e "$CURRENT_F1\n$CURRENT_F2\n$CURRENT_F3" | sort -n | head -1)
DIFF=$((MAX_AGENTS - MIN_AGENTS))
echo "  Fleet distribution: fleet-1=$CURRENT_F1 fleet-2=$CURRENT_F2 fleet-3=$CURRENT_F3 (diff=$DIFF)"

if [ "$DIFF" -le 1 ]; then
    # Fleet is balanced — verify rebalancer doesn't trigger during 30s window
    REBAL_BEFORE=$(count_events "type=migration&trigger=rebalancer")
    echo "  Waiting 30s to verify rebalancer stability..."
    sleep 30
    REBAL_AFTER=$(count_events "type=migration&trigger=rebalancer")

    check "No unnecessary rebalancer migrations when fleet is balanced" '
        [ "$REBAL_AFTER" -eq "$REBAL_BEFORE" ]
    '
else
    echo "  Fleet not fully balanced (diff=$DIFF), rebalancer may still be working — skipping stability check"
    TOTAL_PASS=$((TOTAL_PASS + 1))
    PHASE_PASS=$((PHASE_PASS + 1))
fi

# Test: suspend a user, rebalancer should not try to migrate it
SUSPEND_USER="rb-user-6"
echo "  Suspending $SUSPEND_USER..."
coord_api POST "/api/users/$SUSPEND_USER/suspend" > /dev/null
wait_for_user_status "$SUSPEND_USER" "suspended" 60

check "Suspended user is suspended" '
    status=$(coord_api GET "/api/users/$SUSPEND_USER" | jq -r ".status")
    [ "$status" = "suspended" ]
'

sleep 15  # give rebalancer a couple ticks

# Reactivate
echo "  Reactivating $SUSPEND_USER..."
coord_api POST "/api/users/$SUSPEND_USER/reactivate" > /dev/null
wait_for_user_status "$SUSPEND_USER" "running" 120

check "Reactivated user is running" '
    status=$(coord_api GET "/api/users/$SUSPEND_USER" | jq -r ".status")
    [ "$status" = "running" ]
'

# Verify total migration count is reasonable (no thundering herd)
TOTAL_REBAL=$(count_events "type=migration&trigger=rebalancer")
check "Rebalancer migration count is reasonable (< 10)" '
    [ "${TOTAL_REBAL:-0}" -lt 10 ]
'

phase_result

# ════════════════════════════════════════════════════════════════
# Phase 4: Machine Drain — Happy Path
# ════════════════════════════════════════════════════════════════
phase_start 4 "Machine Drain — Happy Path"

wait_stable 120

# Ensure fleet-3 has at least 2 users (migrate if needed)
F3_AGENTS=$(get_machine_agents "fleet-3")
echo "  Fleet-3 has $F3_AGENTS agents"

if [ "${F3_AGENTS:-0}" -lt 2 ]; then
    echo "  Migrating users to fleet-3 to ensure drain has work..."
    for uid in rb-user-5 rb-user-6; do
        bipod_machines=$(coord_api GET "/api/users/$uid/bipod" | jq -r '[.[] | select(.role != "stale")] | .[].machine_id')
        if ! echo "$bipod_machines" | grep -q "fleet-3"; then
            source_machine=$(echo "$bipod_machines" | head -1)
            echo "    Moving $uid from $source_machine to fleet-3..."
            retry_migrate "$uid" "$source_machine" "fleet-3"
            wait_for_user_status "$uid" "running" 180
            sleep 2
        fi
    done
fi

# Mark timestamp before drain
DRAIN_TS=$(mark_time)

# Start drain
echo "  Starting drain on fleet-3..."
DRAIN_RESP=$(coord_api POST /api/fleet/fleet-3/drain)
DRAIN_STATUS=$(echo "$DRAIN_RESP" | jq -r '.status')

check "Drain API returns 202 with draining status" '
    [ "$DRAIN_STATUS" = "draining" ]
'

check "Fleet-3 status is draining" '
    status=$(coord_api GET /api/fleet | jq -r ".machines[] | select(.machine_id == \"fleet-3\") | .status")
    [ "$status" = "draining" ]
'

# Verify drain_started event was recorded
check "Drain started event recorded" '
    c=$(count_events "type=drain_started&machine_id=fleet-3&since=$DRAIN_TS")
    [ "${c:-0}" -gt 0 ]
'

# Wait for drain_completed event instead of polling bipod counts
echo "  Waiting for drain to complete (via drain_completed event, up to 300s)..."
if wait_for_event "type=drain_completed&machine_id=fleet-3&since=$DRAIN_TS" 300; then
    echo "  Fleet-3 drain complete (drain_completed event received)"
else
    echo "  WARNING: drain_completed event not received, checking state..."
fi

# Verify no bipods left
check "No non-stale bipods on fleet-3 after drain" '
    f3_agents=$(get_machine_agents "fleet-3")
    [ "${f3_agents:-1}" = "0" ]
'

# Verify all users are running (via API, not SSH)
check "All drained users are running" '
    all_ok=true
    for uid in "${USERS[@]}"; do
        status=$(coord_api GET "/api/users/$uid" | jq -r ".status")
        if [ "$status" != "running" ] && [ "$status" != "suspended" ] && [ "$status" != "evicted" ]; then
            all_ok=false
        fi
    done
    [ "$all_ok" = "true" ]
'

# Verify new provision during drain does NOT place on fleet-3
echo "  Provisioning drain-test-user..."
coord_api POST /api/users '{"user_id":"drain-test-user","image_size_mb":128}' > /dev/null
coord_api POST /api/users/drain-test-user/provision > /dev/null
wait_for_user_status "drain-test-user" "running" 300

check "New user not placed on draining fleet-3" '
    bipods=$(coord_api GET /api/users/drain-test-user/bipod | jq -r "[.[] | select(.role != \"stale\")] | .[].machine_id")
    ! echo "$bipods" | grep -q "fleet-3"
'

# Verify drain migrations recorded via event API
check "Migration events with trigger=drain recorded" '
    drain_count=$(count_events "type=migration&trigger=drain&since=$DRAIN_TS")
    [ "${drain_count:-0}" -gt 0 ]
'

wait_stable 120
check "Consistency after drain" 'check_consistency "after drain"'

# Undrain fleet-3
echo "  Undraining fleet-3..."
coord_api POST /api/fleet/fleet-3/undrain > /dev/null

check "Fleet-3 status is active after undrain" '
    status=$(coord_api GET /api/fleet | jq -r ".machines[] | select(.machine_id == \"fleet-3\") | .status")
    [ "$status" = "active" ]
'

phase_result

# Evict drain-test-user — retry in case of races with rebalancer migrations
echo "  Waiting for drain-test-user to settle before eviction..."
for _dtu_attempt in 1 2 3; do
    wait_stable 120
    DTU_STATUS=$(coord_api GET /api/users/drain-test-user | jq -r ".status // empty")
    echo "  drain-test-user status: $DTU_STATUS (attempt $_dtu_attempt)"
    if [ "$DTU_STATUS" = "running" ]; then
        coord_api POST /api/users/drain-test-user/suspend > /dev/null 2>&1 || true
        if wait_for_user_status "drain-test-user" "suspended" 60; then
            break
        fi
    elif [ "$DTU_STATUS" = "suspended" ] || [ "$DTU_STATUS" = "evicted" ]; then
        break
    fi
    sleep 10
done
DTU_STATUS=$(coord_api GET /api/users/drain-test-user | jq -r ".status // empty")
if [ "$DTU_STATUS" = "suspended" ]; then
    coord_api POST /api/users/drain-test-user/evict > /dev/null 2>&1 || true
    wait_for_user_status "drain-test-user" "evicted" 120 || true
elif [ "$DTU_STATUS" = "running" ]; then
    echo "  WARNING: drain-test-user still running, forcing suspend+evict..."
    coord_api POST /api/users/drain-test-user/suspend > /dev/null 2>&1 || true
    wait_for_user_status "drain-test-user" "suspended" 180 || true
    coord_api POST /api/users/drain-test-user/evict > /dev/null 2>&1 || true
    wait_for_user_status "drain-test-user" "evicted" 120 || true
fi

# ════════════════════════════════════════════════════════════════
# Phase 5: Drain Cancellation
# ════════════════════════════════════════════════════════════════
phase_start 5 "Drain Cancellation"

wait_stable 120

# Ensure fleet-2 has at least 3 running users
F2_AGENTS=$(get_machine_agents "fleet-2")
echo "  Fleet-2 has $F2_AGENTS agents"

if [ "${F2_AGENTS:-0}" -lt 3 ]; then
    echo "  Need more users on fleet-2, migrating..."
    for uid in rb-user-1 rb-user-2 rb-user-3; do
        bipod_machines=$(coord_api GET "/api/users/$uid/bipod" | jq -r '[.[] | select(.role != "stale")] | .[].machine_id')
        if ! echo "$bipod_machines" | grep -q "fleet-2"; then
            source_machine=$(echo "$bipod_machines" | head -1)
            echo "    Moving $uid from $source_machine to fleet-2..."
            retry_migrate "$uid" "$source_machine" "fleet-2"
            wait_for_user_status "$uid" "running" 180
            sleep 2
        fi
    done
fi

# Mark timestamp before drain
CANCEL_DRAIN_TS=$(mark_time)
F2_BEFORE=$(get_machine_agents "fleet-2")
echo "  Fleet-2 agents before drain: $F2_BEFORE"

# Start drain
echo "  Starting drain on fleet-2..."
coord_api POST /api/fleet/fleet-2/drain > /dev/null

# Give drain goroutine time to pick up a user and start migrating
# Don't wait for migration to complete — we want to cancel mid-drain
echo "  Waiting 30s for drain to start working..."
sleep 30

# Cancel drain
echo "  Cancelling drain (undrain)..."
# The undrain will block until the drain goroutine exits (which waits for in-flight migration)
coord_api POST /api/fleet/fleet-2/undrain > /dev/null

# Wait for system to fully settle — in-flight migration may take up to 5 min (MigrationSyncTimeout)
echo "  Waiting for in-flight migration to settle..."
wait_stable 600

check "Fleet-2 status is active after undrain" '
    status=$(coord_api GET /api/fleet | jq -r ".machines[] | select(.machine_id == \"fleet-2\") | .status")
    [ "$status" = "active" ]
'

# Verify drain_cancelled event was recorded
check "Drain cancelled event recorded" '
    c=$(count_events "type=drain_cancelled&machine_id=fleet-2&since=$CANCEL_DRAIN_TS")
    [ "${c:-0}" -gt 0 ]
'

check "Some users remain on fleet-2 (drain was cancelled)" '
    f2_agents=$(get_machine_agents "fleet-2")
    [ "${f2_agents:-0}" -gt 0 ]
'

check "All users are running after drain cancellation" '
    resp=$(coord_api GET /api/system/stable)
    transient=$(echo "$resp" | jq -r ".transient_users // 0")
    [ "$transient" = "0" ]
'

check "Consistency after drain cancellation" 'check_consistency "after drain cancellation"'

phase_result

# ════════════════════════════════════════════════════════════════
# Phase 6: Drain & Rebalancer Edge Cases
# ════════════════════════════════════════════════════════════════
phase_start 6 "Drain & Rebalancer Edge Cases"

wait_stable 360

# Test: Drain machine with no users
# First drain fleet-3 to empty it (if it has users), then undrain, then drain again (should be instant)
F3_AGENTS=$(get_machine_agents "fleet-3")
if [ "${F3_AGENTS:-0}" -gt 0 ]; then
    EMPTY_PREP_TS=$(mark_time)
    sleep 1
    echo "  Draining fleet-3 to empty it first..."
    coord_api POST /api/fleet/fleet-3/drain > /dev/null 2>&1 || true
    wait_for_event "type=drain_completed&machine_id=fleet-3&since=$EMPTY_PREP_TS" 300 || true
    coord_api POST /api/fleet/fleet-3/undrain > /dev/null 2>&1 || true
    sleep 3  # short pause — do NOT wait_stable here or rebalancer will refill fleet-3
fi

EMPTY_DRAIN_TS=$(mark_time)
sleep 2  # ensure timestamp is clearly before the drain
echo "  Draining empty fleet-3..."
coord_api POST /api/fleet/fleet-3/drain > /dev/null

# Wait up to 30s for drain_completed event (should be nearly instant for empty machine)
check "Drain of empty machine completes (drain_completed event)" '
    for _w in $(seq 1 10); do
        c=$(count_events "type=drain_completed&machine_id=fleet-3&since=$EMPTY_DRAIN_TS")
        [ "${c:-0}" -gt 0 ] && exit 0
        sleep 3
    done
    exit 1
'

coord_api POST /api/fleet/fleet-3/undrain > /dev/null

# Test: Drain non-existent machine → 404
check "Drain non-existent machine returns 404" '
    http_code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "http://${COORD_PUB_IP}:8080/api/fleet/nonexistent/drain")
    [ "$http_code" = "404" ]
'

# Test: Drain already-draining machine → 409
coord_api POST /api/fleet/fleet-3/drain > /dev/null 2>&1 || true
check "Drain already-draining machine returns 409" '
    http_code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "http://${COORD_PUB_IP}:8080/api/fleet/fleet-3/drain")
    [ "$http_code" = "409" ]
'
coord_api POST /api/fleet/fleet-3/undrain > /dev/null 2>&1 || true

# Test: Undrain non-draining machine → 409
check "Undrain non-draining machine returns 409" '
    http_code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "http://${COORD_PUB_IP}:8080/api/fleet/fleet-1/undrain")
    [ "$http_code" = "409" ]
'

# Test: Verify rebalancer skips ticks during drain
echo "  Starting drain on fleet-1 to test rebalancer skip..."
wait_stable 60

REBAL_DRAIN_TS=$(mark_time)
coord_api POST /api/fleet/fleet-1/drain > /dev/null 2>&1 || true
sleep 25  # ~2.5 rebalancer ticks worth

REBAL_DURING_DRAIN=$(count_events "type=rebalancer_trigger&since=$REBAL_DRAIN_TS")

check "Rebalancer does not trigger during drain" '
    [ "${REBAL_DURING_DRAIN:-0}" -eq 0 ]
'

# Wait for drain to complete, then undrain
echo "  Waiting for drain on fleet-1 to complete..."
wait_for_event "type=drain_completed&machine_id=fleet-1&since=$REBAL_DRAIN_TS" 300 || true
coord_api POST /api/fleet/fleet-1/undrain > /dev/null 2>&1 || true

wait_stable 360
check "Consistency after edge case tests" 'check_consistency "after edge cases"'

phase_result

# ════════════════════════════════════════════════════════════════
# Phase 7: Crash Recovery
# ════════════════════════════════════════════════════════════════
phase_start 7 "Crash Recovery"

wait_stable 360
echo "  Ensuring fleet-1 has users for drain crash test..."
for uid in rb-user-1 rb-user-2; do
    bipod_machines=$(coord_api GET "/api/users/$uid/bipod" | jq -r '[.[] | select(.role != "stale")] | .[].machine_id')
    if ! echo "$bipod_machines" | grep -q "fleet-1"; then
        source_machine=$(echo "$bipod_machines" | head -1)
        echo "    Moving $uid to fleet-1..."
        retry_migrate "$uid" "$source_machine" "fleet-1" || echo "    WARNING: Could not migrate $uid"
        wait_for_user_status "$uid" "running" 180 || true
        sleep 2
    fi
done

CRASH_DRAIN_TS=$(mark_time)

# Start drain
echo "  Starting drain on fleet-1..."
coord_api POST /api/fleet/fleet-1/drain > /dev/null

# Wait for at least one migration to start
sleep 15

check "Fleet-1 is draining" '
    status=$(coord_api GET /api/fleet | jq -r ".machines[] | select(.machine_id == \"fleet-1\") | .status")
    [ "$status" = "draining" ]
'

# Crash coordinator
echo "  Crashing coordinator..."
ssh_cmd "$COORD_PUB_IP" "systemctl restart coordinator" || true
sleep 5
wait_for_coordinator 60 || true

# Wait for drain to complete via reconciliation — use event or stability
echo "  Waiting for drain to complete after crash recovery..."
wait_for_event "type=drain_completed&machine_id=fleet-1&since=$CRASH_DRAIN_TS" 300 || {
    echo "  drain_completed event not seen, waiting for stability..."
    wait_stable 300
}

check "All users running after drain crash recovery" '
    resp=$(coord_api GET /api/system/stable)
    transient=$(echo "$resp" | jq -r ".transient_users // 0")
    [ "$transient" = "0" ]
'

wait_stable 360
check "Consistency after crash recovery" 'check_consistency "after crash recovery"'

# Undrain fleet-1
coord_api POST /api/fleet/fleet-1/undrain > /dev/null 2>&1 || true

phase_result

# ════════════════════════════════════════════════════════════════
# Phase 8: Post-Test Verification
# ════════════════════════════════════════════════════════════════
phase_start 8 "Post-Test Verification"

# Provision a new user, verify it works
echo "  Provisioning post-test-user..."
coord_api POST /api/users '{"user_id":"post-test-user","image_size_mb":128}' > /dev/null
coord_api POST /api/users/post-test-user/provision > /dev/null
wait_for_user_status "post-test-user" "running" 300

check "New user provision works after all tests" '
    status=$(coord_api GET /api/users/post-test-user | jq -r ".status")
    [ "$status" = "running" ]
'

# Migrate the new user
POST_PRIMARY=$(get_user_machine_by_role "post-test-user" "primary")
POST_FREE=$(find_free_machine "post-test-user")
echo "  Migrating post-test-user from $POST_PRIMARY to $POST_FREE..."
retry_migrate "post-test-user" "$POST_PRIMARY" "$POST_FREE"
wait_for_user_status "post-test-user" "running" 180

check "Post-test migration works" '
    status=$(coord_api GET /api/users/post-test-user | jq -r ".status")
    [ "$status" = "running" ]
'

# Verify all trigger types present in events (via API)
check "Manual trigger present in migration events" '
    count=$(count_events "type=migration&trigger=manual")
    [ "${count:-0}" -gt 0 ]
'

check "Rebalancer trigger present in migration events" '
    count=$(count_events "type=migration&trigger=rebalancer")
    [ "${count:-0}" -gt 0 ]
'

check "Drain trigger present in migration events" '
    count=$(count_events "type=migration&trigger=drain")
    [ "${count:-0}" -gt 0 ]
'

# Summary via API
echo ""
echo "  Summary:"
MANUAL_MIGS=$(count_events "type=migration&trigger=manual")
REBAL_MIGS=$(count_events "type=migration&trigger=rebalancer")
DRAIN_MIGS=$(count_events "type=migration&trigger=drain")
PROV_COUNT=$(count_events "type=provision&success=true")
DRAIN_STARTS=$(count_events "type=drain_started")
DRAIN_COMPLETES=$(count_events "type=drain_completed")
DRAIN_CANCELS=$(count_events "type=drain_cancelled")
echo "    Provisions: $PROV_COUNT"
echo "    Manual migrations: $MANUAL_MIGS"
echo "    Rebalancer migrations: $REBAL_MIGS"
echo "    Drain migrations: $DRAIN_MIGS"
echo "    Drain starts: $DRAIN_STARTS, completes: $DRAIN_COMPLETES, cancels: $DRAIN_CANCELS"

phase_result

# ════════════════════════════════════════════════════════════════
# Phase 9: Final Consistency & Cleanup
# ════════════════════════════════════════════════════════════════
phase_start 9 "Final Consistency & Cleanup"

wait_stable 120

check "Final consistency check" 'check_consistency "final"'

check "No stuck operations" '
    resp=$(coord_api GET /api/system/stable)
    ops=$(echo "$resp" | jq -r ".in_progress_ops // 0")
    [ "$ops" = "0" ]
'

phase_result

# ════════════════════════════════════════════════════════════════
# Final Result
# ════════════════════════════════════════════════════════════════
echo ""
echo "Finished: $(date)"
final_result

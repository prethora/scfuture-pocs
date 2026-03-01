#!/bin/bash
set -euo pipefail

source "$(dirname "$0")/common.sh"
load_ips

# ══════════════════════════════════════════════════════════════
# Phase 0: Prerequisites
# ══════════════════════════════════════════════════════════════
phase_start 0 "Prerequisites"

check "Machine-1 reachable via SSH" \
    'ssh_cmd "$MACHINE1_IP" "true"'

check "Machine-2 reachable via SSH" \
    'ssh_cmd "$MACHINE2_IP" "true"'

check "DRBD module loaded on machine-1" \
    'ssh_cmd "$MACHINE1_IP" "lsmod | grep -q drbd"'

check "DRBD module loaded on machine-2" \
    'ssh_cmd "$MACHINE2_IP" "lsmod | grep -q drbd"'

check "Machine agent responding on machine-1" \
    'api "$MACHINE1_IP" GET /status | jq -e .machine_id'

check "Machine agent responding on machine-2" \
    'api "$MACHINE2_IP" GET /status | jq -e .machine_id'

check "Container image built on machine-1" \
    'ssh_cmd "$MACHINE1_IP" "docker images platform/app-container --format={{.Repository}}" | grep -q platform/app-container'

check "Container image built on machine-2" \
    'ssh_cmd "$MACHINE2_IP" "docker images platform/app-container --format={{.Repository}}" | grep -q platform/app-container'

phase_result

# ══════════════════════════════════════════════════════════════
# Phase 1: Single User Provisioning — Full Stack
# ══════════════════════════════════════════════════════════════
phase_start 1 "Single User Provisioning — Full Stack"

# Create images
M1_LOOP=$(api "$MACHINE1_IP" POST /images/alice/create '{"image_size_mb": 512}' | jq -r .loop_device)
M2_LOOP=$(api "$MACHINE2_IP" POST /images/alice/create '{"image_size_mb": 512}' | jq -r .loop_device)

check "Image created on machine-1 (loop=$M1_LOOP)" '[ -n "$M1_LOOP" ] && [ "$M1_LOOP" != "null" ]'
check "Image created on machine-2 (loop=$M2_LOOP)" '[ -n "$M2_LOOP" ] && [ "$M2_LOOP" != "null" ]'

check "[SSH] Image file exists on machine-1" \
    'ssh_cmd "$MACHINE1_IP" "test -f /data/images/alice.img"'
check "[SSH] Image file exists on machine-2" \
    'ssh_cmd "$MACHINE2_IP" "test -f /data/images/alice.img"'

# Configure DRBD
DRBD_CONFIG="{
    \"resource_name\": \"user-alice\",
    \"nodes\": [
        {\"hostname\": \"machine-1\", \"minor\": 0, \"disk\": \"$M1_LOOP\", \"address\": \"$MACHINE1_PRIV\"},
        {\"hostname\": \"machine-2\", \"minor\": 0, \"disk\": \"$M2_LOOP\", \"address\": \"$MACHINE2_PRIV\"}
    ],
    \"port\": 7900
}"

api "$MACHINE1_IP" POST /images/alice/drbd/create "$DRBD_CONFIG" > /dev/null
api "$MACHINE2_IP" POST /images/alice/drbd/create "$DRBD_CONFIG" > /dev/null

check "DRBD configured on machine-1" \
    'ssh_cmd "$MACHINE1_IP" "test -f /etc/drbd.d/user-alice.res"'
check "DRBD configured on machine-2" \
    'ssh_cmd "$MACHINE2_IP" "test -f /etc/drbd.d/user-alice.res"'

# Promote first — initial sync requires a Primary
api "$MACHINE1_IP" POST /images/alice/drbd/promote > /dev/null
check "[SSH] DRBD role is Primary on machine-1" \
    'ssh_cmd "$MACHINE1_IP" "drbdadm status user-alice" | head -1 | grep -q "role:Primary"'

# Wait for DRBD sync
echo "  Waiting for DRBD sync..."
for attempt in $(seq 1 30); do
    PEER_STATE=$(api "$MACHINE1_IP" GET /images/alice/drbd/status | jq -r .peer_disk_state)
    if [ "$PEER_STATE" = "UpToDate" ]; then break; fi
    sleep 2
done

check "DRBD synced (UpToDate/UpToDate)" '[ "$PEER_STATE" = "UpToDate" ]'

# Format Btrfs
api "$MACHINE1_IP" POST /images/alice/format-btrfs > /dev/null
check "[SSH] Host does NOT have alice mounted after format" \
    '! ssh_cmd "$MACHINE1_IP" "mountpoint -q /mnt/users/alice"'

# Start container (device-mount)
api "$MACHINE1_IP" POST /containers/alice/start > /dev/null
check "[SSH] Container alice-agent is running" \
    '[ "$(ssh_cmd "$MACHINE1_IP" "docker inspect alice-agent 2>/dev/null | jq -r .[0].State.Running")" = "true" ]'

phase_result

# ══════════════════════════════════════════════════════════════
# Phase 2: Device-Mount Verification
# ══════════════════════════════════════════════════════════════
phase_start 2 "Device-Mount Verification"

check "[container] /workspace is a mount point" \
    'docker_exec "$MACHINE1_IP" alice-agent mountpoint -q /workspace'

check "[container] Seed data readable (config.json)" \
    'docker_exec "$MACHINE1_IP" alice-agent cat /workspace/data/config.json | jq -e .created'

check "[container] Running as appuser (not root)" \
    'ssh_cmd "$MACHINE1_IP" "docker top alice-agent" | grep -v UID | head -1 | awk "{print \$1}" | grep -qv root'

check "[container] /proc/mounts has no host paths" \
    'MOUNTS=$(docker_exec "$MACHINE1_IP" alice-agent cat /proc/mounts);
     echo "$MOUNTS" | grep -q "/workspace" &&
     ! echo "$MOUNTS" | grep -q "/mnt/users" &&
     ! echo "$MOUNTS" | grep -q "/data/images"'

check "[SSH] Host has NO Btrfs mount for alice" \
    '! ssh_cmd "$MACHINE1_IP" "mount | grep /mnt/users/alice"'

phase_result

# ══════════════════════════════════════════════════════════════
# Phase 3: Data Write + DRBD Replication
# ══════════════════════════════════════════════════════════════
phase_start 3 "Data Write + DRBD Replication"

docker_exec "$MACHINE1_IP" alice-agent sh -c "'echo hello-from-alice > /workspace/data/test.txt'"

check "[container] Data written and readable" \
    '[ "$(docker_exec "$MACHINE1_IP" alice-agent cat /workspace/data/test.txt)" = "hello-from-alice" ]'

check "[SSH] DRBD connection is Connected" \
    'ssh_cmd "$MACHINE1_IP" "drbdadm status user-alice" | grep -q "role:Secondary"'

check "[SSH] DRBD peer disk is UpToDate (write replicated)" \
    'api "$MACHINE1_IP" GET /images/alice/drbd/status | jq -r .peer_disk_state | grep -q UpToDate'

# Give DRBD a moment to replicate (Protocol A is async)
sleep 2

check "DRBD status via API confirms healthy bipod" \
    'STATUS=$(api "$MACHINE1_IP" GET /images/alice/drbd/status);
     [ "$(echo "$STATUS" | jq -r .role)" = "Primary" ] &&
     [ "$(echo "$STATUS" | jq -r .disk_state)" = "UpToDate" ] &&
     [ "$(echo "$STATUS" | jq -r .peer_disk_state)" = "UpToDate" ]'

phase_result

# ══════════════════════════════════════════════════════════════
# Phase 4: Failover via API
# ══════════════════════════════════════════════════════════════
phase_start 4 "Failover via API"

# Stop container on machine-1
api "$MACHINE1_IP" POST /containers/alice/stop > /dev/null
check "Container stopped on machine-1" \
    '[ "$(ssh_cmd "$MACHINE1_IP" "docker inspect alice-agent 2>/dev/null | jq -r .[0].State.Running" 2>/dev/null)" != "true" ]'

# Demote machine-1
api "$MACHINE1_IP" POST /images/alice/drbd/demote > /dev/null
check "Machine-1 demoted to Secondary" \
    'ssh_cmd "$MACHINE1_IP" "drbdadm status user-alice" | head -1 | grep -q "role:Secondary"'

# Promote machine-2
api "$MACHINE2_IP" POST /images/alice/drbd/promote > /dev/null
check "Machine-2 promoted to Primary" \
    'ssh_cmd "$MACHINE2_IP" "drbdadm status user-alice" | head -1 | grep -q "role:Primary"'

# Start container on machine-2 (device-mount — no host mount needed)
api "$MACHINE2_IP" POST /containers/alice/start > /dev/null
check "Container running on machine-2" \
    '[ "$(ssh_cmd "$MACHINE2_IP" "docker inspect alice-agent 2>/dev/null | jq -r .[0].State.Running")" = "true" ]'

# Data survived failover
check "[container m2] Data survived failover" \
    '[ "$(docker_exec "$MACHINE2_IP" alice-agent cat /workspace/data/test.txt)" = "hello-from-alice" ]'

# Config.json from provisioning survived
check "[container m2] Seed data survived failover" \
    'docker_exec "$MACHINE2_IP" alice-agent cat /workspace/data/config.json | jq -e .created'

# Device-mount pattern preserved on failover
check "[container m2] /proc/mounts clean (no host paths)" \
    'MOUNTS=$(docker_exec "$MACHINE2_IP" alice-agent cat /proc/mounts);
     echo "$MOUNTS" | grep -q "/workspace" &&
     ! echo "$MOUNTS" | grep -q "/mnt/users" &&
     ! echo "$MOUNTS" | grep -q "/data/images"'

check "[container m2] Running as appuser" \
    'ssh_cmd "$MACHINE2_IP" "docker top alice-agent" | grep -v UID | head -1 | awk "{print \$1}" | grep -qv root'

phase_result

# ══════════════════════════════════════════════════════════════
# Phase 5: Idempotency Tests
# ══════════════════════════════════════════════════════════════
phase_start 5 "Idempotency Tests"

# Call start on already-running container
check "Start on already-running container → 200" \
    'api "$MACHINE2_IP" POST /containers/alice/start | jq -e .already_existed'

# Call promote on already-Primary
check "Promote on already-Primary → 200" \
    'api "$MACHINE2_IP" POST /images/alice/drbd/promote | jq -e .already_existed'

# Call demote on already-Secondary
check "Demote on already-Secondary → 200" \
    'api "$MACHINE1_IP" POST /images/alice/drbd/demote | jq -e .already_existed'

# Call create on already-existing image
check "Create image that already exists → 200" \
    'api "$MACHINE1_IP" POST /images/alice/create "{\"image_size_mb\": 512}" | jq -e .already_existed'

# Stop, then stop again
api "$MACHINE2_IP" POST /containers/alice/stop > /dev/null
check "Stop already-stopped container → 200" \
    'api "$MACHINE2_IP" POST /containers/alice/stop > /dev/null'

# Delete non-existent user
check "Delete non-existent user → 200" \
    'api "$MACHINE1_IP" DELETE /images/nonexistent > /dev/null'

# DRBD create with same config (already exists)
check "DRBD create with existing resource → 200" \
    'api "$MACHINE1_IP" POST /images/alice/drbd/create "$DRBD_CONFIG" | jq -e .already_existed'

# Format already-formatted filesystem
# (need to re-promote machine-1 briefly to test format idempotency)
api "$MACHINE2_IP" POST /images/alice/drbd/demote > /dev/null
api "$MACHINE1_IP" POST /images/alice/drbd/promote > /dev/null
check "Format on already-formatted → 200 with already_formatted" \
    'api "$MACHINE1_IP" POST /images/alice/format-btrfs | jq -e .already_formatted'

phase_result

# ══════════════════════════════════════════════════════════════
# Phase 6: Full Teardown
# ══════════════════════════════════════════════════════════════
phase_start 6 "Full Teardown"

api "$MACHINE1_IP" DELETE /images/alice > /dev/null
api "$MACHINE2_IP" DELETE /images/alice > /dev/null

check "[SSH m1] No images" \
    '[ -z "$(ssh_cmd "$MACHINE1_IP" "ls /data/images/*.img 2>/dev/null")" ]'
check "[SSH m1] No loop devices" \
    '! ssh_cmd "$MACHINE1_IP" "losetup -a | grep /data/images/"'
check "[SSH m1] No DRBD resources" \
    '[ -z "$(ssh_cmd "$MACHINE1_IP" "ls /etc/drbd.d/user-*.res 2>/dev/null")" ]'
check "[SSH m1] No containers" \
    '[ -z "$(ssh_cmd "$MACHINE1_IP" "docker ps -q --filter name=-agent")" ]'

check "[SSH m2] No images" \
    '[ -z "$(ssh_cmd "$MACHINE2_IP" "ls /data/images/*.img 2>/dev/null")" ]'
check "[SSH m2] No loop devices" \
    '! ssh_cmd "$MACHINE2_IP" "losetup -a | grep /data/images/"'
check "[SSH m2] No DRBD resources" \
    '[ -z "$(ssh_cmd "$MACHINE2_IP" "ls /etc/drbd.d/user-*.res 2>/dev/null")" ]'
check "[SSH m2] No containers" \
    '[ -z "$(ssh_cmd "$MACHINE2_IP" "docker ps -q --filter name=-agent")" ]'

phase_result

# ══════════════════════════════════════════════════════════════
# Phase 7: Multi-User Density
# ══════════════════════════════════════════════════════════════
phase_start 7 "Multi-User Density (3 users on same bipod)"

# Provision 3 users
USERS="alice:0:7900 bob:1:7901 charlie:2:7902"

for entry in $USERS; do
    IFS=: read -r user minor port <<< "$entry"

    # Create images
    M1_L=$(api "$MACHINE1_IP" POST /images/$user/create '{"image_size_mb": 512}' | jq -r .loop_device)
    M2_L=$(api "$MACHINE2_IP" POST /images/$user/create '{"image_size_mb": 512}' | jq -r .loop_device)

    # Configure DRBD
    CONFIG="{
        \"resource_name\": \"user-$user\",
        \"nodes\": [
            {\"hostname\": \"machine-1\", \"minor\": $minor, \"disk\": \"$M1_L\", \"address\": \"$MACHINE1_PRIV\"},
            {\"hostname\": \"machine-2\", \"minor\": $minor, \"disk\": \"$M2_L\", \"address\": \"$MACHINE2_PRIV\"}
        ],
        \"port\": $port
    }"
    api "$MACHINE1_IP" POST /images/$user/drbd/create "$CONFIG" > /dev/null
    api "$MACHINE2_IP" POST /images/$user/drbd/create "$CONFIG" > /dev/null
done

# Promote all first — initial sync requires a Primary
for entry in $USERS; do
    IFS=: read -r user _ _ <<< "$entry"
    api "$MACHINE1_IP" POST /images/$user/drbd/promote > /dev/null
done

# Wait for all DRBD syncs
echo "  Waiting for DRBD sync on all 3 resources..."
sleep 5
for entry in $USERS; do
    IFS=: read -r user _ _ <<< "$entry"
    for attempt in $(seq 1 30); do
        PEER=$(api "$MACHINE1_IP" GET /images/$user/drbd/status | jq -r .peer_disk_state)
        if [ "$PEER" = "UpToDate" ]; then break; fi
        sleep 2
    done
done

# Format and start containers
for entry in $USERS; do
    IFS=: read -r user _ _ <<< "$entry"
    api "$MACHINE1_IP" POST /images/$user/format-btrfs > /dev/null
    api "$MACHINE1_IP" POST /containers/$user/start > /dev/null
done

check "All 3 containers running" \
    '[ "$(ssh_cmd "$MACHINE1_IP" "docker ps --filter name=-agent --format={{.Names}}" | wc -l)" -eq 3 ]'

check "[SSH] 3 DRBD resources active" \
    '[ "$(ssh_cmd "$MACHINE1_IP" "ls /etc/drbd.d/user-*.res | wc -l")" -eq 3 ]'

check "All 3 DRBD resources UpToDate" \
    'for entry in $USERS; do
        IFS=: read -r user _ _ <<< "$entry"
        STATE=$(api "$MACHINE1_IP" GET /images/$user/drbd/status | jq -r .peer_disk_state)
        [ "$STATE" = "UpToDate" ] || exit 1
    done'

# Write unique data to each user
docker_exec "$MACHINE1_IP" alice-agent sh -c "'echo alice-data > /workspace/data/identity.txt'"
docker_exec "$MACHINE1_IP" bob-agent sh -c "'echo bob-data > /workspace/data/identity.txt'"
docker_exec "$MACHINE1_IP" charlie-agent sh -c "'echo charlie-data > /workspace/data/identity.txt'"

# Isolation: each user sees only their own data
check "[container alice] Sees only alice-data" \
    '[ "$(docker_exec "$MACHINE1_IP" alice-agent cat /workspace/data/identity.txt)" = "alice-data" ]'
check "[container bob] Sees only bob-data" \
    '[ "$(docker_exec "$MACHINE1_IP" bob-agent cat /workspace/data/identity.txt)" = "bob-data" ]'
check "[container charlie] Sees only charlie-data" \
    '[ "$(docker_exec "$MACHINE1_IP" charlie-agent cat /workspace/data/identity.txt)" = "charlie-data" ]'

# Metadata isolation: no cross-user leakage in /proc/mounts
check "[container alice] /proc/mounts has no bob or charlie" \
    'MOUNTS=$(docker_exec "$MACHINE1_IP" alice-agent cat /proc/mounts);
     ! echo "$MOUNTS" | grep -q "bob" &&
     ! echo "$MOUNTS" | grep -q "charlie"'

# Resource independence: stopping one user's DRBD doesn't affect others
api "$MACHINE1_IP" POST /containers/alice/stop > /dev/null
check "Bob container still running after alice stopped" \
    '[ "$(ssh_cmd "$MACHINE1_IP" "docker inspect bob-agent 2>/dev/null | jq -r .[0].State.Running")" = "true" ]'
check "Charlie container still running after alice stopped" \
    '[ "$(ssh_cmd "$MACHINE1_IP" "docker inspect charlie-agent 2>/dev/null | jq -r .[0].State.Running")" = "true" ]'

phase_result

# ══════════════════════════════════════════════════════════════
# Phase 8: Status Endpoint Accuracy
# ══════════════════════════════════════════════════════════════
phase_start 8 "Status Endpoint Accuracy"

# Bob and charlie still running from Phase 7
STATUS=$(api "$MACHINE1_IP" GET /status)

check "Status shows bob as running" \
    'echo "$STATUS" | jq -e ".users.bob.container_running == true"'

check "Status shows charlie as running" \
    'echo "$STATUS" | jq -e ".users.charlie.container_running == true"'

check "Status shows alice as NOT running (stopped in Phase 7)" \
    'echo "$STATUS" | jq -e ".users.alice.container_running == false"'

check "Status shows alice image still exists" \
    'echo "$STATUS" | jq -e ".users.alice.image_exists == true"'

check "Status shows bob DRBD as Primary" \
    'echo "$STATUS" | jq -e ".users.bob.drbd_role == \"Primary\""'

# Full cleanup
api "$MACHINE1_IP" POST /cleanup > /dev/null
api "$MACHINE2_IP" POST /cleanup > /dev/null

STATUS_CLEAN=$(api "$MACHINE1_IP" GET /status)
check "Status shows no users after cleanup" \
    '[ "$(echo "$STATUS_CLEAN" | jq ".users | length")" -eq 0 ]'

phase_result

# ══════════════════════════════════════════════════════════════
# Summary
# ══════════════════════════════════════════════════════════════
final_result

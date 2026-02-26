#!/bin/sh
# This runs as root to perform the mount, then drops privileges.
#
# Environment variables:
#   SUBVOL_NAME  — which Btrfs subvolume to mount (e.g., "app-budget")
#   LOOP_DEVICE  — which loop device to mount from (e.g., "/dev/loop0")

set -e

# Mount the specific subvolume
mkdir -p /workspace
mount -o subvol=${SUBVOL_NAME} ${LOOP_DEVICE} /workspace

# Drop to a non-root user for the actual workload.
# SYS_ADMIN, SETUID, SETGID are only available during this init phase —
# once exec replaces this process as appuser, all capabilities are gone
# (non-root users don't inherit capabilities by default in Linux).
adduser -D -h /workspace appuser 2>/dev/null || true
exec su -s /bin/sh appuser -c "exec tail -f /dev/null"

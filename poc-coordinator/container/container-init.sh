#!/bin/sh
set -e

# BLOCK_DEVICE and SUBVOL_NAME are passed as environment variables
# BLOCK_DEVICE = /dev/drbd0 (or whatever minor)
# SUBVOL_NAME = workspace

# Mount the specific subvolume directly to /workspace
mkdir -p /workspace
mount -t btrfs -o subvol="$SUBVOL_NAME" "$BLOCK_DEVICE" /workspace

# Drop to unprivileged user and exec workload
# After exec, this process is replaced — SYS_ADMIN, SETUID, SETGID caps are gone
exec su appuser -s /bin/sh -c "${WORKLOAD_CMD:-"while true; do sleep 60; done"}"

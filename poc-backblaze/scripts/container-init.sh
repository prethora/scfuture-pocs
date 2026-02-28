#!/bin/sh
set -e
mkdir -p /workspace
mount -o subvol=${SUBVOL_NAME} ${BLOCK_DEVICE} /workspace
adduser -D -h /workspace appuser 2>/dev/null || true
exec su -s /bin/sh appuser -c "exec tail -f /dev/null"

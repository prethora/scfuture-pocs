#!/bin/bash
set -euo pipefail

source "$(dirname "$0")/common.sh"
load_ips

DEPLOY_CONFIGS=(
    "$MACHINE1_IP:machine-1"
    "$MACHINE2_IP:machine-2"
)

for config in "${DEPLOY_CONFIGS[@]}"; do
    IFS=: read -r pub_ip node_id <<< "$config"

    echo "Deploying to $node_id ($pub_ip)..."

    # Copy binary
    scp -o StrictHostKeyChecking=no \
        "$PROJECT_DIR/bin/machine-agent" root@$pub_ip:/usr/local/bin/

    # Copy container files
    ssh_cmd "$pub_ip" "mkdir -p /opt/platform/container"
    scp -o StrictHostKeyChecking=no \
        "$PROJECT_DIR/container/Dockerfile" \
        "$PROJECT_DIR/container/container-init.sh" \
        root@$pub_ip:/opt/platform/container/

    # Configure systemd with actual node ID
    ssh_cmd "$pub_ip" "
        sed -i 's/PLACEHOLDER_NODE_ID/$node_id/' /etc/systemd/system/machine-agent.service
        systemctl daemon-reload
    "

    # Set hostname (DRBD matches config on blocks by hostname)
    ssh_cmd "$pub_ip" "hostnamectl set-hostname $node_id"

    # Build container image
    ssh_cmd "$pub_ip" "cd /opt/platform/container && docker build -t platform/app-container ."

    echo "  $node_id deployed."
done

echo "Deploy complete."

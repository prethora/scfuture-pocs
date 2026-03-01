#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/common.sh"
load_ips

SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"

wait_ssh() {
    local ip="$1"
    echo "  Waiting for SSH on $ip..."
    for attempt in $(seq 1 60); do
        if ssh $SSH_OPTS -o ConnectTimeout=5 root@"$ip" "true" 2>/dev/null; then
            return 0
        fi
        sleep 5
    done
    echo "ERROR: SSH timeout on $ip"
    exit 1
}

wait_cloud_init() {
    local ip="$1"
    echo "  Waiting for cloud-init on $ip..."
    for attempt in $(seq 1 120); do
        if ssh $SSH_OPTS root@"$ip" "cloud-init status" 2>/dev/null | grep -q "done"; then
            return 0
        fi
        sleep 5
    done
    echo "ERROR: cloud-init timeout on $ip"
    exit 1
}

echo "═══ Deploying Layer 4.5 ═══"

# ── Deploy coordinator ──
echo ""
echo "Deploying coordinator ($COORD_PUB_IP)..."
wait_ssh "$COORD_PUB_IP"
wait_cloud_init "$COORD_PUB_IP"

scp $SSH_OPTS "$SCFUTURE_DIR/bin/coordinator" root@"$COORD_PUB_IP":/usr/local/bin/coordinator

# Add B2 bucket and retention env vars to coordinator service
ssh $SSH_OPTS root@"$COORD_PUB_IP" "
    sed -i '/Environment=DATA_DIR/a Environment=B2_BUCKET_NAME=${B2_BUCKET_NAME}' /etc/systemd/system/coordinator.service
    sed -i '/Environment=B2_BUCKET_NAME/a Environment=WARM_RETENTION_SECONDS=15' /etc/systemd/system/coordinator.service
    sed -i '/Environment=WARM_RETENTION_SECONDS/a Environment=EVICTION_SECONDS=30' /etc/systemd/system/coordinator.service
    systemctl daemon-reload
"

ssh $SSH_OPTS root@"$COORD_PUB_IP" "hostnamectl set-hostname coordinator"
ssh $SSH_OPTS root@"$COORD_PUB_IP" "systemctl enable --now coordinator"
echo "  Coordinator deployed."

# ── Deploy fleet machines ──
FLEET_CONFIGS="$FLEET1_PUB_IP:fleet-1:$FLEET1_PRIV_IP $FLEET2_PUB_IP:fleet-2:$FLEET2_PRIV_IP $FLEET3_PUB_IP:fleet-3:$FLEET3_PRIV_IP"

for config in $FLEET_CONFIGS; do
    IFS=: read -r pub_ip node_id priv_ip <<< "$config"
    echo ""
    echo "Deploying $node_id ($pub_ip, private: $priv_ip)..."

    wait_ssh "$pub_ip"
    wait_cloud_init "$pub_ip"

    # Copy binary
    scp $SSH_OPTS "$SCFUTURE_DIR/bin/machine-agent" root@"$pub_ip":/usr/local/bin/machine-agent

    # Copy container files
    ssh $SSH_OPTS root@"$pub_ip" "mkdir -p /opt/platform/container"
    scp $SSH_OPTS \
        "$SCFUTURE_DIR/container/Dockerfile" \
        "$SCFUTURE_DIR/container/container-init.sh" \
        root@"$pub_ip":/opt/platform/container/

    # Set hostname (DRBD config must match)
    ssh $SSH_OPTS root@"$pub_ip" "hostnamectl set-hostname $node_id"

    # Configure systemd unit with NODE_ID, NODE_ADDRESS, COORDINATOR_URL, and B2 credentials
    ssh $SSH_OPTS root@"$pub_ip" "
        sed -i 's/PLACEHOLDER_NODE_ID/$node_id/' /etc/systemd/system/machine-agent.service
        sed -i '/Environment=DATA_DIR/a Environment=NODE_ADDRESS=${priv_ip}:8080' /etc/systemd/system/machine-agent.service
        sed -i '/Environment=NODE_ADDRESS/a Environment=COORDINATOR_URL=http://10.0.0.2:8080' /etc/systemd/system/machine-agent.service
        sed -i '/Environment=COORDINATOR_URL/a Environment=B2_KEY_ID=${B2_KEY_ID}' /etc/systemd/system/machine-agent.service
        sed -i '/Environment=B2_KEY_ID/a Environment=B2_APP_KEY=${B2_APP_KEY}' /etc/systemd/system/machine-agent.service
        sed -i '/Environment=B2_APP_KEY/a Environment=B2_BUCKET_NAME=${B2_BUCKET_NAME}' /etc/systemd/system/machine-agent.service
        systemctl daemon-reload
    "

    # Build container image
    ssh $SSH_OPTS root@"$pub_ip" "cd /opt/platform/container && docker build -t platform/app-container ."

    # Verify DRBD module
    ssh $SSH_OPTS root@"$pub_ip" "modprobe drbd"

    # Start machine agent
    ssh $SSH_OPTS root@"$pub_ip" "systemctl enable --now machine-agent"

    echo "  $node_id deployed."
done

echo ""
echo "═══ Deploy complete ═══"

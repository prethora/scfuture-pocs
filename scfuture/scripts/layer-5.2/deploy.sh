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
        local status
        status=$(ssh $SSH_OPTS root@"$ip" "cloud-init status" 2>/dev/null) || true
        if echo "$status" | grep -q "done"; then
            return 0
        fi
        sleep 5
    done
    echo "ERROR: cloud-init timeout on $ip"
    exit 1
}

retry_scp() {
    local src="$1" dst="$2"
    for attempt in 1 2 3 4 5; do
        if scp $SSH_OPTS "$src" "$dst" 2>/dev/null; then
            return 0
        fi
        echo "    SCP failed (attempt $attempt), retrying in 10s..."
        sleep 10
    done
    scp $SSH_OPTS "$src" "$dst" 2>/dev/null
}

echo "═══ Deploying Layer 5.2 ═══"

# ── Deploy coordinator ──
echo ""
echo "Deploying coordinator ($COORD_PUB_IP)..."
wait_ssh "$COORD_PUB_IP"
wait_cloud_init "$COORD_PUB_IP"

retry_scp "$SCFUTURE_DIR/bin/coordinator" root@"$COORD_PUB_IP":/usr/local/bin/coordinator

# Configure coordinator with DATABASE_URL and rebalancer env vars
for _attempt in 1 2 3 4 5; do
    if ssh $SSH_OPTS root@"$COORD_PUB_IP" "
        mkdir -p /etc/systemd/system/coordinator.service.d
        cat > /etc/systemd/system/coordinator.service.d/override.conf << ENVEOF
[Service]
Environment=DATABASE_URL=${DATABASE_URL}
Environment=B2_BUCKET_NAME=${B2_BUCKET_NAME}
Environment=WARM_RETENTION_SECONDS=600
Environment=EVICTION_SECONDS=1200
Environment=REBALANCE_INTERVAL_SECONDS=10
Environment=REBALANCE_THRESHOLD=1
Environment=REBALANCE_STABILIZATION_SECONDS=15
ENVEOF
        # Remove DATA_DIR if present in base service (no longer needed)
        sed -i '/Environment=DATA_DIR/d' /etc/systemd/system/coordinator.service
        systemctl daemon-reload
    " 2>/dev/null; then
        break
    fi
    echo "  Coordinator configure SSH failed (attempt $_attempt), retrying in 10s..."
    sleep 10
done

for _attempt in 1 2 3 4 5; do
    if ssh $SSH_OPTS root@"$COORD_PUB_IP" "hostnamectl set-hostname coordinator" 2>/dev/null; then break; fi
    sleep 10
done
for _attempt in 1 2 3 4 5; do
    if ssh $SSH_OPTS root@"$COORD_PUB_IP" "systemctl enable --now coordinator" 2>/dev/null; then break; fi
    sleep 10
done
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
    retry_scp "$SCFUTURE_DIR/bin/machine-agent" root@"$pub_ip":/usr/local/bin/machine-agent

    # Copy container files
    CONTAINER_COPIED=false
    for _attempt in 1 2 3 4 5 6; do
        if ssh $SSH_OPTS root@"$pub_ip" "mkdir -p /opt/platform/container" 2>/dev/null && \
           scp $SSH_OPTS \
               "$SCFUTURE_DIR/container/Dockerfile" \
               "$SCFUTURE_DIR/container/container-init.sh" \
               root@"$pub_ip":/opt/platform/container/ 2>/dev/null; then
            CONTAINER_COPIED=true
            break
        fi
        echo "    Container files SCP failed (attempt $_attempt), retrying in 10s..."
        sleep 10
    done
    if [ "$CONTAINER_COPIED" = "false" ]; then
        echo "ERROR: Failed to copy container files to $node_id after 6 attempts"
        exit 1
    fi

    # Set hostname (DRBD config must match)
    for _attempt in 1 2 3 4 5; do
        if ssh $SSH_OPTS root@"$pub_ip" "hostnamectl set-hostname $node_id" 2>/dev/null; then
            break
        fi
        sleep 10
    done

    # Configure systemd unit
    for _attempt in 1 2 3 4 5; do
        if ssh $SSH_OPTS root@"$pub_ip" "
            sed -i 's/PLACEHOLDER_NODE_ID/$node_id/' /etc/systemd/system/machine-agent.service
            mkdir -p /etc/systemd/system/machine-agent.service.d
            cat > /etc/systemd/system/machine-agent.service.d/override.conf << ENVEOF
[Service]
Environment=NODE_ADDRESS=${priv_ip}:8080
Environment=COORDINATOR_URL=http://10.0.0.2:8080
Environment=B2_KEY_ID=${B2_KEY_ID}
Environment=B2_APP_KEY=${B2_APP_KEY}
Environment=B2_BUCKET_NAME=${B2_BUCKET_NAME}
ENVEOF
            systemctl daemon-reload
        " 2>/dev/null; then
            break
        fi
        sleep 10
    done

    # Build container image
    for _attempt in 1 2 3; do
        if ssh $SSH_OPTS root@"$pub_ip" "cd /opt/platform/container && docker build -t platform/app-container ." 2>/dev/null; then
            break
        fi
        sleep 5
    done

    # Verify DRBD module
    for _attempt in 1 2 3; do
        if ssh $SSH_OPTS root@"$pub_ip" "modprobe drbd" 2>/dev/null; then
            break
        fi
        sleep 5
    done

    # Start machine agent
    for _attempt in 1 2 3; do
        if ssh $SSH_OPTS root@"$pub_ip" "systemctl enable --now machine-agent" 2>/dev/null; then
            break
        fi
        sleep 5
    done

    echo "  $node_id deployed."
done

echo ""
echo "═══ Deploy complete ═══"

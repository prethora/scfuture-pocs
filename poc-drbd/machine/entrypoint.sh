#!/bin/bash
set -e

echo "[$NODE_ID] Starting SSH daemon..."
/usr/sbin/sshd

echo "[$NODE_ID] Starting Docker daemon..."
dockerd --storage-driver=vfs > /var/log/dockerd.log 2>&1 &

# Wait for Docker to be ready
echo "[$NODE_ID] Waiting for Docker daemon..."
TIMEOUT=30
ELAPSED=0
while ! docker info > /dev/null 2>&1; do
    sleep 1
    ELAPSED=$((ELAPSED + 1))
    if [ $ELAPSED -ge $TIMEOUT ]; then
        echo "[$NODE_ID] FATAL: Docker daemon failed to start after ${TIMEOUT}s"
        cat /var/log/dockerd.log
        exit 1
    fi
done
echo "[$NODE_ID] Docker daemon ready (${ELAPSED}s)"

# Build the app-container image
echo "[$NODE_ID] Building platform/app-container image..."
# Copy container-init.sh into the build context
cp /usr/local/bin/container-init.sh /opt/app-container/container-init.sh
docker build -t platform/app-container /opt/app-container/ > /dev/null 2>&1
echo "[$NODE_ID] App container image built"

if [ "$RUN_DEMO" = "true" ]; then
    # Wait for peer to be SSH-reachable
    echo "[$NODE_ID] Waiting for peer ($PEER_IP) to be SSH-reachable..."
    TIMEOUT=60
    ELAPSED=0
    while ! ssh -o ConnectTimeout=2 root@${PEER_IP} "echo ready" > /dev/null 2>&1; do
        sleep 1
        ELAPSED=$((ELAPSED + 1))
        if [ $ELAPSED -ge $TIMEOUT ]; then
            echo "[$NODE_ID] FATAL: Peer not reachable via SSH after ${TIMEOUT}s"
            exit 1
        fi
    done
    echo "[$NODE_ID] Peer is ready (${ELAPSED}s)"

    # Wait for peer's Docker daemon to be ready too
    echo "[$NODE_ID] Waiting for peer Docker daemon..."
    ELAPSED=0
    while ! ssh root@${PEER_IP} "docker info" > /dev/null 2>&1; do
        sleep 1
        ELAPSED=$((ELAPSED + 1))
        if [ $ELAPSED -ge $TIMEOUT ]; then
            echo "[$NODE_ID] FATAL: Peer Docker daemon not ready after ${TIMEOUT}s"
            exit 1
        fi
    done
    echo "[$NODE_ID] Peer Docker daemon ready"

    exec /usr/local/bin/demo.sh
else
    echo "[$NODE_ID] Standing by (secondary node)..."
    exec tail -f /dev/null
fi

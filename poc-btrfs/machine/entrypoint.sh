#!/bin/bash
set -e

echo "========================================"
echo "  Starting Docker daemon (DinD)..."
echo "========================================"

# Start dockerd in background with overlay2 storage driver
dockerd --storage-driver=vfs --host=unix:///var/run/docker.sock &>/var/log/dockerd.log &

# Wait for Docker to be ready (timeout 30 seconds)
TIMEOUT=30
ELAPSED=0
echo "Waiting for Docker daemon to be ready..."
while ! docker info &>/dev/null; do
    sleep 1
    ELAPSED=$((ELAPSED + 1))
    if [ "$ELAPSED" -ge "$TIMEOUT" ]; then
        echo "ERROR: Docker daemon failed to start within ${TIMEOUT}s"
        cat /var/log/dockerd.log
        exit 1
    fi
done
echo "Docker daemon is ready (took ${ELAPSED}s)"

echo ""
echo "========================================"
echo "  Running demo..."
echo "========================================"
echo ""

/demo.sh

echo ""
echo "Demo complete. Container staying alive for exploration."
echo "Shell in with: docker exec -it <container> bash"
tail -f /dev/null

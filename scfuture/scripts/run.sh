#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

source "$SCRIPT_DIR/common.sh"

echo "=== LAYER 4.1: MACHINE AGENT PoC ==="
echo "Started: $(date)"

# ── Phase 0: Infrastructure ──
echo ""
echo "Phase 0: Infrastructure Setup"

"$SCRIPT_DIR/infra.sh" up

wait_for_cloud_init

load_ips

cd "$PROJECT_DIR"
make build
"$SCRIPT_DIR/deploy.sh"

# Start machine agents
for ip in $MACHINE1_IP $MACHINE2_IP; do
    ssh -o StrictHostKeyChecking=no root@$ip "systemctl start machine-agent"
done

# Wait for agents to be ready
sleep 5

# Verify agents are responding
for ip in $MACHINE1_IP $MACHINE2_IP; do
    curl -sf "http://$ip:8080/status" > /dev/null || { echo "ERROR: Agent on $ip not responding"; exit 1; }
done

phase_result

# ── Run test suite ──
"$SCRIPT_DIR/test_suite.sh"

# ── Teardown ──
echo ""
echo "Tearing down infrastructure..."
"$SCRIPT_DIR/infra.sh" down

echo ""
echo "=== LAYER 4.1 COMPLETE ==="
echo "Finished: $(date)"

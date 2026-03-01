#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCFUTURE_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"

echo "═══ Layer 4.4: Bipod Reformation & Dead Machine Re-integration ═══"
echo "Started: $(date)"
echo ""

# Step 1: Build
echo "Building binaries..."
cd "$SCFUTURE_DIR"
make build
cd "$SCRIPT_DIR"

# Step 2: Infrastructure
echo "Creating infrastructure..."
./infra.sh up

# Step 3: Deploy
echo "Deploying to machines..."
./deploy.sh

# Step 4: Run tests
echo "Running test suite..."
./test_suite.sh
TEST_RESULT=$?

# Step 5: Teardown
echo ""
echo "Tearing down infrastructure..."
./infra.sh down

echo ""
echo "═══ Layer 4.4 Complete ═══"
echo "Finished: $(date)"

exit $TEST_RESULT

#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCFUTURE_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"

echo "═══ Layer 4.5: Suspension, Reactivation & Deletion Lifecycle ═══"
echo "Started: $(date)"
echo ""

# Verify B2 credentials
if [ -z "${B2_KEY_ID:-}" ] || [ -z "${B2_APP_KEY:-}" ]; then
    echo "ERROR: B2_KEY_ID and B2_APP_KEY environment variables are required"
    echo "  export B2_KEY_ID=your-key-id"
    echo "  export B2_APP_KEY=your-app-key"
    exit 1
fi

# Create B2 bucket for this test
BUCKET_NAME="l45-test-$(head -c 8 /dev/urandom | xxd -p)"
echo "Creating B2 bucket: $BUCKET_NAME"
b2 account authorize "$B2_KEY_ID" "$B2_APP_KEY" > /dev/null
b2 bucket create "$BUCKET_NAME" allPrivate > /dev/null
echo "Bucket created: $BUCKET_NAME"
export B2_BUCKET_NAME="$BUCKET_NAME"

# Step 1: Build
echo ""
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
set +e
./test_suite.sh
TEST_RESULT=$?
set -e

# Step 5: Teardown infrastructure
echo ""
echo "Tearing down infrastructure..."
./infra.sh down

# Step 6: Teardown B2 bucket
echo "Deleting B2 bucket: $BUCKET_NAME"
b2 rm --recursive --no-progress "b2://$BUCKET_NAME" 2>/dev/null || true
b2 bucket delete "$BUCKET_NAME" 2>/dev/null || true
echo "B2 bucket deleted."

echo ""
echo "═══ Layer 4.5 Complete ═══"
echo "Finished: $(date)"

exit $TEST_RESULT

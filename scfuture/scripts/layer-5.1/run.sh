#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCFUTURE_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"

echo "═══ Layer 5.1: Tripod Primitive & Manual Live Migration ═══"
echo "Started: $(date)"
echo ""

# Verify required env vars
if [ -z "${DATABASE_URL:-}" ]; then
    echo "ERROR: DATABASE_URL environment variable is required"
    echo "  export DATABASE_URL=postgres://user:pass@host:port/dbname?sslmode=require"
    exit 1
fi

if [ -z "${B2_KEY_ID:-}" ] || [ -z "${B2_APP_KEY:-}" ]; then
    echo "ERROR: B2_KEY_ID and B2_APP_KEY environment variables are required"
    exit 1
fi

# Clean database tables for fresh test
echo "Resetting database tables..."
DBRESET_TMP=$(mktemp /tmp/dbreset.XXXXXX.go)
cat > "$DBRESET_TMP" <<'GOEOF'
package main
import (
	"database/sql"
	"fmt"
	_ "github.com/lib/pq"
	"os"
)
func main() {
	db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil { fmt.Println("db open:", err); os.Exit(1) }
	defer db.Close()
	_, err = db.Exec("DROP TABLE IF EXISTS events, operations, bipods, users, machines CASCADE")
	if err != nil { fmt.Println("drop:", err); os.Exit(1) }
	fmt.Println("Tables dropped.")
}
GOEOF
(cd "$SCFUTURE_DIR" && go run "$DBRESET_TMP")
rm -f "$DBRESET_TMP"

# Create B2 bucket for this test
BUCKET_NAME="l51-test-$(head -c 8 /dev/urandom | xxd -p)"
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

# Step 7: Clean database
echo "Cleaning database tables..."
DBRESET_TMP=$(mktemp /tmp/dbreset.XXXXXX.go)
cat > "$DBRESET_TMP" <<'GOEOF'
package main
import (
	"database/sql"
	"fmt"
	_ "github.com/lib/pq"
	"os"
)
func main() {
	db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil { fmt.Println("db open:", err); return }
	defer db.Close()
	db.Exec("DROP TABLE IF EXISTS events, operations, bipods, users, machines CASCADE")
	fmt.Println("Tables cleaned.")
}
GOEOF
(cd "$SCFUTURE_DIR" && go run "$DBRESET_TMP") || true
rm -f "$DBRESET_TMP"

echo ""
echo "═══ Layer 5.1 Complete ═══"
echo "Finished: $(date)"
exit $TEST_RESULT

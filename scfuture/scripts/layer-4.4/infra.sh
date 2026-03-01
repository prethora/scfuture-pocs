#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/common.sh"

COORD="l44-coordinator"
FLEET_MACHINES=("l44-fleet-1" "l44-fleet-2" "l44-fleet-3")
ALL_MACHINES=("$COORD" "${FLEET_MACHINES[@]}")

# Detect SSH key
SSH_KEY_FILE=""
if [ -f ~/.ssh/id_ed25519.pub ]; then
    SSH_KEY_FILE=~/.ssh/id_ed25519.pub
elif [ -f ~/.ssh/id_rsa.pub ]; then
    SSH_KEY_FILE=~/.ssh/id_rsa.pub
else
    echo "ERROR: No SSH public key found (~/.ssh/id_ed25519.pub or ~/.ssh/id_rsa.pub)"
    exit 1
fi

case "${1:-}" in
    up)
        # Create SSH key if needed
        if ! hcloud ssh-key describe "$SSH_KEY_NAME" &>/dev/null; then
            hcloud ssh-key create --name "$SSH_KEY_NAME" \
                --public-key-from-file "$SSH_KEY_FILE"
        fi

        # Create network
        if ! hcloud network describe "$NETWORK_NAME" &>/dev/null; then
            hcloud network create --name "$NETWORK_NAME" --ip-range "10.0.0.0/24"
            hcloud network add-subnet "$NETWORK_NAME" \
                --ip-range "10.0.0.0/24" --type cloud --network-zone eu-central
        fi

        # Try locations in order
        LOCS=("nbg1" "fsn1" "hel1")

        # Create coordinator
        if ! hcloud server describe "$COORD" &>/dev/null; then
            CREATED=false
            for loc in "${LOCS[@]}"; do
                if hcloud server create \
                    --name "$COORD" \
                    --type "$SERVER_TYPE" \
                    --image "$IMAGE" \
                    --location "$loc" \
                    --ssh-key "$SSH_KEY_NAME" \
                    --network "$NETWORK_NAME" \
                    --user-data-from-file "$SCRIPT_DIR/cloud-init/coordinator.yaml" \
                    2>/dev/null; then
                    echo "Created $COORD at $loc"
                    LOCATION="$loc"
                    CREATED=true
                    break
                fi
                echo "  $loc unavailable for $COORD, trying next..."
            done
            if [ "$CREATED" = "false" ]; then
                echo "ERROR: No available location for $COORD"
                exit 1
            fi
        fi

        # Assign private IP to coordinator (10.0.0.2)
        hcloud server attach-to-network "$COORD" --network "$NETWORK_NAME" --ip 10.0.0.2 2>/dev/null || true

        # Create fleet machines
        FLEET_INDEX=0
        for name in "${FLEET_MACHINES[@]}"; do
            FLEET_INDEX=$((FLEET_INDEX + 1))
            PRIV_IP="10.0.0.$((10 + FLEET_INDEX))"

            if hcloud server describe "$name" &>/dev/null; then
                echo "$name already exists, skipping..."
                continue
            fi

            CREATED=false
            for loc in "${LOCS[@]}"; do
                if hcloud server create \
                    --name "$name" \
                    --type "$SERVER_TYPE" \
                    --image "$IMAGE" \
                    --location "$loc" \
                    --ssh-key "$SSH_KEY_NAME" \
                    --network "$NETWORK_NAME" \
                    --user-data-from-file "$SCRIPT_DIR/cloud-init/fleet.yaml" \
                    2>/dev/null; then
                    echo "Created $name at $loc"
                    CREATED=true
                    break
                fi
                echo "  $loc unavailable for $name, trying next..."
            done
            if [ "$CREATED" = "false" ]; then
                echo "ERROR: No available location for $name"
                exit 1
            fi

            # Assign private IP
            hcloud server attach-to-network "$name" --network "$NETWORK_NAME" --ip "$PRIV_IP" 2>/dev/null || true
        done

        echo "Waiting for servers to be ready..."
        sleep 10

        save_ips
        ;;

    down)
        for name in "${ALL_MACHINES[@]}"; do
            hcloud server delete "$name" 2>/dev/null && echo "Deleted $name" || true
        done
        hcloud network delete "$NETWORK_NAME" 2>/dev/null && echo "Deleted network" || true
        hcloud ssh-key delete "$SSH_KEY_NAME" 2>/dev/null && echo "Deleted SSH key" || true
        rm -f "$IP_FILE"
        ;;

    status)
        for name in "${ALL_MACHINES[@]}"; do
            hcloud server describe "$name" \
                -o format='{{.Name}}: {{.Status}} (pub={{.PublicNet.IPv4.IP}})' \
                2>/dev/null || echo "$name: not found"
        done
        ;;

    *)
        echo "Usage: $0 {up|down|status}"
        exit 1
        ;;
esac

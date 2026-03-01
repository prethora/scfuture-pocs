#!/bin/bash
set -euo pipefail

source "$(dirname "$0")/common.sh"

MACHINES=("poc41-machine-1" "poc41-machine-2")

case "${1:-}" in
    up)
        # Create SSH key if needed
        if ! hcloud ssh-key describe "$SSH_KEY_NAME" &>/dev/null; then
            hcloud ssh-key create --name "$SSH_KEY_NAME" \
                --public-key-from-file ~/.ssh/id_rsa.pub
        fi

        # Create network
        if ! hcloud network describe "$NETWORK_NAME" &>/dev/null; then
            hcloud network create --name "$NETWORK_NAME" --ip-range "10.0.0.0/24"
            hcloud network add-subnet "$NETWORK_NAME" \
                --ip-range "10.0.0.0/24" --type cloud --network-zone eu-central
        fi

        # Create machines
        for name in "${MACHINES[@]}"; do
            if hcloud server describe "$name" &>/dev/null; then
                echo "$name already exists, skipping..."
                continue
            fi

            hcloud server create \
                --name "$name" \
                --type "$SERVER_TYPE" \
                --image "$IMAGE" \
                --location "$LOCATION" \
                --ssh-key "$SSH_KEY_NAME" \
                --network "$NETWORK_NAME" \
                --user-data-from-file "$(dirname "$0")/cloud-init/fleet.yaml"

            echo "Created $name"
        done

        echo "Waiting for servers to be ready..."
        sleep 10

        save_ips
        ;;

    down)
        for name in "${MACHINES[@]}"; do
            hcloud server delete "$name" 2>/dev/null && echo "Deleted $name" || true
        done
        hcloud network delete "$NETWORK_NAME" 2>/dev/null && echo "Deleted network" || true
        hcloud ssh-key delete "$SSH_KEY_NAME" 2>/dev/null && echo "Deleted SSH key" || true
        rm -f "$IP_FILE"
        ;;

    status)
        for name in "${MACHINES[@]}"; do
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

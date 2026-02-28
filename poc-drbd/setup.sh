#!/bin/bash
set -e

# ─── DRBD Bipod PoC — One-time Setup ───
# Downloads Alpine Linux rootfs, installs packages, creates base VM image.
# Run once. Then use run.sh for each PoC execution.

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
VM_DIR="$SCRIPT_DIR/vm"
CACHE_DIR="$VM_DIR/cache"
BASE_IMG="$VM_DIR/alpine-base.img"
ALPINE_VERSION="3.21"
ALPINE_MIRROR="https://dl-cdn.alpinelinux.org/alpine"

GREEN='\033[0;32m'
CYAN='\033[0;36m'
RED='\033[0;31m'
NC='\033[0m'

info() { echo -e "${CYAN}[setup] $1${NC}"; }
pass() { echo -e "${GREEN}[setup] ✓ $1${NC}"; }
fail() { echo -e "${RED}[setup] ✗ $1${NC}"; exit 1; }

# Check prerequisites
command -v qemu-system-x86_64 >/dev/null || fail "QEMU not installed. Run: sudo apt install qemu-system-x86 qemu-utils"
command -v qemu-img >/dev/null || fail "qemu-img not installed. Run: sudo apt install qemu-utils"

mkdir -p "$CACHE_DIR" "$VM_DIR"

# ─── Step 1: Download Alpine minirootfs ───
ROOTFS_TAR="$CACHE_DIR/alpine-minirootfs.tar.gz"
if [ ! -f "$ROOTFS_TAR" ]; then
    info "Downloading Alpine $ALPINE_VERSION minirootfs..."
    curl -fSL -o "$ROOTFS_TAR" \
        "${ALPINE_MIRROR}/v${ALPINE_VERSION}/releases/x86_64/alpine-minirootfs-${ALPINE_VERSION}.0-x86_64.tar.gz"
    pass "Downloaded Alpine rootfs"
else
    info "Using cached Alpine rootfs"
fi

# ─── Step 2: Download Alpine kernel + initramfs ───
# We need the virt kernel for QEMU (includes virtio drivers, DRBD, etc.)
KERNEL_APK="$CACHE_DIR/linux-virt.apk"
if [ ! -f "$VM_DIR/vmlinuz-virt" ]; then
    info "Downloading Alpine virt kernel..."
    # Get the kernel package URL from the repo
    REPO_URL="${ALPINE_MIRROR}/v${ALPINE_VERSION}/main/x86_64"
    KERNEL_PKG=$(curl -fsSL "$REPO_URL/" | grep -oP 'linux-virt-[0-9][^"]*\.apk' | head -1)
    if [ -z "$KERNEL_PKG" ]; then
        fail "Could not find linux-virt package in Alpine repo"
    fi
    curl -fSL -o "$KERNEL_APK" "${REPO_URL}/${KERNEL_PKG}"
    # Extract kernel and initramfs-generating scripts
    # The APK is a tar.gz containing a data.tar.gz
    cd "$CACHE_DIR"
    tar xzf "$KERNEL_APK" 2>/dev/null || true
    if [ -f boot/vmlinuz-virt ]; then
        cp boot/vmlinuz-virt "$VM_DIR/vmlinuz-virt"
        pass "Extracted vmlinuz-virt"
    else
        fail "vmlinuz-virt not found in kernel package"
    fi
    cd "$SCRIPT_DIR"
else
    info "Using cached vmlinuz-virt"
fi

# ─── Step 3: Build base rootfs image ───
if [ -f "$BASE_IMG" ]; then
    info "Base image already exists, skipping build"
    info "(Delete $BASE_IMG to rebuild)"
else
    info "Building base rootfs image..."

    # Create a 4GB raw image
    truncate -s 4G "$BASE_IMG"
    mkfs.ext4 -q -F "$BASE_IMG"

    # Mount it
    MOUNT_DIR=$(mktemp -d)
    LOOP_DEV=$(sudo losetup --find --show "$BASE_IMG")
    sudo mount "$LOOP_DEV" "$MOUNT_DIR"

    # Extract Alpine rootfs
    info "Extracting Alpine rootfs..."
    sudo tar xzf "$ROOTFS_TAR" -C "$MOUNT_DIR"

    # Set up for chroot
    sudo mount -t proc proc "$MOUNT_DIR/proc"
    sudo mount -t sysfs sysfs "$MOUNT_DIR/sys"
    sudo mount --bind /dev "$MOUNT_DIR/dev"
    sudo cp /etc/resolv.conf "$MOUNT_DIR/etc/resolv.conf"

    # Configure Alpine repos
    sudo tee "$MOUNT_DIR/etc/apk/repositories" > /dev/null << EOF
${ALPINE_MIRROR}/v${ALPINE_VERSION}/main
${ALPINE_MIRROR}/v${ALPINE_VERSION}/community
EOF

    # Install packages via chroot
    info "Installing packages (this takes a minute)..."
    sudo chroot "$MOUNT_DIR" /sbin/apk update 2>&1 | tail -1
    sudo chroot "$MOUNT_DIR" /sbin/apk add --no-cache \
        alpine-base \
        linux-virt \
        drbd-utils \
        btrfs-progs \
        openssh \
        bash \
        util-linux \
        e2fsprogs \
        curl \
        jq \
        2>&1 | tail -5

    # Configure the system
    info "Configuring base system..."

    # Set root password
    sudo chroot "$MOUNT_DIR" /bin/sh -c 'echo "root:drbd" | chpasswd'

    # Enable serial console
    sudo tee "$MOUNT_DIR/etc/inittab" > /dev/null << 'EOF'
::sysinit:/sbin/openrc sysinit
::sysinit:/sbin/openrc boot
::wait:/sbin/openrc default
::shutdown:/sbin/openrc shutdown
ttyS0::respawn:/sbin/getty -L 115200 ttyS0 vt100
EOF

    # Configure fstab
    sudo tee "$MOUNT_DIR/etc/fstab" > /dev/null << 'EOF'
/dev/sda    /       ext4    defaults,noatime    0 1
proc        /proc   proc    defaults            0 0
sysfs       /sys    sysfs   defaults            0 0
devtmpfs    /dev    devtmpfs defaults            0 0
EOF

    # Enable services
    sudo chroot "$MOUNT_DIR" /bin/sh -c '
        rc-update add devfs sysinit
        rc-update add dmesg sysinit
        rc-update add mdev sysinit
        rc-update add sshd default
        rc-update add networking boot
        rc-update add seedrng boot
    ' 2>&1

    # Configure SSH: permit root login, no strict host checking
    sudo sed -i 's/#PermitRootLogin.*/PermitRootLogin yes/' "$MOUNT_DIR/etc/ssh/sshd_config"
    sudo sed -i 's/#PasswordAuthentication.*/PasswordAuthentication yes/' "$MOUNT_DIR/etc/ssh/sshd_config"

    # Generate SSH host keys
    sudo chroot "$MOUNT_DIR" /bin/sh -c '
        ssh-keygen -A
        mkdir -p /root/.ssh
        chmod 700 /root/.ssh
        ssh-keygen -t ed25519 -f /root/.ssh/id_ed25519 -N ""
        cp /root/.ssh/id_ed25519.pub /root/.ssh/authorized_keys
        chmod 600 /root/.ssh/authorized_keys
        echo "StrictHostKeyChecking no" > /root/.ssh/config
        echo "UserKnownHostsFile /dev/null" >> /root/.ssh/config
        chmod 600 /root/.ssh/config
    '

    # Default network config (will be overridden per-VM)
    sudo tee "$MOUNT_DIR/etc/network/interfaces" > /dev/null << 'EOF'
auto lo
iface lo inet loopback

auto eth0
iface eth0 inet dhcp
EOF

    # Create modules.dep for the installed kernel
    KVER=$(ls "$MOUNT_DIR/lib/modules/" | head -1)
    if [ -n "$KVER" ]; then
        sudo chroot "$MOUNT_DIR" /sbin/depmod "$KVER" 2>/dev/null || true
    fi

    # Copy the kernel and initramfs from the installed system
    if [ -f "$MOUNT_DIR/boot/vmlinuz-virt" ]; then
        cp "$MOUNT_DIR/boot/vmlinuz-virt" "$VM_DIR/vmlinuz-virt"
    fi
    # Generate initramfs
    if [ -n "$KVER" ]; then
        info "Generating initramfs..."
        sudo chroot "$MOUNT_DIR" /bin/sh -c "
            apk add --no-cache mkinitfs >/dev/null 2>&1
            mkinitfs -o /boot/initramfs-virt $KVER 2>/dev/null
        "
        if [ -f "$MOUNT_DIR/boot/initramfs-virt" ]; then
            sudo cp "$MOUNT_DIR/boot/initramfs-virt" "$VM_DIR/initramfs-virt"
            pass "Generated initramfs-virt"
        fi
    fi

    # Clean up mounts
    sudo umount "$MOUNT_DIR/proc" 2>/dev/null || true
    sudo umount "$MOUNT_DIR/sys" 2>/dev/null || true
    sudo umount "$MOUNT_DIR/dev" 2>/dev/null || true
    sudo umount "$MOUNT_DIR"
    sudo losetup -d "$LOOP_DEV"
    rmdir "$MOUNT_DIR"

    pass "Base rootfs image built: $BASE_IMG"
fi

# ─── Step 4: Verify we have everything ───
echo ""
info "Checking build artifacts..."
[ -f "$VM_DIR/vmlinuz-virt" ] && pass "vmlinuz-virt" || fail "vmlinuz-virt missing"
[ -f "$VM_DIR/initramfs-virt" ] && pass "initramfs-virt" || fail "initramfs-virt missing"
[ -f "$BASE_IMG" ] && pass "alpine-base.img ($(du -h "$BASE_IMG" | cut -f1))" || fail "alpine-base.img missing"

echo ""
pass "Setup complete! Run ./run.sh to start the DRBD PoC."

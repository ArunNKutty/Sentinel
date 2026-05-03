#!/usr/bin/env bash
# Bake the M1 Debian rootfs + extract kernel/initrd. Re-runnable.
set -euo pipefail

DEBIAN_VER=trixie
QCOW_URL="https://cloud.debian.org/images/cloud/${DEBIAN_VER}/latest/debian-13-generic-amd64.qcow2"
SUMS_URL="https://cloud.debian.org/images/cloud/${DEBIAN_VER}/latest/SHA512SUMS"

PLASMA=/opt/plasma
QCOW=${PLASMA}/images/debian-13-generic-amd64.qcow2
RAW=${PLASMA}/images/rootfs-base.raw
SUMS=${PLASMA}/images/SHA512SUMS
ROOT_PW=plasma

sudo install -d -o "$USER" -g "$USER" -m 0755 "$PLASMA" "$PLASMA/debian" "$PLASMA/images"

if [[ ! -f "$QCOW" ]]; then
    curl -fsSL -o "$QCOW" "$QCOW_URL"
fi
curl -fsSL -o "$SUMS" "$SUMS_URL"
( cd "$PLASMA/images" && grep 'debian-13-generic-amd64.qcow2$' SHA512SUMS | sha512sum -c - )

rm -f "$RAW"
qemu-img convert -p -f qcow2 -O raw "$QCOW" "$RAW"
truncate -s 4G "$RAW"

LOOP=$(sudo losetup -fP --show "$RAW")
trap 'sudo umount /mnt/plasma-rootfs 2>/dev/null || true; sudo losetup -d "$LOOP" 2>/dev/null || true' EXIT
sudo mkdir -p /mnt/plasma-rootfs
sudo mount "${LOOP}p1" /mnt/plasma-rootfs

KVER=$(basename /mnt/plasma-rootfs/boot/vmlinuz-* | sed 's/^vmlinuz-//')
sudo cp "/mnt/plasma-rootfs/boot/vmlinuz-${KVER}" "$PLASMA/debian/vmlinuz"
sudo cp "/mnt/plasma-rootfs/boot/initrd.img-${KVER}" "$PLASMA/debian/initrd.img"
sudo chown "$USER:$USER" "$PLASMA/debian/vmlinuz" "$PLASMA/debian/initrd.img"

# Direct shadow edit — chpasswd via chroot needs PAM/proc/sys mounted, this avoids that.
HASH=$(openssl passwd -6 -salt plasma "$ROOT_PW")
sudo sed -i "s|^root:[^:]*:|root:${HASH}:|" /mnt/plasma-rootfs/etc/shadow

sudo touch /mnt/plasma-rootfs/etc/cloud/cloud-init.disabled
sudo chroot /mnt/plasma-rootfs systemctl enable serial-getty@ttyS0.service >/dev/null 2>&1

# Setup static IP for the guest via systemd-networkd
sudo mkdir -p /mnt/plasma-rootfs/etc/systemd/network
sudo bash -c 'cat > /mnt/plasma-rootfs/etc/systemd/network/20-wired.network <<EOF
[Match]
Name=e*

[Network]
DHCP=yes
EOF'

sudo chroot /mnt/plasma-rootfs systemctl enable systemd-networkd systemd-resolved >/dev/null 2>&1 || true
# Link resolv.conf to systemd-resolved stub
sudo ln -sf ../run/systemd/resolve/stub-resolv.conf /mnt/plasma-rootfs/etc/resolv.conf

echo "M1 prep done. kernel=$KVER  rootfs=$RAW"

#!/usr/bin/env bash
# Boot the M1 Debian VM under Cloud Hypervisor. Foreground; Ctrl-A x to exit serial.
set -euo pipefail

PLASMA=/opt/plasma
KERNEL=$PLASMA/debian/vmlinuz
INITRD=$PLASMA/debian/initrd.img
ROOTFS=$PLASMA/images/rootfs-base.raw
API_SOCK=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/ch-m1.sock

for f in "$KERNEL" "$INITRD" "$ROOTFS"; do
    [[ -f "$f" ]] || { echo "missing: $f  (run m1-prep.sh first)" >&2; exit 1; }
done

rm -f "$API_SOCK"

# image_type=raw is explicit: CH v51 auto-detect disables sector-0 writes, which trips ext4.
exec cloud-hypervisor \
    --api-socket "$API_SOCK" \
    --kernel "$KERNEL" \
    --initramfs "$INITRD" \
    --cmdline "console=hvc0 console=ttyS0 root=/dev/vda1 rw panic=5 net.ifnames=0 biosdevname=0" \
    --disk path="$ROOTFS",image_type=raw \
    --net tap=ch-tap0 \
    --cpus boot=2 \
    --memory size=1024M \
    --serial tty \
    --console off

#!/usr/bin/env bash
# Swap the guest kernel to a pinned Debian version.
#
# Why: the NE's real target runs 6.18.10-xilinx, while the Debian cloud image
# ships 6.12. Matching the upstream base version means kwark/gluon are
# developed against the same kernel APIs as the target, instead of a two-year-
# older generation that hides API drift.
#
# This is upstream-version parity only. The vermagic here is
# "6.18.10+deb14-cloud-amd64", not "6.18.10-xilinx": a module built here still
# will not load on the real device, and Xilinx vendor patches are not present.
#
#   sudo ./scripts/kernel-swap.sh                 # default KVER below
#   sudo KVER=6.19.14+deb13-cloud-amd64 ./scripts/kernel-swap.sh
set -euo pipefail

# Pinned to match the target's upstream base version. Debian built this as
# source 6.18.10-1; it has since left the live archive, so it comes from
# snapshot.debian.org by content hash.
KVER="${KVER:-6.18.10+deb14-cloud-amd64}"

PLASMA=/opt/plasma
RAW=${PLASMA}/images/rootfs-base.raw
MNT=/mnt/plasma-kswap
WORK=$(mktemp -d)
SNAP=https://snapshot.debian.org

[[ $EUID -eq 0 ]] || { echo "must run as root (loop-mounts the rootfs)" >&2; exit 1; }
[[ -f "$RAW" ]] || { echo "missing: $RAW (run m1-prep.sh first)" >&2; exit 1; }

cleanup() {
    umount "$MNT/proc" 2>/dev/null || true
    umount "$MNT" 2>/dev/null || true
    [[ -n "${LOOP:-}" ]] && losetup -d "$LOOP" 2>/dev/null || true
    rm -rf "$WORK"
}
trap cleanup EXIT

# Resolve a Debian binary package to a download URL by content hash. Avoids
# pinning a snapshot timestamp, which would go stale on any later change.
fetch_deb() {
    local pkg="$1" out="$2"
    local enc ver hash
    enc="${pkg//+/%2B}"

    ver=$(curl -sf --max-time 60 "$SNAP/mr/binary/$enc/" \
            | grep -oE '"binary_version": *"[^"]*"' | head -1 | sed 's/.*"\([^"]*\)"$/\1/')
    [[ -n "$ver" ]] || { echo "no snapshot version for $pkg" >&2; return 1; }

    hash=$(curl -sf --max-time 60 "$SNAP/mr/binary/$enc/$ver/binfiles?fileinfo=1" \
            | grep -oE '"hash": *"[^"]*"' | head -1 | sed 's/.*"\([^"]*\)"$/\1/')
    [[ -n "$hash" ]] || { echo "no file for $pkg $ver" >&2; return 1; }

    echo "    $pkg $ver"
    curl -sf --max-time 600 -o "$out" "$SNAP/file/$hash"
}

echo "==> fetching kernel $KVER"
fetch_deb "linux-image-${KVER}" "$WORK/linux-image.deb"
dpkg-deb -x "$WORK/linux-image.deb" "$WORK/img"

VMLINUZ=$(find "$WORK/img" -name "vmlinuz-*" | head -1)
[[ -n "$VMLINUZ" ]] || { echo "no vmlinuz in the package" >&2; exit 1; }

# Debian moved the module tree to /usr/lib/modules; handle either location.
MODSRC=$(find "$WORK/img" -maxdepth 4 -type d -path "*lib/modules/$KVER" | head -1)
[[ -n "$MODSRC" ]] || { echo "no module tree for $KVER in the package" >&2; exit 1; }

echo "==> installing into $RAW"
LOOP=$(losetup -fP --show "$RAW")
mkdir -p "$MNT"
mount "${LOOP}p1" "$MNT"

# virtio_blk is a module in Debian's cloud flavour (CONFIG_VIRTIO_BLK=m), so
# the guest cannot reach its root disk without an initrd carrying it. The
# module tree has to land in the rootfs before the initrd is generated.
rm -rf "${MNT:?}/lib/modules/$KVER" "${MNT:?}/usr/lib/modules/$KVER"
install -d "$MNT/usr/lib/modules"
cp -a "$MODSRC" "$MNT/usr/lib/modules/$KVER"
[[ -d "$MNT/lib" && ! -L "$MNT/lib" ]] && install -d "$MNT/lib/modules" && \
    cp -a "$MODSRC" "$MNT/lib/modules/$KVER" 2>/dev/null || true

install -D -m 0644 "$VMLINUZ" "$MNT/boot/vmlinuz-$KVER"
# initramfs-tools reads the kernel config to decide which compression it may
# use; without it every run warns and falls back to assumptions.
CONFIG=$(find "$WORK/img" -name "config-$KVER" | head -1)
[[ -n "$CONFIG" ]] && install -D -m 0644 "$CONFIG" "$MNT/boot/config-$KVER"

echo "==> generating initrd for $KVER (needs virtio_blk to mount root)"
mount --bind /proc "$MNT/proc"
chroot "$MNT" depmod -a "$KVER"
if ! chroot "$MNT" update-initramfs -c -k "$KVER" 2>"$WORK/initrd.err"; then
    sed 's/^/    /' "$WORK/initrd.err" >&2
    echo "update-initramfs failed" >&2
    umount "$MNT/proc"
    exit 1
fi
grep -E '^(E|W):' "$WORK/initrd.err" | sed 's/^/    /' || true

INITRD="$MNT/boot/initrd.img-$KVER"
[[ -f "$INITRD" ]] || { umount "$MNT/proc"; echo "initrd was not generated" >&2; exit 1; }

umount "$MNT/proc"

# An initrd without virtio_blk panics the guest with no root device, so check
# before committing. The check must distinguish "read it and the module is
# missing" from "could not read it at all" — an initramfs is an uncompressed
# cpio concatenated with a compressed one, and a lister lacking the right
# decompressor silently reports only the first few entries, which would
# otherwise look identical to a genuinely broken initrd.
listing=$(lsinitramfs "$INITRD" 2>/dev/null || true)
if [[ $(printf '%s\n' "$listing" | wc -l) -lt 20 ]]; then
    echo "    WARNING: could not read the initrd here (missing decompressor?);" >&2
    echo "             skipping the virtio_blk check — verify the guest boots." >&2
elif printf '%s\n' "$listing" | grep -q virtio_blk; then
    echo "    initrd contains virtio_blk"
else
    echo "!!! initrd has no virtio_blk — the guest would not find its root disk" >&2
    exit 1
fi

install -m 0644 "$VMLINUZ" "$PLASMA/debian/vmlinuz"
install -m 0644 "$INITRD"  "$PLASMA/debian/initrd.img"
# plasmad advertises this to remote clients (GET /v1/engine), which build
# their modules against it.
echo "$KVER" > "$PLASMA/debian/vmlinuz.release"
sync

echo
echo "==> guest kernel is now $KVER"
echo "    Rebuild out-of-tree modules for it:  cd applications && make"
echo "    Existing VMs keep their old kernel until relaunched."

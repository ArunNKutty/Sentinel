#!/usr/bin/env bash
# Inject the NE application bootstrap into the base rootfs. Run once, after
# m1-prep.sh; re-runnable.
#
# This is the only thing that has to live inside the image. Everything else —
# the .ko, the binary — comes from the artifacts disk at boot, so iterating on
# an application never means touching the 4GB base image again.
#
# Uses the same loop-mount pattern as m1-prep.sh.
set -euo pipefail

PLASMA=/opt/plasma
RAW=${PLASMA}/images/rootfs-base.raw
MNT=/mnt/plasma-approotfs

[[ -f "$RAW" ]] || { echo "missing: $RAW (run m1-prep.sh first)" >&2; exit 1; }
[[ $EUID -eq 0 ]] || { echo "must run as root (loop-mounts the rootfs)" >&2; exit 1; }

LOOP=$(losetup -fP --show "$RAW")
trap 'umount "$MNT" 2>/dev/null || true; losetup -d "$LOOP" 2>/dev/null || true' EXIT
mkdir -p "$MNT"
mount "${LOOP}p1" "$MNT"

install -d -m 0755 "$MNT/opt/ne"

# The bootstrap: mount the artifacts disk, derive this VM's identity, load
# every module it finds, then hand off to the application.
cat > "$MNT/usr/local/sbin/ne-bootstrap" <<'BOOTSTRAP'
#!/bin/sh
# Bootstrap the NE application stack from the artifacts disk.
set -eu

ARTIFACTS_LABEL=plasma-ne
NE_DIR=/opt/ne

# No artifacts disk attached is a normal state (a plain VM), not a failure.
if ! blkid -L "$ARTIFACTS_LABEL" >/dev/null 2>&1; then
    echo "ne-bootstrap: no '$ARTIFACTS_LABEL' disk attached, nothing to load"
    exit 0
fi

mountpoint -q "$NE_DIR" || mount -o ro -L "$ARTIFACTS_LABEL" "$NE_DIR"
echo "ne-bootstrap: mounted $ARTIFACTS_LABEL at $NE_DIR"

# Derive a stable, fleet-unique device ID from the primary NIC's MAC. Plasma
# assigns each VM a distinct MAC (02:00:0a:00:00:02, ...), so this is constant
# across reboots of a VM yet different between VMs — the same role a
# provisioned hardware serial plays on a real device.
IFACE=$(ip -o link show | awk -F': ' '$2 != "lo" {print $2; exit}')
MAC=$(cat "/sys/class/net/$IFACE/address" 2>/dev/null || echo "00:00:00:00:00:00")
DEVICE_ID="0x$(echo "$MAC" | tr -d ':')"
echo "ne-bootstrap: iface=$IFACE mac=$MAC device_id=$DEVICE_ID"

for ko in "$NE_DIR"/*.ko; do
    [ -e "$ko" ] || continue
    name=$(basename "$ko" .ko)
    if lsmod | awk '{print $1}' | grep -qx "$name"; then
        echo "ne-bootstrap: $name already loaded"
        continue
    fi
    if insmod "$ko" device_id="$DEVICE_ID" 2>/dev/null; then
        echo "ne-bootstrap: insmod $name device_id=$DEVICE_ID"
    else
        # Not every module takes a device_id parameter.
        insmod "$ko" && echo "ne-bootstrap: insmod $name"
    fi
done

# run.sh is the generic hook: drop one in the artifacts disk to run something
# other than the reference app.
if [ -x "$NE_DIR/run.sh" ]; then
    echo "ne-bootstrap: exec run.sh"
    exec "$NE_DIR/run.sh"
elif [ -x "$NE_DIR/neapp" ]; then
    echo "ne-bootstrap: exec neapp serve"
    exec "$NE_DIR/neapp" serve
else
    echo "ne-bootstrap: no run.sh or neapp in $NE_DIR; modules loaded only"
fi
BOOTSTRAP
chmod 0755 "$MNT/usr/local/sbin/ne-bootstrap"

cat > "$MNT/etc/systemd/system/ne-app.service" <<'UNIT'
[Unit]
Description=NE application stack (artifacts disk, kernel modules, app)
After=network-online.target
Wants=network-online.target

[Service]
Type=exec
ExecStart=/usr/local/sbin/ne-bootstrap
Restart=on-failure
RestartSec=3
StandardOutput=journal+console
StandardError=journal+console

[Install]
WantedBy=multi-user.target
UNIT

# Enable without systemctl: no chroot needed, and it works even though the
# guest's systemd is not running here.
install -d -m 0755 "$MNT/etc/systemd/system/multi-user.target.wants"
ln -sf ../ne-app.service \
    "$MNT/etc/systemd/system/multi-user.target.wants/ne-app.service"

sync
echo "app-prep done: ne-app.service installed and enabled in $RAW"
echo "VMs launched from now on will mount the artifacts disk and load its modules."

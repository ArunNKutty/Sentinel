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
# Bootstrap the NE application stack from the artifacts disk, then start the
# per-node applications a topology file declared.
set -eu

ARTIFACTS_LABEL=plasma-ne
APPS_LABEL=plasma-apps
NE_DIR=/opt/ne
APPS_DIR=/opt/apps
LOG_DIR=/var/log/ne-apps

# Derive a stable, fleet-unique device ID from the primary NIC's MAC. Plasma
# assigns each VM a distinct MAC (02:00:0a:00:00:02, ...), so this is constant
# across reboots of a VM yet different between VMs — the same role a
# provisioned hardware serial plays on a real device.
IFACE=$(ip -o link show | awk -F': ' '$2 != "lo" {print $2; exit}')
MAC=$(cat "/sys/class/net/$IFACE/address" 2>/dev/null || echo "00:00:00:00:00:00")
DEVICE_ID="0x$(echo "$MAC" | tr -d ':')"
export DEVICE_ID
echo "ne-bootstrap: iface=$IFACE mac=$MAC device_id=$DEVICE_ID"

# insmod, passing device_id only to modules that accept it.
load_ko() {
    ko=$1
    name=$(basename "$ko" .ko)
    if lsmod | awk '{print $1}' | grep -qx "$name"; then
        echo "ne-bootstrap: $name already loaded"
        return 0
    fi
    if insmod "$ko" device_id="$DEVICE_ID" 2>/dev/null; then
        echo "ne-bootstrap: insmod $name device_id=$DEVICE_ID"
    else
        insmod "$ko" && echo "ne-bootstrap: insmod $name"
    fi
}

# The per-VM applications disk, built from this node's `apps:` block. Each
# line of apps.list is: name<TAB>type<TAB>relative file<TAB>cmd, where type is
# "kernel" (a .ko to insmod) or "userspace" (a program to run) and cmd may be
# empty. A start script that came with the app is already beside it, so cmd is
# just "./<script>".
#
# Mounted before the artifacts disk so the node's own modules are known first:
# both disks may carry a gluon_sim.ko, and the one the topology named for this
# node is the one that must end up loaded.
apps_list=
if blkid -L "$APPS_LABEL" >/dev/null 2>&1; then
    mkdir -p "$APPS_DIR" "$LOG_DIR"
    mountpoint -q "$APPS_DIR" || mount -o ro -L "$APPS_LABEL" "$APPS_DIR"
    echo "ne-bootstrap: mounted $APPS_LABEL at $APPS_DIR"
    apps_list="$APPS_DIR/apps.list"
fi

# Module names this node declared, so the artifacts disk does not load its own
# copy of one first and leave the node's version sitting unused on disk.
declared_ko=" "
if [ -n "$apps_list" ]; then
    declared_ko=" $(awk -F'\t' '$2 == "kernel" {
        n = $3; sub(/.*\//, "", n); sub(/\.ko$/, "", n); print n
    }' "$apps_list" | tr '\n' ' ')"
fi

# The fleet-wide artifacts disk. Not being attached is a normal state (a plain
# VM), not a failure.
if blkid -L "$ARTIFACTS_LABEL" >/dev/null 2>&1; then
    mkdir -p "$NE_DIR"
    mountpoint -q "$NE_DIR" || mount -o ro -L "$ARTIFACTS_LABEL" "$NE_DIR"
    echo "ne-bootstrap: mounted $ARTIFACTS_LABEL at $NE_DIR"
    for ko in "$NE_DIR"/*.ko; do
        [ -e "$ko" ] || continue
        name=$(basename "$ko" .ko)
        case "$declared_ko" in
            *" $name "*)
                echo "ne-bootstrap: $name declared by this node, skipping the artifacts copy"
                continue
                ;;
        esac
        load_ko "$ko"
    done
else
    echo "ne-bootstrap: no '$ARTIFACTS_LABEL' disk attached, nothing to load"
fi

started=0
if [ -n "$apps_list" ]; then
    while IFS="$(printf '\t')" read -r name apptype file cmd; do
        [ -n "${name:-}" ] || continue
        APP="$APPS_DIR/$file"
        if [ ! -e "$APP" ]; then
            echo "ne-bootstrap: app $name: missing $APP" >&2
            continue
        fi
        # A kernel module with no cmd is loaded, not run. With a cmd it is the
        # cmd's job to load it — a script passing module parameters, say.
        if [ "$apptype" = kernel ] && [ -z "${cmd:-}" ]; then
            load_ko "$APP"
            continue
        fi
        echo "ne-bootstrap: app $name ($apptype): $cmd (log $LOG_DIR/$name.log)"
        # cwd is the app's own directory so a cmd can reference files beside
        # the payload; APP and DEVICE_ID are exported for it to use.
        APP="$APP" APP_DIR="$(dirname "$APP")" \
            sh -c "cd \"\$APP_DIR\" && exec $cmd" \
            >>"$LOG_DIR/$name.log" 2>&1 &
        started=$((started + 1))
    done < "$apps_list"
fi

# A node that declared its own apps has said what it runs, so the fleet-wide
# artifacts disk does not also start its reference app on top — the two copies
# would race for the same ports. Stay alive instead: systemd tears down the
# cgroup when this script exits, and these apps are our children.
if [ "$started" -gt 0 ]; then
    echo "ne-bootstrap: $started app(s) running"
    wait
# run.sh is the generic hook: drop one in the artifacts disk to run something
# other than the reference app.
elif [ -x "$NE_DIR/run.sh" ]; then
    echo "ne-bootstrap: exec run.sh"
    exec "$NE_DIR/run.sh"
elif [ -x "$NE_DIR/neapp" ]; then
    echo "ne-bootstrap: exec neapp serve"
    exec "$NE_DIR/neapp" serve
else
    echo "ne-bootstrap: no run.sh, neapp or apps to start; modules loaded only"
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
echo "VMs launched from now on will mount the artifacts disk, load its modules,"
echo "and start the applications declared for their node."

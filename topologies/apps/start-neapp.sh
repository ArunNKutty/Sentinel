#!/bin/sh
# Start script for the reference app, deployed beside it on the node's apps
# disk and run from that directory. $APP is the payload's path in the guest;
# $DEVICE_ID is derived from the VM's MAC by ne-bootstrap.
set -eu

INSTALL_DIR=/usr/local/bin
INSTALL_PATH="$INSTALL_DIR/$(basename "$APP")"

# The apps disk is mounted read-only, so install the binary onto the rootfs
# and run it from there: the app gets a writable working directory and a
# stable path that stays valid if the disk is ever unmounted.
install -d -m 0755 "$INSTALL_DIR"
install -m 0755 "$APP" "$INSTALL_PATH"
echo "installed $(basename "$APP") to $INSTALL_PATH"

# The app talks to gluon_sim over gennetlink. When this node also declares the
# module, its app entry loads it in parallel with this one, so wait for it
# rather than losing the race on a cold boot. A node without the module (no
# sibling directory on the apps disk) has nothing to wait for.
if [ -d ../gluon-sim ]; then
    i=0
    while [ "$i" -lt 30 ] && ! lsmod | awk '{print $1}' | grep -qx gluon_sim; do
        i=$((i + 1))
        sleep 1
    done
    lsmod | awk '{print $1}' | grep -qx gluon_sim \
        || echo "warning: gluon_sim not loaded after ${i}s, starting anyway" >&2
fi

echo "starting neapp on device $DEVICE_ID"
exec "$INSTALL_PATH" serve

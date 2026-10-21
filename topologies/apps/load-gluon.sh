#!/bin/sh
# Start script for the gluon simulation module, deployed beside the .ko on the
# node's apps disk and run from that directory. $APP is the module's path in
# the guest; $DEVICE_ID is derived from the VM's MAC by ne-bootstrap.
#
# ne-bootstrap only insmods a kernel app itself when the app declares no cmd;
# with this script the load is ours to do, so the module parameters are visible
# here rather than implied by the bootstrap's default.
set -eu

MODULE=$(basename "$APP" .ko)

if lsmod | awk '{print $1}' | grep -qx "$MODULE"; then
    echo "$MODULE already loaded"
else
    echo "insmod $MODULE device_id=$DEVICE_ID"
    insmod "$APP" device_id="$DEVICE_ID"
fi

# Show what the module registered, so the app log carries proof it came up.
modinfo "$APP" 2>/dev/null | sed -n 's/^description:[[:space:]]*/module: /p' || true
lsmod | awk -v m="$MODULE" '$1 == m {print "loaded: " $0}'

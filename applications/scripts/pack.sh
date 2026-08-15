#!/usr/bin/env bash
# Pack applications/dist/ into the artifacts disk that every VM mounts.
#
# One read-only ext4 image is attached to the whole fleet, so adding an app is
# "drop it in dist/, re-pack, relaunch" — no 4GB base-image rebuild, and no
# per-VM copy or per-VM daemon at 50 VMs.
#
# mke2fs -d populates the filesystem directly from a directory, so packing
# needs neither root nor a loop mount. Only the final install to /opt/plasma
# does.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APPS="$(dirname "$HERE")"
DIST="$APPS/dist"
LABEL="plasma-ne"
OUT="${PLASMA_ARTIFACTS:-/opt/plasma/artifacts.img}"
SIZE="${ARTIFACTS_SIZE:-64M}"
STAGE="$APPS/.artifacts.img"

[[ -d "$DIST" ]] || { echo "nothing to pack: $DIST does not exist" >&2; exit 1; }
if [[ -z "$(ls -A "$DIST" 2>/dev/null)" ]]; then
    echo "nothing to pack: $DIST is empty (run 'make build' first)" >&2
    exit 1
fi

echo "==> packing $(ls -1 "$DIST" | wc -l) file(s) from dist/"
ls -1 "$DIST" | sed 's/^/    /'

# Executables must stay executable inside the image; mke2fs -d preserves mode.
chmod 0755 "$DIST"/* 2>/dev/null || true

rm -f "$STAGE"
truncate -s "$SIZE" "$STAGE"
mke2fs -q -t ext4 -L "$LABEL" -d "$DIST" -F "$STAGE" >/dev/null

if [[ -w "$(dirname "$OUT")" ]]; then
    mv -f "$STAGE" "$OUT"
else
    sudo install -D -m 0644 "$STAGE" "$OUT"
    rm -f "$STAGE"
fi

echo "==> wrote $OUT (label=$LABEL, $SIZE)"
echo "    VMs launched from now on will mount it at /opt/ne"

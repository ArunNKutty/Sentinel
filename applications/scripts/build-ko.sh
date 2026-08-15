#!/usr/bin/env bash
# Build gluon_sim.ko for the *guest* kernel.
#
# The host kernel (Ubuntu 6.8) is not the guest kernel (Debian 6.12), so this
# cannot be built natively. It runs in a debian:trixie container against the
# exact headers for whatever kernel the guest image currently ships.
#
# Debian only keeps recent point releases in the live archive, so the image's
# kernel routinely ages out of it (6.12.85 already has). snapshot.debian.org
# keeps every version forever, so we fall back there and fetch the header
# packages by content hash.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APPS="$(dirname "$HERE")"
SRC="$APPS/gluon-sim"
DIST="$APPS/dist"
KERNEL="${PLASMA_KERNEL:-/opt/plasma/debian/vmlinuz}"

[[ -f "$KERNEL" ]] || { echo "guest kernel not found: $KERNEL (run scripts/m1-prep.sh)" >&2; exit 1; }

# Derive the guest kernel version from the image itself, so the module tracks
# whatever m1-prep.sh last installed rather than a hardcoded string.
KVER="$(file -b "$KERNEL" | tr ' ,' '\n\n' | grep -m1 -E '^[0-9]+\.[0-9]+\.[0-9]+' || true)"
[[ -n "$KVER" ]] || { echo "could not determine guest kernel version from $KERNEL" >&2; exit 1; }

# kbuild demands the exact compiler the kernel was built with, so the build
# container has to match the kernel's Debian release: a +deb14 (forky) kernel
# wants gcc-15, which trixie does not ship. Derived rather than hardcoded so a
# later kernel bump does not silently break with "gcc-NN: not found".
case "$KVER" in
    *+deb12*) IMAGE="debian:bookworm" ;;
    *+deb13*) IMAGE="debian:trixie" ;;
    *+deb14*) IMAGE="debian:forky" ;;
    *)        IMAGE="${BUILD_IMAGE:-debian:sid}" ;;
esac

echo "==> guest kernel: $KVER"
echo "==> build container: $IMAGE"
mkdir -p "$DIST"

docker run --rm \
    -v "$SRC:/build" \
    -v "$DIST:/dist" \
    -e KVER="$KVER" \
    -w /build \
    "$IMAGE" bash -euo pipefail -c '
        export DEBIAN_FRONTEND=noninteractive
        apt-get update -qq
        # ca-certificates is required or the HTTPS snapshot fetch fails cert
        # verification; jq is needed to pair architecture->hash correctly.
        # libelf-dev and pahole back kbuild s objtool/BTF steps — without them
        # the compile succeeds and then objtool dies on a missing libelf.so.1.
        apt-get install -y -qq --no-install-recommends \
            build-essential bc kmod ca-certificates curl jq linux-base \
            libelf-dev pahole >/dev/null

        if apt-get install -y -qq --no-install-recommends "linux-headers-${KVER}" 2>/dev/null; then
            echo "==> installed headers from the live Debian archive"
        else
            echo "==> ${KVER} is no longer in the live archive; fetching from snapshot.debian.org"

            SNAP=https://snapshot.debian.org

            # The -common and -kbuild packages are named after the ABI only,
            # without the flavour or architecture:
            #   6.12.85+deb13-amd64        -> 6.12.85+deb13
            #   6.18.10+deb14-cloud-amd64  -> 6.18.10+deb14
            BASE="${KVER%-amd64}"
            for flavour in cloud rt; do BASE="${BASE%-$flavour}"; done

            # Resolve a binary package to a download URL by content hash. This
            # avoids pinning a snapshot timestamp, which would go stale the
            # moment the base image is refreshed.
            fetch_deb() {
                local pkg="$1" arch="$2"
                local enc ver hash
                enc="${pkg//+/%2B}"

                ver="$(curl -sf --max-time 60 "$SNAP/mr/binary/$enc/" \
                        | jq -r ".result[0].binary_version")"
                [ -n "$ver" ] && [ "$ver" != "null" ] || { echo "no snapshot version for $pkg" >&2; return 1; }

                hash="$(curl -sf --max-time 60 "$SNAP/mr/binary/$enc/$ver/binfiles?fileinfo=1" \
                        | jq -r --arg a "$arch" \
                            ".result[] | select(.architecture==\$a or .architecture==\"all\") | .hash" \
                        | head -1)"
                [ -n "$hash" ] && [ "$hash" != "null" ] || { echo "no $arch build of $pkg" >&2; return 1; }

                echo "    $pkg $ver"
                curl -sf --max-time 300 -o "/tmp/${pkg}.deb" "$SNAP/file/$hash"
            }

            fetch_deb "linux-headers-${BASE}-common" all
            fetch_deb "linux-kbuild-${BASE}"         amd64
            fetch_deb "linux-headers-${KVER}"        amd64

            # --force-depends: the headers package Depends on linux-image-*,
            # which is a runtime dependency we do not need in order to compile
            # an out-of-tree module. Everything the build needs is unpacked.
            dpkg -i --force-depends /tmp/linux-*.deb >/dev/null
        fi

        make -C "/usr/src/linux-headers-${KVER}" M=/build modules
        cp /build/gluon_sim.ko /dist/gluon_sim.ko
        chmod 0644 /dist/gluon_sim.ko
    '

# Docker writes as root; hand the artifact back to whoever ran the build so
# the drop folder stays usable without sudo.
if [[ -w "$DIST" ]]; then
    chown "$(id -u):$(id -g)" "$DIST/gluon_sim.ko" 2>/dev/null || \
        sudo chown "$(id -u):$(id -g)" "$DIST/gluon_sim.ko"
fi

echo "==> built $DIST/gluon_sim.ko"
# A vermagic mismatch is the most likely silent failure, so surface it here
# rather than at insmod time inside a VM.
if command -v modinfo >/dev/null 2>&1; then
    echo "    vermagic: $(modinfo -F vermagic "$DIST/gluon_sim.ko" 2>/dev/null || echo '(unreadable)')"
fi

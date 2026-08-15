# applications/ — loading a Go app and a kernel module into Plasma VMs

Proof that Plasma can deploy the shape of the real NE stack into a fleet: a
kernel module and a userspace Go binary, talking over generic netlink, with
VM-to-VM session negotiation.

| Real NE | Stand-in here |
|---|---|
| `gluon` (.ko, simulates hardware) | `gluon_sim.ko` — reports a static per-VM device ID |
| `Atom` (Go userspace daemon) | `neapp` |
| Atom ↔ kwark over gennetlink | `neapp` ↔ `gluon_sim.ko` over generic netlink |
| Atom ↔ Atom session negotiation | X25519 + HKDF handshake between two VMs |

---

## The drop folder

Anything in `dist/` appears at **`/opt/ne` inside every VM**.

```bash
make            # build gluon_sim.ko + neapp into dist/, pack the artifacts disk
make build      # build only
make pack       # pack dist/ -> /opt/plasma/artifacts.img
```

`dist/` is packed into a single read-only ext4 image that every VM mounts, so
adding an application is "drop it in, re-pack, relaunch" — no 4GB base-image
rebuild, and one shared file rather than a copy per VM.

On boot each VM:
1. mounts the artifacts disk (`LABEL=plasma-ne`) at `/opt/ne`,
2. derives a device ID from its MAC — plasma assigns unique ones, so the ID is
   stable per VM and distinct across the fleet,
3. `insmod`s **every** `*.ko` it finds, passing `device_id=0x…`,
4. runs `/opt/ne/run.sh` if present, else `/opt/ne/neapp serve`.

`run.sh` is the generic hook: drop one in to run something other than the
reference app.

---

## First-time setup

```bash
cd applications && make          # build + pack
sudo ../scripts/app-prep.sh      # once: inject the boot bootstrap into the base image
plasma vm launch host            # any VM launched from now on loads it
```

`app-prep.sh` is the only step that touches the base image, and it only
installs the bootstrap — the artifacts themselves always come from `dist/`.

---

## Trying it

`ne-app.service` already runs `neapp serve` on every VM, so the server side is
always up.

```bash
# what the kernel module reports, fetched over gennetlink
./scripts/vm-exec.py host-1 '/opt/ne/neapp id'
#   device-id: 0x000002000a000002  (from gluon_sim.ko over gennetlink)

# negotiate a session key with another VM
./scripts/vm-exec.py host-1 '/opt/ne/neapp handshake 10.0.0.3'

# the peer's own view of the same handshake
./scripts/vm-exec.py host-2 'journalctl -u ne-app --no-pager | tail'
```

**The proof is that both peers print the same session key.** The device ID is
mixed into the HKDF, so the kernel-supplied value genuinely drives the result
rather than being decorative.

`scripts/vm-exec.py` drives the serial console to run a command in a VM. It is
a stopgap until `ne-agent` (M2) lands and gives us a real `plasma vm exec`.

---

## Building the kernel module

`scripts/build-ko.sh` builds in a `debian:trixie` container, because **the host
kernel is not the guest kernel** (Ubuntu 6.8 vs Debian 6.12) — it cannot be
built natively.

It reads the guest kernel version straight from `/opt/plasma/debian/vmlinuz`,
then installs those exact headers. Debian only keeps recent point releases in
the live archive, so the image's kernel routinely ages out of it; the script
falls back to `snapshot.debian.org`, resolving packages by content hash so no
snapshot timestamp has to be pinned.

The module loads unsigned and taints the kernel — expected, since Debian does
not set `CONFIG_MODULE_SIG_FORCE` and cloud-hypervisor boots the kernel
directly with no Secure Boot:

```
gluon_sim: module verification failed: signature and/or required key missing - tainting kernel
gluon_sim: loaded, device_id=0x2000a000002, genl family 'gluon_sim'
```

The build container tracks the kernel's Debian release, because kbuild demands
the exact compiler the kernel was built with: a `+deb13` (trixie) kernel wants
gcc-14, a `+deb14` (forky) kernel wants gcc-15. Getting this wrong fails with
`x86_64-linux-gnu-gcc-15: not found`, not a useful message.

**If the guest kernel changes** — `scripts/kernel-swap.sh`, or re-running
`m1-prep.sh` (which pulls whatever Debian ships as "latest") — the existing
`.ko` stops loading with a vermagic mismatch. Fix: `make` again. The failure is
loud, not silent.

## Kernel version

The guest kernel is pinned to match the NE's real target's *upstream* version:

```bash
sudo ./scripts/kernel-swap.sh                      # default: 6.18.10+deb14-cloud-amd64
sudo KVER=6.19.14+deb13-cloud-amd64 ./scripts/kernel-swap.sh
cd applications && make                            # rebuild modules for it
```

This is upstream-version parity only, and worth being precise about: vermagic
here is `6.18.10+deb14-cloud-amd64`, not `6.18.10-xilinx`. A module built here
will not load on the real device, Xilinx's vendor patches are absent, and the
target is arm64 while this runs x86_64. What it does buy is developing
`kwark`/`gluon` against the same kernel API generation as the target, instead
of a 6.12 that would hide API drift.

---

## Layout

```
applications/
  dist/          ← drop folder; lands at /opt/ne in every VM
  gluon-sim/     C source + kbuild Makefile
  neapp/         Go source (own module)
  scripts/
    build-ko.sh  build the .ko in a container against the guest's headers
    pack.sh      pack dist/ into the artifacts disk
    vm-exec.py   run a command in a VM over its serial console
```

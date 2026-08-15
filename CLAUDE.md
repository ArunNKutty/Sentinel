# Plasma — Network Encryptor Simulation Platform

## What we're building
A CLI-driven simulation platform that spins up VMs on Cloud Hypervisor to simulate
our Network Encryptor (NE) product at fleet scale. Used for development, network
topology testing, interop testing across industry profiles, and validating
kernel-module hot-reloads. A product UI will be layered on top of the CLI/REST
surface later.

## Product context — Network Encryptor (NE)
The NE is a **transparent bump-in-the-wire** appliance. It sits in front of
any one of: a **host machine**, a **router**, or a **NAT** — all traffic
flowing through that endpoint is encrypted/decrypted on the way through.
Transparent means endpoints **don't address the NE** — its presence is
invisible from the host's network stack. Only the management plane (Fabric ↔
Atom) has an IP.

NE-internal components:
- **Atom** — Go userspace daemon. Issues commands to the kernel side.
- **kwark** — kernel module (`.ko`), driver layer.
- **gluon** — kernel module (`.ko`), hardware simulation layer.

Management-plane component:
- **Fabric** — platform that configures, secures, and monitors the NE fleet.
  **Next.js UI + Go backend, shipped as a Docker image.** The Fabric VM is
  special — it **co-locates the full NE stack (Atom + kwark + gluon)** with
  the Fabric app, so Fabric isn't paired with a separate NE the way a host VM
  is. (This exception applies only to Fabric.) Through the UI, operators
  configure, connect to, and monitor the NE fleet. The Fabric Go backend
  drives fleet operations by calling the **Plasma daemon's REST API**.
  Production-side services like KMS or SIEM are out of scope.

In simulation, `kwark` runs with `mode=sim` and peers with `gluon` via a direct
kernel API instead of talking to real silicon over SPI. No virtual SPI bus.

### Atom ↔ kwark transport
Atom runs **inside the NE VM** (PetaLinux) and talks to `kwark` via
**gennetlink** in the same kernel. This is the only mode supported in the
current plan.

**Deferred — split-host mode.** A future variant where **Atom moves out of
the NE onto a host VM** and reaches `kwark` over ethernet via a `k-eth`
translation driver. This is specifically about Atom's location — distinct
from the host VM role itself, which exists for workloads regardless. The
architecture supports it (same NE image, different init path); not in the
build plan for now.

## Platform stack
- **VMM:** Cloud Hypervisor — one process per VM, ~200ms boot, needs KVM.
- **Guest OS roles:**
  - **PetaLinux** — NE VMs (kwark + gluon + Atom).
  - **Alpine or Ubuntu** — Fabric VM. Carries the NE stack (kwark + gluon +
    Atom) + Docker + Fabric app. Kernel differs from PetaLinux, so **kwark
    and gluon are rebuilt against the Alpine/Ubuntu kernel** — two module
    artifacts to maintain, one per kernel.
  - **Alpine** — host VM, runs user workloads (any application — traffic
    generator, app server, etc.) behind an NE on the data plane. Carries no
    NE components.
  - Routers and NATs are **not** VMs — they're lightweight host-side network
    constructs (see Networking).
- **Guest agent:** `ne-agent` on vsock port 9100 — three ops: health, exec, cp.
  No SSH inside guests.
- **Host control plane:** `plasma` daemon exposing a Unix socket + REST API.
  The Fabric Go backend talks to this REST API for fleet operations.
- **Packet capture:** `plasmad` runs `tcpdump` on a VM's host-side tap and
  streams pcap over the same socket, so operators capture without `sudo`.
  Surfaced as `plasma capture`, a Wireshark extcap plugin (`plasma-extcap`,
  VMs appear in Wireshark's interface list), and the topology UI. Unlike
  containerlab there is no `ip netns exec` step — VM taps are already on the
  host. **Filters are passed to tcpdump via `-F <file>`, never argv**, because
  `-z` would otherwise be a root RCE vector in a root daemon.
- **Applications:** `applications/` — drop folder proving the NE stack's shape
  can be deployed at fleet scale. `dist/` is packed into one read-only ext4
  disk (`/opt/plasma/artifacts.img`, `LABEL=plasma-ne`) attached to every VM
  and mounted at `/opt/ne`; a baked-in `ne-app.service` derives a device ID
  from the VM's MAC, `insmod`s every `.ko`, and runs the app. Verified at 10
  VMs: `gluon_sim.ko` (genl family, per-VM device ID) + `neapp` (fetches the ID
  over gennetlink, X25519+HKDF handshake with peers). **The .ko must be built
  against the guest kernel** — `applications/scripts/build-ko.sh` does it in a
  container matching the kernel's Debian release (`+deb13`→trixie/gcc-14,
  `+deb14`→forky/gcc-15, since kbuild demands the exact compiler), fetching
  headers from snapshot.debian.org once the kernel ages out of the live archive.
- **Guest kernel:** pinned by `scripts/kernel-swap.sh`, currently
  `6.18.10+deb14-cloud-amd64` to match the target's upstream version
  (real target is `6.18.10-xilinx`). Upstream-version parity only — different
  vermagic, no Xilinx patches, and x86_64 here vs arm64 there. Debian's cloud
  flavour has `CONFIG_VIRTIO_BLK=m`, so swapping a kernel means regenerating
  the initrd or the guest cannot find its root disk.
- **Topology UI:** `plasma-ui/` — standalone Next.js + React Flow dev tool
  (distinct from Fabric). Its server reaches plasmad over the unix socket, so
  the daemon needs no TCP listener; the UI process must be in the `plasma`
  group and binds 127.0.0.1 only.
- **CLI:** `plasma vm list/shell/exec/cp/logs`, fleet ops, image factory, module push.
- **Networking:** Linux bridges + TAP pairs per VM. Three planes:
  - **Data plane** — protected endpoints connect to each other in a
    configurable topology (star, mesh, decentralized, …). Each NE is a bump
    on the link between its protected endpoint and the rest of the data plane.
    Endpoints are either Alpine **host VMs** or simulated **routers / NATs**
    (host-side constructs: namespaces, iptables NAT, bridge routing — not VMs).
  - **Control plane** — Atom ↔ Atom signaling between NEs (peer state,
    session negotiation) on its own **dedicated port**, separate from the
    data plane.
  - **Management plane** — Fabric ↔ Atom on a dedicated out-of-band port.
    Reaches every NE in the fleet.

## CLI surface
```
plasma up ./plasma.yaml          # boot the fleet
plasma down                      # tear it down

plasma vm list                   # list VMs
plasma vm shell  <id>            # interactive shell over vsock
plasma vm exec   <id> -- <cmd>   # one-shot exec
plasma vm cp     <src> <dst>     # copy in/out (id:/path syntax)
plasma vm logs   <id>            # tail console/journal
plasma vm launch <group>         # add a VM to a group
plasma vm delete <id>            # remove one
plasma vm health <id>            # Atom, kwark, gluon state

plasma capture <id>              # live pcap from a VM's tap (no sudo needed)
  [-f <bpf>] [-w <file>] [--wireshark] [-s <snaplen>]
plasma capture plasma-br0        # the bridge (broadcast/host traffic only)

plasma fleet status              # GROUP COUNT HEALTHY ATOM KUARK
plasma fleet exec -- <cmd>       # run on every VM

plasma image build <profile>     # bake an industry-specific rootfs
plasma image list

plasma module push <ko> [--target id,...] [--restart-atom]

plasma snapshot create <name>    # snapshot whole fleet (or --vm <id> for one)
plasma snapshot restore <name>   # restore to a saved baseline (~1–2s vs reboot)
plasma snapshot list
plasma snapshot delete <name>
```

## Build plan — milestones
See [plasma-build-plan.html](plasma-build-plan.html) for full detail. Each
milestone is independently shippable.

- **M1 — Boot one VM.** PetaLinux kernel rebuilt with virtio, rootfs extracted
  from `.wic`, Cloud Hypervisor launches, `kwark` loaded, Atom running.
- **M2 — ne-agent.** Go binary on vsock :9100 inside the guest. Replaces SSH.
- **M3 — plasma daemon (single VM).** YAML config, lifecycle commands,
  full single-VM CLI.
- **M4 — Fleet.** `count: 50`, sandbox host group, `fleet status/exec`.
- **M4.6 — Snapshot / restore.** Cloud Hypervisor native snapshots, exposed as
  `plasma snapshot create/restore/list/delete`. Whole-fleet or per-VM. Restore
  in 1–2s vs ~15s reboot — major speedup for destructive testing in M5/M6.
  *(Not in original HTML plan — added later.)*
- **M4.7 — Topology DSL.** `plasma.yaml` grows top-level blocks:
  `planes` (auto-managed bridges per plane), `nodes` (VMs by role with optional
  `count` for fleets), `constructs` (routers/NATs as host-side namespaces),
  `pairings` (declarative NE→endpoint binding, e.g. `scheme: zip`), and
  `links` (escape hatch for custom topology). Containerlab-influenced shape;
  Cloud Hypervisor remains the runtime. Defaults handle the common case
  without ever writing a `links` entry.
  *(Not in original HTML plan — added later.)*
- **M5 — Image factory.** `plasma image build <profile>` bakes per-industry
  rootfs (defense, telecom, industrial, healthcare). Mixed-image fleets for
  interop testing.
- **M6 — Module hot-reload.** Push a new `kwark.ko` to the whole fleet in
  <3s, restart Atom, re-establish kwark↔gluon binding + gennetlink.
- **M7 — Fabric.** Master VM (Alpine or Ubuntu), Fabric Docker image deployed,
  Fabric ↔ Atom connectivity to every NE in the fleet. Replaces the original
  HTML M7 (KMS / Controller / SIEM sims), which is dropped — those simulated
  production-side services and aren't in scope.

## Resource budget (96 GB host, 50 NEs + Fabric)
1.5 GB × 50 NE = 75 GB; Fabric VM ~4 GB; CH overhead + host + bridges ~3 GB;
total ~82 GB.

## Key conventions
- VM ID format: `ne-00`, `ne-01`, … (production groups) and `sandbox-1`,
  `sandbox-2`, … (ephemeral test VMs).
- Host paths: `/opt/plasma/{petalinux,images,factory}`,
  socket at `/var/run/plasma.sock`. Guest kernel pinned via `scripts/kernel-swap.sh`.
- Guest paths: `/opt/ne/bin/ne-agent`, `/opt/atom/atom`.
- vsock CID 3, port 9100 for the agent.
- Cross-compile: `CGO_ENABLED=0 GOOS=linux GOARCH=arm64` for arm64 targets;
  swap to `GOARCH=amd64` if the host is x86_64.

## Open questions / unresolved
- **x86_64 vs arm64 host.** Cloud Hypervisor needs KVM and won't emulate.
  Confirm target host arch before M1 — it determines kernel + Go build targets.
- **M6 module push across two kernels** *(decide when M6 lands).* `kwark` and
  `gluon` have separate builds for PetaLinux NEs vs Alpine/Ubuntu Fabric VM.
  Either `plasma module push` selects the right `.ko` per VM kernel
  automatically, or commands are scoped explicitly (`--kernel petalinux` /
  `--kernel alpine`).

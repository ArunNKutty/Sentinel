# Plasma — Network Encryptor Simulation Platform

CLI-driven platform that spins up VMs on Cloud Hypervisor to simulate the
Network Encryptor (NE) product at fleet scale.

---

## Quick reference

| Task | Command |
|------|---------|
| **Get a shell ready** | **`plasma init`** |
| **Destroy everything** | **`plasma exit`** |
| Start the daemon | `sudo plasmad &` |
| Create a VM | `plasma vm launch host` |
| Bring up a topology | `plasma vm --topology ./topologies/ne-lab.yaml` |
| List VMs | `plasma vm list` |
| Shell into a VM | `plasma vm shell host-1` (detach: **Ctrl-X**) |
| Delete one VM | `plasma vm delete host-1` |
| Delete the fleet | `plasma down` |
| Capture packets | `plasma vm capture host-1 --wireshark` |
| **Use an engine on another host** | **`plasma engine login https://<engine>:7443 --token … --fingerprint …`** |
| Bring up a lab (local or remote) | `plasma lab apply ./topologies/ne-lab.yaml --name it-1 --wait` |
| Reach a lab VM's port | `plasma lab tunnel it-1 3000:fabric:3000` |
| Impair / mirror a link | `plasma lab impair it-1 ne-00:eth2 --delay 50ms` · `plasma lab mirror it-1 a:eth1 --to cap:eth3` |
| Delete a lab | `plasma lab delete it-1` |

---

## Prerequisites

- Linux host with KVM enabled
- `cloud-hypervisor` installed and on `$PATH`
- `dnsmasq` installed (`apt install dnsmasq`)
- `tcpdump` installed (`apt install tcpdump`) — required for packet capture
- Base image prepared at `/opt/plasma/images/rootfs-base.raw`
  (run `sudo ./scripts/m1-prep.sh` once to build it)

Optional, for the packet-capture tooling:
- `wireshark` — for live capture and the Wireshark plugin

---

## Quick Start

### 1. One-time host network setup
Re-run this after every host reboot to recreate `plasma-br0` and the NAT rules
for `10.0.0.0/24`:
```bash
cd /home/fortaegis/codebase/simulation/nesimulation
sudo ./scripts/net-host.sh
```

> `plasmad` creates the bridge and per-VM taps itself, so VM-to-VM and
> host-to-VM traffic works without this script. What it adds is IP forwarding
> and masquerading — i.e. VMs reaching the outside network.
> (`scripts/m1-net-host.sh` is the separate M1 manual-boot path: `ch-tap0` on
> `192.168.254.0/24`, used by `scripts/m1-boot.sh` only.)

### 2. Install the binaries
```bash
cd plasma
sudo make install
newgrp plasma          # or log out and back in
```
Installs `plasma`, `plasmad` and `plasma-extcap` to `/usr/local/bin`, creates
the `plasma` group, and adds you to it. **That group membership is what lets
the CLI talks to the daemon without `sudo`** — nothing works until it
applies.

### 3. Guest kernel version
The Debian cloud image ships 6.12; the NE's real target runs 6.18.10-xilinx.
To match the upstream version so modules build against the same kernel APIs:
```bash
sudo ./scripts/kernel-swap.sh          # -> 6.18.10+deb14-cloud-amd64
```
Upstream-version parity only — vermagic is `+deb14-cloud-amd64`, not
`-xilinx`, and this host is x86_64 while the target is arm64. Rebuild
out-of-tree modules afterwards with `cd applications && make`.

### 4. Optional extras
```bash
sudo make install-extcap    # Wireshark plugin: VMs in the interface list
sudo make install-service   # run plasmad under systemd instead of by hand
```

---

## Running Plasma

### `plasma init` — the one command to start with
```bash
plasma init
```
Checks the host prerequisites, starts `plasmad` if it isn't running, and — if
your shell isn't in the `plasma` group — drops you into one that is. Safe to
re-run; it changes nothing already in order.

```
plasma init
  [ok  ] cloud-hypervisor /usr/local/bin/cloud-hypervisor
  [ok  ] base image       /opt/plasma/images/rootfs-base.raw
  [ok  ] guest kernel     6.18.10+deb14-cloud-amd64
  [ok  ] artifacts        /opt/plasma/artifacts.img
  [warn] daemon           not running, starting it...
  [ok  ] daemon           started, listening on /var/run/plasma.sock
  [ok  ] group            this shell is in 'plasma'
  [ok  ] fleet            0 VM(s), 0 running

ready.
```

| Flag | Effect |
|------|--------|
| `--check` | report only, change nothing; non-zero exit if not usable |
| `--no-shell` | never start a shell, just print what to run |

> **Why it starts a shell.** Group membership is fixed when a process starts,
> and no child process can add a group to its parent shell — so a shell that
> predates `sudo make install` can never reach the socket. `plasma init` execs
> `newgrp plasma`, giving you a session that has it; type `exit` to return.
> Non-interactive callers get the `sg plasma -c "..."` form instead, so scripts
> never hang.

### `plasma exit` — the counterpart
```bash
plasma exit          # asks first; -y to skip
```
Destroys everything `init` and the daemon create at runtime: every VM with its
disk and tap, the bridge, dnsmasq, the recorded topology, `state.json`, and
plasmad itself. Afterwards `plasma init` starts from nothing.

It **keeps** the expensive build artifacts, since those are not session state:
the base image, the guest kernel/initrd, and `artifacts.img`. Rebuilding those
means re-running `m1-prep.sh` / `kernel-swap.sh` / `applications && make`.

> It only touches plasma's own resources. Other tenants on the host — libvirt
> and multipass also run `dnsmasq` and own `tap-*` devices — are left alone,
> which a blanket `pkill dnsmasq` would not manage.

### Start the daemon by hand
```bash
sudo plasmad &
```
Root is needed for taps and VM processes; the CLI is not. Skip this if
you ran `plasma init` or installed the systemd service.

---

## Managing VMs

### Create
```bash
plasma vm launch host        # first → host-1 at 10.0.0.2
plasma vm launch host        # second → host-2 at 10.0.0.3
```
IDs are `<group>-N`; addresses come from the lowest free slot in `10.0.0.0/24`.

### From a topology file
Bring up a whole lab in one command, containerlab-style:
```bash
plasma vm --topology ./topologies/ne-lab.yaml
```
```
topology ne-lab: 5 node(s), 3 link(s)
  host-a       host      10.0.0.2  (2 cpu, 1024M)
               eth1 02:00:0b:00:01:02  pt2-eth1 -> plk-…
  host-b       host      10.0.0.3  (2 cpu, 1024M)
               eth1 02:00:0b:00:01:03  pt3-eth1 -> plk-…
  ne-00        ne        10.0.0.4  (2 cpu, 1536M)
               eth1 02:00:0b:00:01:04  pt4-eth1 -> plk-…
               eth2 02:00:0b:00:02:04  pt4-eth2 -> plk-…
  ne-01        ne        10.0.0.5  (2 cpu, 1536M)
               …
  ne-02        ne        10.0.0.6  (2 cpu, 1536M)
```
See [`topologies/ne-lab.yaml`](topologies/ne-lab.yaml) for the full format. In
short:

```yaml
name: ne-lab
topology:
  defaults: {kind: vm, cpus: 2, memory: 1024M}
  kinds:
    vm: {cpus: 2, memory: 1024M}
  groups:
    ne: {memory: 1536M}
  nodes:
    host-a: {group: host}
    ne: {group: ne, count: 3}      # -> ne-00, ne-01, ne-02
  links:
    - endpoints: ["host-a:eth1", "ne-00:eth1"]
```

- Settings resolve **node → group → kind → defaults**, as in containerlab.
- The **node key becomes the VM ID**, so `ne-00` above is `plasma vm shell ne-00`.
- **`count`** expands one entry into a fleet (a plasma addition; containerlab
  has no such field), named exactly as the auto-allocator would.
- **`kind: vm`** boots a VM; **`kind: bridge`** is a shared L2 segment (see
  links below). `router`/`switch`/`nat` are rejected with a clear error until
  they land.
- Node IDs are capped at 11 characters so tap names (`tap-<id>`, 15 bytes max)
  stay unique.

**Links are real NICs.** `eth0` is always the management NIC on `plasma-br0`;
each link adds the next one, and a VM's link interfaces must be `eth1`,
`eth2`, ... with no gaps, because the guest names NICs in the order they are
attached. Three endpoint shapes:

```yaml
  nodes:
    a: {}
    b: {}
    c: {}
    lan: {kind: bridge}                 # host bridge pbr-lan, no VM
  links:
    - endpoints: ["a:eth1", "b:eth1"]   # private bridge plk-<hash> for the pair
    - endpoints: ["a:eth2", "lan:a"]    # joins pbr-lan; ":a" is only a label
    - endpoints: ["b:eth2", "lan:b"]
    - endpoints: ["c:eth1", "lan:c"]
    - endpoints: ["c:eth2", "host:cap0"] # unbridged host tap named cap0
```

**Addresses.** Give a bridge a `subnet` and plasma allocates every VM NIC on
it an address; nothing is written per node. A link can name a counted node
to wire the whole fleet in one line, and `ipv4:` pins one end (a service at a
well-known address):

```yaml
  nodes:
    svc: {}
    node: {count: 100}
    lan: {kind: bridge, subnet: 10.77.0.0/24}
  links:
    - endpoints: [{node: svc, interface: eth1, ipv4: 10.77.0.254}, "lan:svc"]
    - endpoints: ["node:eth1", "lan:node"]   # node-1 .. node-100, eth1 each
```

Allocation is deterministic: lowest free host address, in node order (a
count's members in ordinal order), so node-k gets .k here. `plasma vm list`
shows each VM's addresses in its LINKS column, and the guest reads its own from
`/run/credentials/@system/plasma.nics` (`vm <id>`, then `<nic> <bridge> <addr>`
per line). plasma records and hands out the addresses; configuring them on the
NIC is the application's job, as with everything on a link.

Link segments are pure L2: the host has no address on them, runs no DHCP
there and has IPv6 off, so what crosses a link is only what the VMs send.
In the guest, link NICs are left to the applications: plasma marks them
`Unmanaged=yes` for systemd-networkd (a `network.network.10-plasma-links`
credential on the kernel command line), so they boot down and unaddressed and
the base image's DHCP never touches them — or holds up `network-online.target`
waiting for a lease no link will hand out.
Capture one with `plasma vm capture <id>:eth2` (that NIC's tap) or
`plasma vm capture pbr-lan` (the whole segment). If the host loads
`br_netfilter` with a `DROP` FORWARD policy, bridged link traffic is filtered
too: accept it with e.g.
`iptables -I FORWARD -m physdev --physdev-is-bridged -j ACCEPT`.

Re-applying a topology whose VMs already exist fails per node rather than
duplicating them; `plasma down` first to start clean.

### List
```bash
plasma vm list
```
```
ID      GROUP  STATUS   IP        PID      UPTIME
host-1  host   running  10.0.0.2  <pid>    10s
host-2  host   running  10.0.0.3  <pid>    10s
```

### Shell in
```bash
plasma vm shell host-1
```
Log in with `root` / `plasma`. **Press Ctrl-X to detach** — Ctrl-C is passed
through to the guest, not to `plasma`. Detaching leaves the VM running.

You can attach and detach as often as you like: the console is a pty
(`--serial pty`), which reopens cleanly.

> **VMs launched before this change use a unix socket console and are still
> one-shot.** cloud-hypervisor never reaps a disconnected serial-socket client,
> so once such a VM has been attached to and detached from, its console is dead
> for good. `plasma vm list` still works; to get a usable shell,
> `plasma vm delete <id>` and relaunch so it comes up with a pty.

### Verify connectivity
```bash
ping -c 3 10.0.0.2                    # host → VM
plasma vm shell host-1                # then, inside: ping -c 3 10.0.0.3
```

### Delete
```bash
plasma vm delete host-1     # one VM
plasma down                 # the whole fleet
```
Both stop the `cloud-hypervisor` process, free the tap and DHCP reservation,
and remove the disk, runtime files and state entry. `plasmad` keeps running
after `plasma down`; stop it with `sudo pkill -x plasmad`.

---

## Remote engine: labs from anywhere

A host running `plasmad` is an **engine**. A test suite (or a person) on any
other machine can bring up a lab on it, reach the lab's services, capture its
traffic and impair its links — without a login on the engine, without `sudo`,
and without sharing files with it. The same commands work against the local
engine; nothing about them assumes the client and engine are one host.

A **lab** is one applied topology and everything it created. Names inside it
(`fabric`, `node-1`, `wire`) are scoped to it, each lab gets its own management
network (`plasma-br<N>`, `10.0.<N>.0/24`, its own dnsmasq), and
`plasma lab delete` removes exactly what it made — so two runs never collide.

### On the engine (once)

```bash
cd plasma && sudo make install install-service   # plasmad under systemd
echo 'PLASMAD_LISTEN=:7443' | sudo tee -a /etc/default/plasmad
sudo systemctl restart plasmad
sudo ./scripts/net-host.sh        # NAT for every lab's management network
sudo ./scripts/app-prep.sh        # guest bootstrap that reports "ready"
sudo plasmad --print-login --listen :7443
```

`plasmad` generates a self-signed TLS certificate and a token on first start
(`/var/lib/plasma/tls/`, `/var/lib/plasma/token`, root-only). `--print-login`
prints the exact command for the client. Without `PLASMAD_LISTEN`, plasmad
serves only its Unix socket, as before.

> Re-run `app-prep.sh` after upgrading: a lab is **ready** once every guest
> writes a ready line to its virtio console, which only the new bootstrap does.
> VMs from an older base image boot fine but never report ready.

### On the client

```bash
plasma engine login https://engine.lan:7443 --token <token> --fingerprint sha256:<…>
plasma engine info        # guest kernel, kbuild image, capacity
```

The login is saved in `~/.config/plasma/engine.json` (mode 0600). For CI, set
`PLASMA_ENGINE=https://…`, `PLASMA_TOKEN` and `PLASMA_FINGERPRINT` instead;
`PLASMA_ENGINE=local` forces the local daemon, and `plasma engine logout`
returns to it. The fingerprint is a pin: a client refuses any other
certificate, so the token never goes to the wrong host.

### Run a lab

```bash
plasma lab apply topologies/ne-lab.yaml --name it-1 --wait
plasma lab status it-1
plasma lab tunnel it-1 3000:fabric:3000 8080:fabric:8080 &   # http://localhost:3000
plasma lab shell it-1 fabric                                  # serial console (Ctrl-X)
plasma lab capture it-1 ne-00:eth2 --wireshark
plasma lab mirror it-1 ne-00:eth2 --to capture:eth3           # replaces sudo tc mirred
plasma lab impair it-1 ne-00:eth2 --delay 80ms --loss 0.5     # replaces fault-injector.sh
plasma lab impair it-1 ne-00:eth2 --clear
plasma lab drop it-1 ne-01:eth2 --once --wait --rule proto=udp,src=10.77.0.1,sport=4242,u8@34=5
plasma lab delete it-1
```

- **`apply`** validates the topology on the client, uploads every file it
  names (payloads, start scripts) to the engine's content-addressed store —
  skipping any the engine already has, so a rerun sends only what changed —
  and returns while the lab boots; `--wait` (or `plasma lab wait`) blocks
  until every node is ready. A lab that cannot fit in the engine's memory is
  refused up front.
- **Topology files are unchanged.** `path:` is read on the client. An app can
  instead name an uploaded artifact, `artifact: sha256:…` (from
  `plasma artifact push`). A **bundle** — a `.tar` / `.tar.gz` payload, or
  `type: bundle` — is unpacked into the app's directory, so its `cmd` can be a
  script inside it: `{name: it, artifact: sha256:…, cmd: ./boot.sh}`.
- **Tunnels keep `localhost`.** Each connection rides the API connection, so a
  lab's services are never on an open port, and `http://localhost:3000` (and
  the Keycloak redirect URIs that expect it) keeps working.
- **Capture targets** in a lab: `<vm>`, `<vm>:<ethN>`, a `kind: bridge` node's
  name (the whole segment), or `mgmt`. The Wireshark plugin lists the labs of
  whichever engine the client is logged in to.
- **Impairment** applies to frames delivered *to* that NIC; `--both-ends`
  also applies it to the far end of a VM-to-VM link.
- **Drop rules** (`plasma lab drop`) lose only the frames matching a u32
  rule (protocol, addresses, ports, bytes at offsets), on their way to that
  NIC. With `--once` the engine removes them on the first drop, so no second
  frame slips through; the count is reported. `plasma lab mirror … --stats`
  reports what a mirror copied each way.
- **Building modules for the engine:** `plasma engine info` gives the guest
  kernel release, its headers package and a matching build image; build your
  `.ko` there (as `applications/scripts/build-ko.sh` does) instead of reading
  `/opt/plasma` on the engine.

The legacy, unscoped commands (`plasma vm launch`, `vm list`, `vm shell`,
`vm capture`, `down`) keep working, locally or remotely. `plasma vm --topology`
reads app paths from the engine's own disk, so it stays local-only; `plasma
down` no longer touches VMs that belong to labs.

### Engine API

Everything above is this API; the CLI is one client of it. Over the network
every call needs `Authorization: Bearer <token>`; the Unix socket is limited
to the `plasma` group as before.

| Call | |
|------|---|
| `GET /v1/engine` | version, arch, guest kernel, headers package, kbuild image, capacity, artifact cache |
| `HEAD` / `PUT /v1/artifacts/{sha256}` | is it cached / upload it (the content must hash to the name) |
| `POST /v1/labs` | `{name, topology: "<yaml>", files: {"<path as written>": "sha256:…"}}` → 202, lab status |
| `GET /v1/labs`, `GET /v1/labs/{lab}` | phase, `ready`, per-VM status, IPs, link addresses, mirrors, filters |
| `DELETE /v1/labs/{lab}` | stop a lab (even mid-create) and remove everything it made |
| `GET /v1/labs/{lab}/topology` | the lab's wiring as a graph |
| `GET /v1/labs/{lab}/capture?target=&filter=&snaplen=` | live pcap stream |
| `GET /v1/labs/{lab}/vms/{vm}/tunnel/{port}` | `Upgrade: plasma-stream` → a TCP stream to that port on the VM |
| `GET /v1/labs/{lab}/vms/{vm}/console` | `Upgrade: plasma-stream` → the serial console |
| `PUT` / `GET` / `DELETE /v1/labs/{lab}/links/{vm:ethN}/mirror` | `{to: "vm:ethN"}`; `GET` = packets copied each way |
| `PUT` / `GET` / `DELETE /v1/labs/{lab}/links/{vm:ethN}/drop` | `{rules: [{protocol, src, dst, sport, dport, bytes: [{offset, value, mask}]}], once, timeout}` → `{armed, done, dropped}` |
| `PUT` / `DELETE /v1/labs/{lab}/links/{vm:ethN}/filters` | `{delay, jitter, loss, drop, both_ends}` |

---

## Capturing packets

Each VM's link terminates in a host-side tap (`tap-host-1`) on `plasma-br0`, so
`plasmad` captures there and streams the pcap back. **No `sudo` needed** — only
the `plasma` group.

### From the CLI
```bash
plasma vm capture host-1                      # live packet lines, like tcpdump
plasma vm capture host-1 -f "icmp"            # BPF filter
plasma vm capture host-1 --wireshark          # live into Wireshark
plasma vm capture host-1 -w out.pcap          # to a file
plasma vm capture host-1 -f "port 4500" -s 128
plasma vm capture host-1 | wireshark -k -i -  # explicit pipe
plasma vm capture plasma-br0                  # the bridge (see below)
```
```
$ plasma vm capture host-1 -f icmp
capturing host-1 (Ctrl-C to stop)
07:08:29.145366 IP 10.0.0.1 > 10.0.0.2: ICMP echo request, id 6, seq 1, length 64
07:08:29.145571 IP 10.0.0.2 > 10.0.0.1: ICMP echo reply, id 6, seq 1, length 64
```

The output adapts to where it is going: **readable packet lines on a terminal**
(decoded through your local `tcpdump`), **raw pcap when piped or redirected**,
so `| wireshark -k -i -` and `> out.pcap` both work unchanged.

Ctrl-C stops it. An invalid filter fails immediately with tcpdump's own
message instead of leaving you an empty file.

### Over SSH, into Wireshark on your own machine
`--wireshark` needs a graphical session, so it fails over plain SSH with
*"could not connect to display"*. Instead of forwarding X, stream the pcap to
the Wireshark on your laptop — run this **locally**, not on the plasma host:
```bash
ssh <user>@<host> "plasma vm capture host-1 -f icmp" | wireshark -k -i -
```
`plasma vm capture` writes pcap to stdout whenever stdout is not a terminal, so
this needs nothing installed on your side but Wireshark. (`ssh -X` and then
`--wireshark` also works, if you already run an X server.)

### From Wireshark's interface list
After `sudo make install-extcap`, restart Wireshark: `Plasma VM host-1
(10.0.0.2)` appears alongside your physical interfaces. Double-click to
capture. The plugin runs as you, so you need the `plasma` group.

> **A bridge capture is not a fleet-wide mirror.** Capturing `plasma-br0` sees
> broadcast and host-directed traffic only — a Linux bridge forwards VM-to-VM
> frames port-to-port without passing them up to the bridge device. To see a
> given VM's traffic, capture that VM's tap.

---

## Running applications in VMs

`applications/` is a drop folder: whatever lands in `applications/dist/` shows
up at `/opt/ne` in **every** VM, packed into one read-only disk shared by the
whole fleet. On boot each VM `insmod`s any `*.ko` it finds (passing a device ID
derived from its MAC) and runs the app.

```bash
cd applications && make        # build + pack
sudo ./scripts/app-prep.sh     # once: inject the boot bootstrap into the base image
plasma vm launch host          # VMs launched from now on load it
```

The reference payload proves the NE stack's shape end to end — a kernel module
reached over generic netlink, and two VMs deriving a shared session key from
their kernel-supplied device IDs. See
[`applications/README.md`](applications/README.md).

---

## Credentials

| Field    | Value    |
|----------|----------|
| Username | `root`   |
| Password | `plasma` |

---

## Troubleshooting

| Symptom | Cause / fix |
|---------|-------------|
| `no access to /var/run/plasma.sock` | Your **session** predates the group. Run `plasma init` — it starts a shell that has it (or `newgrp plasma` / re-login) |
| `plasmad is not running` | Run `plasma init`, or start it by hand: `sudo plasmad &` |
| CLI: *"could not connect to display"* / Qt platform plugin error | `--wireshark` needs a GUI session. Over SSH, pipe to a local Wireshark instead — see "Over SSH" above |
| `plasma: unknown command "capture"` | Stale binary — re-run `sudo make install` |
| `disk already exists` on launch | `state.json` and the runtime disk are out of sync |
| Ctrl-C won't exit `vm shell` | Use **Ctrl-X**; Ctrl-C goes to the guest |
| A VM's console never responds | It predates the pty console (its record has `console_socket`, not `console_pty`). Unrecoverable for that VM — `plasma vm delete <id>` and relaunch |
| `insmod: vermagic mismatch` in a guest | The base image's kernel changed; rebuild with `cd applications && make` |
| `another plasmad is already running` | Correct — a second daemon would hijack the socket and corrupt state. Stop the first: `sudo pkill -x plasmad` |
| VMs launch without `/opt/ne` | No artifacts disk: run `cd applications && make`, and `sudo ./scripts/app-prep.sh` once |
| Guest kernel reverted to 6.12 | `m1-prep.sh` was re-run; redo `kernel-swap.sh` → `app-prep.sh` → `applications && make` |

If you ever clean up by hand (e.g. the daemon was killed mid-launch), remove
both sides together — `sudo rm -rf /var/run/plasma/vms/<id>` **and**
`/opt/plasma/runtime/<id>.raw`, plus the matching entry in
`/var/lib/plasma/state.json`. Deleting one without the other is what produces
`disk already exists`. `plasmad` logs any unclaimed disk it finds at startup.

---

## Network layout

```
Host (10.0.0.1)  ←→  plasma-br0  ←→  host-1 (10.0.0.2)
                                  ←→  host-2 (10.0.0.3)
                                  ←→  host-N (10.0.0.N+1)
```

- Bridge: `plasma-br0`
- DHCP: `dnsmasq` with per-VM MAC→IP reservations
- NAT: host forwards VM traffic to the external network
- Capture point: `tap-<id>`, the host end of each VM's link

---

## Key paths

| Path | Purpose |
|------|---------|
| `/opt/plasma/images/rootfs-base.raw` | Base disk image (read-only) |
| `/opt/plasma/runtime/<id>.raw`       | Per-VM copy-on-write disk   |
| `/opt/plasma/debian/vmlinuz`         | Guest kernel                |
| `/opt/plasma/debian/initrd.img`      | Guest initrd                |
| `/var/lib/plasma/state.json`         | VM registry (survives reboot) |
| `/var/run/plasma/vms/<id>/`          | Per-VM CH socket + console log |
| `/var/run/plasma/dnsmasq/`           | Per-VM DHCP host entries    |
| `/var/run/plasma.sock`               | Control socket, `root:plasma` 0660 |
| `/usr/local/bin/{plasma,plasmad,plasma-extcap}` | Installed binaries |
| `~/.config/wireshark/extcap/plasma-extcap` | Wireshark capture plugin |

---

## Further reading

- Full build plan and milestones → [`plasma-build-plan.html`](plasma-build-plan.html)
- Architecture and CLI surface → [`CLAUDE.md`](CLAUDE.md)

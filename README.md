# Plasma — Network Encryptor Simulation Platform

CLI-driven platform that spins up VMs on Cloud Hypervisor to simulate the
Network Encryptor (NE) product at fleet scale.

---

## Quick reference

| Task | Command |
|------|---------|
| Start the daemon | `sudo plasmad &` |
| Start the UI | `cd plasma-ui && npm run dev` → http://127.0.0.1:7080 |
| Create a VM | `plasma vm launch host` |
| List VMs | `plasma vm list` |
| Shell into a VM | `plasma vm shell host-1` (detach: **Ctrl-X**) |
| Delete one VM | `plasma vm delete host-1` |
| Delete the fleet | `plasma down` |
| Capture packets | `plasma capture host-1 --wireshark` |

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
- Node.js 18+ and npm — for the topology UI

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
the CLI and UI talk to the daemon without `sudo`** — nothing works until it
applies.

### 3. Optional extras
```bash
sudo make install-extcap    # Wireshark plugin: VMs in the interface list
cd ../plasma-ui && npm install
sudo make install-service   # run plasmad under systemd instead of by hand
```

---

## Running Plasma

### Start the daemon
```bash
sudo plasmad &
```
Root is needed for taps and VM processes; the CLI and UI are not. Skip this if
you installed the systemd service.

### Start the UI
```bash
cd plasma && make ui-dev        # http://127.0.0.1:7080
```
`make ui-dev` runs the server under `sg plasma`, so it works even if your shell
session predates the group. Plain `npm run dev` from `plasma-ui/` works too —
but only once `id -nG` actually lists `plasma` (see Troubleshooting).

The UI reaches `plasmad` over the same unix socket the CLI uses, so the daemon
needs no network listener. Two consequences: the process running the UI **must
be in the `plasma` group**, and it binds `127.0.0.1` only — anyone who can
reach it inherits your plasma-group access.

**Viewing it over SSH.** The UI is bound to loopback, so forward the port
rather than exposing it — in VS Code use the **PORTS** panel, or from a
terminal:
```bash
ssh -L 7080:127.0.0.1:7080 <user>@<host>     # then browse 127.0.0.1:7080 locally
```

---

## Managing VMs

### Create
```bash
plasma vm launch host        # first → host-1 at 10.0.0.2
plasma vm launch host        # second → host-2 at 10.0.0.3
```
IDs are `<group>-N`; addresses come from the lowest free slot in `10.0.0.0/24`.

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

> **Do not type `exit` or `logout`.** Logging out makes the guest's getty
> respawn and write to a serial socket whose client has gone; cloud-hypervisor
> keeps the stale connection, so the console goes dead for every later client
> and that VM can never be attached again. Always leave with Ctrl-X.

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

## Capturing packets

Each VM's link terminates in a host-side tap (`tap-host-1`) on `plasma-br0`, so
`plasmad` captures there and streams the pcap back. **No `sudo` needed** — only
the `plasma` group.

### From the UI
1. Start the daemon and the UI, then open http://127.0.0.1:7080.
2. The topology draws `plasma-br0` with every VM hanging off it — each node
   showing status, IP, tap and uptime.
3. Optionally type a BPF filter (e.g. `icmp`) in the header box; it applies to
   whatever you click next.
4. On any VM node press:
   - **Wireshark** — opens a live capture on your desktop
   - **.pcap** — streams a download (cancel the download to stop the capture)
   - **Copy** — copies the equivalent `plasma capture` command

> **The Wireshark button needs a graphical session.** The UI server spawns
> Wireshark as you, so over plain SSH it returns *"No graphical display
> detected"*. Use `ssh -X`, run the UI on the machine you're sitting at, or
> fall back to `plasma capture <id> --wireshark`.

### From the CLI
```bash
plasma capture host-1 --wireshark          # live into Wireshark
plasma capture host-1 -w out.pcap          # to a file
plasma capture host-1 -f "icmp"            # BPF filter
plasma capture host-1 -f "port 4500" -s 128
plasma capture host-1 | wireshark -k -i -  # explicit pipe
plasma capture plasma-br0                  # the bridge (see below)
```
Ctrl-C stops it. An invalid filter fails immediately with tcpdump's own
message instead of leaving you an empty file.

### Over SSH, into Wireshark on your own machine
`--wireshark` needs a graphical session, so it fails over plain SSH with
*"could not connect to display"*. Instead of forwarding X, stream the pcap to
the Wireshark on your laptop — run this **locally**, not on the plasma host:
```bash
ssh <user>@<host> "plasma capture host-1 -f icmp" | wireshark -k -i -
```
`plasma capture` writes pcap to stdout whenever stdout is not a terminal, so
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
| `no access to /var/run/plasma.sock` | Your **session** predates the group. Check with `id -nG`; if `getent group plasma` lists you but `id -nG` doesn't, reconnect your SSH/VS Code session, or use `newgrp plasma` / `sg plasma -c "<cmd>"` for now |
| `plasmad is not running` | Start it: `sudo plasmad &` |
| CLI: *"could not connect to display"* / Qt platform plugin error | `--wireshark` needs a GUI session. Over SSH, pipe to a local Wireshark instead — see "Over SSH" above |
| UI: *"No graphical display detected"* | Same cause, caught before a capture starts. Use the **.pcap** button, or the SSH pipe |
| `plasma: unknown command "capture"` | Stale binary — re-run `sudo make install` |
| `disk already exists` on launch | `state.json` and the runtime disk are out of sync |
| Ctrl-C won't exit `vm shell` | Use **Ctrl-X**; Ctrl-C goes to the guest |
| A VM's console never responds again | Someone typed `exit` in `vm shell`. Unrecoverable for that VM — `plasma vm delete <id>` and relaunch |
| `insmod: vermagic mismatch` in a guest | The base image's kernel changed; rebuild with `cd applications && make` |

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
| `plasma-ui/`                         | Topology + capture web UI   |

---

## Further reading

- Full build plan and milestones → [`plasma-build-plan.html`](plasma-build-plan.html)
- Architecture and CLI surface → [`CLAUDE.md`](CLAUDE.md)

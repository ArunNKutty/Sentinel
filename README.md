# Plasma — Network Encryptor Simulation Platform

CLI-driven platform that spins up VMs on Cloud Hypervisor to simulate the
Network Encryptor (NE) product at fleet scale.

---

## Prerequisites

- Linux host with KVM enabled
- `cloud-hypervisor` installed and on `$PATH`
- `dnsmasq` installed (`apt install dnsmasq`)
- Base image prepared at `/opt/plasma/images/rootfs-base.raw`
  (run `sudo ./scripts/m1-prep.sh` once to build it)

---

## Quick Start

### 1. One-time host network setup
Re-run this after every host reboot to recreate the virtual bridge and NAT:
```bash
cd /home/fortaegis/codebase/simulation/nesimulation
sudo ./scripts/m1-net-host.sh
```

### 2. Build the binaries
```bash
cd plasma
go build -o /tmp/plasmad ./cmd/plasmad
go build -o /tmp/plasma  ./cmd/plasma
```

### 3. Start the control-plane daemon
```bash
sudo /tmp/plasmad &
```

### 4. Launch VMs
```bash
# Launch first VM  → assigned 10.0.0.2
sudo /tmp/plasma vm launch host

# Launch second VM → assigned 10.0.0.3
sudo /tmp/plasma vm launch host
```

### 5. List the fleet
```bash
sudo /tmp/plasma vm list
```

Expected output:
```
ID      GROUP  STATUS   IP        PID      UPTIME
host-1  host   running  10.0.0.2  <pid>    10s
host-2  host   running  10.0.0.3  <pid>    10s
```

### 6. Verify connectivity

**Host → VM:**
```bash
ping -c 3 10.0.0.2
ping -c 3 10.0.0.3
```

**VM → VM** (login with `root` / `plasma`):
```bash
sudo /tmp/plasma vm shell host-1
# inside the VM:
ping -c 3 10.0.0.3
```

---

## Credentials

| Field    | Value    |
|----------|----------|
| Username | `root`   |
| Password | `plasma` |

---

## Shutdown & Clean Up

```bash
sudo pkill -f cloud-hypervisor
sudo pkill -f plasmad
sudo rm -rf /var/run/plasma/vms /var/run/plasma/state.json
sudo rm -f  /opt/plasma/runtime/*.raw
```

---

## Network Layout

```
Host (10.0.0.1)  ←→  plasma-br0  ←→  host-1 (10.0.0.2)
                                  ←→  host-2 (10.0.0.3)
                                  ←→  host-N (10.0.0.N+1)
```

- Bridge: `plasma-br0`
- DHCP: `dnsmasq` with per-VM MAC→IP reservations
- NAT: host forwards VM traffic to the external network

---

## Key Paths

| Path | Purpose |
|------|---------|
| `/opt/plasma/images/rootfs-base.raw` | Base disk image (read-only) |
| `/opt/plasma/runtime/<id>.raw`       | Per-VM copy-on-write disk   |
| `/opt/plasma/debian/vmlinuz`         | Guest kernel                |
| `/opt/plasma/debian/initrd.img`      | Guest initrd                |
| `/var/run/plasma/state.json`         | Live VM registry            |
| `/var/run/plasma/dnsmasq/`           | Per-VM DHCP host entries    |

---

## Further Reading

- Full build plan and milestones → [`plasma-build-plan.html`](plasma-build-plan.html)
- Architecture and CLI surface → [`CLAUDE.md`](CLAUDE.md)

package plasma

import (
	"errors"
	"path/filepath"
	"time"
)

const (
	SocketPath = "/var/run/plasma.sock"
	// State outlives reboots, so it lives next to the disks it references
	// rather than on the tmpfs-backed /var/run.
	StateDir  = "/var/lib/plasma"
	StateFile = "/var/lib/plasma/state.json"
	// Per-VM runtime scratch (CH api socket, console log) is reboot-scoped.
	RunDir = "/var/run/plasma"
	VMsDir = "/var/run/plasma/vms"
	// Where state lived before it moved to /var/lib; adopted once on startup.
	LegacyStateFile = "/var/run/plasma/state.json"
	// LinksFile holds links declared by the last applied topology. Kept beside
	// state.json because, like state, it outlives a reboot.
	LinksFile  = "/var/lib/plasma/links.json"
	RuntimeDir = "/opt/plasma/runtime"
	BaseRootfs = "/opt/plasma/images/rootfs-base.raw"
	// ArtifactsDisk carries user applications (see applications/) into every
	// VM. Attached read-only, so one image is shared by the whole fleet rather
	// than copied per VM. Optional: VMs boot fine without it.
	ArtifactsDisk = "/opt/plasma/artifacts.img"
	KernelPath    = "/opt/plasma/debian/vmlinuz"
	InitrdPath    = "/opt/plasma/debian/initrd.img"
	GuestCmdline  = "root=/dev/vda1 rw panic=5 net.ifnames=0 biosdevname=0 quiet"

	Bridge          = "plasma-br0"
	DnsmasqHostsDir = "/var/run/plasma/dnsmasq"
	DnsmasqPidFile  = "/var/run/plasma-dnsmasq.pid"
)

// ErrNotFound is returned (wrapped with the ID) when a lookup targets a VM
// that isn't in state.
var ErrNotFound = errors.New("no such VM")

type Status string

const (
	StatusRunning Status = "running"
	StatusExited  Status = "exited"
)

type VmRecord struct {
	ID            string `json:"id"`
	Group         string `json:"group"`
	Status        Status `json:"status"`
	PID           int    `json:"pid"`
	APISocket     string `json:"api_socket"`
	ConsoleSocket string `json:"console_socket"`
	// ConsolePTY is the guest's serial line as a pty (/dev/pts/N). Unlike a
	// unix socket, a pty can be reopened, so `vm shell` can attach and detach
	// repeatedly. Empty on VMs launched before this became the default.
	ConsolePTY string    `json:"console_pty"`
	DiskPath   string    `json:"disk_path"`
	MAC        string    `json:"mac"`
	IP         string    `json:"ip"`
	TapDevice  string    `json:"tap_device"`
	CPUs       int       `json:"cpus,omitempty"`
	Memory     string    `json:"memory,omitempty"`
	StartedAt  time.Time `json:"started_at"`
}

// ConsoleSocketPath is the per-VM Unix socket that carries the guest's raw
// ttyS0 stream (cloud-hypervisor `--serial socket=`). Deterministic from the
// ID, so both plasmad and the CLI can compute it without a round trip.
func ConsoleSocketPath(id string) string {
	return filepath.Join(VMsDir, id, "console.sock")
}

// maxIfaceName is IFNAMSIZ-1: the kernel caps interface names at 15 usable
// bytes, so a long VM ID has to be truncated to form a tap name.
const maxIfaceName = 15

// TapDeviceName is the host-side tap for a VM. Launch creates the device under
// this name and capture attaches to it, so both must derive it identically.
func TapDeviceName(id string) string {
	tap := "tap-" + id
	if len(tap) > maxIfaceName {
		tap = tap[:maxIfaceName]
	}
	return tap
}

type LaunchRequest struct {
	Group string `json:"group"`
	// ID names the VM explicitly instead of letting the daemon allocate the
	// next <group>-N. Set when launching from a topology file, where the node
	// key is the VM's identity.
	ID string `json:"id,omitempty"`
	// CPUs and Memory override the launcher defaults; zero values inherit.
	CPUs   int    `json:"cpus,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// DeclaredLink is a link from a topology file. It records intent only: every
// VM currently gets one NIC on the shared bridge, so links are drawn in the
// UI but do not yet create their own L2 segment. Wiring arrives with the
// router/switch/NAT kinds.
type DeclaredLink struct {
	ANode      string `json:"a_node"`
	AInterface string `json:"a_interface"`
	BNode      string `json:"b_node"`
	BInterface string `json:"b_interface"`
}

// LinksDocument is what the daemon persists from an applied topology.
type LinksDocument struct {
	Topology string         `json:"topology"`
	Links    []DeclaredLink `json:"links"`
}

type ListResponse struct {
	VMs []VmRecord `json:"vms"`
}

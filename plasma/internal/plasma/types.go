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
	// AppsDiskLabel is the label on the per-VM applications disk. Unlike the
	// artifacts disk, every VM gets its own image, so the label is reused —
	// each guest only ever sees the one attached to it.
	AppsDiskLabel = "plasma-apps"
	// AppsMount is where the guest mounts that disk.
	AppsMount    = "/opt/apps"
	KernelPath   = "/opt/plasma/debian/vmlinuz"
	InitrdPath   = "/opt/plasma/debian/initrd.img"
	GuestCmdline = "root=/dev/vda1 rw panic=5 net.ifnames=0 biosdevname=0 quiet"

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
	ConsolePTY string `json:"console_pty"`
	DiskPath   string `json:"disk_path"`
	MAC        string `json:"mac"`
	IP         string `json:"ip"`
	TapDevice  string `json:"tap_device"`
	CPUs       int    `json:"cpus,omitempty"`
	Memory     string `json:"memory,omitempty"`
	// AppsDisk is the per-VM applications image, empty when the node declared
	// no apps. Recorded so Delete can remove it.
	AppsDisk  string    `json:"apps_disk,omitempty"`
	Apps      []AppSpec `json:"apps,omitempty"`
	StartedAt time.Time `json:"started_at"`
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
	// Apps are payloads to deploy into this VM. Paths are read by the daemon,
	// so they must be absolute on the host running plasmad.
	Apps []AppSpec `json:"apps,omitempty"`
}

// AppSpec is one application deployed onto a node: a payload to carry in and
// how to start it. It is both the YAML shape in a topology file and the wire
// shape on /v1/vms.
type AppSpec struct {
	Name string `yaml:"name" json:"name"`
	// Type is AppTypeKernel (a .ko the guest insmods) or AppTypeUserspace (a
	// program the guest runs: Go binary, Python script, anything executable).
	// Inferred from the path's extension when omitted.
	Type string `yaml:"type,omitempty" json:"type,omitempty"`
	// Path is the host-side payload: a Go executable, a .ko, a script.
	Path string `yaml:"path" json:"path"`
	// Cmd starts the app. Two forms, distinguished by whether it names a file
	// on the host: a start script (absolute, or relative to the topology
	// file) is packed onto the apps disk beside the payload and executed
	// there — the guest cannot reach host paths — while anything else is a
	// command line run by /bin/sh in the guest, e.g. `make -C /opt/apps/x run`.
	// Either way it runs with the app's directory as cwd and $APP pointing at
	// the deployed payload. Empty means insmod (kernel) or exec the payload
	// (userspace).
	Cmd string `yaml:"cmd,omitempty" json:"cmd,omitempty"`
	// Script is the host path of a start script to deploy, set by topology
	// resolution when Cmd named one. Not written by hand.
	Script string `yaml:"-" json:"script,omitempty"`
}

// App types. A topology may state one explicitly; otherwise a .ko is a kernel
// module and everything else is userspace.
const (
	AppTypeKernel    = "kernel"
	AppTypeUserspace = "userspace"
)

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

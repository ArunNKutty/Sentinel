package plasma

import "time"

const (
	SocketPath   = "/var/run/plasma.sock"
	StateDir     = "/var/run/plasma"
	StateFile    = "/var/run/plasma/state.json"
	VMsDir       = "/var/run/plasma/vms"
	RuntimeDir   = "/opt/plasma/runtime"
	BaseRootfs   = "/opt/plasma/images/rootfs-base.raw"
	KernelPath   = "/opt/plasma/debian/vmlinuz"
	InitrdPath   = "/opt/plasma/debian/initrd.img"
	GuestCmdline = "root=/dev/vda1 rw panic=5 net.ifnames=0 biosdevname=0 quiet"
)

type Status string

const (
	StatusRunning Status = "running"
	StatusExited  Status = "exited"
)

type VmRecord struct {
	ID        string    `json:"id"`
	Group     string    `json:"group"`
	Status    Status    `json:"status"`
	PID       int       `json:"pid"`
	APISocket string    `json:"api_socket"`
	DiskPath  string    `json:"disk_path"`
	MAC       string    `json:"mac"`
	IP        string    `json:"ip"`
	StartedAt time.Time `json:"started_at"`
}

type LaunchRequest struct {
	Group string `json:"group"`
}

type ListResponse struct {
	VMs []VmRecord `json:"vms"`
}

package plasma

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
)

// LabNet is the network namespace a VM is launched into. Index 0 is the
// legacy, unscoped fleet (`plasma vm launch`, `plasma vm --topology`) and
// reproduces the names plasma has always used, so existing state keeps
// working. Every lab gets its own index, and with it its own management
// bridge, /24, dnsmasq and host interface names, so two labs never collide.
type LabNet struct {
	// ID is the lab's name; empty for the legacy fleet.
	ID    string
	Index int
}

// MaxLabs bounds lab indexes to the third octet of 10.0.<n>.0/24.
const MaxLabs = 254

func (l LabNet) legacy() bool { return l.Index == 0 }

// MgmtBridge is the lab's management bridge, every VM's eth0.
func (l LabNet) MgmtBridge() string {
	if l.legacy() {
		return Bridge
	}
	return fmt.Sprintf("plasma-br%d", l.Index)
}

func (l LabNet) Subnet() string  { return fmt.Sprintf("10.0.%d.0/24", l.Index) }
func (l LabNet) Gateway() string { return fmt.Sprintf("10.0.%d.1", l.Index) }
func (l LabNet) VMAddr(octet int) string {
	return fmt.Sprintf("10.0.%d.%d", l.Index, octet)
}

// MgmtMAC keeps the legacy 02:00:0a:00:00:<octet> at index 0.
func (l LabNet) MgmtMAC(octet int) string {
	return fmt.Sprintf("02:00:0a:00:%02x:%02x", l.Index, octet)
}

// MgmtTap is a VM's management tap. Lab taps are keyed on index and octet,
// not the VM ID, so any node name fits IFNAMSIZ: "pt254-254" is 9 bytes.
func (l LabNet) MgmtTap(id string, octet int) string {
	if l.legacy() {
		return TapDeviceName(id)
	}
	return fmt.Sprintf("pt%d-%d", l.Index, octet)
}

// LinkTap is a link NIC's tap: "pt254-254-eth99" is exactly 15 bytes.
func (l LabNet) LinkTap(octet int, nic string) string {
	if l.legacy() {
		return NicTapName(octet, nic)
	}
	return fmt.Sprintf("pt%d-%d-%s", l.Index, octet, nic)
}

// LinkMAC keeps the legacy 02:00:0b:00:<idx>:<octet> at index 0.
func (l LabNet) LinkMAC(octet, idx int) string {
	return fmt.Sprintf("02:00:0b:%02x:%02x:%02x", l.Index, idx, octet)
}

// HostBridge maps a topology's logical bridge (plk-<hash>, pbr-<name>) to the
// host interface behind it. In a lab the name is hashed with the lab ID, so
// two labs declaring `wire:` get separate segments; the prefix is kept so
// isLinkBridge still recognises it as plasma's own.
func (l LabNet) HostBridge(logical string) string {
	if l.legacy() {
		return logical
	}
	prefix := LinkBridgePrefix
	if strings.HasPrefix(logical, NodeBridgePrefix) {
		prefix = NodeBridgePrefix
	}
	sum := sha256.Sum256([]byte(l.ID + "|" + logical))
	return prefix + hex.EncodeToString(sum[:4])
}

// LabsRunDir holds every lab's reboot-scoped scratch: VM dirs, dnsmasq.
const LabsRunDir = "/var/run/plasma/labs"

// LabsRuntimeDir holds every lab's disks.
const LabsRuntimeDir = "/opt/plasma/runtime/labs"

func (l LabNet) vmDir(id string) string {
	if l.legacy() {
		return filepath.Join(VMsDir, id)
	}
	return filepath.Join(LabsRunDir, l.ID, "vms", id)
}

func (l LabNet) diskDir() string {
	if l.legacy() {
		return RuntimeDir
	}
	return filepath.Join(LabsRuntimeDir, l.ID)
}

func (l LabNet) dnsmasqHostsDir() string {
	if l.legacy() {
		return DnsmasqHostsDir
	}
	return filepath.Join(LabsRunDir, l.ID, "dnsmasq")
}

func (l LabNet) dnsmasqPidFile() string {
	if l.legacy() {
		return DnsmasqPidFile
	}
	return filepath.Join(LabsRunDir, l.ID, "dnsmasq.pid")
}

// ConsoleLogPath is the file behind a VM's virtio console (hvc0). The guest
// bootstrap writes ReadyMarker there once its applications are started.
func (l LabNet) ConsoleLogPath(id string) string {
	return filepath.Join(l.vmDir(id), "console.log")
}

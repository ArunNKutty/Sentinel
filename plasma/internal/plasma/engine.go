package plasma

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// EngineInfo is GET /v1/engine: what a remote client needs to know before it
// builds for, and brings a lab up on, this engine.
type EngineInfo struct {
	Version string `json:"version"`
	// Arch is the engine host's, and so the guests', architecture.
	Arch string `json:"arch"`
	// GuestKernel is the release every guest boots (uname -r in the VM). A
	// client's kernel modules must be built for exactly this.
	GuestKernel string `json:"guest_kernel"`
	// KernelHeaders is the Debian package carrying that kernel's build tree.
	KernelHeaders string `json:"kernel_headers,omitempty"`
	// KbuildImage is a container image whose compiler matches the one the
	// guest kernel was built with; kbuild refuses any other.
	KbuildImage string `json:"kbuild_image,omitempty"`
	BaseImage   string `json:"base_image"`
	// MgmtSubnets is the space lab management networks are carved from.
	MgmtSubnets string        `json:"mgmt_subnets"`
	Capacity    Capacity      `json:"capacity"`
	Artifacts   ArtifactUsage `json:"artifacts"`
}

type Capacity struct {
	CPUs int `json:"cpus"`
	// Memory is in bytes. Free is what a new lab may still claim: total, less
	// the host's reserve, less what running VMs were given. VMs are counted at
	// their configured size because that is what they can grow to.
	MemoryTotal     int64 `json:"memory_total"`
	MemoryAvailable int64 `json:"memory_available"`
	MemoryReserved  int64 `json:"memory_reserved"`
	MemoryCommitted int64 `json:"memory_committed"`
	MemoryFree      int64 `json:"memory_free"`
	Labs            int   `json:"labs"`
	MaxLabs         int   `json:"max_labs"`
	VMs             int   `json:"vms"`
	VMsRunning      int   `json:"vms_running"`
}

type ArtifactUsage struct {
	Count int   `json:"count"`
	Bytes int64 `json:"bytes"`
}

// Version is the plasma build; set with -ldflags "-X ...plasma.Version=...".
var Version = "dev"

// KbuildImageFor suggests the container a module for this kernel must be
// built in: Debian kernels carry their release in the version, and kbuild
// demands the exact compiler that release shipped with.
func KbuildImageFor(release string) string {
	for tag, image := range map[string]string{
		"+deb12": "debian:bookworm",
		"+deb13": "debian:trixie",
		"+deb14": "debian:forky",
	} {
		if strings.Contains(release, tag) {
			return image
		}
	}
	return ""
}

// readMeminfo returns MemTotal and MemAvailable in bytes.
func readMeminfo() (total, avail int64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, _ := strconv.ParseInt(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = kb << 10
		case "MemAvailable:":
			avail = kb << 10
		}
	}
	return total, avail
}

// hostReserve is memory never handed to VMs: 10% of the host, at least 2 GiB.
func hostReserve(total int64) int64 {
	return max(total/10, 2<<30)
}

// ComputeCapacity sums what the engine holds now.
func ComputeCapacity(vms []VmRecord, labs int) Capacity {
	total, avail := readMeminfo()
	c := Capacity{
		CPUs: runtime.NumCPU(), MemoryTotal: total, MemoryAvailable: avail,
		MemoryReserved: hostReserve(total), Labs: labs, MaxLabs: MaxLabs, VMs: len(vms),
	}
	for _, v := range vms {
		if v.Status != StatusRunning {
			continue
		}
		c.VMsRunning++
		if b, err := ParseMemory(v.Memory); err == nil {
			c.MemoryCommitted += b
		}
	}
	c.MemoryFree = max(c.MemoryTotal-c.MemoryReserved-c.MemoryCommitted, 0)
	return c
}

// ParseMemory reads Cloud Hypervisor's size syntax: 1024M, 2G, 512K, or
// bytes. Empty is the launcher default.
func ParseMemory(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		s = defaultMemory
	}
	mult := int64(1)
	switch strings.ToUpper(s[len(s)-1:]) {
	case "K":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	}
	if mult > 1 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("memory %q: want a size such as 1024M or 2G", s)
	}
	return n * mult, nil
}

// KernelRelease is the guest kernel's release string. A `<kernel>.release`
// file beside it wins: kernel-swap.sh writes one, and an arm64 Image has no
// bzImage header to read the release from.
func KernelRelease(path string) string {
	if b, err := os.ReadFile(path + ".release"); err == nil {
		if r := strings.TrimSpace(string(b)); r != "" {
			return r
		}
	}
	return bzImageRelease(path)
}

// bzImageRelease reads the release string out of a bzImage. The setup header
// stores a pointer to it at offset 0x20E, relative to 0x200 — scanning for
// something version-shaped instead would happily return the Debian package
// version (6.18.10-1) rather than the release (6.18.10+deb14-cloud-amd64),
// and the release is what a module's vermagic must match.
func bzImageRelease(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var ptr [2]byte
	if _, err := f.ReadAt(ptr[:], 0x20E); err != nil {
		return "installed"
	}
	off := int64(ptr[0]) | int64(ptr[1])<<8
	if off == 0 {
		return "installed"
	}

	buf := make([]byte, 128)
	n, err := f.ReadAt(buf, off+0x200)
	if n == 0 && err != nil {
		return "installed"
	}
	// The string is NUL-terminated and followed by the build details.
	s, _, _ := strings.Cut(string(buf[:n]), "\x00")
	if s = strings.TrimSpace(s); s == "" {
		return "installed"
	}
	// Keep just the release, dropping " (builder@host) #1 SMP ...".
	release, _, _ := strings.Cut(s, " ")
	return release
}

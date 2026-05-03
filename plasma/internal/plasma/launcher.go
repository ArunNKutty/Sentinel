package plasma

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func Launch(group string, state *State) (VmRecord, error) {
	id := state.NextID(group)

	vmDir := filepath.Join(VMsDir, id)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return VmRecord{}, fmt.Errorf("vm dir: %w", err)
	}
	if err := os.MkdirAll(RuntimeDir, 0o755); err != nil {
		return VmRecord{}, fmt.Errorf("runtime dir: %w", err)
	}

	disk := filepath.Join(RuntimeDir, id+".raw")
	if err := createDisk(disk); err != nil {
		return VmRecord{}, err
	}

	apiSock := filepath.Join(vmDir, "ch.sock")
	logPath := filepath.Join(vmDir, "ch.log")

	idx := len(state.List()) + 2
	ip := fmt.Sprintf("10.0.0.%d", idx)
	mac := fmt.Sprintf("02:00:0a:00:00:%02x", idx)
	tapDev := fmt.Sprintf("tap-%s", id)
	if len(tapDev) > 15 {
		tapDev = tapDev[:15]
	}

	if err := setupNetwork(tapDev, mac, ip); err != nil {
		return VmRecord{}, fmt.Errorf("network: %w", err)
	}

	pid, err := startCloudHypervisor(apiSock, disk, logPath, tapDev, mac)
	if err != nil {
		return VmRecord{}, err
	}

	rec := VmRecord{
		ID:        id,
		Group:     group,
		Status:    StatusRunning,
		PID:       pid,
		APISocket: apiSock,
		DiskPath:  disk,
		MAC:       mac,
		IP:        ip,
		StartedAt: time.Now().UTC(),
	}
	if err := state.Add(rec); err != nil {
		return VmRecord{}, fmt.Errorf("persist state: %w", err)
	}
	return rec, nil
}

func createDisk(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("disk already exists: %s", path)
	}
	cmd := exec.Command("cp", "--sparse=always", BaseRootfs, path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cp rootfs: %w: %s", err, out)
	}
	return nil
}

func startCloudHypervisor(apiSock, disk, logPath, tapDev, mac string) (int, error) {
	logF, err := os.Create(logPath)
	if err != nil {
		return 0, fmt.Errorf("open log: %w", err)
	}

	netArg := fmt.Sprintf("tap=%s,mac=%s", tapDev, mac)

	cmd := exec.Command(
		"cloud-hypervisor",
		"--api-socket", apiSock,
		"--kernel", KernelPath,
		"--initramfs", InitrdPath,
		"--cmdline", GuestCmdline,
		"--disk", "path="+disk+",image_type=raw",
		"--net", netArg,
		"--cpus", "boot=2",
		"--memory", "size=1024M",
		"--serial", "off",
		"--console", "off",
	)
	cmd.Stdout = logF
	cmd.Stderr = logF
	// Setsid so the VM survives plasmad restart.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		logF.Close()
		return 0, fmt.Errorf("start cloud-hypervisor: %w", err)
	}
	go func() {
		cmd.Wait()
		logF.Close()
	}()
	return cmd.Process.Pid, nil
}

func setupNetwork(tapDev, mac, ip string) error {
	br := "plasma-br0"

	// Create bridge
	exec.Command("ip", "link", "add", "name", br, "type", "bridge").Run()
	exec.Command("ip", "addr", "add", "10.0.0.1/24", "dev", br).Run()
	exec.Command("ip", "link", "set", br, "up").Run()

	// Create tap
	exec.Command("ip", "tuntap", "add", "name", tapDev, "mode", "tap").Run()
	exec.Command("ip", "link", "set", tapDev, "master", br).Run()
	exec.Command("ip", "link", "set", tapDev, "up").Run()

	// Setup dnsmasq hosts dir
	hostsDir := "/var/run/plasma/dnsmasq"
	os.MkdirAll(hostsDir, 0o755)

	// Write host entry
	hostFile := filepath.Join(hostsDir, tapDev)
	os.WriteFile(hostFile, []byte(fmt.Sprintf("%s,%s\n", mac, ip)), 0o644)

	// Ensure dnsmasq running
	if err := exec.Command("pgrep", "-f", "dnsmasq.*"+br).Run(); err != nil {
		exec.Command("dnsmasq",
			"--interface="+br,
			"--bind-interfaces",
			"--except-interface=lo",
			"--dhcp-range=10.0.0.2,10.0.0.254,24h",
			"--dhcp-hostsdir="+hostsDir,
			"--pid-file=/var/run/plasma-dnsmasq.pid",
		).Run()
	} else {
		// SIGHUP dnsmasq to reload hostsdir
		pidData, _ := os.ReadFile("/var/run/plasma-dnsmasq.pid")
		if string(pidData) != "" {
			exec.Command("kill", "-HUP", string(pidData)).Run()
		} else {
			exec.Command("pkill", "-HUP", "-f", "dnsmasq.*"+br).Run()
		}
	}

	return nil
}

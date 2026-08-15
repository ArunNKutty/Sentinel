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
	id, ip, mac, err := state.Allocate(group)
	if err != nil {
		return VmRecord{}, err
	}

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

	tapDev := TapDeviceName(id)

	if err := setupNetwork(tapDev, mac, ip); err != nil {
		return VmRecord{}, fmt.Errorf("network: %w", err)
	}

	consoleSock := ConsoleSocketPath(id)
	pid, err := startCloudHypervisor(apiSock, consoleSock, disk, logPath, tapDev, mac)
	if err != nil {
		return VmRecord{}, err
	}
	shareConsoleSocket(consoleSock)

	rec := VmRecord{
		ID:            id,
		Group:         group,
		Status:        StatusRunning,
		PID:           pid,
		APISocket:     apiSock,
		ConsoleSocket: consoleSock,
		DiskPath:      disk,
		MAC:           mac,
		IP:            ip,
		TapDevice:     tapDev,
		StartedAt:     time.Now().UTC(),
	}
	if err := state.Add(rec); err != nil {
		return VmRecord{}, fmt.Errorf("persist state: %w", err)
	}
	return rec, nil
}

// OrphanDisks returns runtime disks that no record in state claims. They are
// left from a run whose state was lost; the daemon reports them but never
// deletes them, since only the operator knows whether they still matter.
func OrphanDisks(state *State) ([]string, error) {
	entries, err := os.ReadDir(RuntimeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	claimed := make(map[string]bool)
	for _, v := range state.List() {
		claimed[v.DiskPath] = true
	}
	var orphans []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".raw" {
			continue
		}
		path := filepath.Join(RuntimeDir, e.Name())
		if !claimed[path] {
			orphans = append(orphans, path)
		}
	}
	return orphans, nil
}

func createDisk(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("disk already exists: %s (orphan from a lost run; remove it to reuse the ID)", path)
	}
	cmd := exec.Command("cp", "--sparse=always", BaseRootfs, path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cp rootfs: %w: %s", err, out)
	}
	return nil
}

func startCloudHypervisor(apiSock, consoleSock, disk, logPath, tapDev, mac string) (int, error) {
	logF, err := os.Create(logPath)
	if err != nil {
		return 0, fmt.Errorf("open log: %w", err)
	}

	netArg := fmt.Sprintf("tap=%s,mac=%s", tapDev, mac)

	disks := []string{"path=" + disk + ",image_type=raw"}
	// The artifacts disk is shared read-only across the fleet, so many VMs can
	// hold it open at once. Only attach it when it exists — a fleet with no
	// applications built must still launch.
	if _, err := os.Stat(ArtifactsDisk); err == nil {
		disks = append(disks, "path="+ArtifactsDisk+",image_type=raw,readonly=on")
	}

	args := []string{
		"--api-socket", apiSock,
		"--kernel", KernelPath,
		"--initramfs", InitrdPath,
		"--cmdline", GuestCmdline,
		"--disk",
	}
	args = append(args, disks...)
	args = append(args,
		"--net", netArg,
		"--cpus", "boot=2",
		"--memory", "size=1024M",
		"--serial", "socket="+consoleSock,
		"--console", "off",
	)

	cmd := exec.Command("cloud-hypervisor", args...)
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

// shareConsoleSocket waits for cloud-hypervisor to create the serial socket
// (it binds this at VMM init, before the guest kernel boots, so this is
// normally sub-100ms) and opens it up to SocketGroup so the CLI can dial it
// without sudo, same as the control socket. Best-effort: plasmad runs the VM
// either way, `plasma vm shell` just fails with a clear dial error if this
// never lands.
func shareConsoleSocket(path string) {
	for i := 0; i < 40; i++ {
		if _, err := os.Stat(path); err == nil {
			os.Chmod(path, 0o660)
			chownSocketGroup(path)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func setupNetwork(tapDev, mac, ip string) error {
	// Create bridge
	exec.Command("ip", "link", "add", "name", Bridge, "type", "bridge").Run()
	exec.Command("ip", "addr", "add", "10.0.0.1/24", "dev", Bridge).Run()
	exec.Command("ip", "link", "set", Bridge, "up").Run()

	// Create tap
	exec.Command("ip", "tuntap", "add", "name", tapDev, "mode", "tap").Run()
	exec.Command("ip", "link", "set", tapDev, "master", Bridge).Run()
	exec.Command("ip", "link", "set", tapDev, "up").Run()

	// Setup dnsmasq hosts dir
	os.MkdirAll(DnsmasqHostsDir, 0o755)

	// Write host entry
	hostFile := filepath.Join(DnsmasqHostsDir, tapDev)
	os.WriteFile(hostFile, []byte(fmt.Sprintf("%s,%s\n", mac, ip)), 0o644)

	// Ensure dnsmasq running
	if err := exec.Command("pgrep", "-f", "dnsmasq.*"+Bridge).Run(); err != nil {
		exec.Command("dnsmasq",
			"--interface="+Bridge,
			"--bind-interfaces",
			"--except-interface=lo",
			"--dhcp-range=10.0.0.2,10.0.0.254,24h",
			"--dhcp-hostsdir="+DnsmasqHostsDir,
			"--pid-file="+DnsmasqPidFile,
		).Run()
	} else {
		reloadDnsmasq()
	}

	return nil
}

// reloadDnsmasq SIGHUPs the running dnsmasq so it picks up a hosts-dir change
// (a new VM added, or one removed by Delete). No-op if dnsmasq isn't up.
func reloadDnsmasq() {
	pidData, _ := os.ReadFile(DnsmasqPidFile)
	if string(pidData) != "" {
		exec.Command("kill", "-HUP", string(pidData)).Run()
	} else {
		exec.Command("pkill", "-HUP", "-f", "dnsmasq.*"+Bridge).Run()
	}
}

// Delete stops the VM's cloud-hypervisor process, frees its network
// resources, removes its runtime files and disk, and drops it from state.
// Best-effort past the state lookup: an already-dead process or a network
// resource that never came up won't block cleanup.
func Delete(id string, state *State) (VmRecord, error) {
	rec, ok, err := state.Remove(id)
	if err != nil {
		return VmRecord{}, fmt.Errorf("persist state: %w", err)
	}
	if !ok {
		return VmRecord{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	stopProcess(rec.PID)
	teardownNetwork(rec.TapDevice)
	os.RemoveAll(filepath.Join(VMsDir, rec.ID))
	os.Remove(rec.DiskPath)

	return rec, nil
}

// stopProcess sends SIGTERM and waits up to 2s for exit before escalating to
// SIGKILL. No-op if the PID is already gone.
func stopProcess(pid int) {
	if pid <= 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if proc.Signal(syscall.SIGTERM) != nil {
		return
	}
	for i := 0; i < 20; i++ {
		if !processAlive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	proc.Signal(syscall.SIGKILL)
}

func teardownNetwork(tapDev string) {
	if tapDev == "" {
		return
	}
	exec.Command("ip", "link", "del", tapDev).Run()

	hostFile := filepath.Join(DnsmasqHostsDir, tapDev)
	if os.Remove(hostFile) == nil {
		reloadDnsmasq()
	}
}

package plasma

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// LaunchOpts are per-VM overrides; zero values take the launcher defaults.
type LaunchOpts struct {
	CPUs   int
	Memory string
	// Apps are deployed on a per-VM disk built at launch (see apps.go).
	Apps []AppSpec
}

const (
	defaultCPUs   = 2
	defaultMemory = "1024M"
)

func (o LaunchOpts) cpus() int {
	if o.CPUs > 0 {
		return o.CPUs
	}
	return defaultCPUs
}

func (o LaunchOpts) memory() string {
	if o.Memory != "" {
		return o.Memory
	}
	return defaultMemory
}

// Launch adds a VM to a group, letting the daemon pick the next <group>-N.
func Launch(group string, state *State) (VmRecord, error) {
	id, ip, mac, err := state.Allocate(group)
	if err != nil {
		return VmRecord{}, err
	}
	return launch(id, group, ip, mac, LaunchOpts{}, state)
}

// LaunchNamed starts a VM under a caller-chosen ID, as topology files require.
func LaunchNamed(id, group string, opts LaunchOpts, state *State) (VmRecord, error) {
	ip, mac, err := state.AllocateNamed(id)
	if err != nil {
		return VmRecord{}, err
	}
	return launch(id, group, ip, mac, opts, state)
}

func launch(id, group, ip, mac string, opts LaunchOpts, state *State) (VmRecord, error) {
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

	appsDisk, err := buildAppsDisk(id, opts.Apps)
	if err != nil {
		os.Remove(disk)
		return VmRecord{}, fmt.Errorf("applications: %w", err)
	}

	apiSock := filepath.Join(vmDir, "ch.sock")
	logPath := filepath.Join(vmDir, "ch.log")

	tapDev := TapDeviceName(id)

	if err := setupNetwork(tapDev, mac, ip); err != nil {
		return VmRecord{}, fmt.Errorf("network: %w", err)
	}

	pid, err := startCloudHypervisor(apiSock, disk, appsDisk, logPath, tapDev, mac, opts)
	if err != nil {
		return VmRecord{}, err
	}
	consolePTY := shareConsolePTY(apiSock)

	rec := VmRecord{
		ID:         id,
		Group:      group,
		Status:     StatusRunning,
		PID:        pid,
		APISocket:  apiSock,
		ConsolePTY: consolePTY,
		DiskPath:   disk,
		MAC:        mac,
		IP:         ip,
		TapDevice:  tapDev,
		CPUs:       opts.cpus(),
		Memory:     opts.memory(),
		AppsDisk:   appsDisk,
		Apps:       opts.Apps,
		StartedAt:  time.Now().UTC(),
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

func startCloudHypervisor(apiSock, disk, appsDisk, logPath, tapDev, mac string, opts LaunchOpts) (int, error) {
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
	// The applications disk is this VM's alone, but nothing in the guest ever
	// writes to it, so read-only keeps the payloads exactly as packed.
	if appsDisk != "" {
		disks = append(disks, "path="+appsDisk+",image_type=raw,readonly=on")
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
		"--cpus", fmt.Sprintf("boot=%d", opts.cpus()),
		"--memory", "size="+opts.memory(),
		// A pty, not socket=: cloud-hypervisor never reaps a disconnected
		// serial socket client, so after one `vm shell` detaches the console
		// is dead for every later client. A pty can be reopened repeatedly.
		"--serial", "pty",
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

// shareConsolePTY asks cloud-hypervisor which pty it allocated for the guest
// serial line and opens it up to SocketGroup so the CLI can attach without
// sudo, same as the control socket.
//
// The pty path is only discoverable through the VMM's API (it is not logged),
// and the API takes a moment to come up, hence the retry. Best-effort: the VM
// runs either way and `plasma vm shell` reports a clear error if this is empty.
func shareConsolePTY(apiSock string) string {
	for i := 0; i < 60; i++ {
		if pty, err := queryConsolePTY(apiSock); err == nil && pty != "" {
			// Root created it; hand access to the plasma group.
			os.Chmod(pty, 0o660)
			chownSocketGroup(pty)
			return pty
		}
		time.Sleep(100 * time.Millisecond)
	}
	return ""
}

// queryConsolePTY reads the serial device path out of cloud-hypervisor's
// vm.info over its API socket.
func queryConsolePTY(apiSock string) (string, error) {
	c := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", apiSock)
			},
		},
		Timeout: 2 * time.Second,
	}
	resp, err := c.Get("http://localhost/api/v1/vm.info")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var info struct {
		Config struct {
			Serial struct {
				Mode string `json:"mode"`
				File string `json:"file"`
			} `json:"serial"`
		} `json:"config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", err
	}
	if info.Config.Serial.Mode != "Pty" {
		return "", fmt.Errorf("serial mode is %q, not Pty", info.Config.Serial.Mode)
	}
	return info.Config.Serial.File, nil
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
	if rec.AppsDisk != "" {
		os.Remove(rec.AppsDisk)
	}

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

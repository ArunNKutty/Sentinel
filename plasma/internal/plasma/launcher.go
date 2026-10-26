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
	"strconv"
	"strings"
	"syscall"
	"time"
)

// LaunchOpts are per-VM overrides; zero values take the launcher defaults.
type LaunchOpts struct {
	CPUs   int
	Memory string
	// Apps are deployed on a per-VM disk built at launch (see apps.go).
	Apps []AppSpec
	// Nics are link NICs beyond eth0 (see nics.go).
	Nics []NicSpec
	// Net is the lab the VM belongs to; the zero value is the legacy fleet.
	Net LabNet
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
	ip, mac, err := state.AllocateNamed(opts.Net, id)
	if err != nil {
		return VmRecord{}, err
	}
	return launch(id, group, ip, mac, opts, state)
}

func launch(id, group, ip, mac string, opts LaunchOpts, state *State) (rec VmRecord, err error) {
	net := opts.Net
	if err := ValidateNics(opts.Nics); err != nil {
		return VmRecord{}, err
	}
	nics, err := planNics(net, ip, opts.Nics)
	if err != nil {
		return VmRecord{}, err
	}
	if err := addressConflict(nics, state.List()); err != nil {
		return VmRecord{}, err
	}
	octet, err := ipOctet(ip)
	if err != nil {
		return VmRecord{}, err
	}

	vmDir := net.vmDir(id)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return VmRecord{}, fmt.Errorf("vm dir: %w", err)
	}
	if err := os.MkdirAll(net.diskDir(), 0o755); err != nil {
		return VmRecord{}, fmt.Errorf("runtime dir: %w", err)
	}

	// Undo whatever was created if a later step fails, so a failed launch
	// leaves no disk, tap or VM dir behind to block a retry under the same ID.
	var undo []func()
	defer func() {
		if err != nil {
			for i := len(undo) - 1; i >= 0; i-- {
				undo[i]()
			}
		}
	}()
	undo = append(undo, func() { os.RemoveAll(vmDir) })

	disk := filepath.Join(net.diskDir(), id+".raw")
	if err := createDisk(disk); err != nil {
		return VmRecord{}, err
	}
	undo = append(undo, func() { os.Remove(disk) })

	appsDisk, err := buildAppsDisk(filepath.Join(net.diskDir(), id+"-apps.img"), opts.Apps)
	if err != nil {
		return VmRecord{}, fmt.Errorf("applications: %w", err)
	}
	if appsDisk != "" {
		undo = append(undo, func() { os.Remove(appsDisk) })
	}

	apiSock := filepath.Join(vmDir, "ch.sock")
	logPath := filepath.Join(vmDir, "ch.log")
	tapDev := net.MgmtTap(id, octet)

	// Teardown is best-effort, so it is registered before setup: a setup that
	// fails halfway still has its half undone.
	undo = append(undo, func() { teardownNetwork(net, tapDev) })
	if err := setupNetwork(net, tapDev, mac, ip); err != nil {
		return VmRecord{}, fmt.Errorf("network: %w", err)
	}
	undo = append(undo, func() { teardownNics(nics) })
	if err := setupNics(nics); err != nil {
		return VmRecord{}, fmt.Errorf("link nics: %w", err)
	}

	pid, err := startCloudHypervisor(id, apiSock, disk, appsDisk, logPath, net.ConsoleLogPath(id), tapDev, mac, nics, opts)
	if err != nil {
		return VmRecord{}, err
	}
	undo = append(undo, func() { stopProcess(pid) })
	consolePTY := shareConsolePTY(apiSock)

	rec = VmRecord{
		Lab:        net.ID,
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
		Nics:       nics,
		StartedAt:  time.Now().UTC(),
	}
	if err := state.Add(rec); err != nil {
		return VmRecord{}, fmt.Errorf("persist state: %w", err)
	}
	return rec, nil
}

// OrphanDisks returns runtime disks that no record in state claims, in the
// legacy runtime dir and in every lab's. They are left from a run whose state
// was lost; the daemon reports them but never deletes them, since only the
// operator knows whether they still matter.
func OrphanDisks(state *State) ([]string, error) {
	claimed := make(map[string]bool)
	for _, v := range state.List() {
		claimed[v.DiskPath] = true
	}
	dirs := []string{RuntimeDir}
	if labs, err := os.ReadDir(LabsRuntimeDir); err == nil {
		for _, l := range labs {
			if l.IsDir() {
				dirs = append(dirs, filepath.Join(LabsRuntimeDir, l.Name()))
			}
		}
	}
	var orphans []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".raw" {
				continue
			}
			path := filepath.Join(dir, e.Name())
			if !claimed[path] {
				orphans = append(orphans, path)
			}
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

func startCloudHypervisor(id, apiSock, disk, appsDisk, logPath, consoleLog, tapDev, mac string, nics []NicRecord, opts LaunchOpts) (int, error) {
	logF, err := os.Create(logPath)
	if err != nil {
		return 0, fmt.Errorf("open log: %w", err)
	}

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

	// A fresh file per boot, so a ready marker from an earlier VM of the same
	// name can never be mistaken for this one's.
	os.Remove(consoleLog)
	args := append(chArgs(id, apiSock, disks, tapDev, mac, nics, opts), "--console", "file="+consoleLog)
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

// chArgs is cloud-hypervisor's command line. The management NIC is listed
// first and link NICs follow in order: the guest names NICs in the order they
// are attached, which is what makes eth0 the management NIC and a declared
// eth2 the guest's eth2.
func chArgs(id, apiSock string, disks []string, tapDev, mac string, nics []NicRecord, opts LaunchOpts) []string {
	nets := []string{fmt.Sprintf("tap=%s,mac=%s", tapDev, mac)}
	for _, n := range nics {
		nets = append(nets, fmt.Sprintf("tap=%s,mac=%s", n.Tap, n.MAC))
	}

	cmdline := GuestCmdline
	if len(nics) > 0 {
		cmdline += " " + linkNetworkCredential(nics) + " " + linkInfoCredential(id, nics)
	}

	args := []string{
		"--api-socket", apiSock,
		"--kernel", KernelPath,
		"--initramfs", InitrdPath,
		"--cmdline", cmdline,
		"--disk",
	}
	args = append(args, disks...)
	args = append(args, "--net")
	args = append(args, nets...)
	return append(args,
		"--cpus", fmt.Sprintf("boot=%d", opts.cpus()),
		"--memory", "size="+opts.memory(),
		// A pty, not socket=: cloud-hypervisor never reaps a disconnected
		// serial socket client, so after one `vm shell` detaches the console
		// is dead for every later client. A pty can be reopened repeatedly.
		"--serial", "pty",
	)
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

// setupNetwork gives a VM its management tap on the lab's bridge and a DHCP
// reservation, bringing the bridge and its dnsmasq up on first use.
func setupNetwork(net LabNet, tapDev, mac, ip string) error {
	if err := ensureMgmtNetwork(net); err != nil {
		return err
	}
	br := net.MgmtBridge()
	if out, err := exec.Command("ip", "tuntap", "add", "name", tapDev, "mode", "tap").CombinedOutput(); err != nil {
		return fmt.Errorf("create tap %s: %v: %s", tapDev, err, out)
	}
	if out, err := exec.Command("ip", "link", "set", tapDev, "master", br).CombinedOutput(); err != nil {
		return fmt.Errorf("attach %s to %s: %v: %s", tapDev, br, err, out)
	}
	if out, err := exec.Command("ip", "link", "set", tapDev, "up").CombinedOutput(); err != nil {
		return fmt.Errorf("up %s: %v: %s", tapDev, err, out)
	}
	hostFile := filepath.Join(net.dnsmasqHostsDir(), tapDev)
	if err := os.WriteFile(hostFile, []byte(fmt.Sprintf("%s,%s\n", mac, ip)), 0o644); err != nil {
		return fmt.Errorf("dhcp reservation: %w", err)
	}
	reloadDnsmasq(net)
	return nil
}

// ensureMgmtNetwork brings up a lab's management bridge, its gateway address
// and a dnsmasq serving only that bridge. Idempotent.
func ensureMgmtNetwork(net LabNet) error {
	br := net.MgmtBridge()
	if _, err := os.Stat(filepath.Join("/sys/class/net", br)); err != nil {
		if out, err := exec.Command("ip", "link", "add", "name", br, "type", "bridge").CombinedOutput(); err != nil {
			return fmt.Errorf("create bridge %s: %v: %s", br, err, out)
		}
	}
	// Fails harmlessly when the address is already there.
	exec.Command("ip", "addr", "add", net.Gateway()+"/24", "dev", br).Run()
	if out, err := exec.Command("ip", "link", "set", br, "up").CombinedOutput(); err != nil {
		return fmt.Errorf("up bridge %s: %v: %s", br, err, out)
	}
	if err := os.MkdirAll(net.dnsmasqHostsDir(), 0o755); err != nil {
		return err
	}
	if dnsmasqRunning(net) {
		return nil
	}
	out, err := exec.Command("dnsmasq",
		"--interface="+br,
		"--bind-interfaces",
		"--except-interface=lo",
		fmt.Sprintf("--dhcp-range=10.0.%d.2,10.0.%d.254,24h", net.Index, net.Index),
		"--dhcp-hostsdir="+net.dnsmasqHostsDir(),
		"--pid-file="+net.dnsmasqPidFile(),
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("start dnsmasq on %s: %v: %s", br, err, out)
	}
	return nil
}

func dnsmasqPID(net LabNet) int {
	data, _ := os.ReadFile(net.dnsmasqPidFile())
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}

func dnsmasqRunning(net LabNet) bool {
	if processAlive(dnsmasqPID(net)) {
		return true
	}
	// A legacy dnsmasq may predate the pid file.
	return net.legacy() && exec.Command("pgrep", "-f", "dnsmasq.*"+Bridge).Run() == nil
}

// reloadDnsmasq SIGHUPs the lab's dnsmasq so it picks up a hosts-dir change
// (a new VM added, or one removed by Delete). No-op if dnsmasq isn't up.
func reloadDnsmasq(net LabNet) {
	if pid := dnsmasqPID(net); processAlive(pid) {
		syscall.Kill(pid, syscall.SIGHUP)
	} else if net.legacy() {
		exec.Command("pkill", "-HUP", "-f", "dnsmasq.*"+Bridge).Run()
	}
}

// teardownMgmtNetwork stops a lab's dnsmasq and removes its bridge. The
// legacy plasma-br0 is left to `plasma exit`, as before.
func teardownMgmtNetwork(net LabNet) {
	if net.legacy() {
		return
	}
	if pid := dnsmasqPID(net); processAlive(pid) {
		stopProcess(pid)
	}
	exec.Command("ip", "link", "del", net.MgmtBridge()).Run()
	os.RemoveAll(filepath.Join(LabsRunDir, net.ID))
}

// Delete stops the VM's cloud-hypervisor process, frees its network
// resources, removes its runtime files and disk, and drops it from state.
// Best-effort past the state lookup: an already-dead process or a network
// resource that never came up won't block cleanup.
func Delete(net LabNet, id string, state *State) (VmRecord, error) {
	rec, ok, err := state.Remove(net.ID, id)
	if err != nil {
		return VmRecord{}, fmt.Errorf("persist state: %w", err)
	}
	if !ok {
		return VmRecord{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	stopProcess(rec.PID)
	teardownNetwork(net, rec.TapDevice)
	teardownNics(rec.Nics)
	os.RemoveAll(net.vmDir(rec.ID))
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

func teardownNetwork(net LabNet, tapDev string) {
	if tapDev == "" {
		return
	}
	exec.Command("ip", "link", "del", tapDev).Run()

	hostFile := filepath.Join(net.dnsmasqHostsDir(), tapDev)
	if os.Remove(hostFile) == nil {
		reloadDnsmasq(net)
	}
}

// planNics names the taps and MACs for a VM's link NICs. Pure, so a launch
// can fail on a bad request before anything is created on the host.
//
// In a lab, the topology's logical names become lab-scoped host names: a
// bridge is hashed with the lab ID, and a host endpoint gets a generated tap
// instead of the global name it asked for (two labs could both ask for it,
// and nobody on the engine host uses it by name). Segment keeps the logical
// name, which is what the guest is told and what captures accept.
func planNics(net LabNet, ip string, specs []NicSpec) ([]NicRecord, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	octet, err := ipOctet(ip)
	if err != nil {
		return nil, err
	}
	out := make([]NicRecord, 0, len(specs))
	for _, n := range specs {
		idx, _ := nicIndex(n.Name) // ValidateNics has vouched for the name
		rec := NicRecord{Name: n.Name, MAC: net.LinkMAC(octet, idx), Address: n.Address}
		switch {
		case net.legacy() && n.HostName != "":
			rec.Tap = n.HostName
		case n.HostName != "":
			rec.Tap = net.LinkTap(octet, n.Name)
			rec.Segment = HostEndpoint + ":" + n.HostName
		default:
			rec.Tap = net.LinkTap(octet, n.Name)
			rec.Bridge = net.HostBridge(n.Bridge)
			if rec.Bridge != n.Bridge {
				rec.Segment = n.Bridge
			}
		}
		out = append(out, rec)
	}
	return out, nil
}

// setupNics creates each link NIC's tap and, unless it is a host endpoint,
// attaches it to its link bridge, creating the bridge on first use. Unlike
// plasma-br0 these are pure L2 segments: no address, no DHCP, and IPv6 off on
// the bridge and the taps, so the host itself never puts a frame (router
// solicitations, MLD) on a link — what crosses it is only what the VMs send.
func setupNics(nics []NicRecord) error {
	for _, n := range nics {
		if n.Bridge == "" {
			// A host endpoint's name is the caller's choice; refuse to adopt
			// an interface that already exists rather than hijack it.
			if _, err := os.Stat(filepath.Join("/sys/class/net", n.Tap)); err == nil {
				return fmt.Errorf("%s: host interface %s already exists", n.Name, n.Tap)
			}
		} else if err := ensureLinkBridge(n.Bridge); err != nil {
			return err
		}
		if out, err := exec.Command("ip", "tuntap", "add", "name", n.Tap, "mode", "tap").CombinedOutput(); err != nil {
			return fmt.Errorf("%s: create tap %s: %v: %s", n.Name, n.Tap, err, out)
		}
		disableIPv6(n.Tap)
		if n.Bridge != "" {
			if out, err := exec.Command("ip", "link", "set", n.Tap, "master", n.Bridge).CombinedOutput(); err != nil {
				return fmt.Errorf("%s: attach %s to %s: %v: %s", n.Name, n.Tap, n.Bridge, err, out)
			}
		}
		if out, err := exec.Command("ip", "link", "set", n.Tap, "up").CombinedOutput(); err != nil {
			return fmt.Errorf("%s: up %s: %v: %s", n.Name, n.Tap, err, out)
		}
	}
	return nil
}

// ensureLinkBridge creates a link bridge if it is not already there; the
// second VM on a segment finds the first one's.
func ensureLinkBridge(br string) error {
	if _, err := os.Stat(filepath.Join("/sys/class/net", br)); err == nil {
		return nil
	}
	if out, err := exec.Command("ip", "link", "add", "name", br, "type", "bridge",
		"stp_state", "0", "mcast_snooping", "0").CombinedOutput(); err != nil {
		return fmt.Errorf("create bridge %s: %v: %s", br, err, out)
	}
	disableIPv6(br)
	if out, err := exec.Command("ip", "link", "set", br, "up").CombinedOutput(); err != nil {
		return fmt.Errorf("up bridge %s: %v: %s", br, err, out)
	}
	return nil
}

// disableIPv6 stops the kernel giving an interface a link-local address.
// Best-effort: a host without IPv6 has nothing to disable.
func disableIPv6(iface string) {
	os.WriteFile(filepath.Join("/proc/sys/net/ipv6/conf", iface, "disable_ipv6"), []byte("1"), 0o644)
}

// teardownNics removes a VM's link taps, then any link bridge they leave
// without ports. Best-effort, like teardownNetwork.
func teardownNics(nics []NicRecord) {
	for _, n := range nics {
		exec.Command("ip", "link", "del", n.Tap).Run()
	}
	for _, n := range nics {
		if n.Bridge == "" || !isLinkBridge(n.Bridge) {
			continue
		}
		ports, err := os.ReadDir(filepath.Join("/sys/class/net", n.Bridge, "brif"))
		if err == nil && len(ports) == 0 {
			exec.Command("ip", "link", "del", n.Bridge).Run()
		}
	}
}

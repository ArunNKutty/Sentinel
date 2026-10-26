package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/fortaegis/plasma/internal/plasma"
)

// daemonLog is where a plasmad started by `init` writes, when it is not under
// systemd (which would capture output in the journal instead).
const daemonLog = "/var/run/plasma/plasmad.log"

// Two separate things stop plasma working from a fresh shell, and they need
// different fixes:
//
//   - plasmad is not running          -> start it (needs root)
//   - the shell is not in the plasma  -> cannot be fixed in place. Group
//     group                              membership is fixed when a process
//     starts, and a child can never add a group to its parent shell, so the
//     only way out is to exec a new shell that has it.
//
// `plasma init` checks both, fixes the first, and hands you a session with the
// second.

type checkState int

const (
	checkOK checkState = iota
	checkWarn
	checkFail
)

func (c checkState) marker() string {
	switch c {
	case checkOK:
		return "ok  "
	case checkWarn:
		return "warn"
	default:
		return "FAIL"
	}
}

func report(state checkState, name, detail string) {
	fmt.Printf("  [%s] %-16s %s\n", state.marker(), name, detail)
}

func initCmd() *cobra.Command {
	var (
		noShell bool
		check   bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Check the environment, start plasmad, and enter a plasma-group shell",
		Long: "Get a shell ready to drive plasma.\n\n" +
			"Verifies the host prerequisites, starts plasmad if it is not running,\n" +
			"and — because group membership cannot be changed in an existing shell —\n" +
			"drops you into a new shell carrying the 'plasma' group if yours lacks it.\n\n" +
			"Safe to re-run: it changes nothing that is already in order.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			c.SilenceUsage = true
			return runInit(check, noShell)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only report status; change nothing")
	cmd.Flags().BoolVar(&noShell, "no-shell", false, "do not start a shell even if the group is missing")
	return cmd
}

func runInit(checkOnly, noShell bool) error {
	fmt.Println("plasma init")

	problems := checkPrerequisites()

	// The daemon has to come up before the group shell, because exec'ing a
	// shell replaces this process and nothing after it would run.
	inGroup := processInGroup(plasma.SocketGroup)
	state := daemonState()

	switch {
	case state == daemonUp:
		report(checkOK, "daemon", "running on "+plasma.SocketPath)
	case state == daemonNoAccess:
		// Running fine — the group is what is missing, and that is reported
		// (and fixed) below. Starting another daemon here would be wrong.
		report(checkOK, "daemon", "running, but not reachable from this shell")
	case checkOnly:
		report(checkFail, "daemon", "not running (start it with: sudo plasmad)")
		problems++
	default:
		report(checkWarn, "daemon", "not running, starting it...")
		if err := startDaemon(); err != nil {
			report(checkFail, "daemon", err.Error())
			return fmt.Errorf("could not start plasmad: %w", err)
		}
		report(checkOK, "daemon", "started, listening on "+plasma.SocketPath)
		state = daemonUp
	}

	if inGroup {
		report(checkOK, "group", "this shell is in '"+plasma.SocketGroup+"'")
	} else if isMemberOnDisk(plasma.SocketGroup) {
		// The account is a member but this shell predates it — the usual case
		// right after `make install`.
		report(checkWarn, "group", "your account is in '"+plasma.SocketGroup+
			"' but this shell predates it")
	} else {
		report(checkFail, "group", "not a member of '"+plasma.SocketGroup+
			"' (fix: sudo usermod -aG "+plasma.SocketGroup+" $USER)")
		problems++
	}

	// Only meaningful once we can actually reach the daemon.
	if state == daemonUp && inGroup {
		reportFleet()
	}

	if checkOnly {
		if problems > 0 {
			return fmt.Errorf("%d check(s) failed", problems)
		}
		if !inGroup {
			// Everything is installed and running, but this shell still cannot
			// reach the socket, so calling it "ready" would be a lie.
			return fmt.Errorf("this shell is not in the '%s' group; "+
				"run 'plasma init' to start one that is", plasma.SocketGroup)
		}
		fmt.Println("\nready.")
		return nil
	}
	if problems > 0 {
		return fmt.Errorf("%d check(s) failed; fix the FAIL lines above", problems)
	}

	if inGroup {
		fmt.Println("\nready.")
		return nil
	}
	if noShell {
		fmt.Printf("\nRun 'newgrp %s' (or re-login) before using plasma.\n", plasma.SocketGroup)
		return nil
	}
	return enterGroupShell()
}

// checkPrerequisites reports on the host pieces plasma needs, returning how
// many are outright broken.
func checkPrerequisites() int {
	problems := 0

	for _, bin := range []string{"cloud-hypervisor", "plasmad"} {
		if path, err := exec.LookPath(bin); err == nil {
			report(checkOK, bin, path)
		} else {
			report(checkFail, bin, "not on PATH (run: sudo make install)")
			problems++
		}
	}
	if _, err := exec.LookPath("tcpdump"); err != nil {
		report(checkWarn, "tcpdump", "not installed; `plasma vm capture` will not work")
	}

	if _, err := os.Stat(plasma.BaseRootfs); err == nil {
		report(checkOK, "base image", plasma.BaseRootfs)
	} else {
		report(checkFail, "base image", plasma.BaseRootfs+" missing (run: sudo ./scripts/m1-prep.sh)")
		problems++
	}

	if v := guestKernelVersion(); v != "" {
		report(checkOK, "guest kernel", v)
	} else {
		report(checkFail, "guest kernel", plasma.KernelPath+" missing (run: sudo ./scripts/m1-prep.sh)")
		problems++
	}
	if _, err := os.Stat(plasma.InitrdPath); err != nil {
		report(checkFail, "guest initrd", plasma.InitrdPath+" missing")
		problems++
	}

	if _, err := os.Stat(plasma.ArtifactsDisk); err == nil {
		report(checkOK, "artifacts", plasma.ArtifactsDisk)
	} else {
		report(checkWarn, "artifacts", "none; VMs boot without /opt/ne (build: cd applications && make)")
	}

	// The bridge is created on first launch, so its absence is normal.
	if _, err := os.Stat("/sys/class/net/" + plasma.Bridge); err == nil {
		report(checkOK, "bridge", plasma.Bridge)
	} else {
		report(checkOK, "bridge", plasma.Bridge+" will be created on first launch")
	}
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); err == nil &&
		strings.TrimSpace(string(b)) != "1" {
		report(checkWarn, "ip_forward", "disabled; VMs cannot reach the outside network "+
			"(run: sudo ./scripts/net-host.sh)")
	}

	return problems
}

func guestKernelVersion() string { return plasma.KernelRelease(plasma.KernelPath) }

func reportFleet() {
	vms, err := plasma.NewClient(plasma.SocketPath).List()
	if err != nil {
		return
	}
	running := 0
	for _, v := range vms {
		if v.Status == plasma.StatusRunning {
			running++
		}
	}
	report(checkOK, "fleet", fmt.Sprintf("%d VM(s), %d running", len(vms), running))
}

type daemonStatus int

const (
	daemonDown daemonStatus = iota
	// daemonNoAccess means plasmad is up but this shell lacks the group, which
	// looks identical to "down" unless the dial error is inspected. Confusing
	// the two would make init try to start a second daemon.
	daemonNoAccess
	daemonUp
)

func daemonState() daemonStatus {
	conn, err := net.DialTimeout("unix", plasma.SocketPath, time.Second)
	if err == nil {
		conn.Close()
		return daemonUp
	}
	if errors.Is(err, fs.ErrPermission) {
		return daemonNoAccess
	}
	return daemonDown
}

func daemonReachable() bool { return daemonState() == daemonUp }

// processInGroup reports whether THIS process carries the group. That is the
// question that matters: /etc/group membership is irrelevant until a login
// picks it up.
func processInGroup(name string) bool {
	grp, err := user.LookupGroup(name)
	if err != nil {
		return false
	}
	gid, err := strconv.Atoi(grp.Gid)
	if err != nil {
		return false
	}
	groups, err := os.Getgroups()
	if err != nil {
		return false
	}
	for _, g := range groups {
		if g == gid {
			return true
		}
	}
	return false
}

// isMemberOnDisk reports whether the account is listed in the group, even if
// the current shell has not picked it up yet.
func isMemberOnDisk(name string) bool {
	grp, err := user.LookupGroup(name)
	if err != nil {
		return false
	}
	u, err := user.Current()
	if err != nil {
		return false
	}
	ids, err := u.GroupIds()
	if err != nil {
		return false
	}
	for _, id := range ids {
		if id == grp.Gid {
			return true
		}
	}
	return false
}

func startDaemon() error {
	if systemdUnitInstalled() {
		out, err := exec.Command("sudo", "systemctl", "start", "plasmad").CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl start plasmad: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return waitForDaemon()
	}

	// Refresh sudo credentials first, on the terminal, so the password prompt
	// is not swallowed by the detached daemon below.
	if exec.Command("sudo", "-n", "true").Run() != nil {
		fmt.Println("       (sudo needed to start plasmad)")
		v := exec.Command("sudo", "-v")
		v.Stdin, v.Stdout, v.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := v.Run(); err != nil {
			return fmt.Errorf("sudo: %w", err)
		}
	}

	// Setsid so the daemon outlives this command and its shell.
	script := fmt.Sprintf("mkdir -p %s && exec plasmad >>%s 2>&1",
		plasma.RunDir, daemonLog)
	cmd := exec.Command("sudo", "sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // reap, so the child does not linger as a zombie

	if err := waitForDaemon(); err != nil {
		return fmt.Errorf("%w (see %s)", err, daemonLog)
	}
	return nil
}

func systemdUnitInstalled() bool {
	out, err := exec.Command("systemctl", "list-unit-files", "plasmad.service", "--no-legend").Output()
	return err == nil && strings.Contains(string(out), "plasmad.service")
}

func waitForDaemon() error {
	for i := 0; i < 100; i++ {
		if daemonReachable() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("plasmad did not start listening on %s within 10s", plasma.SocketPath)
}

// enterGroupShell replaces this process with a shell carrying the plasma
// group. Exec rather than spawn, so the user's terminal is handed straight to
// the new shell and exiting it returns them where they started.
func enterGroupShell() error {
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		// In a script or pipeline, silently starting a shell would hang.
		fmt.Printf("\nNot a terminal, so no shell was started.\n"+
			"Run commands as: sg %s -c \"plasma vm list\"\n", plasma.SocketGroup)
		return nil
	}

	newgrp, err := exec.LookPath("newgrp")
	if err != nil {
		return fmt.Errorf("newgrp not found; run 'sg %s -c \"<command>\"' or re-login",
			plasma.SocketGroup)
	}

	fmt.Printf("\nStarting a shell with the '%s' group. Type 'exit' to return.\n\n",
		plasma.SocketGroup)
	return syscall.Exec(newgrp, []string{"newgrp", plasma.SocketGroup}, os.Environ())
}

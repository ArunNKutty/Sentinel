package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/fortaegis/plasma/internal/plasma"
)

// `plasma exit` is the counterpart to `plasma init`: it removes everything
// those two commands and the daemon create at runtime, so a later `init`
// starts from nothing.
//
// It deliberately keeps the expensive, rebuild-only artifacts — the base
// image, the guest kernel/initrd, and the applications disk — because
// regenerating those means re-running m1-prep.sh / kernel-swap.sh / make, and
// nothing about them is per-session state.

func exitCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "exit",
		Short: "Tear down the fleet and all plasma runtime state",
		Long: "Destroy everything `plasma init` and the daemon create at runtime:\n" +
			"every VM, its disk and tap, the bridge, dnsmasq, the recorded topology,\n" +
			"the saved state, and plasmad itself.\n\n" +
			"Kept, because they are build artifacts rather than session state:\n" +
			"  the base image, the guest kernel/initrd, and the applications disk.\n\n" +
			"After this, `plasma init` starts from a clean slate.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			c.SilenceUsage = true
			return runExit(yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

func runExit(yes bool) error {
	fmt.Println("plasma exit")

	// Ask the daemon first: deleting through it frees taps, disks and DHCP
	// reservations properly, which a plain kill would leave behind.
	var vms []plasma.VmRecord
	c := plasma.NewClient(plasma.SocketPath)
	if daemonState() == daemonUp {
		vms, _ = c.List()
	}
	strays := plasmaProcesses("cloud-hypervisor", plasma.VMsDir)

	if !yes && !confirmExit(len(vms), len(strays)) {
		fmt.Println("cancelled.")
		return nil
	}

	for _, v := range vms {
		if err := c.Delete(v.ID); err != nil {
			report(checkWarn, "vm "+v.ID, err.Error())
			continue
		}
		report(checkOK, "vm "+v.ID, "deleted")
	}

	// Anything still alive here was orphaned by a lost state file, so the
	// daemon no longer knows about it.
	if strays = plasmaProcesses("cloud-hypervisor", plasma.VMsDir); len(strays) > 0 {
		if err := sudoKill(strays); err != nil {
			report(checkWarn, "orphan VMs", err.Error())
		} else {
			report(checkOK, "orphan VMs", fmt.Sprintf("killed %d stray cloud-hypervisor process(es)", len(strays)))
		}
	}

	if err := stopDaemon(); err != nil {
		report(checkWarn, "daemon", err.Error())
	} else {
		report(checkOK, "daemon", "stopped")
	}

	if err := removeHostState(); err != nil {
		report(checkFail, "cleanup", err.Error())
		return err
	}
	report(checkOK, "network", "bridge "+plasma.Bridge+" and dnsmasq removed")
	report(checkOK, "state", "state.json, links.json and runtime files removed")

	fmt.Println("\nkept (rebuild artifacts, not session state):")
	for _, p := range []string{plasma.BaseRootfs, plasma.KernelPath, plasma.InitrdPath, plasma.ArtifactsDisk} {
		if _, err := os.Stat(p); err == nil {
			fmt.Println("  " + p)
		}
	}
	fmt.Println("\nclean. run 'plasma init' to start again.")
	return nil
}

func confirmExit(vmCount, strayCount int) bool {
	what := fmt.Sprintf("%d VM(s)", vmCount)
	if strayCount > 0 {
		what += fmt.Sprintf(" plus %d orphaned VM process(es)", strayCount)
	}
	fmt.Printf("\nThis destroys %s, their disks, the bridge, saved state and plasmad.\n", what)

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		// Nothing can answer, and silently destroying a fleet would be worse
		// than refusing.
		fmt.Println("Not a terminal; re-run with --yes to confirm.")
		return false
	}
	fmt.Print("Continue? [y/N] ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// plasmaProcesses finds live processes whose command line names the binary and
// mentions a plasma-owned path. Scanning /proc rather than running pkill is
// deliberate: a pkill pattern also matches the shell that carries it, so the
// cleanup would kill itself — and an over-broad match could take out an
// unrelated cloud-hypervisor VM on the same host.
func plasmaProcesses(binary, mustContain string) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	self := os.Getpid()
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		cmd := strings.ReplaceAll(string(raw), "\x00", " ")
		// argv[0] must be the binary itself, so a shell merely mentioning it
		// does not match.
		argv0 := cmd
		if i := strings.IndexByte(cmd, ' '); i > 0 {
			argv0 = cmd[:i]
		}
		if filepath.Base(argv0) != binary || !strings.Contains(cmd, mustContain) {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

func sudoKill(pids []int) error {
	args := []string{"kill", "-TERM"}
	for _, p := range pids {
		args = append(args, strconv.Itoa(p))
	}
	out, err := exec.Command("sudo", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("kill: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func stopDaemon() error {
	if systemdUnitInstalled() {
		out, err := exec.Command("sudo", "systemctl", "stop", "plasmad").CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl stop plasmad: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	pids := plasmaProcesses("plasmad", "")
	if len(pids) == 0 {
		return nil
	}
	return sudoKill(pids)
}

// removeHostState clears everything else in one privileged step, so the user
// is prompted for a password at most once.
func removeHostState() error {
	dnsmasq := plasmaProcesses("dnsmasq", plasma.Bridge)

	var b strings.Builder
	for _, p := range dnsmasq {
		fmt.Fprintf(&b, "kill -TERM %d 2>/dev/null || true\n", p)
	}
	// The bridge can only go once its taps are gone, which VM deletion above
	// has already handled.
	fmt.Fprintf(&b, "ip link del %s 2>/dev/null || true\n", plasma.Bridge)
	fmt.Fprintf(&b, "rm -rf %s\n", plasma.RunDir)
	fmt.Fprintf(&b, "rm -f %s %s %s\n", plasma.SocketPath, plasma.StateFile, plasma.LinksFile)
	fmt.Fprintf(&b, "rm -f %s\n", plasma.DnsmasqPidFile)
	// Per-VM copy-on-write disks only; the base image lives elsewhere.
	fmt.Fprintf(&b, "rm -f %s/*.raw\n", plasma.RuntimeDir)

	out, err := exec.Command("sudo", "sh", "-c", b.String()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

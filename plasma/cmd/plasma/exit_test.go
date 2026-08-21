package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// startNamed runs /bin/sleep under a chosen argv[0] by invoking it through a
// symlink, which is the only way to get a genuinely long-lived process with a
// controlled command line. (Passing extra arguments to sleep does not work:
// a non-numeric operand makes it exit immediately, which silently turns these
// into tests of a dead process.)
func startNamed(t *testing.T, dir, name string) *exec.Cmd {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(dir, name)
	if err := os.Symlink("/bin/sleep", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	cmd := exec.Command(link, "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start %s: %v", link, err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	// Give the kernel a moment to publish /proc/<pid>/cmdline.
	for i := 0; i < 50; i++ {
		if b, err := os.ReadFile(filepath.Join("/proc",
			itoa(cmd.Process.Pid), "cmdline")); err == nil && len(b) > 0 {
			return cmd
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d never appeared in /proc", cmd.Process.Pid)
	return nil
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestPlasmaProcessesFindsTargetBinary(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run", "plasma", "vms")
	proc := startNamed(t, dir, "cloud-hypervisor")

	var found bool
	for _, pid := range plasmaProcesses("cloud-hypervisor", dir) {
		if pid == proc.Process.Pid {
			found = true
		}
	}
	if !found {
		t.Fatal("did not match a live process whose argv[0] is cloud-hypervisor " +
			"and whose command line contains the plasma path")
	}
}

// The dangerous false positive: something that merely mentions the binary. A
// `pkill -f cloud-hypervisor` pattern would kill this, including the very
// shell running the cleanup.
func TestPlasmaProcessesIgnoresMentionsInOtherCommands(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cloud-hypervisor-mentioned", "vms")
	decoy := startNamed(t, dir, "decoy")

	for _, pid := range plasmaProcesses("cloud-hypervisor", "vms") {
		if pid == decoy.Process.Pid {
			t.Fatal("matched a process that only mentions cloud-hypervisor in its path; " +
				"argv[0] must be the binary itself")
		}
	}
}

func TestPlasmaProcessesNeverMatchesSelf(t *testing.T) {
	for _, pid := range plasmaProcesses(filepath.Base(os.Args[0]), "/") {
		if pid == os.Getpid() {
			t.Fatal("plasmaProcesses returned its own pid, so a cleanup would kill itself")
		}
	}
}

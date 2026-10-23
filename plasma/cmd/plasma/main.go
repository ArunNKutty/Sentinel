package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/fortaegis/plasma/internal/plasma"
)

// detachByte is the escape hotkey that ends a `plasma vm shell` session
// locally without sending it to the guest.
const detachByte = 0x18 // Ctrl-X

func main() {
	root := &cobra.Command{
		Use:           "plasma",
		Short:         "Control plane CLI for the NE simulation platform",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	var topoPath string
	vm := &cobra.Command{
		Use:   "vm",
		Short: "VM lifecycle commands",
		Long: "VM lifecycle commands.\n\n" +
			"With --topology, brings up every node declared in a containerlab-style\n" +
			"topology file instead of launching VMs one at a time:\n" +
			"  plasma vm --topology ./ne-lab.yaml",
		RunE: func(cmd *cobra.Command, args []string) error {
			if topoPath == "" {
				return cmd.Help()
			}
			cmd.SilenceUsage = true
			return runTopology(topoPath)
		},
	}
	vm.Flags().StringVar(&topoPath, "topology", "", "bring up a topology file (YAML)")
	// capture is VM-scoped, so it belongs beside list/shell/delete rather than
	// at the top level.
	vm.AddCommand(launchCmd(), listCmd(), shellCmd(), deleteCmd(), captureCmd())
	root.AddCommand(initCmd(), exitCmd(), vm, downCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// runTopology brings up every node a topology file declares. Nodes are
// launched in the file's sorted order so addresses are assigned predictably.
func runTopology(path string) error {
	topo, err := plasma.LoadTopoFile(path)
	if err != nil {
		return err
	}

	name := topo.Name
	if name == "" {
		name = filepath.Base(path)
	}
	fmt.Printf("topology %s: %d node(s)", name, len(topo.Nodes))
	if len(topo.Bridges) > 0 {
		fmt.Printf(", %d bridge(s)", len(topo.Bridges))
	}
	if len(topo.Links) > 0 {
		fmt.Printf(", %d link(s)", len(topo.Links))
	}
	fmt.Println()

	c := plasma.NewClient(plasma.SocketPath)

	var failed []string
	for _, n := range topo.Nodes {
		rec, err := c.LaunchSpec(plasma.LaunchRequest{
			ID: n.ID, Group: n.Group, CPUs: n.CPUs, Memory: n.Memory, Apps: n.Apps, Nics: n.Nics,
		})
		if err != nil {
			fmt.Printf("  %-12s FAILED  %v\n", n.ID, err)
			failed = append(failed, n.ID)
			continue
		}
		fmt.Printf("  %-12s %-9s %s  (%d cpu, %s)\n",
			rec.ID, rec.Group, rec.IP, rec.CPUs, rec.Memory)
		for _, a := range n.Apps {
			fmt.Printf("  %-12s   %s app %s <- %s\n", "", a.Type, a.Name, a.Path)
		}
		for _, nic := range rec.Nics {
			to := nic.Bridge
			if to == "" {
				to = "host (unbridged)"
			}
			if nic.Address != "" {
				to += "  " + nic.Address
			}
			fmt.Printf("  %-12s   %s %s  %s -> %s\n", "", nic.Name, nic.MAC, nic.Tap, to)
		}
	}

	// Record the declared links even on partial failure, so /v1/topology reflects
	// what the file asked for.
	if err := c.SetLinks(plasma.LinksDocument{Topology: name, Links: topo.Links}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record links: %v\n", err)
	}

	if len(failed) > 0 {
		return fmt.Errorf("%d of %d node(s) failed: %s",
			len(failed), len(topo.Nodes), strings.Join(failed, ", "))
	}
	return nil
}

func launchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "launch <group>",
		Short: "Launch a new VM in the given group",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := plasma.NewClient(plasma.SocketPath)
			rec, err := c.Launch(args[0])
			if err != nil {
				return err
			}
			fmt.Printf("launched %s (pid %d)\n", rec.ID, rec.PID)
			return nil
		},
	}
}

func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List VMs",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c := plasma.NewClient(plasma.SocketPath)
			vms, err := c.List()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tGROUP\tSTATUS\tIP\tLINKS\tPID\tUPTIME")
			for _, v := range vms {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
					v.ID, v.Group, v.Status, v.IP, linkAddresses(v.Nics), v.PID, uptime(v.StartedAt))
			}
			return tw.Flush()
		},
	}
}

// linkAddresses is the LINKS column: each addressed link NIC as
// name=address, e.g. "eth1=10.78.0.1 eth2=10.77.0.1"; "-" when there are none.
func linkAddresses(nics []plasma.NicRecord) string {
	var out []string
	for _, n := range nics {
		if n.Address != "" {
			out = append(out, n.Name+"="+strings.SplitN(n.Address, "/", 2)[0])
		}
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, " ")
}

func deleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <id>",
		Aliases: []string{"rm"},
		Short:   "Delete a VM",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := plasma.NewClient(plasma.SocketPath)
			if err := c.Delete(args[0]); err != nil {
				return err
			}
			fmt.Printf("deleted %s\n", args[0])
			return nil
		},
	}
}

func downCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Tear down every VM in the fleet",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c := plasma.NewClient(plasma.SocketPath)
			vms, err := c.List()
			if err != nil {
				return err
			}
			if len(vms) == 0 {
				fmt.Println("no VMs running")
				return nil
			}
			var failed []string
			for _, v := range vms {
				if err := c.Delete(v.ID); err != nil {
					failed = append(failed, fmt.Sprintf("%s: %v", v.ID, err))
					continue
				}
				fmt.Printf("deleted %s\n", v.ID)
			}
			if len(failed) > 0 {
				return fmt.Errorf("failed to delete %d VM(s):\n  %s", len(failed), strings.Join(failed, "\n  "))
			}
			return nil
		},
	}
}

func captureCmd() *cobra.Command {
	var (
		filter    string
		outPath   string
		snaplen   int
		toWiresha bool
	)
	cmd := &cobra.Command{
		Use:   "capture <id>",
		Short: "Stream a live packet capture from a VM",
		Long: "Stream pcap from a VM's host-side tap device.\n\n" +
			"The daemon runs the capture, so this needs no sudo — only membership\n" +
			"of the 'plasma' group.\n\n" +
			"Examples:\n" +
			"  plasma vm capture host-1 --wireshark\n" +
			"  plasma vm capture host-1 | wireshark -k -i -\n" +
			"  plasma vm capture host-1 -f icmp -w icmp.pcap\n" +
			"  plasma vm capture " + plasma.CaptureAllTarget + "   # the whole bridge, not one VM\n" +
			"  plasma vm capture node1:eth2   # one link NIC's tap\n" +
			"  plasma vm capture pbr-wire     # a whole link segment",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runCapture(args[0], filter, outPath, snaplen, toWiresha)
		},
	}
	cmd.Flags().StringVarP(&filter, "filter", "f", "", "BPF filter expression (e.g. \"icmp\", \"port 4500\")")
	cmd.Flags().StringVarP(&outPath, "write", "w", "", "write pcap to a file instead of stdout")
	cmd.Flags().IntVarP(&snaplen, "snaplen", "s", 0, "bytes captured per packet (0 = full frame)")
	cmd.Flags().BoolVarP(&toWiresha, "wireshark", "W", false, "pipe the capture straight into Wireshark")
	return cmd
}

func runCapture(id, filter, outPath string, snaplen int, toWireshark bool) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Straight to a terminal, the stream would be unreadable binary. Rather
	// than refuse, decode it the way `tcpdump -i eth0` would — that is what
	// this command looks like, so it should behave like it.
	toTerminal := outPath == "" && !toWireshark && term.IsTerminal(int(os.Stdout.Fd()))

	c := plasma.NewClient(plasma.SocketPath)
	stream, err := c.Capture(ctx, id, plasma.CaptureOptions{Filter: filter, Snaplen: snaplen})
	if err != nil {
		return err
	}
	defer stream.Close()

	switch {
	case toWireshark:
		return pipeToWireshark(ctx, id, stream)
	case outPath != "":
		f, err := os.Create(outPath)
		if err != nil {
			return err
		}
		defer f.Close()
		fmt.Fprintf(os.Stderr, "capturing %s -> %s (Ctrl-C to stop)\n", id, outPath)
		_, err = io.Copy(f, stream)
		return captureExitErr(ctx, err)
	case toTerminal:
		return printPackets(ctx, id, stream)
	default:
		// Piped or redirected: hand over raw pcap, so `| wireshark -k -i -`
		// and `> file.pcap` both work.
		_, err = io.Copy(os.Stdout, stream)
		return captureExitErr(ctx, err)
	}
}

// printPackets renders the live stream as readable packet lines by feeding it
// to a local tcpdump, which is already a prerequisite. Reusing it costs no new
// dependency and gives exactly the output people expect from this shape of
// command.
func printPackets(ctx context.Context, id string, stream io.Reader) error {
	bin, err := exec.LookPath("tcpdump")
	if err != nil {
		return fmt.Errorf("tcpdump not found on PATH, so the capture cannot be shown here: " +
			"use -w <file>, --wireshark, or pipe this command somewhere")
	}

	// -l line-buffers so packets appear as they arrive rather than in blocks;
	// -n skips DNS lookups, which would stall output in a lab with no resolver.
	td := exec.CommandContext(ctx, bin, "-r", "-", "-n", "-l")
	td.Stdin = stream
	td.Stdout = os.Stdout
	// tcpdump announces "reading from file -, link-type EN10MB" on stderr;
	// hold it back and only surface it if tcpdump actually fails.
	var errBuf strings.Builder
	td.Stderr = &errBuf

	fmt.Fprintf(os.Stderr, "capturing %s (Ctrl-C to stop)\n", id)
	if err := td.Run(); err != nil && ctx.Err() == nil {
		if msg := strings.TrimSpace(errBuf.String()); msg != "" {
			return fmt.Errorf("tcpdump: %s", msg)
		}
		return err
	}
	return captureExitErr(ctx, nil)
}

func pipeToWireshark(ctx context.Context, id string, stream io.Reader) error {
	bin, err := exec.LookPath("wireshark")
	if err != nil {
		return fmt.Errorf("wireshark not found on PATH: %w", err)
	}
	// -k starts capturing immediately, -i - reads the pcap stream from stdin.
	ws := exec.CommandContext(ctx, bin, "-k", "-i", "-")
	ws.Stdin = io.NopCloser(stream)
	ws.Stderr = os.Stderr
	if err := ws.Start(); err != nil {
		return fmt.Errorf("start wireshark: %w", err)
	}
	fmt.Fprintf(os.Stderr, "capturing %s in Wireshark (close it or press Ctrl-C to stop)\n", id)
	if err := ws.Wait(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("wireshark: %w", err)
	}
	return nil
}

// captureExitErr treats a Ctrl-C as a clean stop rather than a failure.
func captureExitErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "\ncapture stopped")
		return nil
	}
	return err
}

func shellCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shell <id>",
		Short: "Attach an interactive shell to a VM's serial console",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runShell(args[0])
		},
	}
}

// openConsole attaches to a VM's serial line. New VMs expose it as a pty,
// which can be reopened as often as you like; VMs launched before that change
// still carry a unix socket, which cloud-hypervisor only ever serves to one
// client. Both are supported so an existing fleet keeps working.
func openConsole(id string) (io.ReadWriteCloser, func(), error) {
	vms, err := plasma.NewClient(plasma.SocketPath).List()
	if err != nil {
		return nil, nil, err
	}
	var rec plasma.VmRecord
	for _, v := range vms {
		if v.ID == id {
			rec = v
			break
		}
	}
	if rec.ID == "" {
		return nil, nil, fmt.Errorf("no such VM: %s", id)
	}
	if rec.Status != plasma.StatusRunning {
		return nil, nil, fmt.Errorf("%s is %s", id, rec.Status)
	}

	if rec.ConsolePTY != "" {
		f, err := os.OpenFile(rec.ConsolePTY, os.O_RDWR, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("open console %s: %w", rec.ConsolePTY, err)
		}
		// The pty's line discipline would echo our keystrokes on top of the
		// guest's own echo, so put the pty in raw mode too — not just stdin.
		prev, err := term.MakeRaw(int(f.Fd()))
		if err != nil {
			f.Close()
			return nil, nil, fmt.Errorf("raw mode on %s: %w", rec.ConsolePTY, err)
		}
		return f, func() { term.Restore(int(f.Fd()), prev); f.Close() }, nil
	}

	sock := rec.ConsoleSocket
	if sock == "" {
		sock = plasma.ConsoleSocketPath(id)
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to console for %s: %w (is it running?)", id, err)
	}
	return conn, func() { conn.Close() }, nil
}

func runShell(id string) error {
	conn, closeConsole, err := openConsole(id)
	if err != nil {
		return err
	}
	defer closeConsole()

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return fmt.Errorf("stdin is not a terminal")
	}
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("set raw mode: %w", err)
	}
	defer term.Restore(fd, oldState)

	fmt.Fprintf(os.Stderr, "Connected to %s. Press Ctrl-X to detach.\r\n\r\n", id)

	go io.Copy(os.Stdout, conn)
	// getty prints its login prompt once, to whoever's attached at the time;
	// if that was nobody, a blank line makes it reprint immediately instead
	// of leaving the session looking dead until the ~60s login timeout.
	conn.Write([]byte("\r\n"))

	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || (n > 0 && buf[0] == detachByte) {
			break
		}
		if n > 0 {
			if _, err := conn.Write(buf[:n]); err != nil {
				break
			}
		}
	}

	fmt.Fprintf(os.Stderr, "\r\nDisconnected from %s.\r\n", id)
	return nil
}

func uptime(start time.Time) string {
	d := time.Since(start).Round(time.Second)
	return d.String()
}

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
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

	vm := &cobra.Command{Use: "vm", Short: "VM lifecycle commands"}
	vm.AddCommand(launchCmd(), listCmd(), shellCmd(), deleteCmd())
	root.AddCommand(vm, downCmd(), captureCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
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
			fmt.Fprintln(tw, "ID\tGROUP\tSTATUS\tIP\tPID\tUPTIME")
			for _, v := range vms {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
					v.ID, v.Group, v.Status, v.IP, v.PID, uptime(v.StartedAt))
			}
			return tw.Flush()
		},
	}
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
			"  plasma capture host-1 --wireshark\n" +
			"  plasma capture host-1 | wireshark -k -i -\n" +
			"  plasma capture host-1 -f icmp -w icmp.pcap\n" +
			"  plasma capture " + plasma.CaptureAllTarget + "   # the bridge itself",
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

	// Binary pcap on a terminal is unreadable garbage; tcpdump refuses the
	// same way. Only bail when there is no other sink configured.
	if outPath == "" && !toWireshark && term.IsTerminal(int(os.Stdout.Fd())) {
		return fmt.Errorf("refusing to write pcap to a terminal: use -w <file>, --wireshark, or pipe to another command")
	}

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
	default:
		_, err = io.Copy(os.Stdout, stream)
		return captureExitErr(ctx, err)
	}
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

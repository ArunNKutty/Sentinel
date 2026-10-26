package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/fortaegis/plasma/internal/plasma"
)

// `plasma lab` drives labs on an engine, local or remote. Nothing here reads
// or writes the engine's disk: files go up as artifacts, and the lab is
// reached through the API (console, tunnels, captures, link operations).

func labCmd() *cobra.Command {
	lab := &cobra.Command{
		Use:   "lab",
		Short: "Bring up and drive labs: one topology and everything it creates",
		Long: "A lab is one applied topology. Its names (fabric, node-1, wire) are its own,\n" +
			"so labs never collide, and deleting one removes exactly what it made.\n\n" +
			"Works against a local engine or a remote one (plasma engine login).\n\n" +
			"  plasma lab apply topologies/ne-lab.yaml --name it-1 --wait\n" +
			"  plasma lab tunnel it-1 3000:fabric:3000 fabric:8080 &\n" +
			"  plasma lab impair it-1 node-1:eth2 --delay 50ms --loss 1\n" +
			"  plasma lab delete it-1",
	}
	lab.AddCommand(labApplyCmd(), labListCmd(), labStatusCmd(), labWaitCmd(), labDeleteCmd(),
		labShellCmd(), labTunnelCmd(), labCaptureCmd(), labMirrorCmd(), labImpairCmd(), labDropCmd())
	return lab
}

func labApplyCmd() *cobra.Command {
	var (
		name    string
		wait    bool
		timeout time.Duration
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "apply <topology.yaml>",
		Short: "Upload a topology's files and bring the lab up",
		Long: "Validates the topology here, uploads every file it names (skipping any the\n" +
			"engine already has), and asks the engine to bring the lab up. Prints the\n" +
			"lab's name; with --wait, returns once every node has reported ready.",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			c.SilenceUsage = true
			return runLabApply(args[0], name, wait, timeout, asJSON)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "lab name (default: the topology's name plus a random suffix)")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until every node is ready")
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Minute, "how long --wait waits")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the lab's status as JSON")
	return cmd
}

func runLabApply(path, name string, wait bool, timeout time.Duration, asJSON bool) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Resolving here catches a bad topology before anything is uploaded, and
	// yields every file it names.
	topo, err := plasma.LoadLabTopoFile(path)
	if err != nil {
		return err
	}
	yaml, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	c := newClient()
	logf := func(format string, a ...any) {
		if !asJSON {
			fmt.Printf(format, a...)
		}
	}
	logf("topology %s: %d node(s) on %s\n", topo.Name, len(topo.Nodes), c.Where())

	keys := make([]string, 0, len(topo.Files))
	for k := range topo.Files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	files := make(map[string]string, len(keys))
	for _, k := range keys {
		digest, sent, err := c.PushArtifact(ctx, topo.Files[k])
		if err != nil {
			return fmt.Errorf("upload %s: %w", k, err)
		}
		files[k] = plasma.ArtifactRef(digest)
		verb := "cached  "
		if sent {
			verb = "uploaded"
		}
		logf("  %s %s  %s\n", verb, digest[:12], k)
	}

	st, err := c.CreateLab(ctx, plasma.CreateLabRequest{Name: name, Topology: string(yaml), Files: files})
	if err != nil {
		return err
	}
	logf("lab %s: %s (management %s)\n", st.ID, st.Phase, st.Mgmt.Subnet)
	if wait {
		if st, err = waitLab(ctx, c, st.ID, timeout, !asJSON); err != nil {
			return err
		}
	}
	if asJSON {
		return printJSON(st)
	}
	if !wait {
		fmt.Printf("check with: plasma lab status %s   (or: plasma lab wait %s)\n", st.ID, st.ID)
	}
	return nil
}

// waitLab polls until the lab is ready, has failed, or the timeout passes,
// printing progress as nodes come up.
func waitLab(ctx context.Context, c *plasma.Client, id string, timeout time.Duration, progress bool) (plasma.LabStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	last := ""
	for {
		st, err := c.Lab(ctx, id)
		if err != nil {
			return st, err
		}
		ready := 0
		for _, v := range st.VMs {
			if v.Ready {
				ready++
			}
		}
		line := fmt.Sprintf("  %s: %d/%d node(s) launched, %d ready", st.Phase, len(st.VMs), len(st.Nodes), ready)
		if progress && line != last {
			fmt.Println(line)
			last = line
		}
		switch {
		case st.Ready:
			if progress {
				fmt.Printf("lab %s is ready\n", id)
			}
			return st, nil
		case st.Phase == plasma.LabFailed:
			return st, labFailure(st)
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return st, fmt.Errorf("lab %s not ready after %s; not ready: %s "+
					"(a node whose image predates scripts/app-prep.sh's ready marker never reports ready)",
					id, timeout, strings.Join(notReady(st), ", "))
			}
			return st, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func labFailure(st plasma.LabStatus) error {
	var b strings.Builder
	fmt.Fprintf(&b, "lab %s failed: %s", st.ID, st.Error)
	nodes := make([]string, 0, len(st.Failed))
	for n := range st.Failed {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	for _, n := range nodes {
		fmt.Fprintf(&b, "\n  %s: %s", n, st.Failed[n])
	}
	return errors.New(b.String())
}

func notReady(st plasma.LabStatus) []string {
	have := map[string]plasma.LabVM{}
	for _, v := range st.VMs {
		have[v.ID] = v
	}
	var out []string
	for _, n := range st.Nodes {
		if v, ok := have[n]; !ok || !v.Ready {
			out = append(out, n)
		}
	}
	return out
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func labListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List labs",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			c.SilenceUsage = true
			labs, err := newClient().Labs(context.Background())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(labs)
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "LAB\tPHASE\tREADY\tTOPOLOGY\tMGMT\tAGE")
			for _, l := range labs {
				ready := 0
				for _, v := range l.VMs {
					if v.Ready {
						ready++
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%s\t%s\t%s\n", l.ID, l.Phase, ready, len(l.Nodes),
					l.Topology, l.Mgmt.Subnet, uptime(l.CreatedAt))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func labStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status <lab>",
		Short: "Show a lab and each of its VMs",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			c.SilenceUsage = true
			st, err := newClient().Lab(context.Background(), args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(st)
			}
			fmt.Printf("lab %s (%s): %s, ready=%v, management %s on %s\n",
				st.ID, st.Topology, st.Phase, st.Ready, st.Mgmt.Subnet, st.Mgmt.Bridge)
			if st.Error != "" {
				fmt.Println("error:", st.Error)
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "VM\tGROUP\tSTATUS\tREADY\tIP\tLINKS\tAPPS\tUPTIME")
			for _, v := range st.VMs {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%s\t%s\t%s\t%s\n", v.ID, v.Group, v.Status, v.Ready, v.IP,
					linkAddresses(v.Nics), orDash(strings.Join(v.Apps, ",")), uptime(v.StartedAt))
			}
			for _, n := range notLaunched(st) {
				fmt.Fprintf(tw, "%s\t-\tnot launched\tfalse\t-\t-\t-\t-\n", n)
			}
			tw.Flush()
			for n, why := range st.Failed {
				fmt.Printf("  %s: %s\n", n, why)
			}
			for link, to := range st.Mirrors {
				fmt.Printf("mirror  %s -> %s\n", link, to)
			}
			for link, f := range st.Filters {
				fmt.Printf("impair  %s %s\n", link, describeFilter(f))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func notLaunched(st plasma.LabStatus) []string {
	have := map[string]bool{}
	for _, v := range st.VMs {
		have[v.ID] = true
	}
	var out []string
	for _, n := range st.Nodes {
		if !have[n] {
			out = append(out, n)
		}
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func labWaitCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait <lab>",
		Short: "Wait until every node of a lab is ready",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			c.SilenceUsage = true
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			_, err := waitLab(ctx, newClient(), args[0], timeout, true)
			return err
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Minute, "how long to wait")
	return cmd
}

func labDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <lab>...",
		Aliases: []string{"rm"},
		Short:   "Delete labs and everything they created",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			c.SilenceUsage = true
			cl := newClient()
			var failed []string
			for _, id := range args {
				if err := cl.DeleteLab(context.Background(), id); err != nil {
					failed = append(failed, fmt.Sprintf("%s: %v", id, err))
					continue
				}
				fmt.Printf("deleted lab %s\n", id)
			}
			if len(failed) > 0 {
				return errors.New(strings.Join(failed, "\n"))
			}
			return nil
		},
	}
}

func labShellCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shell <lab> <vm>",
		Short: "Attach to a lab VM's serial console",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			c.SilenceUsage = true
			return runShell(args[0], args[1])
		},
	}
}

type tunnelSpec struct {
	bind string
	vm   string
	port int
}

// parseTunnel reads [[bind:]local:]vm:port.
func parseTunnel(s string) (tunnelSpec, error) {
	parts := strings.Split(s, ":")
	bad := fmt.Errorf("tunnel %q: want [[bind:]localport:]vm:port, e.g. 3000:fabric:3000", s)
	if len(parts) < 2 || len(parts) > 4 {
		return tunnelSpec{}, bad
	}
	port, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil || port < 1 || port > 65535 {
		return tunnelSpec{}, bad
	}
	t := tunnelSpec{bind: "127.0.0.1", vm: parts[len(parts)-2], port: port}
	local := port
	if len(parts) >= 3 {
		if local, err = strconv.Atoi(parts[len(parts)-3]); err != nil || local < 0 || local > 65535 {
			return tunnelSpec{}, bad
		}
	}
	if len(parts) == 4 {
		t.bind = parts[0]
	}
	t.bind = net.JoinHostPort(t.bind, strconv.Itoa(local))
	return t, nil
}

func labTunnelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tunnel <lab> [[bind:]localport:]vm:port...",
		Short: "Forward local ports to lab VMs, through the engine's API",
		Long: "Listens on this machine (127.0.0.1 unless a bind address is given) and\n" +
			"carries each connection to the VM's port over the API connection, so a\n" +
			"lab's services are reachable only through the engine and only with its\n" +
			"token. Runs until interrupted; prints \"tunnels ready\" once listening.\n\n" +
			"  plasma lab tunnel it-1 3000:fabric:3000 8080:fabric:8080 fabric:9000\n" +
			"  plasma lab tunnel it-1 0:fabric:22      # any free local port",
		Args: cobra.MinimumNArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			c.SilenceUsage = true
			return runTunnels(args[0], args[1:])
		},
	}
}

func runTunnels(lab string, specs []string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	c := newClient()

	st, err := c.Lab(ctx, lab)
	if err != nil {
		return err
	}
	// A node not yet launched is fine: connections wait for nothing, they
	// fail until the VM is up, so a script can open its tunnels right after
	// `lab apply` and poll through them.
	nodes := map[string]bool{}
	for _, n := range st.Nodes {
		nodes[n] = true
	}

	var listeners []net.Listener
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	for _, s := range specs {
		t, err := parseTunnel(s)
		if err != nil {
			return err
		}
		if !nodes[t.vm] {
			return fmt.Errorf("lab %s has no VM %q", lab, t.vm)
		}
		ln, err := net.Listen("tcp", t.bind)
		if err != nil {
			return err
		}
		listeners = append(listeners, ln)
		fmt.Printf("tunnel %s -> %s/%s:%d\n", ln.Addr(), lab, t.vm, t.port)
		go acceptTunnel(ctx, c, lab, t, ln)
	}
	fmt.Println("tunnels ready")
	<-ctx.Done()
	return nil
}

func acceptTunnel(ctx context.Context, c *plasma.Client, lab string, t tunnelSpec, ln net.Listener) {
	for {
		local, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			remote, err := c.Tunnel(ctx, lab, t.vm, t.port)
			if err != nil {
				fmt.Fprintf(os.Stderr, "tunnel %s -> %s:%d: %v\n", ln.Addr(), t.vm, t.port, err)
				local.Close()
				return
			}
			plasma.Splice(local, remote)
		}()
	}
}

func labCaptureCmd() *cobra.Command {
	var (
		filter    string
		outPath   string
		snaplen   int
		toWiresha bool
	)
	cmd := &cobra.Command{
		Use:   "capture <lab> <target>",
		Short: "Stream a live packet capture from a lab",
		Long: "Targets: <vm> (its management NIC), <vm>:<ethN> (a link NIC), the name\n" +
			"of a `kind: bridge` node (that whole segment), or mgmt (the lab's\n" +
			"management bridge: host-directed and broadcast traffic only).\n\n" +
			"  plasma lab capture it-1 fabric:eth1 --wireshark\n" +
			"  plasma lab capture it-1 wire -f 'udp port 4500' -w wire.pcap",
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			c.SilenceUsage = true
			lab, target := args[0], args[1]
			return streamCapture(lab+"/"+target, outPath, toWiresha, func(ctx context.Context) (io.ReadCloser, error) {
				return newClient().LabCapture(ctx, lab, target, plasma.CaptureOptions{Filter: filter, Snaplen: snaplen})
			})
		},
	}
	cmd.Flags().StringVarP(&filter, "filter", "f", "", "BPF filter expression (e.g. \"icmp\", \"port 4500\")")
	cmd.Flags().StringVarP(&outPath, "write", "w", "", "write pcap to a file instead of stdout")
	cmd.Flags().IntVarP(&snaplen, "snaplen", "s", 0, "bytes captured per packet (0 = full frame)")
	cmd.Flags().BoolVarP(&toWiresha, "wireshark", "W", false, "pipe the capture straight into Wireshark")
	return cmd
}

func labMirrorCmd() *cobra.Command {
	var (
		to     string
		off    bool
		stats  bool
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "mirror <lab> <vm:ethN> (--to <vm:ethN> | --off | --stats)",
		Short: "Copy a link NIC's traffic, both directions, out of another NIC",
		Long: "Every frame crossing the first NIC is also sent out of the second, typically\n" +
			"a capture VM's `host:` endpoint. Replaces `sudo tc ... mirred` on the engine.",
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			c.SilenceUsage = true
			cl := newClient()
			switch {
			case stats:
				st, err := cl.MirrorStatus(context.Background(), args[0], args[1])
				if err != nil {
					return err
				}
				if asJSON {
					return printJSON(st)
				}
				fmt.Printf("%s -> %s: ingress %d pkt (%d bytes), egress %d pkt (%d bytes)\n", st.Link, st.To,
					st.Ingress.Packets, st.Ingress.Bytes, st.Egress.Packets, st.Egress.Bytes)
			case off:
				if err := cl.ClearMirror(context.Background(), args[0], args[1]); err != nil {
					return err
				}
				fmt.Printf("mirror on %s removed\n", args[1])
			case to != "":
				if err := cl.SetMirror(context.Background(), args[0], args[1], to); err != nil {
					return err
				}
				fmt.Printf("mirroring %s -> %s\n", args[1], to)
			default:
				return fmt.Errorf("give --to <vm:ethN> or --off")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "NIC to mirror into (vm:ethN)")
	cmd.Flags().BoolVar(&off, "off", false, "stop mirroring")
	cmd.Flags().BoolVar(&stats, "stats", false, "show what the mirror has copied each way")
	cmd.Flags().BoolVar(&asJSON, "json", false, "with --stats, print as JSON")
	return cmd
}

func labDropCmd() *cobra.Command {
	var (
		rules   []string
		once    bool
		timeout string
		wait    bool
		status  bool
		clear   bool
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "drop <lab> <vm:ethN> (--rule SPEC... [--once [--wait]] | --status | --clear)",
		Short: "Drop chosen frames on their way to a link NIC",
		Long: "Installs tc u32 drop rules on the frames the engine delivers to that NIC; a\n" +
			"frame matching any --rule is dropped. A rule is comma-separated matches:\n" +
			"  proto=udp|tcp|icmp|<n>  src=<ip[/len]>  dst=<ip[/len]>  sport=<n>  dport=<n>\n" +
			"  u8@<offset>=<value>[/<mask>]   (offset from the start of the IP header)\n\n" +
			"--once removes the rules on the first drop (or after --timeout); the engine\n" +
			"watches for it, so no second frame slips through. --clear removes them and\n" +
			"prints how many frames they dropped.\n\n" +
			"  plasma lab drop it-1 node-2:eth2 --once --wait \\\n" +
			"    --rule proto=udp,src=10.77.0.1,sport=4242,u8@31=0x42,u8@34=5 \\\n" +
			"    --rule proto=udp,src=10.77.0.1,sport=4242,u8@32=0x42,u8@35=5",
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			c.SilenceUsage = true
			cl := newClient()
			ctx := context.Background()
			lab, link := args[0], args[1]
			var st plasma.DropStatus
			var err error
			switch {
			case clear:
				st, err = cl.ClearDrop(ctx, lab, link)
			case status:
				st, err = cl.DropStatus(ctx, lab, link)
			case len(rules) > 0:
				req := plasma.DropRequest{Once: once, Timeout: timeout}
				for _, spec := range rules {
					r, perr := parseDropRule(spec)
					if perr != nil {
						return perr
					}
					req.Rules = append(req.Rules, r)
				}
				st, err = cl.SetDrop(ctx, lab, link, req)
				for err == nil && wait && once && !st.Done {
					time.Sleep(20 * time.Millisecond)
					st, err = cl.DropStatus(ctx, lab, link)
				}
			default:
				return fmt.Errorf("give --rule, --status or --clear")
			}
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(st)
			}
			state := "armed"
			if !st.Armed {
				state = "removed"
			}
			fmt.Printf("%s: %s, %d frame(s) dropped\n", link, state, st.Dropped)
			if wait && once && st.Dropped == 0 {
				return fmt.Errorf("no matching frame on %s before the timeout", link)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&rules, "rule", nil, "a drop rule (repeatable; any one matching drops the frame)")
	cmd.Flags().BoolVar(&once, "once", false, "remove the rules after the first drop")
	cmd.Flags().StringVar(&timeout, "timeout", "", "with --once, give up after this long (default 30s)")
	cmd.Flags().BoolVar(&wait, "wait", false, "with --once, wait for the drop (or the timeout) and fail if nothing matched")
	cmd.Flags().BoolVar(&status, "status", false, "show the rules' state and count")
	cmd.Flags().BoolVar(&clear, "clear", false, "remove the rules and report what they dropped")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

// parseDropRule reads proto=udp,src=10.77.0.1,sport=4242,u8@31=0x42,...
func parseDropRule(spec string) (plasma.DropRule, error) {
	var r plasma.DropRule
	num := func(v string) (int, error) {
		n, err := strconv.ParseInt(v, 0, 32)
		return int(n), err
	}
	for _, part := range strings.Split(spec, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return r, fmt.Errorf("rule %q: %q is not key=value", spec, part)
		}
		var err error
		switch {
		case k == "proto" || k == "protocol":
			switch v {
			case "udp":
				r.Protocol = 17
			case "tcp":
				r.Protocol = 6
			case "icmp":
				r.Protocol = 1
			default:
				r.Protocol, err = num(v)
			}
		case k == "src":
			r.Src = v
		case k == "dst":
			r.Dst = v
		case k == "sport":
			r.Sport, err = num(v)
		case k == "dport":
			r.Dport, err = num(v)
		case strings.HasPrefix(k, "u8@"):
			var b plasma.ByteMatch
			if b.Offset, err = num(strings.TrimPrefix(k, "u8@")); err != nil {
				break
			}
			val, mask, hasMask := strings.Cut(v, "/")
			if b.Value, err = num(val); err == nil && hasMask {
				b.Mask, err = num(mask)
			}
			r.Bytes = append(r.Bytes, b)
		default:
			return r, fmt.Errorf("rule %q: unknown key %q", spec, k)
		}
		if err != nil {
			return r, fmt.Errorf("rule %q: %s: %v", spec, k, err)
		}
	}
	return r, nil
}

func labImpairCmd() *cobra.Command {
	var (
		f     plasma.LinkFilter
		clear bool
	)
	cmd := &cobra.Command{
		Use:   "impair <lab> <vm:ethN> [--delay D] [--jitter D] [--loss PCT] [--drop] [--both-ends] | --clear",
		Short: "Delay, lose or drop the frames delivered to a link NIC",
		Long: "Applies netem to the frames the engine delivers to that NIC. --both-ends\n" +
			"also applies it to the far end of a VM-to-VM link, impairing it both ways.\n" +
			"Replaces fault-injector.sh / `sudo tc ... netem` on the engine.\n\n" +
			"  plasma lab impair it-1 node-1:eth2 --delay 80ms --jitter 10ms --loss 0.5\n" +
			"  plasma lab impair it-1 node-1:eth2 --drop        # cut the link\n" +
			"  plasma lab impair it-1 node-1:eth2 --clear",
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			c.SilenceUsage = true
			cl := newClient()
			if clear {
				if err := cl.ClearFilter(context.Background(), args[0], args[1]); err != nil {
					return err
				}
				fmt.Printf("impairment on %s cleared\n", args[1])
				return nil
			}
			if err := cl.SetFilter(context.Background(), args[0], args[1], f); err != nil {
				return err
			}
			fmt.Printf("impairing %s: %s\n", args[1], describeFilter(f))
			return nil
		},
	}
	cmd.Flags().StringVar(&f.Delay, "delay", "", "added delay (e.g. 50ms)")
	cmd.Flags().StringVar(&f.Jitter, "jitter", "", "delay variation (needs --delay)")
	cmd.Flags().Float64Var(&f.Loss, "loss", 0, "percentage of frames lost (0-100)")
	cmd.Flags().BoolVar(&f.Drop, "drop", false, "lose every frame")
	cmd.Flags().BoolVar(&f.BothEnds, "both-ends", false, "also impair the far end of a VM-to-VM link")
	cmd.Flags().BoolVar(&clear, "clear", false, "remove the impairment")
	return cmd
}

func describeFilter(f plasma.LinkFilter) string {
	var parts []string
	if f.Delay != "" {
		d := "delay " + f.Delay
		if f.Jitter != "" {
			d += "±" + f.Jitter
		}
		parts = append(parts, d)
	}
	switch {
	case f.Drop:
		parts = append(parts, "drop all")
	case f.Loss > 0:
		parts = append(parts, fmt.Sprintf("loss %g%%", f.Loss))
	}
	if f.BothEnds {
		parts = append(parts, "both ends")
	}
	return strings.Join(parts, ", ")
}

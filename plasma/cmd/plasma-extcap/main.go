// Command plasma-extcap exposes plasma VMs to Wireshark as capture interfaces.
//
// Wireshark discovers external capture sources by running every binary in its
// extcap directory (the same mechanism behind sshdump and udpdump). This one
// asks plasmad which VMs are up and streams pcap for whichever the user picks,
// so VMs appear in Wireshark's normal interface list.
//
// It runs as the invoking user, so that user must be in the "plasma" group —
// `make install` arranges that.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/fortaegis/plasma/internal/plasma"
)

// Interface value prefixes. Wireshark echoes the chosen value back via
// --extcap-interface, so the prefix is how we tell a VM tap from a bridge
// without the two ever colliding.
const (
	vmPrefix     = "plasmavm-"
	bridgePrefix = "plasmanet-"
	// labPrefix values are plasmalab-<lab>/<target>.
	labPrefix = "plasmalab-"
)

func main() {
	args := parseArgs(os.Args[1:])

	var err error
	switch {
	case args.has("extcap-interfaces"):
		err = listInterfaces()
	case args.has("extcap-dlts"):
		err = listDLTs()
	case args.has("extcap-config"):
		err = listConfig()
	case args.has("extcap-version"):
		printExtcapLine()
	case args.has("capture"):
		err = runCapture(args)
	default:
		printExtcapLine()
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "plasma-extcap:", err)
		os.Exit(1)
	}
}

// client honours PLASMA_ENGINE and `plasma engine login`, so Wireshark on
// a laptop lists a remote engine's VMs.
func client() *plasma.Client {
	c, err := plasma.ClientFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "plasma-extcap:", err)
		os.Exit(1)
	}
	return c
}

func printExtcapLine() {
	fmt.Println("extcap {version=1.0}{display=Plasma}{help=https://github.com/fortaegis/plasma}")
}

// listInterfaces reports every capturable target. A missing or unreachable
// daemon is not an error here: Wireshark runs this on every startup, and
// failing would put an error dialog in front of the user just for having
// plasmad stopped. Report no interfaces instead.
func listInterfaces() error {
	printExtcapLine()

	vms, err := client().List()
	if err != nil {
		fmt.Fprintln(os.Stderr, "plasma-extcap: no interfaces:", err)
		return nil
	}

	for _, v := range vms {
		if v.Status != plasma.StatusRunning {
			continue
		}
		fmt.Printf("interface {value=%s%s}{display=Plasma VM %s (%s)}\n",
			vmPrefix, v.ID, v.ID, v.IP)
		for _, n := range v.Nics {
			fmt.Printf("interface {value=%s%s:%s}{display=Plasma VM %s %s (%s)}\n",
				vmPrefix, v.ID, n.Name, v.ID, n.Name, n.Tap)
		}
	}

	fmt.Printf("interface {value=%s%s}{display=Plasma bridge %s (broadcast + host traffic only)}\n",
		bridgePrefix, plasma.CaptureAllTarget, plasma.CaptureAllTarget)

	labs, _ := client().Labs(context.Background())
	for _, l := range labs {
		for _, v := range l.VMs {
			if v.Status != plasma.StatusRunning {
				continue
			}
			fmt.Printf("interface {value=%s%s/%s}{display=Plasma lab %s: %s (%s)}\n",
				labPrefix, l.ID, v.ID, l.ID, v.ID, v.IP)
			for _, n := range v.Nics {
				fmt.Printf("interface {value=%s%s/%s:%s}{display=Plasma lab %s: %s %s}\n",
					labPrefix, l.ID, v.ID, n.Name, l.ID, v.ID, n.Name)
			}
		}
		for _, b := range l.Bridges {
			fmt.Printf("interface {value=%s%s/%s}{display=Plasma lab %s: segment %s}\n",
				labPrefix, l.ID, b, l.ID, b)
		}
	}
	return nil
}

// listDLTs reports the link type. Taps and bridges both carry Ethernet frames.
func listDLTs() error {
	fmt.Println("dlt {number=1}{name=EN10MB}{display=Ethernet}")
	return nil
}

func listConfig() error {
	fmt.Println("arg {number=0}{call=--filter}{display=Capture filter}{type=string}" +
		"{tooltip=BPF expression, e.g. icmp or port 4500}{default=}")
	fmt.Println("arg {number=1}{call=--snaplen}{display=Snapshot length}{type=integer}" +
		"{tooltip=Bytes captured per packet; 0 captures the full frame}{default=0}{range=0,262144}")
	return nil
}

func runCapture(args argSet) error {
	iface := args.get("extcap-interface")
	fifo := args.get("fifo")
	if iface == "" || fifo == "" {
		return fmt.Errorf("--capture needs --extcap-interface and --fifo")
	}

	lab, target, err := decodeInterface(iface)
	if err != nil {
		return err
	}

	// Wireshark's own capture-filter box arrives as --extcap-capture-filter and
	// should win over the value saved in our config dialog.
	filter := args.get("extcap-capture-filter")
	if filter == "" {
		filter = args.get("filter")
	}
	snaplen, _ := strconv.Atoi(args.get("snaplen"))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	opts := plasma.CaptureOptions{Filter: filter, Snaplen: snaplen}
	var stream io.ReadCloser
	if lab != "" {
		stream, err = client().LabCapture(ctx, lab, target, opts)
	} else {
		stream, err = client().Capture(ctx, target, opts)
	}
	if err != nil {
		return err
	}
	defer stream.Close()

	// Opening the fifo blocks until Wireshark opens the read end, which it has
	// already done by the time it spawns us.
	out, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open fifo: %w", err)
	}
	defer out.Close()

	// A broken pipe here just means the user hit Stop in Wireshark.
	if _, err := io.Copy(out, stream); err != nil && ctx.Err() == nil {
		if !strings.Contains(err.Error(), "broken pipe") {
			return err
		}
	}
	return nil
}

// decodeInterface maps a Wireshark interface value back to a plasma capture
// target.
func decodeInterface(iface string) (lab, target string, err error) {
	switch {
	case strings.HasPrefix(iface, vmPrefix):
		return "", strings.TrimPrefix(iface, vmPrefix), nil
	case strings.HasPrefix(iface, bridgePrefix):
		return "", strings.TrimPrefix(iface, bridgePrefix), nil
	case strings.HasPrefix(iface, labPrefix):
		if lab, target, ok := strings.Cut(strings.TrimPrefix(iface, labPrefix), "/"); ok {
			return lab, target, nil
		}
	}
	return "", "", fmt.Errorf("unknown interface %q", iface)
}

// argSet is a parsed extcap command line. Wireshark mixes bare flags,
// "--key value" and "--key=value" in the same invocation, which the standard
// flag package will not accept, so parse it directly.
type argSet map[string]string

func (a argSet) has(k string) bool { _, ok := a[k]; return ok }
func (a argSet) get(k string) string {
	return a[k]
}

func parseArgs(argv []string) argSet {
	out := argSet{}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if !strings.HasPrefix(arg, "--") {
			continue
		}
		key := strings.TrimPrefix(arg, "--")

		if k, v, ok := strings.Cut(key, "="); ok {
			out[k] = v
			continue
		}
		// A following non-flag token is this flag's value; otherwise it is a
		// bare switch such as --capture.
		if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "--") {
			out[key] = argv[i+1]
			i++
			continue
		}
		out[key] = ""
	}
	return out
}

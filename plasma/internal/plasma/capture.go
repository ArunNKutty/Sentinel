package plasma

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// Packet capture runs on the host side of a VM's link. Unlike containerlab,
// which has to enter a container's network namespace to reach an interface,
// every plasma VM already exposes its tap on the host — so capture is a plain
// tcpdump on that device. plasmad is root, which is what lets an unprivileged
// operator capture at all: the daemon owns tcpdump and streams the pcap back.

const (
	// CaptureAllTarget captures on the bridge itself rather than one VM's tap.
	// Note this is NOT a fleet-wide mirror: a Linux bridge forwards frames
	// port-to-port without passing them up to the bridge device, so this sees
	// host-directed and broadcast traffic only. Per-VM taps are the reliable
	// way to see a given VM's traffic.
	CaptureAllTarget = Bridge

	// defaultSnaplen of 0 tells tcpdump to use its own default (full frame on
	// modern versions).
	defaultSnaplen = 0

	// captureDropUser is who tcpdump drops to after opening the capture
	// socket. It still writes to the already-open stdout, so -w - keeps
	// working after the privilege drop.
	captureDropUser = "nobody"

	// pcapGlobalHeaderLen is the fixed 24-byte pcap file header tcpdump emits
	// once the capture is live. Reading it is how we confirm a healthy start.
	pcapGlobalHeaderLen = 24
)

type CaptureOptions struct {
	// Interface is the host device to capture on, e.g. "tap-host-1".
	Interface string
	// Filter is a BPF expression ("icmp", "port 4500"). Passed to tcpdump
	// through a file, never argv — see buildCaptureCmd.
	Filter string
	// Snaplen caps bytes captured per packet; 0 means tcpdump's default.
	Snaplen int
}

// ErrNotRunning is returned when a capture targets a VM that exists but has no
// live process, so its tap is gone.
var ErrNotRunning = errors.New("VM is not running")

// ErrBadFilter marks a BPF expression tcpdump could not parse, so callers can
// report it as a client mistake rather than a daemon fault.
var ErrBadFilter = errors.New("invalid capture filter")

// ResolveCaptureTarget maps a capture target to a host interface name. The
// target is a VM ID (its management tap), "<id>:<ethN>" for one of its link
// NICs, a link bridge some running VM is on, or CaptureAllTarget for the
// management bridge.
func ResolveCaptureTarget(target string, state *State) (string, error) {
	if target == CaptureAllTarget {
		return Bridge, nil
	}
	id, nic, hasNic := strings.Cut(target, ":")
	for _, v := range state.List() {
		if !hasNic && v.Status == StatusRunning && isLinkBridge(target) {
			for _, n := range v.Nics {
				if n.Bridge == target {
					return target, nil
				}
			}
		}
		if v.ID != id {
			continue
		}
		if v.Status != StatusRunning {
			return "", fmt.Errorf("%w: %s is %s", ErrNotRunning, v.ID, v.Status)
		}
		if hasNic {
			for _, n := range v.Nics {
				if n.Name == nic {
					return n.Tap, nil
				}
			}
			return "", fmt.Errorf("%w: %s has no link NIC %s", ErrNotFound, v.ID, nic)
		}
		// TapDevice was added after the first records were written; derive it
		// for anything persisted before that.
		if v.TapDevice != "" {
			return v.TapDevice, nil
		}
		return TapDeviceName(v.ID), nil
	}
	return "", fmt.Errorf("%w: %s", ErrNotFound, target)
}

// StartCapture runs tcpdump on opts.Interface and returns its pcap stream. The
// caller must Close the reader; doing so, or cancelling ctx, stops tcpdump and
// cleans up the filter file. Without that the daemon would leak root tcpdumps
// every time a client disconnects.
func StartCapture(ctx context.Context, opts CaptureOptions) (io.ReadCloser, error) {
	if _, err := exec.LookPath("tcpdump"); err != nil {
		return nil, fmt.Errorf("tcpdump not found on PATH: %w", err)
	}

	filterFile, err := writeFilterFile(opts.Filter)
	if err != nil {
		return nil, err
	}

	cleanup := func() {
		if filterFile != "" {
			os.Remove(filterFile)
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	cmd := buildCaptureCmd(ctx, opts, filterFile)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		cleanup()
		return nil, fmt.Errorf("capture stdout: %w", err)
	}
	// tcpdump writes its banner and any fatal error to stderr; keep it so a
	// failed capture can say why instead of just returning an empty stream.
	var stderr capturePrefix
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		cancel()
		cleanup()
		return nil, fmt.Errorf("start tcpdump: %w", err)
	}

	// tcpdump validates the filter and opens the device only after it starts,
	// so a bad filter fails here rather than at exec time. Block until it
	// emits the pcap global header, which it writes as soon as the capture is
	// actually live. Without this the caller would commit an HTTP 200 and only
	// then discover the capture was dead, leaving the user a silent 0-byte
	// pcap instead of an error.
	header := make([]byte, pcapGlobalHeaderLen)
	if _, err := io.ReadFull(stdout, header); err != nil {
		cancel()
		cmd.Wait()
		cleanup()
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return nil, fmt.Errorf("capture failed to start: %w", err)
		}
		if strings.Contains(msg, "parse filter") || strings.Contains(msg, "syntax error") {
			return nil, fmt.Errorf("%w: %s", ErrBadFilter, msg)
		}
		return nil, fmt.Errorf("tcpdump: %s", msg)
	}

	return &captureStream{
		stdout:  stdout,
		body:    io.MultiReader(bytes.NewReader(header), stdout),
		cmd:     cmd,
		ctx:     ctx,
		cancel:  cancel,
		cleanup: cleanup,
		stderr:  &stderr,
	}, nil
}

// buildCaptureCmd assembles the tcpdump invocation.
//
// The filter is deliberately NOT passed as an argument. tcpdump's -z flag runs
// an arbitrary command, so a filter that reached argv and began with "-" would
// be a root code-execution vector in a daemon that runs as root. Reading the
// filter from a file via -F removes that class of bug entirely.
func buildCaptureCmd(ctx context.Context, opts CaptureOptions, filterFile string) *exec.Cmd {
	args := []string{
		"-i", opts.Interface,
		// -U writes each packet through immediately; without it tcpdump block
		// buffers and a live Wireshark shows nothing until the buffer fills.
		"-U",
		"-w", "-",
		"-Z", captureDropUser,
	}
	if opts.Snaplen > 0 {
		args = append(args, "-s", strconv.Itoa(opts.Snaplen))
	}
	if filterFile != "" {
		args = append(args, "-F", filterFile)
	}
	return exec.CommandContext(ctx, "tcpdump", args...)
}

// writeFilterFile stores a BPF expression for tcpdump -F. Returns "" for an
// empty filter, meaning capture everything.
func writeFilterFile(filter string) (string, error) {
	if filter == "" {
		return "", nil
	}
	f, err := os.CreateTemp("", "plasma-capture-filter-*.bpf")
	if err != nil {
		return "", fmt.Errorf("filter file: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(filter + "\n"); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("write filter: %w", err)
	}
	return f.Name(), nil
}

// captureStream ties the pcap reader's lifetime to the tcpdump process.
type captureStream struct {
	stdout io.ReadCloser
	// body replays the already-consumed pcap header ahead of the live stream.
	body     io.Reader
	cmd      *exec.Cmd
	ctx      context.Context
	cancel   context.CancelFunc
	cleanup  func()
	stderr   *capturePrefix
	waitOnce sync.Once
	waitErr  error
}

// wait reaps tcpdump exactly once; both Read and Close can reach it.
func (c *captureStream) wait() error {
	c.waitOnce.Do(func() { c.waitErr = c.cmd.Wait() })
	return c.waitErr
}

func (c *captureStream) Read(p []byte) (int, error) {
	n, err := c.body.Read(p)
	if errors.Is(err, io.EOF) && n == 0 {
		// tcpdump is done. Distinguish "we stopped it" from "it died on its
		// own" by exit status — its startup banner goes to stderr, so the
		// presence of stderr output alone does not mean failure.
		werr := c.wait()
		if c.ctx.Err() == nil && werr != nil {
			if msg := strings.TrimSpace(c.stderr.String()); msg != "" {
				return 0, fmt.Errorf("tcpdump: %s", msg)
			}
			return 0, fmt.Errorf("tcpdump: %w", werr)
		}
	}
	return n, err
}

func (c *captureStream) Close() error {
	c.cancel()
	c.stdout.Close()
	c.wait()
	c.cleanup()
	return nil
}

// capturePrefix keeps the first chunk of tcpdump's stderr for error reporting
// without letting a chatty capture grow unbounded.
type capturePrefix struct {
	buf []byte
}

func (c *capturePrefix) Write(p []byte) (int, error) {
	const max = 4096
	if room := max - len(c.buf); room > 0 {
		if len(p) > room {
			c.buf = append(c.buf, p[:room]...)
		} else {
			c.buf = append(c.buf, p...)
		}
	}
	return len(p), nil
}

func (c *capturePrefix) String() string {
	return string(c.buf)
}

package plasma

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestTapDeviceNameTruncatesToIfnamsiz(t *testing.T) {
	// The kernel rejects interface names longer than 15 bytes, so a long VM ID
	// has to be cut. Launch and capture both derive the name this way, which is
	// the point of having one helper.
	cases := []struct{ id, want string }{
		{"host-1", "tap-host-1"},
		{"ne-00", "tap-ne-00"},
		{"a-very-long-vm-identifier", "tap-a-very-long"},
	}
	for _, c := range cases {
		if got := TapDeviceName(c.id); got != c.want {
			t.Errorf("TapDeviceName(%q) = %q, want %q", c.id, got, c.want)
		}
		if len(TapDeviceName(c.id)) > 15 {
			t.Errorf("TapDeviceName(%q) exceeds IFNAMSIZ-1", c.id)
		}
	}
}

// The filter must never reach tcpdump's argv. tcpdump's -z flag runs an
// arbitrary command, so a filter that landed in argv and started with "-"
// would be remote code execution as root in a daemon that runs as root.
func TestCaptureFilterNeverReachesArgv(t *testing.T) {
	const evil = "-z /bin/sh"

	file, err := writeFilterFile(evil)
	if err != nil {
		t.Fatalf("writeFilterFile: %v", err)
	}
	defer os.Remove(file)

	cmd := buildCaptureCmd(context.Background(),
		CaptureOptions{Interface: "tap-host-1", Filter: evil}, file)

	for _, arg := range cmd.Args {
		if strings.Contains(arg, "/bin/sh") || arg == "-z" {
			t.Fatalf("filter text leaked into argv: %v", cmd.Args)
		}
	}

	// It should be passed by reference to the file instead.
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "-F "+file) {
		t.Fatalf("filter not passed via -F: %v", cmd.Args)
	}

	// And the file must hold exactly what the caller asked for.
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read filter file: %v", err)
	}
	if strings.TrimSpace(string(body)) != evil {
		t.Fatalf("filter file = %q, want %q", body, evil)
	}
}

func TestBuildCaptureCmdFlags(t *testing.T) {
	cmd := buildCaptureCmd(context.Background(),
		CaptureOptions{Interface: "tap-host-1", Snaplen: 128}, "")
	got := strings.Join(cmd.Args, " ")

	for _, want := range []string{
		"-i tap-host-1",
		"-U",        // packet-buffered, or a live Wireshark stays empty
		"-w -",      // pcap to stdout
		"-Z nobody", // drop root right after opening the socket
		"-s 128",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}

	// No filter means no -F at all, rather than an empty file.
	if strings.Contains(got, "-F") {
		t.Errorf("unexpected -F with no filter: %q", got)
	}
}

func TestWriteFilterFileEmpty(t *testing.T) {
	path, err := writeFilterFile("")
	if err != nil || path != "" {
		t.Fatalf("writeFilterFile(\"\") = %q, %v; want \"\", nil", path, err)
	}
}

func TestResolveCaptureTarget(t *testing.T) {
	s := newTestState(t,
		VmRecord{ID: "host-1", Group: "host", IP: "10.0.0.2",
			Status: StatusRunning, TapDevice: "tap-host-1"},
		VmRecord{ID: "host-2", Group: "host", IP: "10.0.0.3",
			Status: StatusExited, TapDevice: "tap-host-2"},
		// Written before VmRecord carried a TapDevice.
		VmRecord{ID: "host-3", Group: "host", IP: "10.0.0.4", Status: StatusRunning},
	)

	if got, err := ResolveCaptureTarget("host-1", s); err != nil || got != "tap-host-1" {
		t.Errorf("running VM = %q, %v; want tap-host-1", got, err)
	}
	if got, err := ResolveCaptureTarget("host-3", s); err != nil || got != "tap-host-3" {
		t.Errorf("legacy record = %q, %v; want derived tap-host-3", got, err)
	}
	if got, err := ResolveCaptureTarget(CaptureAllTarget, s); err != nil || got != Bridge {
		t.Errorf("bridge target = %q, %v; want %s", got, err, Bridge)
	}

	// A stopped VM has no tap, so this is a distinct condition from "unknown".
	if _, err := ResolveCaptureTarget("host-2", s); !errors.Is(err, ErrNotRunning) {
		t.Errorf("exited VM err = %v, want ErrNotRunning", err)
	}
	if _, err := ResolveCaptureTarget("nope", s); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown VM err = %v, want ErrNotFound", err)
	}
}

func TestStartCaptureReportsBadFilter(t *testing.T) {
	if _, err := os.Stat("/usr/bin/tcpdump"); err != nil {
		t.Skip("tcpdump not installed")
	}
	// A capture that cannot start must fail here rather than returning a
	// stream that yields nothing — otherwise the caller commits an HTTP 200
	// and the user gets a silent, empty pcap.
	_, err := StartCapture(context.Background(), CaptureOptions{
		Interface: "lo",
		Filter:    "definitely not a bpf expression",
	})
	if err == nil {
		t.Fatal("want an error for an unparseable filter, got nil")
	}
	if !errors.Is(err, ErrBadFilter) {
		t.Logf("note: got %v", err)
	}
}

func TestResolveCaptureTargetLinkNics(t *testing.T) {
	s := newTestState(t,
		VmRecord{ID: "node1", IP: "10.0.0.3", Status: StatusRunning, TapDevice: "tap-node1",
			Nics: []NicRecord{{Name: "eth1", Tap: "pt3-eth1", Bridge: "pbr-wire"}}},
		VmRecord{ID: "fabric", IP: "10.0.0.2", Status: StatusRunning, TapDevice: "tap-fabric",
			Nics: []NicRecord{{Name: "eth1", Tap: "fabcap"}}},
	)
	for target, want := range map[string]string{
		"node1:eth1":  "pt3-eth1",
		"fabric:eth1": "fabcap",
		"pbr-wire":    "pbr-wire",
	} {
		if got, err := ResolveCaptureTarget(target, s); err != nil || got != want {
			t.Errorf("%s = %q, %v; want %s", target, got, err, want)
		}
	}
	if _, err := ResolveCaptureTarget("node1:eth9", s); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing NIC err = %v, want ErrNotFound", err)
	}
	// Only a bridge some running VM is on; never an arbitrary host bridge.
	if _, err := ResolveCaptureTarget("pbr-other", s); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown bridge err = %v, want ErrNotFound", err)
	}
}

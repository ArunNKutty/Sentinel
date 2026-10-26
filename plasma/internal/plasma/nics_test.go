package plasma

import (
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestLinkBridgeNameIsOrderInsensitive(t *testing.T) {
	// Each end's launch derives the name alone, so both must agree.
	ab := LinkBridgeName("a", "eth1", "b", "eth2")
	if ba := LinkBridgeName("b", "eth2", "a", "eth1"); ab != ba {
		t.Errorf("a-b = %s, b-a = %s; want the same bridge", ab, ba)
	}
	if other := LinkBridgeName("a", "eth2", "b", "eth1"); other == ab {
		t.Errorf("different interface pairs share bridge %s", ab)
	}
	if !strings.HasPrefix(ab, LinkBridgePrefix) || len(ab) > maxIfaceName {
		t.Errorf("bridge %q must carry %q and fit IFNAMSIZ", ab, LinkBridgePrefix)
	}
}

func TestNicNamesNeverCollide(t *testing.T) {
	// The extremes still fit IFNAMSIZ.
	if tap := NicTapName(254, "eth99"); len(tap) > maxIfaceName {
		t.Errorf("tap %q is over %d bytes", tap, maxIfaceName)
	}
	// A link NIC's MAC must differ from every management MAC, and from the
	// same VM's other NICs.
	seen := map[string]bool{}
	for octet := 2; octet <= 254; octet++ {
		seen[fmt.Sprintf("02:00:0a:00:00:%02x", octet)] = true // as State allocates
	}
	for octet := 2; octet <= 254; octet++ {
		for idx := 1; idx <= 4; idx++ {
			mac := NicMAC(octet, idx)
			if seen[mac] {
				t.Fatalf("NIC MAC %s collides", mac)
			}
			seen[mac] = true
		}
	}
}

func TestValidateNics(t *testing.T) {
	good := []NicSpec{{Name: "eth1", Bridge: "pbr-ctl"}, {Name: "eth2", HostName: "fabcap"}}
	if err := ValidateNics(good); err != nil {
		t.Errorf("valid nics rejected: %v", err)
	}

	bad := map[string][]NicSpec{
		// The API is reachable without a topology file, so the daemon must
		// refuse to attach a VM to a bridge it does not own.
		"foreign bridge": {{Name: "eth1", Bridge: "docker0"}},
		"out of order":   {{Name: "eth2", Bridge: "pbr-a"}, {Name: "eth1", Bridge: "pbr-b"}},
		"both targets":   {{Name: "eth1", Bridge: "pbr-a", HostName: "cap0"}},
		"no target":      {{Name: "eth1"}},
		"eth0":           {{Name: "eth0", Bridge: "pbr-a"}},
	}
	for name, nics := range bad {
		if err := ValidateNics(nics); err == nil {
			t.Errorf("%s: accepted %+v", name, nics)
		}
	}
}

func TestPlanNics(t *testing.T) {
	got, err := planNics(LabNet{}, "10.0.0.7", []NicSpec{
		{Name: "eth1", Bridge: "pbr-ctl"},
		{Name: "eth2", HostName: "fabcap"},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	want := []NicRecord{
		{Name: "eth1", Tap: "pt7-eth1", MAC: "02:00:0b:00:01:07", Bridge: "pbr-ctl"},
		// A host endpoint's tap is the name the topology gave it.
		{Name: "eth2", Tap: "fabcap", MAC: "02:00:0b:00:02:07"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("planNics = %+v, want %+v", got, want)
	}
}

func TestPlanNicsInALab(t *testing.T) {
	lab := LabNet{ID: "it-1", Index: 3}
	got, err := planNics(lab, "10.0.3.7", []NicSpec{
		{Name: "eth1", Bridge: "pbr-ctl"},
		{Name: "eth2", HostName: "fabcap"},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	want := []NicRecord{
		{Name: "eth1", Tap: "pt3-7-eth1", MAC: "02:00:0b:03:01:07", Bridge: lab.HostBridge("pbr-ctl"), Segment: "pbr-ctl"},
		// In a lab a host endpoint is not given the global name it asked for.
		{Name: "eth2", Tap: "pt3-7-eth2", MAC: "02:00:0b:03:02:07", Segment: "host:fabcap"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("planNics = %+v, want %+v", got, want)
	}
	other := LabNet{ID: "it-2", Index: 4}
	if lab.HostBridge("pbr-ctl") == other.HostBridge("pbr-ctl") {
		t.Error("two labs share a bridge for the same segment name")
	}
	if b := lab.HostBridge("pbr-ctl"); len(b) > maxIfaceName || !isLinkBridge(b) {
		t.Errorf("lab bridge %q is not a plasma bridge name that fits IFNAMSIZ", b)
	}
	if tap := (LabNet{ID: "x", Index: MaxLabs}).LinkTap(254, "eth99"); len(tap) > maxIfaceName {
		t.Errorf("lab link tap %q exceeds IFNAMSIZ-1", tap)
	}
}

func TestLegacyLabNetKeepsNames(t *testing.T) {
	var legacy LabNet
	if legacy.MgmtBridge() != Bridge || legacy.MgmtMAC(7) != "02:00:0a:00:00:07" ||
		legacy.LinkMAC(7, 1) != NicMAC(7, 1) || legacy.MgmtTap("host-1", 7) != TapDeviceName("host-1") ||
		legacy.HostBridge("pbr-x") != "pbr-x" || legacy.VMAddr(7) != "10.0.0.7" {
		t.Error("index 0 must reproduce the pre-lab names")
	}
}

func TestChArgsOrdersNics(t *testing.T) {
	// The guest names NICs in attach order: management first, then eth1...
	args := chArgs("vm", "/s", []string{"path=/d"}, "tap-a", "02:00:0a:00:00:07",
		[]NicRecord{
			{Name: "eth1", Tap: "pt7-eth1", MAC: "02:00:0b:00:01:07"},
			{Name: "eth2", Tap: "pt7-eth2", MAC: "02:00:0b:00:02:07"},
		}, LaunchOpts{})

	i := slices.Index(args, "--net")
	if i < 0 || i+3 >= len(args) {
		t.Fatalf("no --net with three values in %v", args)
	}
	want := []string{
		"tap=tap-a,mac=02:00:0a:00:00:07",
		"tap=pt7-eth1,mac=02:00:0b:00:01:07",
		"tap=pt7-eth2,mac=02:00:0b:00:02:07",
	}
	if got := args[i+1 : i+4]; !slices.Equal(got, want) {
		t.Errorf("--net %v, want %v", got, want)
	}
	if args[i+4] != "--cpus" {
		t.Errorf("--net values run into %q; want --cpus next", args[i+4])
	}
}

func TestLinkNICsAreUnmanagedInTheGuest(t *testing.T) {
	nics := []NicRecord{{Name: "eth1"}, {Name: "eth2"}}
	args := chArgs("vm", "/s", nil, "tap-a", "m", nics, LaunchOpts{})
	cmdline := args[slices.Index(args, "--cmdline")+1]

	const prefix = "systemd.set_credential_binary=network.network.10-plasma-links:"
	i := strings.Index(cmdline, prefix)
	if i < 0 {
		t.Fatalf("cmdline %q carries no network credential", cmdline)
	}
	unit, err := base64.StdEncoding.DecodeString(strings.Fields(cmdline[i+len(prefix):])[0])
	if err != nil {
		t.Fatalf("credential is not base64: %v", err)
	}
	want := "[Match]\nName=eth1 eth2\n\n[Link]\nUnmanaged=yes\n"
	if string(unit) != want {
		t.Errorf("unit = %q, want %q", unit, want)
	}
	// A VM without links boots exactly as before.
	plain := chArgs("vm", "/s", nil, "tap-a", "m", nil, LaunchOpts{})
	if got := plain[slices.Index(plain, "--cmdline")+1]; got != GuestCmdline {
		t.Errorf("cmdline without links = %q, want %q", got, GuestCmdline)
	}
}

func TestLinkInfoCredential(t *testing.T) {
	arg := linkInfoCredential("node-1", []NicRecord{
		{Name: "eth1", Bridge: "pbr-ctl", Address: "10.78.0.1/24"},
		{Name: "eth2", Bridge: "pbr-wire"},
		{Name: "eth3", Tap: "a2it-cap"},
	})
	const prefix = "systemd.set_credential_binary=plasma.nics:"
	if !strings.HasPrefix(arg, prefix) {
		t.Fatalf("arg %q lacks %q", arg, prefix)
	}
	body, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(arg, prefix))
	if err != nil {
		t.Fatal(err)
	}
	want := "vm node-1\neth1 pbr-ctl 10.78.0.1/24\neth2 pbr-wire -\neth3 host:a2it-cap -\n"
	if string(body) != want {
		t.Errorf("credential = %q, want %q", body, want)
	}

	// In a lab the guest is told the topology's names, not the host's.
	body = credential(t, linkInfoCredential("node-1", []NicRecord{
		{Name: "eth1", Bridge: "pbr-1a2b3c4d", Segment: "pbr-ctl", Address: "10.78.0.1/24"},
		{Name: "eth2", Tap: "pt3-7-eth2", Segment: "host:a2it-cap"},
	}))
	want = "vm node-1\neth1 pbr-ctl 10.78.0.1/24\neth2 host:a2it-cap -\n"
	if string(body) != want {
		t.Errorf("credential = %q, want %q", body, want)
	}
}

func credential(t *testing.T, arg string) []byte {
	t.Helper()
	body, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(arg, "systemd.set_credential_binary=plasma.nics:"))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestAddressConflict(t *testing.T) {
	running := []VmRecord{{ID: "node-1", Nics: []NicRecord{{Name: "eth2", Bridge: "pbr-wire", Address: "10.77.0.1/24"}}}}
	clash := []NicRecord{{Name: "eth2", Bridge: "pbr-wire", Address: "10.77.0.1/24"}}
	if err := addressConflict(clash, running); err == nil {
		t.Error("the same address on the same bridge was accepted")
	}
	// The same address on another segment is a different network.
	other := []NicRecord{{Name: "eth2", Bridge: "pbr-other", Address: "10.77.0.1/24"}}
	if err := addressConflict(other, running); err != nil {
		t.Errorf("same address on another bridge rejected: %v", err)
	}
}

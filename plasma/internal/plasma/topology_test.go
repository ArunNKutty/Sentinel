package plasma

import (
	"strings"
	"testing"
	"time"
)

func TestBuildTopology(t *testing.T) {
	start := time.Now().Add(-90 * time.Second)
	s := newTestState(t,
		VmRecord{ID: "host-1", Group: "host", IP: "10.0.0.2", MAC: "02:00:0a:00:00:02",
			Status: StatusRunning, TapDevice: "tap-host-1", StartedAt: start},
		VmRecord{ID: "host-2", Group: "host", IP: "10.0.0.3",
			Status: StatusExited, TapDevice: "tap-host-2"},
	)

	topo := BuildTopology(s, func() int64 { return time.Now().Unix() }, LinksDocument{})

	if len(topo.Planes) != 1 || topo.Planes[0].Bridge != Bridge {
		t.Fatalf("planes = %+v, want one %s plane", topo.Planes, Bridge)
	}
	if topo.Planes[0].CaptureNote == "" {
		t.Error("plane should carry the note that a bridge capture is not a fleet-wide mirror")
	}
	if len(topo.Nodes) != 2 || len(topo.Links) != 2 {
		t.Fatalf("got %d nodes / %d links, want 2 / 2", len(topo.Nodes), len(topo.Links))
	}

	running, exited := topo.Nodes[0], topo.Nodes[1]
	if !running.Capturable {
		t.Error("running VM should be capturable")
	}
	// A stopped VM's tap is gone, so it must not be offered for capture.
	if exited.Capturable {
		t.Error("exited VM must not be capturable")
	}
	if running.UptimeSecs < 89 || running.UptimeSecs > 92 {
		t.Errorf("uptime = %ds, want ~90s", running.UptimeSecs)
	}
	if exited.UptimeSecs != 0 {
		t.Errorf("exited uptime = %d, want 0", exited.UptimeSecs)
	}

	if topo.Links[0].From != "host-1" || topo.Links[0].To != Bridge ||
		topo.Links[0].Via != "tap-host-1" {
		t.Errorf("link = %+v, want host-1 -> %s via tap-host-1", topo.Links[0], Bridge)
	}
}

func TestBuildTopologyDerivesMissingTap(t *testing.T) {
	// Records written before VmRecord carried TapDevice must still map to the
	// right interface.
	s := newTestState(t,
		VmRecord{ID: "host-9", Group: "host", IP: "10.0.0.9", Status: StatusRunning},
	)
	topo := BuildTopology(s, func() int64 { return time.Now().Unix() }, LinksDocument{})
	if topo.Nodes[0].Tap != "tap-host-9" {
		t.Errorf("tap = %q, want derived tap-host-9", topo.Nodes[0].Tap)
	}
}

func TestBuildTopologyMergesDeclaredLinks(t *testing.T) {
	s := newTestState(t,
		VmRecord{ID: "a", Group: "a", IP: "10.0.0.2", Status: StatusRunning, TapDevice: "tap-a"},
		VmRecord{ID: "b", Group: "b", IP: "10.0.0.3", Status: StatusRunning, TapDevice: "tap-b"},
	)
	declared := LinksDocument{
		Topology: "lab",
		Links:    []DeclaredLink{{ANode: "a", AInterface: "eth1", BNode: "b", BInterface: "eth1"}},
	}

	topo := BuildTopology(s, func() int64 { return 0 }, declared)

	if topo.Declared != "lab" {
		t.Errorf("declared = %q, want lab", topo.Declared)
	}
	// Two real tap-to-bridge links plus the one declared link.
	if len(topo.Links) != 3 {
		t.Fatalf("got %d links, want 3", len(topo.Links))
	}
	var wired, unwired int
	for _, l := range topo.Links {
		if l.Wired {
			wired++
		} else {
			unwired++
		}
	}
	// The distinction matters: a declared link has no L2 segment behind it,
	// and must not be reported as though it does.
	if wired != 2 || unwired != 1 {
		t.Errorf("got %d wired / %d declared, want 2 / 1", wired, unwired)
	}
}

func TestBuildTopologyEmptyFleet(t *testing.T) {
	topo := BuildTopology(newTestState(t), func() int64 { return 0 }, LinksDocument{})
	if len(topo.Nodes) != 0 || len(topo.Links) != 0 {
		t.Errorf("empty fleet should have no nodes or links, got %+v", topo)
	}
	// The plane still exists so the bridge is reported (and capturable).
	if len(topo.Planes) != 1 {
		t.Errorf("want the plane present even with no VMs, got %+v", topo.Planes)
	}
}

func TestBuildTopologyWiredNics(t *testing.T) {
	s := newTestState(t,
		VmRecord{ID: "a", Group: "a", IP: "10.0.0.2", Status: StatusRunning, TapDevice: "tap-a",
			Nics: []NicRecord{
				{Name: "eth1", Tap: "pt2-eth1", Bridge: "pbr-wire"},
				{Name: "eth2", Tap: "cap0"},
			}},
		VmRecord{ID: "b", Group: "b", IP: "10.0.0.3", Status: StatusRunning, TapDevice: "tap-b",
			Nics: []NicRecord{{Name: "eth1", Tap: "pt3-eth1", Bridge: "pbr-wire"}}},
		VmRecord{ID: "c", Group: "c", IP: "10.0.0.4", Status: StatusExited, TapDevice: "tap-c",
			Nics: []NicRecord{{Name: "eth1", Tap: "pt4-eth1", Bridge: "pbr-wire"}}},
	)
	declared := LinksDocument{Topology: "lab", Links: []DeclaredLink{
		{ANode: "a", AInterface: "eth1", BNode: "wire", BInterface: "a"},
		{ANode: "b", AInterface: "eth1", BNode: "wire", BInterface: "b"},
		{ANode: "c", AInterface: "eth1", BNode: "wire", BInterface: "c"},
		{ANode: "a", AInterface: "eth2", BNode: "host", BInterface: "cap0"},
	}}

	topo := BuildTopology(s, func() int64 { return 0 }, declared)

	// The management plane, the shared bridge once, and the host tap.
	var planes []string
	for _, p := range topo.Planes {
		planes = append(planes, p.Bridge)
	}
	if strings.Join(planes, ",") != Bridge+",pbr-wire,cap0" {
		t.Errorf("planes = %v, want %s, pbr-wire, cap0", planes, Bridge)
	}

	var wired, unwired []string
	for _, l := range topo.Links {
		if l.To == Bridge {
			continue
		}
		if l.Wired {
			wired = append(wired, l.From+"->"+l.To+" via "+l.Via)
		} else {
			unwired = append(unwired, l.From)
		}
	}
	want := []string{"a->pbr-wire via pt2-eth1", "a->cap0 via cap0", "b->pbr-wire via pt3-eth1"}
	if strings.Join(wired, "; ") != strings.Join(want, "; ") {
		t.Errorf("wired = %v, want %v", wired, want)
	}
	// A wired link must not be drawn twice; only the stopped VM's link falls
	// back to its declared, dashed form.
	if len(unwired) != 1 || unwired[0] != "c" {
		t.Errorf("declared-only links from %v, want just c", unwired)
	}
}

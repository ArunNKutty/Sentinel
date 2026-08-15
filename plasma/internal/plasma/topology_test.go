package plasma

import (
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

	topo := BuildTopology(s, func() int64 { return time.Now().Unix() })

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
	// A stopped VM's tap is gone, so the UI must not offer a capture button.
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
	topo := BuildTopology(s, func() int64 { return time.Now().Unix() })
	if topo.Nodes[0].Tap != "tap-host-9" {
		t.Errorf("tap = %q, want derived tap-host-9", topo.Nodes[0].Tap)
	}
}

func TestBuildTopologyEmptyFleet(t *testing.T) {
	topo := BuildTopology(newTestState(t), func() int64 { return 0 })
	if len(topo.Nodes) != 0 || len(topo.Links) != 0 {
		t.Errorf("empty fleet should have no nodes or links, got %+v", topo)
	}
	// The plane still exists so the UI can render (and capture on) the bridge.
	if len(topo.Planes) != 1 {
		t.Errorf("want the plane present even with no VMs, got %+v", topo.Planes)
	}
}

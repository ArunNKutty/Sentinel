package plasma

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Topology is the fleet's wiring as the UI draws it. Today every VM hangs off
// a single bridge, so the graph is a star. The shape (planes / nodes / links
// rather than a flat VM list) anticipates M4.7, where planes become real
// (data, control, management), routers and NATs join as host-side constructs,
// and NEs sit as bumps on the links — none of which fits a flat list.

type Topology struct {
	Planes []Plane `json:"planes"`
	Nodes  []Node  `json:"nodes"`
	Links  []Link  `json:"links"`
	// Declared names the topology file last applied, if any.
	Declared string `json:"declared,omitempty"`
}

type Plane struct {
	Name   string `json:"name"`
	Bridge string `json:"bridge"`
	Subnet string `json:"subnet"`
	// Gateway is the host's address on the bridge.
	Gateway string `json:"gateway"`
	// CaptureTarget is what to pass to `plasma vm capture` for this plane.
	CaptureTarget string `json:"capture_target"`
	// CaptureNote records that a bridge capture is not a fleet-wide mirror, so
	// the UI can say so rather than implying it sees everything.
	CaptureNote string `json:"capture_note"`
}

type Node struct {
	ID     string `json:"id"`
	Group  string `json:"group"`
	Role   string `json:"role"`
	Status Status `json:"status"`
	IP     string `json:"ip"`
	MAC    string `json:"mac"`
	Tap    string `json:"tap"`
	PID    int    `json:"pid"`
	// Capturable is false for VMs that are not running, whose tap is gone.
	Capturable bool   `json:"capturable"`
	UptimeSecs int64  `json:"uptime_secs"`
	Plane      string `json:"plane"`
}

type Link struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Via is the host interface carrying the link, and the capture point for it.
	Via   string `json:"via"`
	Plane string `json:"plane"`
	// Wired distinguishes links that exist in the kernel (a VM's tap on the
	// bridge) from ones a topology file merely declared. Declared links have
	// no L2 segment of their own yet, and the UI must not imply otherwise.
	Wired bool `json:"wired"`
	// Label carries the declared interface pair, e.g. "eth1 <-> eth1".
	Label string `json:"label,omitempty"`
}

const defaultPlane = "data"

// BuildTopology derives the graph from current state plus whatever links the
// last applied topology file declared. Declared links are passed in rather
// than read from disk here so this stays a pure function of its inputs.
func BuildTopology(state *State, now func() int64, declared LinksDocument) Topology {
	plane := Plane{
		Name:          defaultPlane,
		Bridge:        Bridge,
		Subnet:        "10.0.0.0/24",
		Gateway:       "10.0.0.1",
		CaptureTarget: CaptureAllTarget,
		CaptureNote: "Bridge capture sees host-directed and broadcast traffic only; " +
			"VM-to-VM frames are forwarded port-to-port and do not appear here. " +
			"Capture a VM's tap to see its traffic.",
	}

	vms := state.List()
	topo := Topology{
		Planes: []Plane{plane},
		Nodes:  make([]Node, 0, len(vms)),
		Links:  make([]Link, 0, len(vms)),
	}

	for _, v := range vms {
		tap := v.TapDevice
		if tap == "" {
			tap = TapDeviceName(v.ID)
		}
		running := v.Status == StatusRunning

		var uptime int64
		if running && !v.StartedAt.IsZero() {
			uptime = now() - v.StartedAt.Unix()
			if uptime < 0 {
				uptime = 0
			}
		}

		topo.Nodes = append(topo.Nodes, Node{
			ID:         v.ID,
			Group:      v.Group,
			Role:       v.Group,
			Status:     v.Status,
			IP:         v.IP,
			MAC:        v.MAC,
			Tap:        tap,
			PID:        v.PID,
			Capturable: running,
			UptimeSecs: uptime,
			Plane:      defaultPlane,
		})
		topo.Links = append(topo.Links, Link{
			From:  v.ID,
			To:    Bridge,
			Via:   tap,
			Plane: defaultPlane,
			Wired: true,
		})
	}

	// Overlay whatever the last applied topology file declared. These are
	// intent, not kernel state, so they are marked unwired.
	topo.Declared = declared.Topology
	for _, d := range declared.Links {
		topo.Links = append(topo.Links, Link{
			From:  d.ANode,
			To:    d.BNode,
			Plane: defaultPlane,
			Wired: false,
			Label: strings.TrimSpace(d.AInterface + " ↔ " + d.BInterface),
		})
	}

	return topo
}

// SaveLinks persists the links an applied topology declared.
func SaveLinks(doc LinksDocument) error {
	if err := os.MkdirAll(filepath.Dir(LinksFile), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := LinksFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, LinksFile)
}

// LoadLinks reads the declared links; a missing file simply means no topology
// has been applied.
func LoadLinks() (LinksDocument, error) {
	var doc LinksDocument
	data, err := os.ReadFile(LinksFile)
	if err != nil {
		return doc, err
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, err
	}
	return doc, nil
}

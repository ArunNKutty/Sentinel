package plasma

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Topology is the fleet's wiring as /v1/topology reports it. Every VM hangs off the
// management bridge, and each link a topology wired adds a plane of its own:
// a link bridge, or a host endpoint's unbridged tap. Routers and NATs join as
// host-side constructs in M4.7.

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
	// a reader can say so rather than implying it sees everything.
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
	// no L2 segment of their own yet, and a reader must not imply otherwise.
	Wired bool `json:"wired"`
	// Label carries the declared interface pair, e.g. "eth1 <-> eth1".
	Label string `json:"label,omitempty"`
}

const defaultPlane = "data"

// BuildTopology derives the legacy fleet's graph from current state plus
// whatever links the last applied topology file declared. Declared links are
// passed in rather than read from disk here so this stays a pure function of
// its inputs.
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
	return buildTopology(state.InLab(""), plane, false, now, declared)
}

// buildTopology is the graph for one set of VMs on one management plane. In a
// lab, planes and links carry the topology's own segment names, and capture
// targets are the lab's (a bridge node's name, <vm>:<ethN>).
func buildTopology(vms []VmRecord, plane Plane, lab bool, now func() int64, declared LinksDocument) Topology {
	// wired is keyed "vm:ethN" for every link NIC of a running VM.
	wired := make(map[string]bool)
	seenPlane := make(map[string]bool)
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
			Plane:      plane.Name,
		})
		topo.Links = append(topo.Links, Link{
			From:  v.ID,
			To:    plane.Bridge,
			Via:   tap,
			Plane: plane.Name,
			Wired: true,
		})

		for _, n := range v.Nics {
			wired[v.ID+":"+n.Name] = running
			if !running {
				continue
			}
			to := n.Bridge
			if to == "" {
				to = n.Tap
			}
			if n.Segment != "" {
				to = n.Segment
			}
			if !seenPlane[to] {
				seenPlane[to] = true
				p := linkPlane(v.ID, n)
				if lab {
					p.Name = to
					p.CaptureTarget = v.ID + ":" + n.Name
					if strings.HasPrefix(n.Segment, NodeBridgePrefix) {
						p.CaptureTarget = strings.TrimPrefix(n.Segment, NodeBridgePrefix)
					}
				}
				topo.Planes = append(topo.Planes, p)
			}
			topo.Links = append(topo.Links, Link{
				From:  v.ID,
				To:    to,
				Via:   n.Tap,
				Plane: to,
				Wired: true,
				Label: strings.TrimSpace(n.Name + " " + n.Address),
			})
		}
	}

	// Overlay what the last applied topology file declared, for links whose
	// VM end is not wired (not running, or never launched). These are intent,
	// not kernel state, so they are marked unwired.
	topo.Declared = declared.Topology
	for _, d := range declared.Links {
		if wired[d.ANode+":"+d.AInterface] || wired[d.BNode+":"+d.BInterface] {
			continue
		}
		topo.Links = append(topo.Links, Link{
			From:  d.ANode,
			To:    d.BNode,
			Plane: plane.Name,
			Wired: false,
			Label: strings.TrimSpace(d.AInterface + " ↔ " + d.BInterface),
		})
	}

	return topo
}

// linkPlane describes the segment behind a link NIC, as first seen from VM id.
func linkPlane(id string, n NicRecord) Plane {
	if n.Bridge == "" {
		return Plane{
			Name:          "host:" + n.Tap,
			Bridge:        n.Tap,
			CaptureTarget: id + ":" + n.Name,
			CaptureNote:   "An unbridged host tap: its only traffic is " + id + "'s " + n.Name + ".",
		}
	}
	return Plane{
		Name:          n.Bridge,
		Bridge:        n.Bridge,
		CaptureTarget: n.Bridge,
		CaptureNote: "A link bridge: capturing on it sees every frame between the VMs on this " +
			"segment, since none of them is addressed to the host.",
	}
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

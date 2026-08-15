package plasma

// Topology is the fleet's wiring as the UI draws it. Today every VM hangs off
// a single bridge, so the graph is a star. The shape (planes / nodes / links
// rather than a flat VM list) anticipates M4.7, where planes become real
// (data, control, management), routers and NATs join as host-side constructs,
// and NEs sit as bumps on the links — none of which fits a flat list.

type Topology struct {
	Planes []Plane `json:"planes"`
	Nodes  []Node  `json:"nodes"`
	Links  []Link  `json:"links"`
}

type Plane struct {
	Name   string `json:"name"`
	Bridge string `json:"bridge"`
	Subnet string `json:"subnet"`
	// Gateway is the host's address on the bridge.
	Gateway string `json:"gateway"`
	// CaptureTarget is what to pass to `plasma capture` for this plane.
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
}

const defaultPlane = "data"

// BuildTopology derives the graph from current state.
func BuildTopology(state *State, now func() int64) Topology {
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
		})
	}

	return topo
}

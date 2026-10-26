package plasma

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// A lab is one applied topology and everything it created: its VMs, its
// management network, its link segments, its tc state. Names inside a lab
// (fabric, node-1, wire) are scoped to it, so two runs never collide, and
// deleting a lab removes exactly what it made.

type LabPhase string

const (
	LabCreating LabPhase = "creating"
	LabRunning  LabPhase = "running"
	LabFailed   LabPhase = "failed"
	LabDeleting LabPhase = "deleting"
)

// LabsFile persists lab records beside state.json.
const LabsFile = "/var/lib/plasma/labs.json"

// ReadyMarker is what the guest bootstrap writes to its virtio console once
// its applications are started (see scripts/app-prep.sh).
const ReadyMarker = "PLASMA-READY"

var (
	ErrLabNotFound = errors.New("no such lab")
	ErrLabExists   = errors.New("lab already exists")
	ErrNoCapacity  = errors.New("engine does not have the capacity")
	ErrBadLab      = errors.New("invalid lab")
	// ErrNotSet is a mirror or drop rule asked about where none is set.
	ErrNotSet = errors.New("not set")
)

var labNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

type LabRecord struct {
	ID        string    `json:"id"`
	Index     int       `json:"index"`
	Topology  string    `json:"topology,omitempty"`
	Phase     LabPhase  `json:"phase"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// Nodes are the lab's VM IDs in launch order.
	Nodes   []string       `json:"nodes"`
	Bridges []string       `json:"bridges,omitempty"`
	Links   []DeclaredLink `json:"links,omitempty"`
	// Failed maps a node that did not launch to why.
	Failed  map[string]string     `json:"failed,omitempty"`
	Mirrors map[string]string     `json:"mirrors,omitempty"`
	Filters map[string]LinkFilter `json:"filters,omitempty"`
}

func (r *LabRecord) net() LabNet { return LabNet{ID: r.ID, Index: r.Index} }

type MgmtNetwork struct {
	Bridge  string `json:"bridge"`
	Subnet  string `json:"subnet"`
	Gateway string `json:"gateway"`
}

type LabVM struct {
	ID     string `json:"id"`
	Group  string `json:"group"`
	Status Status `json:"status"`
	IP     string `json:"ip"`
	// Ready means the guest reported its applications started.
	Ready     bool        `json:"ready"`
	CPUs      int         `json:"cpus,omitempty"`
	Memory    string      `json:"memory,omitempty"`
	Nics      []NicRecord `json:"nics,omitempty"`
	Apps      []string    `json:"apps,omitempty"`
	StartedAt time.Time   `json:"started_at"`
}

// LabStatus is GET /v1/labs/{lab}.
type LabStatus struct {
	LabRecord
	// Ready is true once every node launched and reported ready.
	Ready bool        `json:"ready"`
	Mgmt  MgmtNetwork `json:"mgmt"`
	VMs   []LabVM     `json:"vms"`
}

// CreateLabRequest is POST /v1/labs.
type CreateLabRequest struct {
	// Name is the lab ID; generated from the topology's name when empty.
	Name string `json:"name,omitempty"`
	// Topology is the topology file's YAML, as written.
	Topology string `json:"topology"`
	// Files maps each path the topology names to the artifact holding it,
	// e.g. {"../dist/neapp": "sha256:..."}.
	Files map[string]string `json:"files,omitempty"`
}

type Labs struct {
	mu       sync.Mutex
	path     string
	labs     map[string]*LabRecord
	applying map[string]*labApply
	ready    map[string]bool
	// drops are armed drop rules, keyed "<lab>|<vm:ethN>". In memory only:
	// their tc filters do not outlive the taps, and a one-shot rule's watcher
	// does not outlive the daemon.
	drops map[string]*dropState

	state    *State
	store    *ArtifactStore
	launchMu *sync.Mutex
	log      *slog.Logger
}

type labApply struct {
	cancel context.CancelFunc
	done   chan struct{}
	// need is the lab's total memory, held against capacity until its VMs
	// are running and counted for themselves.
	need int64
}

// LoadLabs reads the lab records. A lab caught mid-create or mid-delete by a
// daemon restart is marked failed: its goroutine is gone, and only a delete
// can bring it to a known state.
func LoadLabs(path string, state *State, store *ArtifactStore, launchMu *sync.Mutex, log *slog.Logger) (*Labs, error) {
	l := &Labs{
		path: path, labs: map[string]*LabRecord{}, applying: map[string]*labApply{},
		ready: map[string]bool{}, drops: map[string]*dropState{}, state: state, store: store, launchMu: launchMu, log: log,
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(data) > 0 {
		var recs []*LabRecord
		if err := json.Unmarshal(data, &recs); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for _, r := range recs {
			if r.Phase == LabCreating || r.Phase == LabDeleting {
				r.Error = fmt.Sprintf("plasmad restarted while the lab was %s; delete it", r.Phase)
				r.Phase = LabFailed
			}
			l.labs[r.ID] = r
		}
	}
	return l, nil
}

func (l *Labs) saveLocked() error {
	recs := make([]*LabRecord, 0, len(l.labs))
	for _, r := range l.labs {
		recs = append(recs, r)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Index < recs[j].Index })
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// Count is the number of labs.
func (l *Labs) Count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.labs)
}

// Create validates a lab and starts bringing it up in the background; poll
// Status for its progress. Everything that can be checked without booting a
// VM — the topology, its artifacts, the name, the engine's capacity — is
// checked here, so a bad request fails at once rather than minutes in.
func (l *Labs) Create(req CreateLabRequest) (LabStatus, error) {
	topo, err := ResolveLabTopology([]byte(req.Topology), req.Files, l.store)
	if err != nil {
		return LabStatus{}, fmt.Errorf("%w: %v", ErrBadLab, err)
	}

	id := req.Name
	if id == "" {
		id, err = generateLabID(topo.Name)
		if err != nil {
			return LabStatus{}, err
		}
	}
	if !labNamePattern.MatchString(id) {
		return LabStatus{}, fmt.Errorf("%w: name %q must be lowercase letters, digits and '-', at most 40", ErrBadLab, id)
	}

	var need int64
	for _, n := range topo.Nodes {
		m, err := ParseMemory(n.Memory)
		if err != nil {
			return LabStatus{}, fmt.Errorf("%w: node %s: %v", ErrBadLab, n.ID, err)
		}
		need += m
	}

	l.mu.Lock()
	if _, taken := l.labs[id]; taken {
		l.mu.Unlock()
		return LabStatus{}, fmt.Errorf("%w: %s", ErrLabExists, id)
	}
	index := l.freeIndexLocked()
	if index == 0 {
		l.mu.Unlock()
		return LabStatus{}, fmt.Errorf("%w: all %d lab networks are in use", ErrNoCapacity, MaxLabs)
	}
	if c := ComputeCapacity(l.state.List(), len(l.labs)); need > c.MemoryFree-l.pendingLocked() {
		l.mu.Unlock()
		return LabStatus{}, fmt.Errorf("%w: the lab needs %d MiB of memory, %d MiB is free",
			ErrNoCapacity, need>>20, c.MemoryFree>>20)
	}

	rec := &LabRecord{
		ID: id, Index: index, Topology: topo.Name, Phase: LabCreating, CreatedAt: time.Now().UTC(),
		Bridges: topo.Bridges, Links: topo.Links,
	}
	for _, n := range topo.Nodes {
		rec.Nodes = append(rec.Nodes, n.ID)
	}
	l.labs[id] = rec
	if err := l.saveLocked(); err != nil {
		delete(l.labs, id)
		l.mu.Unlock()
		return LabStatus{}, fmt.Errorf("persist lab: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &labApply{cancel: cancel, done: make(chan struct{}), need: need}
	l.applying[id] = run
	l.mu.Unlock()

	l.log.Info("lab creating", "lab", id, "index", index, "nodes", len(topo.Nodes))
	go l.apply(ctx, run, rec.net(), topo)
	return l.Status(id)
}

// pendingLocked is memory promised to labs still launching, less what their
// already-running VMs count on their own.
func (l *Labs) pendingLocked() int64 {
	var total int64
	for id, run := range l.applying {
		p := run.need
		for _, v := range l.state.InLab(id) {
			if m, err := ParseMemory(v.Memory); err == nil && v.Status == StatusRunning {
				p -= m
			}
		}
		total += max(p, 0)
	}
	return total
}

func (l *Labs) freeIndexLocked() int {
	used := map[int]bool{}
	for _, r := range l.labs {
		used[r.Index] = true
	}
	for i := 1; i <= MaxLabs; i++ {
		if !used[i] {
			return i
		}
	}
	return 0
}

func generateLabID(topology string) (string, error) {
	base := strings.ToLower(topology)
	base = regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	if len(base) > 30 {
		base = base[:30]
	}
	if base == "" {
		base = "lab"
	}
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base + "-" + hex.EncodeToString(b), nil
}

// apply launches every node in order. Nodes that fail are recorded and the
// rest still launch, so a client sees everything that went wrong at once.
func (l *Labs) apply(ctx context.Context, run *labApply, net LabNet, topo *ResolvedTopology) {
	defer close(run.done)

	failed := map[string]string{}
	if err := ensureMgmtNetwork(net); err != nil {
		for _, n := range topo.Nodes {
			failed[n.ID] = "management network: " + strings.TrimSpace(err.Error())
		}
	} else {
		for _, n := range topo.Nodes {
			if ctx.Err() != nil {
				failed[n.ID] = "lab deleted before it launched"
				continue
			}
			l.launchMu.Lock()
			rec, err := LaunchNamed(n.ID, n.Group, LaunchOpts{
				CPUs: n.CPUs, Memory: n.Memory, Apps: n.Apps, Nics: n.Nics, Net: net,
			}, l.state)
			l.launchMu.Unlock()
			if err != nil {
				l.log.Error("lab node failed", "lab", net.ID, "node", n.ID, "err", err)
				failed[n.ID] = strings.TrimSpace(err.Error())
				continue
			}
			l.log.Info("lab node launched", "lab", net.ID, "node", rec.ID, "ip", rec.IP, "pid", rec.PID)
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.labs[net.ID]
	if !ok || r.Phase != LabCreating {
		return // being deleted
	}
	r.Phase = LabRunning
	if len(failed) > 0 {
		r.Phase = LabFailed
		r.Failed = failed
		r.Error = fmt.Sprintf("%d of %d node(s) failed to launch", len(failed), len(topo.Nodes))
	}
	delete(l.applying, net.ID)
	if err := l.saveLocked(); err != nil {
		l.log.Error("persist lab", "lab", net.ID, "err", err)
	}
	l.log.Info("lab applied", "lab", net.ID, "phase", r.Phase)
}

// Delete removes a lab and everything it created. A lab still being created
// is stopped first. VMs are torn down in parallel: each takes up to a few
// seconds to stop, and a 100-node lab would otherwise take minutes.
func (l *Labs) Delete(id string) error {
	l.mu.Lock()
	r, ok := l.labs[id]
	if !ok {
		l.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrLabNotFound, id)
	}
	r.Phase = LabDeleting
	l.saveLocked()
	run := l.applying[id]
	net := r.net()
	l.mu.Unlock()

	if run != nil {
		run.cancel()
		<-run.done
	}

	vms := l.state.InLab(id)
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, v := range vms {
		wg.Add(1)
		sem <- struct{}{}
		go func(vm string) {
			defer wg.Done()
			defer func() { <-sem }()
			if _, err := Delete(net, vm, l.state); err != nil {
				l.log.Warn("lab vm delete", "lab", id, "vm", vm, "err", err)
			}
		}(v.ID)
	}
	wg.Wait()
	teardownMgmtNetwork(net)
	os.RemoveAll(filepath.Join(LabsRuntimeDir, id))

	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.labs, id)
	delete(l.applying, id)
	for k := range l.ready {
		if strings.HasPrefix(k, id+"/") {
			delete(l.ready, k)
		}
	}
	for k, d := range l.drops {
		if strings.HasPrefix(k, id+"|") {
			d.cancel()
			delete(l.drops, k)
		}
	}
	l.log.Info("lab deleted", "lab", id, "vms", len(vms))
	return l.saveLocked()
}

func (l *Labs) record(id string) (LabRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.labs[id]
	if !ok {
		return LabRecord{}, fmt.Errorf("%w: %s", ErrLabNotFound, id)
	}
	c := *r
	return c, nil
}

// Status reports a lab and each of its VMs.
func (l *Labs) Status(id string) (LabStatus, error) {
	r, err := l.record(id)
	if err != nil {
		return LabStatus{}, err
	}
	net := r.net()
	st := LabStatus{
		LabRecord: r,
		Mgmt:      MgmtNetwork{Bridge: net.MgmtBridge(), Subnet: net.Subnet(), Gateway: net.Gateway()},
	}
	byID := map[string]VmRecord{}
	for _, v := range l.state.InLab(id) {
		byID[v.ID] = v
	}
	allReady := r.Phase == LabRunning && len(r.Nodes) > 0
	for _, node := range r.Nodes {
		v, ok := byID[node]
		if !ok {
			allReady = false
			continue
		}
		vm := LabVM{
			ID: v.ID, Group: v.Group, Status: v.Status, IP: v.IP, CPUs: v.CPUs, Memory: v.Memory,
			Nics: v.Nics, StartedAt: v.StartedAt,
		}
		for _, a := range v.Apps {
			vm.Apps = append(vm.Apps, a.Name)
		}
		vm.Ready = v.Status == StatusRunning && processAlive(v.PID) && l.vmReady(net, v)
		if !vm.Ready {
			allReady = false
		}
		st.VMs = append(st.VMs, vm)
	}
	st.Ready = allReady
	return st, nil
}

// vmReady looks for the guest's ready marker on its console, caching a hit:
// the marker never goes away for that boot.
func (l *Labs) vmReady(net LabNet, v VmRecord) bool {
	key := net.ID + "/" + v.ID + "@" + v.StartedAt.String()
	l.mu.Lock()
	hit := l.ready[key]
	l.mu.Unlock()
	if hit {
		return true
	}
	if !consoleHasMarker(net.ConsoleLogPath(v.ID)) {
		return false
	}
	l.mu.Lock()
	l.ready[key] = true
	l.mu.Unlock()
	return true
}

func consoleHasMarker(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	// The marker comes early in boot; bound the read in case a getty on the
	// console fills the file.
	data, _ := io.ReadAll(io.LimitReader(f, 4<<20))
	return strings.Contains(string(data), ReadyMarker)
}

// List reports every lab.
func (l *Labs) List() []LabStatus {
	l.mu.Lock()
	ids := make([]string, 0, len(l.labs))
	for id := range l.labs {
		ids = append(ids, id)
	}
	l.mu.Unlock()
	sort.Strings(ids)
	out := make([]LabStatus, 0, len(ids))
	for _, id := range ids {
		if st, err := l.Status(id); err == nil {
			out = append(out, st)
		}
	}
	return out
}

// VM finds a running VM of a lab.
func (l *Labs) VM(lab, id string) (LabNet, VmRecord, error) {
	r, err := l.record(lab)
	if err != nil {
		return LabNet{}, VmRecord{}, err
	}
	v, ok := l.state.Get(lab, id)
	if !ok {
		return LabNet{}, VmRecord{}, fmt.Errorf("%w: %s in lab %s", ErrNotFound, id, lab)
	}
	if v.Status != StatusRunning || !processAlive(v.PID) {
		return LabNet{}, VmRecord{}, fmt.Errorf("%w: %s is %s", ErrNotRunning, id, v.Status)
	}
	return r.net(), v, nil
}

// CaptureTarget maps a lab capture target to a host interface: "mgmt" (the
// management bridge), "<vm>" (its management tap), "<vm>:<ethN>" (a link
// NIC's tap) or the name of a `kind: bridge` node (that whole segment).
func (l *Labs) CaptureTarget(lab, target string) (string, error) {
	r, err := l.record(lab)
	if err != nil {
		return "", err
	}
	net := r.net()
	if target == "mgmt" {
		return net.MgmtBridge(), nil
	}
	for _, b := range r.Bridges {
		if b == target {
			br := net.HostBridge(NodeBridgeName(b))
			if _, err := os.Stat(filepath.Join("/sys/class/net", br)); err != nil {
				return "", fmt.Errorf("%w: bridge %s has no running VM on it", ErrNotRunning, b)
			}
			return br, nil
		}
	}
	if vm, nic, ok := strings.Cut(target, ":"); ok {
		_, n, err := l.nic(lab, vm, nic)
		return n.Tap, err
	}
	_, v, err := l.VM(lab, target)
	if err != nil {
		return "", err
	}
	return v.TapDevice, nil
}

// nic finds a running VM's link NIC.
func (l *Labs) nic(lab, vm, name string) (VmRecord, NicRecord, error) {
	_, v, err := l.VM(lab, vm)
	if err != nil {
		return VmRecord{}, NicRecord{}, err
	}
	for _, n := range v.Nics {
		if n.Name == name {
			return v, n, nil
		}
	}
	return VmRecord{}, NicRecord{}, fmt.Errorf("%w: %s has no link NIC %s", ErrNotFound, vm, name)
}

func (l *Labs) linkNIC(lab, link string) (VmRecord, NicRecord, error) {
	vm, nic, ok := strings.Cut(link, ":")
	if !ok {
		return VmRecord{}, NicRecord{}, fmt.Errorf("%w: link %q: want <vm>:<ethN>", ErrBadLab, link)
	}
	return l.nic(lab, vm, nic)
}

// peer is the other end of a VM-to-VM link, if this NIC is on one.
func (l *Labs) peer(lab string, v VmRecord, n NicRecord) (NicRecord, bool) {
	if !strings.HasPrefix(n.Bridge, LinkBridgePrefix) {
		return NicRecord{}, false
	}
	for _, o := range l.state.InLab(lab) {
		for _, on := range o.Nics {
			if on.Bridge == n.Bridge && !(o.ID == v.ID && on.Name == n.Name) {
				return on, true
			}
		}
	}
	return NicRecord{}, false
}

func (l *Labs) update(lab string, fn func(*LabRecord)) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.labs[lab]
	if !ok {
		return fmt.Errorf("%w: %s", ErrLabNotFound, lab)
	}
	fn(r)
	return l.saveLocked()
}

// SetFilter impairs the frames delivered to a link NIC (and its peer's, with
// BothEnds on a VM-to-VM link).
func (l *Labs) SetFilter(lab, link string, f LinkFilter) error {
	if _, err := f.netemArgs(); err != nil {
		return fmt.Errorf("%w: %v", ErrBadLab, err)
	}
	v, n, err := l.linkNIC(lab, link)
	if err != nil {
		return err
	}
	taps := []string{n.Tap}
	if f.BothEnds {
		p, ok := l.peer(lab, v, n)
		if !ok {
			return fmt.Errorf("%w: both_ends needs a VM-to-VM link; %s is on %s", ErrBadLab, link, n.Bridge)
		}
		taps = append(taps, p.Tap)
	}
	for _, t := range taps {
		if err := applyFilter(t, f); err != nil {
			return err
		}
	}
	return l.update(lab, func(r *LabRecord) {
		if r.Filters == nil {
			r.Filters = map[string]LinkFilter{}
		}
		r.Filters[link] = f
	})
}

func (l *Labs) ClearFilter(lab, link string) error {
	v, n, err := l.linkNIC(lab, link)
	if err != nil {
		return err
	}
	r, _ := l.record(lab)
	taps := []string{n.Tap}
	if r.Filters[link].BothEnds {
		if p, ok := l.peer(lab, v, n); ok {
			taps = append(taps, p.Tap)
		}
	}
	for _, t := range taps {
		if err := clearFilter(t); err != nil {
			return err
		}
	}
	return l.update(lab, func(r *LabRecord) { delete(r.Filters, link) })
}

// SetMirror copies a link NIC's traffic, both directions, out of another NIC
// of the same lab.
func (l *Labs) SetMirror(lab, link, to string) error {
	if link == to {
		return fmt.Errorf("%w: a NIC cannot mirror to itself", ErrBadLab)
	}
	_, src, err := l.linkNIC(lab, link)
	if err != nil {
		return err
	}
	_, dst, err := l.linkNIC(lab, to)
	if err != nil {
		return err
	}
	if err := applyMirror(src.Tap, dst.Tap); err != nil {
		return err
	}
	return l.update(lab, func(r *LabRecord) {
		if r.Mirrors == nil {
			r.Mirrors = map[string]string{}
		}
		r.Mirrors[link] = to
	})
}

func (l *Labs) ClearMirror(lab, link string) error {
	_, src, err := l.linkNIC(lab, link)
	if err != nil {
		return err
	}
	if err := clearMirror(src.Tap); err != nil {
		return err
	}
	return l.update(lab, func(r *LabRecord) { delete(r.Mirrors, link) })
}

// Topology reports a lab's wiring as a graph, like /v1/topology does for the
// legacy fleet.
func (l *Labs) Topology(lab string) (Topology, error) {
	r, err := l.record(lab)
	if err != nil {
		return Topology{}, err
	}
	net := r.net()
	mgmt := Plane{
		Name: "mgmt", Bridge: net.MgmtBridge(), Subnet: net.Subnet(), Gateway: net.Gateway(),
		CaptureTarget: "mgmt",
		CaptureNote:   "Bridge capture sees host-directed and broadcast traffic only; capture a VM to see its traffic.",
	}
	return buildTopology(l.state.InLab(lab), mgmt, true, func() int64 { return time.Now().Unix() },
		LinksDocument{Topology: r.Topology, Links: r.Links}), nil
}

// MirrorStatus reports a mirror and how much it has copied each way.
func (l *Labs) MirrorStatus(lab, link string) (MirrorStatus, error) {
	r, err := l.record(lab)
	if err != nil {
		return MirrorStatus{}, err
	}
	to, ok := r.Mirrors[link]
	if !ok {
		return MirrorStatus{}, fmt.Errorf("%w: no mirror on %s", ErrNotSet, link)
	}
	_, n, err := l.linkNIC(lab, link)
	if err != nil {
		return MirrorStatus{}, err
	}
	return MirrorStatus{
		Link: link, To: to,
		Ingress: filterCount(n.Tap, "ingress", mirrorPref),
		Egress:  filterCount(n.Tap, "egress", mirrorPref),
	}, nil
}

type dropState struct {
	tap    string
	once   bool
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	done    bool
	dropped int64
}

func (d *dropState) status(link string) DropStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := DropStatus{Link: link, Once: d.once, Done: d.done, Armed: !d.done, Dropped: d.dropped}
	if !d.done {
		st.Dropped = dropCount(d.tap)
	}
	return st
}

// finish removes the rules and keeps the final count, unless the request
// was replaced or cleared meanwhile: then its rules are no longer this
// state's to remove.
func (d *dropState) finish() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.done || d.ctx.Err() != nil {
		return
	}
	d.dropped = dropCount(d.tap)
	clearDrop(d.tap)
	d.done = true
}

// watch ends a one-shot request on its first drop, or at its timeout.
func (d *dropState) watch(timeout time.Duration) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-deadline.C:
			d.finish()
			return
		case <-tick.C:
			if dropCount(d.tap) > 0 {
				d.finish()
				return
			}
		}
	}
}

// SetDrop arms drop rules on a link NIC, replacing any already there.
func (l *Labs) SetDrop(lab, link string, req DropRequest) (DropStatus, error) {
	if err := req.validate(); err != nil {
		return DropStatus{}, fmt.Errorf("%w: %v", ErrBadLab, err)
	}
	timeout, _ := req.timeout()
	_, n, err := l.linkNIC(lab, link)
	if err != nil {
		return DropStatus{}, err
	}
	key := lab + "|" + link

	l.mu.Lock()
	old := l.drops[key]
	delete(l.drops, key)
	l.mu.Unlock()
	if old != nil {
		old.cancel()
		old.mu.Lock() // wait out a finish in progress
		old.mu.Unlock()
	}

	if err := installDrop(n.Tap, req.Rules); err != nil {
		return DropStatus{}, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &dropState{tap: n.Tap, once: req.Once, ctx: ctx, cancel: cancel}
	l.mu.Lock()
	l.drops[key] = d
	l.mu.Unlock()
	if req.Once {
		go d.watch(timeout)
	}
	return d.status(link), nil
}

func (l *Labs) DropStatus(lab, link string) (DropStatus, error) {
	if _, err := l.record(lab); err != nil {
		return DropStatus{}, err
	}
	l.mu.Lock()
	d := l.drops[lab+"|"+link]
	l.mu.Unlock()
	if d == nil {
		return DropStatus{}, fmt.Errorf("%w: no drop rules on %s", ErrNotSet, link)
	}
	return d.status(link), nil
}

// ClearDrop removes a link NIC's drop rules and reports what they dropped.
func (l *Labs) ClearDrop(lab, link string) (DropStatus, error) {
	_, n, err := l.linkNIC(lab, link)
	if err != nil {
		return DropStatus{}, err
	}
	key := lab + "|" + link
	l.mu.Lock()
	d := l.drops[key]
	delete(l.drops, key)
	l.mu.Unlock()
	if d == nil {
		clearDrop(n.Tap)
		return DropStatus{Link: link}, nil
	}
	d.cancel()
	d.mu.Lock()
	if !d.done {
		d.dropped = dropCount(d.tap)
		clearDrop(d.tap)
		d.done = true
	}
	st := DropStatus{Link: link, Once: d.once, Done: true, Dropped: d.dropped}
	d.mu.Unlock()
	return st, nil
}

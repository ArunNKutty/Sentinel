package plasma

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Topology files follow containerlab's shape — name / topology with
// defaults, kinds, groups, nodes and links, resolved
// node -> group -> kind -> defaults — so the format is familiar and the
// mental model transfers. Three deliberate differences:
//
//   - `count` expands one node entry into a fleet. containerlab has no such
//     field, but declaring 50 NEs one stanza at a time is not workable.
//   - `apps` deploy per-node payloads (a binary, a .ko) into the VM. Also a
//     plasma addition: containerlab starts a container image, we start VMs
//     from one base rootfs and hand each node its own applications.
//   - `links` become NICs, eth1 upward; eth0 is always the management NIC on
//     plasma-br0. A link between two VMs gets a private two-port bridge. A
//     link to a `kind: bridge` node joins that node's host bridge, which is
//     how more than two VMs share one segment; a bridge with a `subnet` also
//     gives each VM NIC on it an address (see allocateAddresses). A link to
//     `host:<ifname>` leaves the VM's tap unbridged on the host under that
//     name (containerlab's host endpoint), e.g. as a port to mirror into.

// KindVM is a node plasma boots.
const KindVM = "vm"

// KindBridge is a node that is a host Linux bridge rather than a VM: an L2
// segment links can attach VMs to. Like containerlab's bridge kind, except
// plasma creates the bridge (named NodeBridgeName) instead of expecting one.
const KindBridge = "bridge"

// HostEndpoint is the reserved node name for containerlab's host endpoint.
const HostEndpoint = "host"

type TopoFile struct {
	Name     string      `yaml:"name"`
	Topology TopoSection `yaml:"topology"`
}

type TopoSection struct {
	Defaults NodeSpec            `yaml:"defaults"`
	Kinds    map[string]NodeSpec `yaml:"kinds"`
	Groups   map[string]NodeSpec `yaml:"groups"`
	Nodes    map[string]NodeSpec `yaml:"nodes"`
	Links    []TopoLink          `yaml:"links"`
}

// NodeSpec is shared by defaults/kinds/groups/nodes so the inheritance chain
// can merge them uniformly. Zero values mean "inherit".
type NodeSpec struct {
	Kind   string `yaml:"kind"`
	Group  string `yaml:"group"`
	CPUs   int    `yaml:"cpus"`
	Memory string `yaml:"memory"`
	Count  int    `yaml:"count"`
	// Subnet (bridge nodes only) is the segment's IPv4 prefix, e.g.
	// 10.77.0.0/24. Every VM NIC on the bridge is then given an address from
	// it, so a fleet's link addresses are allocated, not written out.
	Subnet string `yaml:"subnet"`
	// Apps are deployed onto the node and started at boot. Inherited whole:
	// a node that declares any apps replaces the group's list rather than
	// adding to it, so what a node runs is readable from one stanza.
	Apps []AppSpec `yaml:"apps"`
}

type TopoLink struct {
	Type      string         `yaml:"type"`
	Endpoints []LinkEndpoint `yaml:"endpoints"`
}

type LinkEndpoint struct {
	Node      string `yaml:"node"`
	Interface string `yaml:"interface"`
	// IPv4 pins this VM end's address on a bridge with a subnet, e.g. a
	// well-known service address; every other end is allocated one.
	IPv4 string `yaml:"ipv4"`
}

// UnmarshalYAML accepts containerlab's brief form ("node:iface") as well as
// its extended mapping form, since both appear throughout their docs.
func (e *LinkEndpoint) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		var s string
		if err := value.Decode(&s); err != nil {
			return err
		}
		node, iface, ok := strings.Cut(s, ":")
		if !ok {
			return fmt.Errorf("endpoint %q must be \"node:interface\"", s)
		}
		e.Node, e.Interface = node, iface
		return nil
	}
	// The mapping form decodes outside the file's KnownFields check, so a
	// misspelt key (ipv6:, iface:) would otherwise vanish silently.
	for i := 0; i+1 < len(value.Content); i += 2 {
		switch k := value.Content[i].Value; k {
		case "node", "interface", "ipv4":
		default:
			return fmt.Errorf("link endpoint: unknown field %q (want node, interface, ipv4)", k)
		}
	}
	type raw LinkEndpoint // avoid recursing into this method
	var r raw
	if err := value.Decode(&r); err != nil {
		return err
	}
	*e = LinkEndpoint(r)
	if e.Node == "" {
		return fmt.Errorf("link endpoint is missing 'node'")
	}
	return nil
}

// ResolvedNode is one concrete VM to launch, after inheritance and count
// expansion.
type ResolvedNode struct {
	ID     string
	Kind   string
	Group  string
	CPUs   int
	Memory string
	Apps   []AppSpec
	// Nics are the node's link NICs, eth1 upward, in order.
	Nics []NicSpec
}

// ResolvedTopology is a topology file reduced to exactly what to launch.
type ResolvedTopology struct {
	Name  string
	Nodes []ResolvedNode
	// Bridges are the `kind: bridge` nodes. Nothing is launched for them: the
	// daemon creates each bridge when the first VM linked to it boots.
	Bridges []string
	Links   []DeclaredLink
}

// idPattern keeps node names usable as interface and file names.
var idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

func LoadTopoFile(path string) (*ResolvedTopology, error) {
	// App paths resolve against the file's directory and are then opened by
	// plasmad, whose working directory is not the caller's: they must leave
	// here absolute even when the file was named relatively.
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f TopoFile
	// KnownFields catches typos like `noeds:` that would otherwise silently
	// produce an empty topology.
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// App paths are written relative to the topology file, the way every
	// other tool that references artifacts from a manifest reads them.
	return f.resolve(filepath.Dir(path))
}

// Resolve resolves a topology whose app paths are already absolute (or
// relative to the process's working directory).
func (f *TopoFile) Resolve() (*ResolvedTopology, error) { return f.resolve("") }

func (f *TopoFile) resolve(baseDir string) (*ResolvedTopology, error) {
	if len(f.Topology.Nodes) == 0 {
		return nil, fmt.Errorf("topology has no nodes")
	}

	out := &ResolvedTopology{Name: f.Name}
	members := make(map[string][]string) // counted node key -> its VM ids
	seen := make(map[string]string)      // vm id -> declaring node entry
	bridges := make(map[string]string)   // bridge node -> its subnet, or ""

	// Sort so launch order (and therefore IP assignment) is deterministic
	// rather than dependent on Go's random map iteration.
	names := make([]string, 0, len(f.Topology.Nodes))
	for name := range f.Topology.Nodes {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if name == HostEndpoint {
			return nil, fmt.Errorf("node name %q is reserved for host link endpoints", name)
		}
		spec, err := f.resolveSpec(name)
		if err != nil {
			return nil, err
		}
		if spec.Kind == KindBridge {
			if err := checkBridgeNode(name, f.Topology.Nodes[name], spec); err != nil {
				return nil, err
			}
			bridges[name] = f.Topology.Nodes[name].Subnet
			out.Bridges = append(out.Bridges, name)
			continue
		}
		if f.Topology.Nodes[name].Subnet != "" {
			return nil, fmt.Errorf("node %q: subnet applies to bridge nodes only", name)
		}
		if spec.Kind != KindVM {
			return nil, fmt.Errorf("node %q: kind %q is not supported yet (only %q and %q; router/switch/nat are planned)",
				name, spec.Kind, KindVM, KindBridge)
		}
		if spec.Group == "" {
			spec.Group = name
		}

		apps, err := resolveApps(name, spec.Apps, baseDir)
		if err != nil {
			return nil, err
		}

		ids, err := expandIDs(name, spec)
		if err != nil {
			return nil, err
		}
		if spec.Count > 1 {
			members[name] = ids
		}
		for _, id := range ids {
			if prev, dup := seen[id]; dup {
				return nil, fmt.Errorf("node %q and %q both produce VM %q", prev, name, id)
			}
			seen[id] = name
			out.Nodes = append(out.Nodes, ResolvedNode{
				ID: id, Kind: spec.Kind, Group: spec.Group,
				CPUs: spec.CPUs, Memory: spec.Memory, Apps: apps,
			})
		}
	}

	for b := range bridges {
		if prev, dup := seen[b]; dup {
			return nil, fmt.Errorf("bridge node %q has the same name as a VM from node %q", b, prev)
		}
	}

	expanded, err := expandFleetLinks(f.Topology.Links, members, seen, bridges)
	if err != nil {
		return nil, err
	}
	links, nics, err := resolveLinks(expanded, seen, bridges)
	if err != nil {
		return nil, err
	}
	order := make([]string, len(out.Nodes))
	for i, n := range out.Nodes {
		order[i] = n.ID
	}
	if err := allocateAddresses(nics, order, bridges); err != nil {
		return nil, err
	}
	for _, vm := range order {
		if err := ValidateNics(nics[vm]); err != nil {
			return nil, fmt.Errorf("node %q: %w", vm, err)
		}
	}
	out.Links = links
	for i := range out.Nodes {
		out.Nodes[i].Nics = nics[out.Nodes[i].ID]
	}
	return out, nil
}

// checkBridgeNode rejects VM-only settings on a bridge node. Only what the
// node entry itself declares is checked: cpus and memory inherited from
// defaults or a kind are meant for VMs and simply do not apply.
func checkBridgeNode(name string, node NodeSpec, spec NodeSpec) error {
	if node.Count > 1 {
		return fmt.Errorf("bridge node %q: count does not apply to a bridge", name)
	}
	if len(node.Apps) > 0 || len(spec.Apps) > 0 {
		return fmt.Errorf("bridge node %q: a bridge cannot run apps", name)
	}
	if !idPattern.MatchString(name) {
		return fmt.Errorf("node name %q must start alphanumeric and contain only letters, digits, '-' or '_'", name)
	}
	if node.Subnet != "" {
		p, err := netip.ParsePrefix(node.Subnet)
		if err != nil || !p.Addr().Is4() || p != p.Masked() || p.Bits() > 30 {
			return fmt.Errorf("bridge node %q: subnet %q must be an IPv4 network such as 10.77.0.0/24", name, node.Subnet)
		}
	}
	// The bridge's interface name is pbr-<name>, held to the same budget as
	// a VM's tap-<id>.
	return checkIDLength(name)
}

// resolveSpec merges node -> group -> kind -> defaults, in that precedence.
func (f *TopoFile) resolveSpec(name string) (NodeSpec, error) {
	node := f.Topology.Nodes[name]
	spec := node

	if node.Group != "" {
		g, ok := f.Topology.Groups[node.Group]
		if !ok && len(f.Topology.Groups) > 0 {
			// A group that is only a plasma group name (not declared in the
			// groups: section) is fine — it just contributes no settings.
			g = NodeSpec{}
		}
		spec = merge(spec, g)
	}

	kind := spec.Kind
	if kind == "" {
		kind = f.Topology.Defaults.Kind
	}
	if kind != "" {
		if k, ok := f.Topology.Kinds[kind]; ok {
			spec = merge(spec, k)
		}
		spec.Kind = kind
	}

	spec = merge(spec, f.Topology.Defaults)
	if spec.Kind == "" {
		return spec, fmt.Errorf("node %q: no kind set and topology.defaults.kind is empty", name)
	}
	return spec, nil
}

// merge fills empty fields of a from b. Count is never inherited: it belongs
// to the node entry that declares it, or a kind would silently multiply every
// node using it.
func merge(a, b NodeSpec) NodeSpec {
	if a.Kind == "" {
		a.Kind = b.Kind
	}
	if a.Group == "" {
		a.Group = b.Group
	}
	if a.CPUs == 0 {
		a.CPUs = b.CPUs
	}
	if a.Memory == "" {
		a.Memory = b.Memory
	}
	if len(a.Apps) == 0 {
		a.Apps = b.Apps
	}
	return a
}

// resolveApps validates a node's app list and turns every path into an
// absolute one, so the daemon — which has no idea where the topology file
// lives — can read the payloads.
func resolveApps(node string, apps []AppSpec, baseDir string) ([]AppSpec, error) {
	if len(apps) == 0 {
		return nil, nil
	}
	out := make([]AppSpec, 0, len(apps))
	seen := make(map[string]bool, len(apps))
	for i, a := range apps {
		if a.Name == "" {
			return nil, fmt.Errorf("node %q: apps[%d] is missing 'name'", node, i)
		}
		if !idPattern.MatchString(a.Name) {
			return nil, fmt.Errorf("node %q: app name %q must start alphanumeric and contain only letters, digits, '-' or '_'", node, a.Name)
		}
		if seen[a.Name] {
			return nil, fmt.Errorf("node %q: app %q is declared twice", node, a.Name)
		}
		seen[a.Name] = true
		if a.Path == "" {
			return nil, fmt.Errorf("node %q: app %q is missing 'path'", node, a.Name)
		}
		// Tabs and newlines would break the apps.list the guest parses, and a
		// command spanning lines is a mistake anyway.
		if strings.ContainsAny(a.Cmd, "\t\n") {
			return nil, fmt.Errorf("node %q: app %q: cmd cannot contain tabs or newlines", node, a.Name)
		}

		// Fail here rather than at launch: a typo'd path should not take down
		// half a fleet after the first VMs are already up.
		path, err := appFile(baseDir, a.Path)
		if err != nil {
			return nil, fmt.Errorf("node %q: app %q: %w", node, a.Name, err)
		}

		typ, err := appType(node, a, path)
		if err != nil {
			return nil, err
		}

		script, cmd, err := appCommand(baseDir, a.Cmd)
		if err != nil {
			return nil, fmt.Errorf("node %q: app %q: cmd: %w", node, a.Name, err)
		}
		if script != "" && filepath.Base(script) == filepath.Base(path) {
			return nil, fmt.Errorf("node %q: app %q: start script and payload are both named %q; they share one directory in the guest",
				node, a.Name, filepath.Base(path))
		}
		if typ == AppTypeUserspace && cmd == "" && script == "" {
			// The payload itself is what runs, so it has to be runnable.
			// A Python script needs `cmd: python3 $APP` (or a shebang).
			cmd = "$APP"
		}

		out = append(out, AppSpec{Name: a.Name, Type: typ, Path: path, Cmd: cmd, Script: script})
	}
	return out, nil
}

// appFile resolves one host-side path from a topology file and checks it is
// a readable regular file.
func appFile(baseDir, p string) (string, error) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(baseDir, p)
	}
	p = filepath.Clean(p)
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("path %s is a directory, want a file", p)
	}
	return p, nil
}

// appType settles kernel vs userspace. An explicit type is authoritative but
// still has to agree with the payload: a `.ko` cannot be run and a program
// cannot be insmod'ed, so a mismatch is a mistake worth catching before boot.
func appType(node string, a AppSpec, path string) (string, error) {
	isKO := strings.HasSuffix(path, ".ko")
	switch a.Type {
	case "":
		if isKO {
			return AppTypeKernel, nil
		}
		return AppTypeUserspace, nil
	case AppTypeKernel:
		if !isKO {
			return "", fmt.Errorf("node %q: app %q: type %q needs a .ko, got %s",
				node, a.Name, AppTypeKernel, filepath.Base(path))
		}
		return AppTypeKernel, nil
	case AppTypeUserspace:
		if isKO {
			return "", fmt.Errorf("node %q: app %q: %s is a kernel module, so type must be %q",
				node, a.Name, filepath.Base(path), AppTypeKernel)
		}
		return AppTypeUserspace, nil
	default:
		return "", fmt.Errorf("node %q: app %q: unknown type %q (want %q or %q)",
			node, a.Name, a.Type, AppTypeKernel, AppTypeUserspace)
	}
}

// appCommand decides which of the two cmd forms was written. A cmd naming a
// file on the host is a start script: it is deployed beside the payload and
// invoked from there, because nothing in the guest can reach a host path.
// Anything else — `make -C /opt/apps/foo run`, `python3 $APP --serve` — is a
// command line for the guest's shell.
//
// The split is by "does this resolve to a file", not by looking for spaces or
// a leading '/', so an absolute script that does not exist is reported as the
// missing file it is rather than silently becoming a command that fails at
// boot on a serial console.
func appCommand(baseDir, cmd string) (script, guestCmd string, err error) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "", "", nil
	}
	if looksLikePath(cmd) {
		script, err := appFile(baseDir, cmd)
		if err != nil {
			return "", "", err
		}
		// cwd is the app's directory in the guest, so this is the script.
		return script, "./" + filepath.Base(script), nil
	}
	return "", cmd, nil
}

// looksLikePath is true for a bare path — one token, no shell metacharacters.
// `./run.sh --flag` is a command line, not a file to deploy.
func looksLikePath(cmd string) bool {
	return !strings.ContainsAny(cmd, " \t$&|;<>()*?\"'")
}

// expandIDs turns one node entry into concrete VM IDs, following the same
// naming convention the auto-allocator uses (see State.nextIDLocked) so a
// topology-declared fleet is named exactly like `plasma vm launch <group>`
// would name it.
func expandIDs(name string, spec NodeSpec) ([]string, error) {
	if !idPattern.MatchString(name) {
		return nil, fmt.Errorf("node name %q must start alphanumeric and contain only letters, digits, '-' or '_'", name)
	}
	if spec.Count < 0 {
		return nil, fmt.Errorf("node %q: count cannot be negative", name)
	}
	if spec.Count <= 1 {
		if err := checkIDLength(name); err != nil {
			return nil, err
		}
		return []string{name}, nil
	}

	ids := make([]string, 0, spec.Count)
	for i := 0; i < spec.Count; i++ {
		id := FleetMemberID(name, i)
		if err := checkIDLength(id); err != nil {
			return nil, fmt.Errorf("node %q with count=%d: %w", name, spec.Count, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// checkIDLength guards the tap-name budget: TapDeviceName truncates to 15
// bytes, so IDs longer than 11 characters can collide on the interface name
// even though the VM IDs differ.
func checkIDLength(id string) error {
	const maxID = maxIfaceName - len("tap-")
	if len(id) > maxID {
		return fmt.Errorf("VM id %q is %d chars; max %d so tap names stay unique", id, len(id), maxID)
	}
	return nil
}

// expandFleetLinks lets a link name a counted node by its key: "node:eth2"
// to a bridge, with `node: {count: 100}`, becomes one link per member
// (node-1:eth2 ... node-100:eth2). Only links to a bridge expand, where every
// member joining the one segment is the only possible meaning.
func expandFleetLinks(links []TopoLink, members map[string][]string, vms map[string]string, bridges map[string]string) ([]TopoLink, error) {
	var out []TopoLink
	for i, l := range links {
		fleet := -1
		for j, e := range l.Endpoints {
			if _, isVM := vms[e.Node]; !isVM && len(members[e.Node]) > 0 {
				fleet = j
			}
		}
		if fleet < 0 || len(l.Endpoints) != 2 {
			out = append(out, l)
			continue
		}
		e, other := l.Endpoints[fleet], l.Endpoints[1-fleet]
		if !isBridge(bridges, other.Node) {
			return nil, fmt.Errorf("links[%d]: %q is a fleet (count %d); a fleet can only be linked to a bridge",
				i, e.Node, len(members[e.Node]))
		}
		if e.IPv4 != "" {
			return nil, fmt.Errorf("links[%d]: ipv4 cannot pin a whole fleet (%q) to one address", i, e.Node)
		}
		for _, id := range members[e.Node] {
			m := LinkEndpoint{Node: id, Interface: e.Interface}
			ends := []LinkEndpoint{m, other}
			if fleet == 1 {
				ends = []LinkEndpoint{other, m}
			}
			out = append(out, TopoLink{Type: l.Type, Endpoints: ends})
		}
	}
	return out, nil
}

// endpointKind classifies one end of a link.
type endpointKind int

const (
	endVM endpointKind = iota
	endBridge
	endHost
)

func isBridge(bridges map[string]string, name string) bool {
	_, ok := bridges[name]
	return ok
}

// resolveLinks validates the links and turns them into each VM's NICs. The
// declared form is returned as well, for /v1/topology to report links whose
// VMs are not running.
func resolveLinks(links []TopoLink, vms map[string]string, bridges map[string]string) ([]DeclaredLink, map[string][]NicSpec, error) {
	out := make([]DeclaredLink, 0, len(links))
	nics := make(map[string][]NicSpec)
	used := make(map[string]int)       // "vm:ethN" -> declaring link
	hostIfaces := make(map[string]int) // host ifname -> declaring link

	kindOf := func(e LinkEndpoint) (endpointKind, bool) {
		switch {
		case e.Node == HostEndpoint:
			return endHost, true
		case isBridge(bridges, e.Node):
			return endBridge, true
		default:
			_, ok := vms[e.Node]
			return endVM, ok
		}
	}

	for i, l := range links {
		if len(l.Endpoints) != 2 {
			return nil, nil, fmt.Errorf("links[%d]: need exactly 2 endpoints, got %d", i, len(l.Endpoints))
		}
		a, b := l.Endpoints[0], l.Endpoints[1]
		ka, ok := kindOf(a)
		if !ok {
			return nil, nil, fmt.Errorf("links[%d]: endpoint %q refers to an undeclared node", i, a.Node)
		}
		kb, ok := kindOf(b)
		if !ok {
			return nil, nil, fmt.Errorf("links[%d]: endpoint %q refers to an undeclared node", i, b.Node)
		}
		// Put the VM first; every link needs at least one.
		if ka != endVM {
			a, b, ka, kb = b, a, kb, ka
		}
		if ka != endVM {
			return nil, nil, fmt.Errorf("links[%d]: %s to %s has no VM end; a link must attach at least one VM",
				i, a.Node, b.Node)
		}
		if b.IPv4 != "" || (a.IPv4 != "" && kb != endBridge) {
			return nil, nil, fmt.Errorf("links[%d]: ipv4 can only pin a VM's end of a link to a bridge with a subnet", i)
		}

		ends := []LinkEndpoint{a}
		if kb == endVM {
			ends = append(ends, b)
		}
		for _, e := range ends {
			if _, ok := nicIndex(e.Interface); !ok {
				return nil, nil, fmt.Errorf("links[%d]: %s:%s: a VM's link interface must be eth1, eth2, ... (eth0 is the management NIC)",
					i, e.Node, e.Interface)
			}
			key := e.Node + ":" + e.Interface
			if prev, dup := used[key]; dup {
				return nil, nil, fmt.Errorf("links[%d]: %s is already wired by links[%d]", i, key, prev)
			}
			used[key] = i
		}

		switch kb {
		case endVM:
			if a.Node == b.Node {
				return nil, nil, fmt.Errorf("links[%d]: %s links to itself", i, a.Node)
			}
			br := LinkBridgeName(a.Node, a.Interface, b.Node, b.Interface)
			nics[a.Node] = append(nics[a.Node], NicSpec{Name: a.Interface, Bridge: br})
			nics[b.Node] = append(nics[b.Node], NicSpec{Name: b.Interface, Bridge: br})
		case endBridge:
			// The bridge side's interface is only a label; containerlab
			// names the bridge port, plasma's ports are the VMs' taps. A
			// pinned address rides in Address for allocateAddresses.
			nics[a.Node] = append(nics[a.Node], NicSpec{Name: a.Interface, Bridge: NodeBridgeName(b.Node), Address: a.IPv4})
		case endHost:
			if err := checkHostIface(b.Interface); err != nil {
				return nil, nil, fmt.Errorf("links[%d]: host:%s: %w", i, b.Interface, err)
			}
			if prev, dup := hostIfaces[b.Interface]; dup {
				return nil, nil, fmt.Errorf("links[%d]: host:%s is already used by links[%d]", i, b.Interface, prev)
			}
			hostIfaces[b.Interface] = i
			nics[a.Node] = append(nics[a.Node], NicSpec{Name: a.Interface, HostName: b.Interface})
		}

		out = append(out, DeclaredLink{
			ANode: l.Endpoints[0].Node, AInterface: l.Endpoints[0].Interface,
			BNode: l.Endpoints[1].Node, BInterface: l.Endpoints[1].Interface,
		})
	}

	// The guest names NICs in the order they are attached, so they are kept
	// in eth order; resolve validates them (no gaps) once addresses are set.
	for vm, list := range nics {
		sort.Slice(list, func(i, j int) bool {
			x, _ := nicIndex(list[i].Name)
			y, _ := nicIndex(list[j].Name)
			return x < y
		})
		nics[vm] = list
	}
	return out, nics, nil
}

// allocateAddresses gives every VM NIC on a bridge with a subnet its address.
// Pins are taken first; the rest get the lowest free host address, VM by VM
// in resolution order (sorted node keys, a count's members in ordinal order)
// and NIC by NIC, so the same file always yields the same addresses: with
// nothing else on the bridge, the first member of `node: {count: 100}` gets
// .1 and the hundredth .100. A NIC on a bridge without a subnet gets none.
func allocateAddresses(nics map[string][]NicSpec, order []string, bridges map[string]string) error {
	subnets := map[string]netip.Prefix{}
	for name, s := range bridges {
		if s != "" {
			subnets[NodeBridgeName(name)] = netip.MustParsePrefix(s) // checked by checkBridgeNode
		}
	}
	used := map[string]map[netip.Addr]string{} // bridge -> address -> "vm:ethN"
	for br := range subnets {
		used[br] = map[netip.Addr]string{}
	}
	for _, vm := range order {
		for _, n := range nics[vm] {
			if n.Address == "" {
				continue
			}
			p, ok := subnets[n.Bridge]
			if !ok {
				return fmt.Errorf("%s:%s: ipv4 %s pins an address on a bridge without a subnet", vm, n.Name, n.Address)
			}
			a, err := netip.ParseAddr(n.Address)
			if err != nil || !hostAddress(p, a) {
				return fmt.Errorf("%s:%s: ipv4 %q is not a host address in %s", vm, n.Name, n.Address, p)
			}
			if prev, dup := used[n.Bridge][a]; dup {
				return fmt.Errorf("%s:%s: ipv4 %s is already pinned by %s", vm, n.Name, a, prev)
			}
			used[n.Bridge][a] = vm + ":" + n.Name
		}
	}
	for _, vm := range order {
		for i, n := range nics[vm] {
			p, ok := subnets[n.Bridge]
			if !ok {
				continue
			}
			if n.Address != "" {
				nics[vm][i].Address = netip.PrefixFrom(netip.MustParseAddr(n.Address), p.Bits()).String()
				continue
			}
			a := p.Addr().Next()
			for ; hostAddress(p, a); a = a.Next() {
				if _, taken := used[n.Bridge][a]; !taken {
					break
				}
			}
			if !hostAddress(p, a) {
				return fmt.Errorf("%s:%s: no free address left in %s", vm, n.Name, p)
			}
			used[n.Bridge][a] = vm + ":" + n.Name
			nics[vm][i].Address = netip.PrefixFrom(a, p.Bits()).String()
		}
	}
	return nil
}

// hostAddress reports whether a is a usable host address in p: inside it,
// and neither its network nor its broadcast address.
func hostAddress(p netip.Prefix, a netip.Addr) bool {
	if !p.Contains(a) || a == p.Addr() {
		return false
	}
	n, b := p.Addr().As4(), a.As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	bcast := (uint32(n[0])<<24 | uint32(n[1])<<16 | uint32(n[2])<<8 | uint32(n[3])) | host
	return uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3]) != bcast
}

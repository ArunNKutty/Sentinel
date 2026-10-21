package plasma

import (
	"fmt"
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
//   - `links` are parsed and validated but not yet wired. Every VM still gets
//     a single NIC on plasma-br0; per-link L2 segments arrive with the
//     router/switch/NAT kinds.

// KindVM is the only kind that can be launched today.
const KindVM = "vm"

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
}

// ResolvedTopology is a topology file reduced to exactly what to launch.
type ResolvedTopology struct {
	Name  string
	Nodes []ResolvedNode
	Links []DeclaredLink
}

// idPattern keeps node names usable as interface and file names.
var idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

func LoadTopoFile(path string) (*ResolvedTopology, error) {
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
	seen := make(map[string]string) // vm id -> declaring node entry

	// Sort so launch order (and therefore IP assignment) is deterministic
	// rather than dependent on Go's random map iteration.
	names := make([]string, 0, len(f.Topology.Nodes))
	for name := range f.Topology.Nodes {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		spec, err := f.resolveSpec(name)
		if err != nil {
			return nil, err
		}
		if spec.Kind != KindVM {
			return nil, fmt.Errorf("node %q: kind %q is not supported yet (only %q; router/switch/nat are planned)",
				name, spec.Kind, KindVM)
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

	links, err := resolveLinks(f.Topology.Links, seen)
	if err != nil {
		return nil, err
	}
	out.Links = links
	return out, nil
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

func resolveLinks(links []TopoLink, nodes map[string]string) ([]DeclaredLink, error) {
	out := make([]DeclaredLink, 0, len(links))
	for i, l := range links {
		if len(l.Endpoints) != 2 {
			return nil, fmt.Errorf("links[%d]: need exactly 2 endpoints, got %d", i, len(l.Endpoints))
		}
		for _, e := range l.Endpoints {
			if _, ok := nodes[e.Node]; !ok {
				return nil, fmt.Errorf("links[%d]: endpoint %q refers to an undeclared node", i, e.Node)
			}
		}
		out = append(out, DeclaredLink{
			ANode: l.Endpoints[0].Node, AInterface: l.Endpoints[0].Interface,
			BNode: l.Endpoints[1].Node, BInterface: l.Endpoints[1].Interface,
		})
	}
	return out, nil
}

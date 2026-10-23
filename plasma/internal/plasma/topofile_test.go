package plasma

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadTopo(t *testing.T, body string) (*ResolvedTopology, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "topo.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write topo: %v", err)
	}
	return LoadTopoFile(path)
}

func TestResolveInheritance(t *testing.T) {
	// containerlab's precedence is node -> group -> kind -> defaults, and the
	// whole file format is worthless if that order is wrong.
	topo, err := loadTopo(t, `
name: lab
topology:
  defaults:
    kind: vm
    cpus: 1
    memory: 512M
  kinds:
    vm:
      cpus: 2
      memory: 1024M
  groups:
    ne:
      memory: 1536M
  nodes:
    dflt: {}
    grp:
      group: ne
    node:
      group: ne
      cpus: 8
      memory: 4096M
`)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	got := map[string]ResolvedNode{}
	for _, n := range topo.Nodes {
		got[n.ID] = n
	}

	// kind wins over defaults
	if n := got["dflt"]; n.CPUs != 2 || n.Memory != "1024M" {
		t.Errorf("dflt = %d cpu/%s, want kind's 2/1024M", n.CPUs, n.Memory)
	}
	// group wins over kind
	if n := got["grp"]; n.Memory != "1536M" || n.CPUs != 2 {
		t.Errorf("grp = %d cpu/%s, want 2/1536M", n.CPUs, n.Memory)
	}
	// node wins over everything
	if n := got["node"]; n.CPUs != 8 || n.Memory != "4096M" {
		t.Errorf("node = %d cpu/%s, want 8/4096M", n.CPUs, n.Memory)
	}
}

func TestResolveCountExpansion(t *testing.T) {
	topo, err := loadTopo(t, `
name: lab
topology:
  defaults: {kind: vm}
  nodes:
    ne:
      group: ne
      count: 3
    sandbox:
      group: sandbox
      count: 2
    solo: {}
`)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	var ids []string
	for _, n := range topo.Nodes {
		ids = append(ids, n.ID)
	}
	joined := strings.Join(ids, ",")

	// Expansion must follow the same convention as the auto-allocator, or a
	// topology-declared fleet would be named differently from `vm launch`.
	for _, want := range []string{"ne-00", "ne-01", "ne-02", "sandbox-1", "sandbox-2", "solo"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %v", want, ids)
		}
	}
	if len(topo.Nodes) != 6 {
		t.Errorf("got %d nodes, want 6: %v", len(topo.Nodes), ids)
	}
}

func TestResolveRejectsUnsupportedKind(t *testing.T) {
	_, err := loadTopo(t, `
name: lab
topology:
  nodes:
    r1: {kind: router}
`)
	if err == nil || !strings.Contains(err.Error(), "not supported yet") {
		t.Fatalf("want a clear 'not supported yet' error for kind: router, got %v", err)
	}
}

func TestResolveLinkFormats(t *testing.T) {
	// Both containerlab spellings appear in their docs, so both must parse.
	topo, err := loadTopo(t, `
name: lab
topology:
  defaults: {kind: vm}
  nodes:
    a: {}
    b: {}
  links:
    - endpoints: ["a:eth1", "b:eth1"]
    - type: veth
      endpoints:
        - node: a
          interface: eth2
        - node: b
          interface: eth2
`)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(topo.Links) != 2 {
		t.Fatalf("got %d links, want 2", len(topo.Links))
	}
	if topo.Links[0].ANode != "a" || topo.Links[0].AInterface != "eth1" {
		t.Errorf("brief form parsed wrong: %+v", topo.Links[0])
	}
	if topo.Links[1].BNode != "b" || topo.Links[1].BInterface != "eth2" {
		t.Errorf("extended form parsed wrong: %+v", topo.Links[1])
	}
}

func TestResolveRejectsLinkToUnknownNode(t *testing.T) {
	_, err := loadTopo(t, `
name: lab
topology:
  defaults: {kind: vm}
  nodes:
    a: {}
  links:
    - endpoints: ["a:eth1", "ghost:eth1"]
`)
	if err == nil || !strings.Contains(err.Error(), "undeclared node") {
		t.Fatalf("want an undeclared-node error, got %v", err)
	}
}

func TestResolveRejectsDuplicateIDs(t *testing.T) {
	// `ne` with count and an explicit `ne-00` both produce ne-00.
	_, err := loadTopo(t, `
name: lab
topology:
  defaults: {kind: vm}
  nodes:
    ne:
      group: ne
      count: 2
    ne-00: {}
`)
	if err == nil || !strings.Contains(err.Error(), "both produce") {
		t.Fatalf("want a duplicate-ID error, got %v", err)
	}
}

func TestResolveRejectsOverlongID(t *testing.T) {
	// Tap names are capped at 15 bytes, so long IDs would collide on the
	// interface name even though the VM IDs differ.
	_, err := loadTopo(t, `
name: lab
topology:
  defaults: {kind: vm}
  nodes:
    this-name-is-far-too-long: {}
`)
	if err == nil || !strings.Contains(err.Error(), "max") {
		t.Fatalf("want a length error, got %v", err)
	}
}

func TestResolveRejectsUnknownField(t *testing.T) {
	// A typo should fail loudly rather than silently yielding no nodes.
	_, err := loadTopo(t, `
name: lab
topology:
  noeds:
    a: {}
`)
	if err == nil {
		t.Fatal("want an error for a misspelled section, got nil")
	}
}

func TestResolveDefaultsGroupToNodeName(t *testing.T) {
	topo, err := loadTopo(t, `
name: lab
topology:
  defaults: {kind: vm}
  nodes:
    web: {}
`)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if topo.Nodes[0].Group != "web" {
		t.Errorf("group = %q, want it to fall back to the node name", topo.Nodes[0].Group)
	}
}

func TestResolveApps(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "neapp")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "start.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec \"$APP\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ko := filepath.Join(dir, "gluon_sim.ko")
	if err := os.WriteFile(ko, []byte("elf"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "topo.yaml")
	// App paths are relative to the topology file, and a group's apps are
	// inherited by a node that declares none.
	body := `
name: lab
topology:
  defaults:
    kind: vm
  groups:
    host:
      apps:
        - name: base
          path: ./neapp
  nodes:
    inherits:
      group: host
    overrides:
      group: host
      apps:
        - name: script
          type: userspace
          path: ./neapp
          cmd: ./start.sh
        - name: shellcmd
          path: ./neapp
          cmd: make -C /opt/apps/shellcmd run
        - name: mod
          type: kernel
          path: ./gluon_sim.ko
    bare:
      group: host
      count: 2
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	topo, err := LoadTopoFile(path)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	byNode := map[string]map[string]AppSpec{}
	for _, n := range topo.Nodes {
		apps := map[string]AppSpec{}
		for _, a := range n.Apps {
			apps[a.Name] = a
		}
		byNode[n.ID] = apps
	}

	if a, ok := byNode["inherits"]["base"]; !ok || a.Path != bin {
		t.Errorf("inherits apps = %+v, want the group's base app at %s", byNode["inherits"], bin)
	} else if a.Type != AppTypeUserspace || a.Cmd != "$APP" {
		// No type and no cmd: a plain binary that just gets executed.
		t.Errorf("base = %+v, want userspace running $APP", a)
	}

	over := byNode["overrides"]
	if len(over) != 3 {
		t.Fatalf("overrides apps = %+v, want only its own three", over)
	}
	// A cmd naming a file is a start script: deployed beside the payload and
	// invoked from the app's directory.
	if a := over["script"]; a.Script != script || a.Cmd != "./start.sh" {
		t.Errorf("script app = %+v, want script %s run as ./start.sh", a, script)
	}
	// Anything else is a command line for the guest's shell, deployed nothing.
	if a := over["shellcmd"]; a.Script != "" || a.Cmd != "make -C /opt/apps/shellcmd run" {
		t.Errorf("shellcmd app = %+v, want the make command passed through", a)
	}
	// A kernel module with no cmd is loaded, not run.
	if a := over["mod"]; a.Type != AppTypeKernel || a.Cmd != "" {
		t.Errorf("mod app = %+v, want kernel with no cmd", a)
	}

	// count expansion carries the apps onto every replica.
	for _, id := range []string{"bare-1", "bare-2"} {
		if a, ok := byNode[id]["base"]; !ok {
			t.Errorf("%s apps = %+v, want the group's base app", id, byNode[id])
		} else if a.Path != bin {
			t.Errorf("%s base path = %s, want %s", id, a.Path, bin)
		}
	}
}

func TestResolveAppsRejectsBadSpecs(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"neapp", "gluon_sim.ko", "start.sh"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]struct{ apps, want string }{
		"missing name": {"        - path: ./neapp", "missing 'name'"},
		"missing path": {"        - name: a", "missing 'path'"},
		"no such file": {"        - {name: a, path: ./nope}", "no such file"},
		"a directory":  {"        - {name: a, path: .}", "is a directory"},
		"duplicate": {"        - {name: a, path: ./neapp}\n" +
			"        - {name: a, path: ./neapp}", "declared twice"},
		"cmd with newline": {"        - {name: a, path: ./neapp, cmd: \"one\\ntwo\"}",
			"cannot contain tabs or newlines"},
		"unknown type": {"        - {name: a, type: bpf, path: ./neapp}", "unknown type"},
		"kernel type on a binary": {"        - {name: a, type: kernel, path: ./neapp}",
			"needs a .ko"},
		"userspace type on a module": {"        - {name: a, type: userspace, path: ./gluon_sim.ko}",
			"must be \"kernel\""},
		// A cmd that looks like a path but names nothing is a typo'd script,
		// not a command the guest should be left to fail on.
		"missing start script": {"        - {name: a, path: ./neapp, cmd: ./nope.sh}",
			"no such file"},
		"script clashes with payload": {"        - {name: a, path: ./start.sh, cmd: ./start.sh}",
			"share one directory"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, "topo.yaml")
			body := "name: lab\ntopology:\n  defaults:\n    kind: vm\n  nodes:\n    n:\n      apps:\n" + tc.apps + "\n"
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadTopoFile(path)
			if err == nil {
				t.Fatalf("accepted %s", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestExampleTopologyFile keeps the shipped example honest: it is the first
// thing anyone runs, so a typo or a path that no longer exists is a bug.
func TestExampleTopologyFile(t *testing.T) {
	path := filepath.Join("..", "..", "..", "topologies", "ne-lab.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skip("example topology not present")
	}
	topo, err := LoadTopoFile(path)
	if err != nil {
		t.Fatalf("load example: %v", err)
	}
	var kernel, userspace int
	for _, n := range topo.Nodes {
		for _, a := range n.Apps {
			switch a.Type {
			case AppTypeKernel:
				kernel++
			case AppTypeUserspace:
				userspace++
			}
		}
	}
	// The example is the documentation for the feature, so it has to show
	// both halves of it.
	if kernel == 0 || userspace == 0 {
		t.Errorf("example topology has %d kernel and %d userspace app(s); want both", kernel, userspace)
	}
}

func TestResolveWiresLinks(t *testing.T) {
	// The integration rig's shape: two shared segments as bridge nodes, a
	// point-to-point link, and an unbridged host tap to mirror into.
	topo, err := loadTopo(t, `
name: lab
topology:
  defaults: {kind: vm}
  nodes:
    fabric: {}
    node1: {}
    node2: {}
    ctl: {kind: bridge}
  links:
    - endpoints: ["node1:eth1", "ctl:node1"]
    - endpoints: ["ctl:node2", "node2:eth1"]
    - endpoints: ["fabric:eth1", "ctl:fabric"]
    - endpoints: ["node2:eth2", "node1:eth2"]
    - endpoints: ["fabric:eth2", "host:fabcap"]
`)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(topo.Bridges) != 1 || topo.Bridges[0] != "ctl" {
		t.Errorf("bridges = %v, want [ctl]", topo.Bridges)
	}
	// A bridge is not a VM: nothing may be launched for it.
	nics := map[string][]NicSpec{}
	for _, n := range topo.Nodes {
		if n.ID == "ctl" {
			t.Fatal("bridge node ctl was resolved as a VM to launch")
		}
		nics[n.ID] = n.Nics
	}

	p2p := LinkBridgeName("node1", "eth2", "node2", "eth2")
	want := map[string][]NicSpec{
		"fabric": {{Name: "eth1", Bridge: "pbr-ctl"}, {Name: "eth2", HostName: "fabcap"}},
		"node1":  {{Name: "eth1", Bridge: "pbr-ctl"}, {Name: "eth2", Bridge: p2p}},
		// The bridge was written first on this link; the VM end still wins.
		"node2": {{Name: "eth1", Bridge: "pbr-ctl"}, {Name: "eth2", Bridge: p2p}},
	}
	for id, w := range want {
		got := nics[id]
		if len(got) != len(w) {
			t.Errorf("%s nics = %+v, want %+v", id, got, w)
			continue
		}
		for i := range w {
			if got[i] != w[i] {
				t.Errorf("%s nic %d = %+v, want %+v", id, i, got[i], w[i])
			}
		}
	}
	if len(topo.Links) != 5 {
		t.Errorf("got %d declared links, want 5", len(topo.Links))
	}
}

func TestResolveRejectsBadLinks(t *testing.T) {
	cases := []struct {
		name, nodes, links, want string
	}{
		{"gap in NIC numbering", "a: {}\n    b: {}",
			`["a:eth1", "b:eth1"]` + "\n    - endpoints: [\"a:eth3\", \"b:eth2\"]", "no gaps"},
		{"management NIC", "a: {}\n    b: {}", `["a:eth0", "b:eth1"]`, "eth0 is the management NIC"},
		{"not ethN", "a: {}\n    b: {}", `["a:ens4", "b:eth1"]`, "eth1, eth2"},
		{"NIC wired twice", "a: {}\n    b: {}\n    c: {}",
			`["a:eth1", "b:eth1"]` + "\n    - endpoints: [\"a:eth1\", \"c:eth1\"]", "already wired"},
		{"link to itself", "a: {}", `["a:eth1", "a:eth2"]`, "links to itself"},
		{"no VM end", "a: {}\n    s: {kind: bridge}", `["s:x", "host:cap0"]`, "no VM end"},
		{"host name reused", "a: {}\n    b: {}",
			`["a:eth1", "host:cap0"]` + "\n    - endpoints: [\"b:eth1\", \"host:cap0\"]", "already used"},
		{"host name is plasma's", "a: {}", `["a:eth1", "host:tap-x"]`, "reserved prefix"},
		{"host name too long", "a: {}", `["a:eth1", "host:abcdefghijklmnop"]`, "at most 15"},
		{"node named host", "host: {}", `["host:eth1", "host:cap0"]`, "reserved for host link endpoints"},
		{"bridge with apps", "s: {kind: bridge, apps: [{name: x, path: /etc/hostname}]}\n    a: {}",
			`["a:eth1", "s:a"]`, "cannot run apps"},
		{"bridge with count", "s: {kind: bridge, count: 2}\n    a: {}", `["a:eth1", "s:a"]`, "count does not apply"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadTopo(t, "name: lab\ntopology:\n  defaults: {kind: vm}\n  nodes:\n    "+
				c.nodes+"\n  links:\n    - endpoints: "+c.links+"\n")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestResolveAppsAbsoluteFromRelativeFile(t *testing.T) {
	// plasmad opens the payloads from its own working directory, so a file
	// named relatively must still yield absolute app paths.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.bin"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "topo.yaml"), []byte(`
name: lab
topology:
  defaults: {kind: vm}
  nodes:
    a:
      apps: [{name: app, path: app.bin}]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	topo, err := LoadTopoFile("topo.yaml")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if p := topo.Nodes[0].Apps[0].Path; !filepath.IsAbs(p) {
		t.Errorf("app path %q is relative; the daemon cannot open it", p)
	}
}

func TestResolveAllocatesLinkAddresses(t *testing.T) {
	// The scale case: a counted fleet gets .1, .2, ... in ordinal order (not
	// node-1, node-10, node-11 string order), and a pinned service address
	// is kept out of the pool.
	topo, err := loadTopo(t, `
name: lab
topology:
  defaults: {kind: vm}
  nodes:
    fabric: {}
    node: {count: 12}
    wire: {kind: bridge, subnet: 10.77.0.0/24}
    ctl: {kind: bridge}
  links:
    - endpoints: [{node: fabric, interface: eth1, ipv4: 10.77.0.254}, "wire:fabric"]
    - endpoints: ["fabric:eth2", "ctl:fabric"]
`+fleetLinks(12)+`
`)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := map[string]string{}
	for _, n := range topo.Nodes {
		for _, nic := range n.Nics {
			got[n.ID+":"+nic.Name] = nic.Address
		}
	}
	want := map[string]string{
		"fabric:eth1":  "10.77.0.254/24",
		"fabric:eth2":  "", // ctl has no subnet: no address
		"node-1:eth1":  "10.77.0.1/24",
		"node-2:eth1":  "10.77.0.2/24",
		"node-10:eth1": "10.77.0.10/24",
		"node-12:eth1": "10.77.0.12/24",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
}

// fleetLinks wires node-1..node-n's eth1 to the wire bridge.
func fleetLinks(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "    - endpoints: [\"node-%d:eth1\", \"wire:n%d\"]\n", i, i)
	}
	return b.String()
}

func TestResolveRejectsBadAddressing(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"subnet on a VM", `
  nodes:
    a: {subnet: 10.0.9.0/24}`, "bridge nodes only"},
		{"not a network", `
  nodes:
    a: {}
    w: {kind: bridge, subnet: 10.0.9.1/24}
  links:
    - endpoints: ["a:eth1", "w:a"]`, "must be an IPv4 network"},
		{"pin outside the subnet", `
  nodes:
    a: {}
    w: {kind: bridge, subnet: 10.0.9.0/24}
  links:
    - endpoints: [{node: a, interface: eth1, ipv4: 10.0.8.5}, "w:a"]`, "not a host address"},
		{"pin on the broadcast address", `
  nodes:
    a: {}
    w: {kind: bridge, subnet: 10.0.9.0/24}
  links:
    - endpoints: [{node: a, interface: eth1, ipv4: 10.0.9.255}, "w:a"]`, "not a host address"},
		{"pin without a subnet", `
  nodes:
    a: {}
    w: {kind: bridge}
  links:
    - endpoints: [{node: a, interface: eth1, ipv4: 10.0.9.5}, "w:a"]`, "without a subnet"},
		{"same pin twice", `
  nodes:
    a: {}
    b: {}
    w: {kind: bridge, subnet: 10.0.9.0/24}
  links:
    - endpoints: [{node: a, interface: eth1, ipv4: 10.0.9.5}, "w:a"]
    - endpoints: [{node: b, interface: eth1, ipv4: 10.0.9.5}, "w:b"]`, "already pinned"},
		{"subnet too small", `
  nodes:
    a: {count: 3}
    w: {kind: bridge, subnet: 10.0.9.0/30}
  links:
    - endpoints: ["a-1:eth1", "w:a"]
    - endpoints: ["a-2:eth1", "w:b"]
    - endpoints: ["a-3:eth1", "w:c"]`, "no free address"},
		{"misspelt endpoint field", `
  nodes:
    a: {}
    w: {kind: bridge, subnet: 10.0.9.0/24}
  links:
    - endpoints: [{node: a, interface: eth1, ipv6: 10.0.9.5}, "w:a"]`, "unknown field"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadTopo(t, "name: lab\ntopology:\n  defaults: {kind: vm}"+c.body+"\n")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestResolveExpandsFleetLinks(t *testing.T) {
	// One link line wires a whole counted fleet to a bridge.
	topo, err := loadTopo(t, `
name: lab
topology:
  defaults: {kind: vm}
  nodes:
    node: {count: 3}
    wire: {kind: bridge, subnet: 10.77.0.0/24}
  links:
    - endpoints: ["node:eth1", "wire:node"]
`)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for i, n := range topo.Nodes {
		want := NicSpec{Name: "eth1", Bridge: "pbr-wire", Address: fmt.Sprintf("10.77.0.%d/24", i+1)}
		if len(n.Nics) != 1 || n.Nics[0] != want {
			t.Errorf("%s nics = %+v, want [%+v]", n.ID, n.Nics, want)
		}
	}
	if len(topo.Links) != 3 {
		t.Errorf("got %d declared links, want 3 (one per member)", len(topo.Links))
	}

	for name, body := range map[string]string{
		"fleet to a VM": `
    a: {}
  links:
    - endpoints: ["node:eth1", "a:eth1"]`,
		"fleet pinned": `
    wire: {kind: bridge, subnet: 10.77.0.0/24}
  links:
    - endpoints: [{node: node, interface: eth1, ipv4: 10.77.0.5}, "wire:x"]`,
	} {
		_, err := loadTopo(t, "name: lab\ntopology:\n  defaults: {kind: vm}\n  nodes:\n    node: {count: 2}"+body+"\n")
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

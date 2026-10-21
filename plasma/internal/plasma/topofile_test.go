package plasma

import (
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

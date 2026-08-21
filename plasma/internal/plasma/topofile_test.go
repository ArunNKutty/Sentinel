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

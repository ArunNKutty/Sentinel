package plasma

import (
	"path/filepath"
	"strconv"
	"testing"
)

func newTestState(t *testing.T, vms ...VmRecord) *State {
	t.Helper()
	s, err := LoadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	for _, v := range vms {
		if err := s.Add(v); err != nil {
			t.Fatalf("add %s: %v", v.ID, err)
		}
	}
	return s
}

func TestAllocateEmptyState(t *testing.T) {
	s := newTestState(t)

	id, ip, mac, err := s.Allocate("host")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if id != "host-1" || ip != "10.0.0.2" || mac != "02:00:0a:00:00:02" {
		t.Fatalf("got %s/%s/%s, want host-1/10.0.0.2/02:00:0a:00:00:02", id, ip, mac)
	}
}

func TestAllocateNeGroupIsZeroIndexedAndPadded(t *testing.T) {
	s := newTestState(t)

	id, _, _, err := s.Allocate("ne")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if id != "ne-00" {
		t.Fatalf("got %s, want ne-00", id)
	}

	s = newTestState(t, VmRecord{ID: "ne-00", Group: "ne", IP: "10.0.0.2"})
	if id, _, _, _ = s.Allocate("ne"); id != "ne-01" {
		t.Fatalf("got %s, want ne-01", id)
	}
}

// The bug this guards: allocating from a count rather than the highest value
// hands a live VM's ID and IP to a second VM once one in the middle is gone.
func TestAllocateAfterMiddleVMRemoved(t *testing.T) {
	s := newTestState(t,
		VmRecord{ID: "host-1", Group: "host", IP: "10.0.0.2"},
		VmRecord{ID: "host-2", Group: "host", IP: "10.0.0.3"},
		VmRecord{ID: "host-3", Group: "host", IP: "10.0.0.4"},
	)
	// Drop host-2, as a delete would.
	s.mu.Lock()
	s.vms = append(s.vms[:1], s.vms[2:]...)
	s.mu.Unlock()

	id, ip, _, err := s.Allocate("host")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if id != "host-4" {
		t.Fatalf("got id %s, want host-4 (highest+1, not count+1)", id)
	}
	// The freed address is reusable; the freed ID is not, so records stay
	// unambiguous over the lifetime of the fleet.
	if ip != "10.0.0.3" {
		t.Fatalf("got ip %s, want the freed 10.0.0.3", ip)
	}
}

func TestAllocateGroupsAreIndependent(t *testing.T) {
	s := newTestState(t,
		VmRecord{ID: "host-1", Group: "host", IP: "10.0.0.2"},
		VmRecord{ID: "host-2", Group: "host", IP: "10.0.0.3"},
	)

	id, ip, _, err := s.Allocate("fabric")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if id != "fabric-1" {
		t.Fatalf("got id %s, want fabric-1", id)
	}
	// Addresses are fleet-wide even though IDs are per-group.
	if ip != "10.0.0.4" {
		t.Fatalf("got ip %s, want 10.0.0.4", ip)
	}
}

func TestAllocateExhaustedSubnet(t *testing.T) {
	s := newTestState(t)
	s.mu.Lock()
	for n := 2; n <= 254; n++ {
		s.vms = append(s.vms, VmRecord{ID: "host", Group: "host", IP: "10.0.0." + strconv.Itoa(n)})
	}
	s.mu.Unlock()

	if _, _, _, err := s.Allocate("host"); err == nil {
		t.Fatal("want an error when 10.0.0.0/24 is full, got nil")
	}
}

func TestRemove(t *testing.T) {
	s := newTestState(t,
		VmRecord{ID: "host-1", Group: "host", IP: "10.0.0.2"},
		VmRecord{ID: "host-2", Group: "host", IP: "10.0.0.3"},
	)

	rec, ok, err := s.Remove("", "host-1")
	if err != nil || !ok {
		t.Fatalf("remove: ok=%v err=%v", ok, err)
	}
	if rec.ID != "host-1" {
		t.Fatalf("got removed record %s, want host-1", rec.ID)
	}
	if got := s.List(); len(got) != 1 || got[0].ID != "host-2" {
		t.Fatalf("got %v, want only host-2 left", got)
	}

	// The change is durable.
	reloaded, err := LoadState(s.path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.List(); len(got) != 1 || got[0].ID != "host-2" {
		t.Fatalf("reloaded %v, want only host-2 left", got)
	}
}

func TestRemoveUnknownID(t *testing.T) {
	s := newTestState(t, VmRecord{ID: "host-1", Group: "host", IP: "10.0.0.2"})

	_, ok, err := s.Remove("", "host-9")
	if err != nil || ok {
		t.Fatalf("remove unknown: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
	if got := s.List(); len(got) != 1 {
		t.Fatalf("state changed on a no-op remove: %v", got)
	}
}

func TestReconcileMarksDeadVMsExited(t *testing.T) {
	s := newTestState(t,
		VmRecord{ID: "host-1", Group: "host", IP: "10.0.0.2", Status: StatusRunning, PID: 1},
		// PID 0 is never a live process.
		VmRecord{ID: "host-2", Group: "host", IP: "10.0.0.3", Status: StatusRunning, PID: 0},
	)

	stale, err := s.Reconcile()
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(stale) != 1 || stale[0] != "host-2" {
		t.Fatalf("got stale %v, want [host-2]", stale)
	}

	// The change is durable: a reload sees the same thing.
	reloaded, err := LoadState(s.path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	for _, v := range reloaded.List() {
		want := StatusRunning
		if v.ID == "host-2" {
			want = StatusExited
		}
		if v.Status != want {
			t.Fatalf("%s status %s, want %s", v.ID, v.Status, want)
		}
	}
}

func TestAdoptLegacyState(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "run", "state.json")
	current := filepath.Join(dir, "lib", "state.json")

	old := newTestState(t, VmRecord{ID: "host-1", Group: "host", IP: "10.0.0.2"})
	old.path = legacy
	old.mu.Lock()
	if err := old.saveLocked(); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	old.mu.Unlock()

	moved, err := AdoptLegacyState(legacy, current)
	if err != nil || !moved {
		t.Fatalf("adopt: moved=%v err=%v", moved, err)
	}
	s, err := LoadState(current)
	if err != nil {
		t.Fatalf("load adopted: %v", err)
	}
	if got := s.List(); len(got) != 1 || got[0].ID != "host-1" {
		t.Fatalf("adopted %v, want one host-1 record", got)
	}

	// Second run is a no-op — the legacy file is gone and state stays put.
	if moved, err := AdoptLegacyState(legacy, current); err != nil || moved {
		t.Fatalf("second adopt: moved=%v err=%v", moved, err)
	}
}

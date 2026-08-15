package plasma

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

type State struct {
	mu   sync.Mutex
	path string
	vms  []VmRecord
}

// AdoptLegacyState moves a pre-existing state file from its old location to
// the current one, so a daemon upgrade does not silently start from empty
// state while the disks it describes are still on the host. Reports whether it
// moved anything.
func AdoptLegacyState(legacy, current string) (bool, error) {
	if _, err := os.Stat(current); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if _, err := os.Stat(legacy); err != nil {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(current), 0o755); err != nil {
		return false, err
	}
	data, err := os.ReadFile(legacy)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(current, data, 0o644); err != nil {
		return false, err
	}
	return true, os.Remove(legacy)
}

func LoadState(path string) (*State, error) {
	s := &State{path: path}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s.vms); err != nil {
		return nil, fmt.Errorf("parse state file: %w", err)
	}
	return s, nil
}

func (s *State) List() []VmRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]VmRecord, len(s.vms))
	copy(out, s.vms)
	return out
}

func (s *State) Add(rec VmRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vms = append(s.vms, rec)
	return s.saveLocked()
}

// Remove deletes the record with the given ID and returns it. ok is false
// (and state is left untouched) if no record matched.
func (s *State) Remove(id string) (rec VmRecord, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, v := range s.vms {
		if v.ID != id {
			continue
		}
		rec = v
		s.vms = append(s.vms[:i], s.vms[i+1:]...)
		if err := s.saveLocked(); err != nil {
			return VmRecord{}, false, err
		}
		return rec, true, nil
	}
	return VmRecord{}, false, nil
}

// Allocate picks the identity for a new VM in the given group: the next free
// ID and the lowest unused address in the VM subnet. Both are derived from the
// highest/used values rather than from a count, so deleting a VM in the middle
// of a group does not hand the same ID or IP to two VMs.
func (s *State) Allocate(group string) (id, ip, mac string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	octet, err := s.freeOctetLocked()
	if err != nil {
		return "", "", "", err
	}
	return s.nextIDLocked(group), fmt.Sprintf("10.0.0.%d", octet),
		fmt.Sprintf("02:00:0a:00:00:%02x", octet), nil
}

// nextIDLocked follows the CLAUDE.md convention: ne-NN (zero-indexed, padded
// width 2) for the "ne" group; <group>-N (one-indexed, unpadded) otherwise.
func (s *State) nextIDLocked(group string) string {
	prefix := group + "-"
	highest := -1
	for _, v := range s.vms {
		if v.Group != group || !strings.HasPrefix(v.ID, prefix) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(v.ID, prefix))
		if err != nil {
			continue
		}
		if n > highest {
			highest = n
		}
	}
	next := highest + 1
	if group == "ne" {
		return fmt.Sprintf("ne-%02d", next)
	}
	if next < 1 {
		next = 1
	}
	return fmt.Sprintf("%s-%d", group, next)
}

// freeOctetLocked returns the lowest final octet in 10.0.0.0/24 not already
// held by a record. .1 is the host side of the bridge.
func (s *State) freeOctetLocked() (int, error) {
	used := make(map[int]bool, len(s.vms))
	for _, v := range s.vms {
		i := strings.LastIndex(v.IP, ".")
		if i < 0 {
			continue
		}
		if n, err := strconv.Atoi(v.IP[i+1:]); err == nil {
			used[n] = true
		}
	}
	for n := 2; n <= 254; n++ {
		if !used[n] {
			return n, nil
		}
	}
	return 0, fmt.Errorf("no free address in 10.0.0.0/24")
}

// Reconcile marks records whose process is no longer alive as exited. State
// survives reboots now, so the VMs it describes may not.
func (s *State) Reconcile() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var stale []string
	for i, v := range s.vms {
		if v.Status != StatusRunning || processAlive(v.PID) {
			continue
		}
		s.vms[i].Status = StatusExited
		stale = append(stale, v.ID)
	}
	if len(stale) == 0 {
		return nil, nil
	}
	return stale, s.saveLocked()
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// EPERM means the process is there, just not ours to signal — alive.
	return errors.Is(err, syscall.EPERM) || errors.Is(err, os.ErrPermission)
}

func (s *State) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.vms, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

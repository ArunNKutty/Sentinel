package plasma

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type State struct {
	mu   sync.Mutex
	path string
	vms  []VmRecord
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

// NextID returns the next sequential ID for the given group, following the
// CLAUDE.md convention: ne-NN (zero-indexed, padded width 2) for the "ne"
// group; <group>-N (one-indexed, unpadded) otherwise.
func (s *State) NextID(group string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, v := range s.vms {
		if v.Group == group {
			count++
		}
	}
	if group == "ne" {
		return fmt.Sprintf("ne-%02d", count)
	}
	return fmt.Sprintf("%s-%d", group, count+1)
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

// Package state keeps the claims this node prepared, across plugin restarts.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sync"
)

const version = 1

// Device is one prepared device of a claim.
type Device struct {
	Request      string   `json:"request"`
	Pool         string   `json:"pool"`
	Device       string   `json:"device"`
	ShareID      string   `json:"shareID,omitempty"`
	VRAMBytes    int64    `json:"vramBytes"`
	CDIDeviceIDs []string `json:"cdiDeviceIDs"`
}

type checkpoint struct {
	Version int                 `json:"version"`
	Claims  map[string][]Device `json:"claims"`
}

// Store is a JSON checkpoint file. It is safe for concurrent use.
type Store struct {
	mu     sync.Mutex
	path   string
	claims map[string][]Device
}

// Open loads the checkpoint at path. A missing file is an empty checkpoint;
// an unreadable or foreign one is an error, so prepared claims are never
// forgotten silently.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload replaces the in-memory claims with the file's content. Two plugin
// instances overlap during a rolling update and share the file, so callers
// reload at the start of every operation instead of trusting memory.
func (s *Store) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	claims, err := load(s.path)
	if err != nil {
		return err
	}
	s.claims = claims
	return nil
}

func load(path string) (map[string][]Device, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string][]Device{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read checkpoint: %w", err)
	}
	var cp checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, fmt.Errorf("decode checkpoint %s: %w", path, err)
	}
	if cp.Version != version {
		return nil, fmt.Errorf("checkpoint %s: unsupported version %d", path, cp.Version)
	}
	if cp.Claims == nil {
		return map[string][]Device{}, nil
	}
	return cp.Claims, nil
}

// Get returns a copy of the claim's devices.
func (s *Store) Get(uid string) ([]Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.claims[uid]
	return cloneDevices(d), ok
}

// Put records a claim and writes the checkpoint.
func (s *Store) Put(uid string, devs []Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.claims[uid]
	s.claims[uid] = cloneDevices(devs)
	if err := s.save(); err != nil {
		if had {
			s.claims[uid] = prev
		} else {
			delete(s.claims, uid)
		}
		return err
	}
	return nil
}

// Delete forgets a claim and writes the checkpoint.
func (s *Store) Delete(uid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.claims[uid]
	if !had {
		return nil
	}
	delete(s.claims, uid)
	if err := s.save(); err != nil {
		s.claims[uid] = prev
		return err
	}
	return nil
}

// All returns a deep copy of every recorded claim.
func (s *Store) All() map[string][]Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]Device, len(s.claims))
	for uid, d := range s.claims {
		out[uid] = cloneDevices(d)
	}
	return out
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(checkpoint{Version: version, Claims: s.claims}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode checkpoint: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write checkpoint: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace checkpoint: %w", err)
	}
	return nil
}

func cloneDevices(in []Device) []Device {
	if in == nil {
		return nil
	}
	out := make([]Device, len(in))
	for i, d := range in {
		d.CDIDeviceIDs = slices.Clone(d.CDIDeviceIDs)
		out[i] = d
	}
	return out
}

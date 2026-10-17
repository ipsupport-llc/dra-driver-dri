// Package vram reads the video memory of render-node GPUs.
package vram

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfs"
)

// ErrUnknown means the VRAM size of a GPU could not be determined.
var ErrUnknown = errors.New("VRAM size unknown")

// Backend reads VRAM for GPUs bound to one kernel driver.
type Backend interface {
	Total(gpu sysfs.GPU) (int64, error)
	Used(gpu sysfs.GPU) (int64, error)
}

// AMDGPU reads the amdgpu driver's sysfs counters.
type AMDGPU struct{}

// Total returns mem_info_vram_total.
func (AMDGPU) Total(g sysfs.GPU) (int64, error) {
	return readInt(filepath.Join(g.DevicePath, "mem_info_vram_total"))
}

// Used returns mem_info_vram_used.
func (AMDGPU) Used(g sysfs.GPU) (int64, error) {
	return readInt(filepath.Join(g.DevicePath, "mem_info_vram_used"))
}

func readInt(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

// Overrides sets the VRAM size per PCI address (lower-case).
type Overrides map[string]int64

// LoadOverrides reads a YAML map of PCI address to quantity, for example:
//
//	"0000:03:00.0": 16Gi
//
// An empty path yields no overrides.
func LoadOverrides(path string) (Overrides, error) {
	out := Overrides{}
	if path == "" {
		return out, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read overrides: %w", err)
	}
	raw := map[string]string{}
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse overrides %s: %w", path, err)
	}
	for addr, q := range raw {
		qty, err := resource.ParseQuantity(q)
		if err != nil {
			return nil, fmt.Errorf("overrides %s: %s: %w", path, addr, err)
		}
		if qty.Sign() <= 0 {
			return nil, fmt.Errorf("overrides %s: %s: VRAM must be positive", path, addr)
		}
		out[strings.ToLower(addr)] = qty.Value()
	}
	return out, nil
}

// Detector picks the VRAM source for a GPU: an override first, then the
// backend of its kernel driver.
type Detector struct {
	Backends  map[string]Backend // keyed by kernel driver name
	Overrides Overrides
}

// NewDetector returns a Detector with every built-in backend.
func NewDetector(o Overrides) Detector {
	return Detector{Backends: map[string]Backend{"amdgpu": AMDGPU{}}, Overrides: o}
}

// Total returns the GPU's VRAM size in bytes, or an error wrapping ErrUnknown.
func (d Detector) Total(g sysfs.GPU) (int64, error) {
	if v, ok := d.Overrides[g.PCIAddress]; ok {
		return v, nil
	}
	b, ok := d.Backends[g.KernelDriver]
	if !ok {
		return 0, fmt.Errorf("%s: no VRAM backend for kernel driver %q: %w", g.PCIAddress, g.KernelDriver, ErrUnknown)
	}
	v, err := b.Total(g)
	if err != nil {
		return 0, fmt.Errorf("%s: %w: %v", g.PCIAddress, ErrUnknown, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("%s: backend reported %d bytes: %w", g.PCIAddress, v, ErrUnknown)
	}
	return v, nil
}

// Used returns the VRAM in use; ok is false when no backend can tell.
func (d Detector) Used(g sysfs.GPU) (int64, bool) {
	b, ok := d.Backends[g.KernelDriver]
	if !ok {
		return 0, false
	}
	v, err := b.Used(g)
	if err != nil {
		return 0, false
	}
	return v, true
}

// Package cdi writes per-claim CDI specs that hand a container one render node.
//
// The layout follows kubernetes-sigs/dra-example-driver (Apache-2.0):
// one transient spec per claim, kind "k8s.<driver-name>/gpu".
package cdi

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdiparser "tags.cncf.io/container-device-interface/pkg/parser"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

const class = "gpu"

// Edit is one GPU handed to a claim.
type Edit struct {
	Device     string // DRA device name
	ShareID    string // empty when the allocation is not shared
	RenderNode string // e.g. "renderD128"
	PCIAddress string
	Vendor     string
	VRAMBytes  int64
}

// Writer writes and removes claim specs in one CDI spec directory.
type Writer struct {
	cache  *cdiapi.Cache
	vendor string
}

// NewWriter creates root if needed and returns a Writer for driverName.
func NewWriter(root, driverName string) (*Writer, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create CDI spec dir: %w", err)
	}
	cache, err := cdiapi.NewCache(cdiapi.WithSpecDirs(root), cdiapi.WithAutoRefresh(false))
	if err != nil {
		return nil, fmt.Errorf("CDI cache: %w", err)
	}
	return &Writer{cache: cache, vendor: "k8s." + driverName}, nil
}

func (w *Writer) deviceName(claimUID string, e Edit) string {
	n := claimUID + "-" + e.Device
	if e.ShareID != "" {
		n += "-" + e.ShareID
	}
	return n
}

// QualifiedID is the CDI device ID the kubelet passes to the runtime.
func (w *Writer) QualifiedID(claimUID string, e Edit) string {
	return cdiparser.QualifiedName(w.vendor, class, w.deviceName(claimUID, e))
}

func (w *Writer) specName(claimUID string) string {
	return cdiapi.GenerateTransientSpecName(w.vendor, class, claimUID)
}

// WriteClaim writes (or replaces) the spec of one claim.
func (w *Writer) WriteClaim(claimUID string, edits []Edit) error {
	spec := &cdispec.Spec{Kind: w.vendor + "/" + class}
	for _, e := range edits {
		mode := os.FileMode(0o666)
		spec.Devices = append(spec.Devices, cdispec.Device{
			Name: w.deviceName(claimUID, e),
			ContainerEdits: cdispec.ContainerEdits{
				DeviceNodes: []*cdispec.DeviceNode{{
					Path:        "/dev/dri/" + e.RenderNode,
					Permissions: "rw",
					FileMode:    &mode,
				}},
				Env: env(e),
			},
		})
	}
	version, err := cdispec.MinimumRequiredVersion(spec)
	if err != nil {
		return fmt.Errorf("CDI spec version: %w", err)
	}
	spec.Version = version
	if err := w.cache.WriteSpec(spec, w.specName(claimUID)); err != nil {
		return fmt.Errorf("write CDI spec for claim %s: %w", claimUID, err)
	}
	return nil
}

// env describes one GPU twice: as DRA_GPU_* for the common one-GPU
// container, and as DRA_GPU_<DEVICE>_* so a container with several GPUs
// (several claims, or a claim with several devices) sees each of them; the
// unsuffixed set then names one of them, unspecified which.
func env(e Edit) []string {
	key := "DRA_GPU_" + strings.ToUpper(strings.ReplaceAll(e.Device, "-", "_")) + "_"
	var out []string
	for _, prefix := range []string{"DRA_GPU_", key} {
		out = append(out,
			prefix+"PCI_BUS_ID="+e.PCIAddress,
			prefix+"VENDOR="+e.Vendor,
			fmt.Sprintf("%sVRAM_BYTES=%d", prefix, e.VRAMBytes),
		)
	}
	return out
}

// DeleteClaim removes the claim's spec; a missing spec is not an error.
func (w *Writer) DeleteClaim(claimUID string) error {
	err := w.cache.RemoveSpec(w.specName(claimUID))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove CDI spec for claim %s: %w", claimUID, err)
	}
	return nil
}

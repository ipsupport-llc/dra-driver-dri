// Package sysfs finds GPUs that expose a DRM render node.
package sysfs

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// GPU is one DRM render node backed by a PCI device.
type GPU struct {
	RenderNode   string // e.g. "renderD128"
	PCIAddress   string // e.g. "0000:06:00.0"
	VendorID     string // e.g. "0x1002"
	DeviceID     string // e.g. "0x6798"
	KernelDriver string // e.g. "amdgpu"
	DevicePath   string // resolved sysfs directory of the PCI device
}

var pciAddress = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)

// Scan returns the render-node GPUs under root (normally "/sys"), sorted by
// PCI address. Render nodes that are not PCI devices, have no bound kernel
// driver or lack vendor/device IDs are skipped.
func Scan(root string) ([]GPU, error) {
	nodes, err := filepath.Glob(filepath.Join(root, "class", "drm", "renderD*"))
	if err != nil {
		return nil, fmt.Errorf("list render nodes: %w", err)
	}
	var gpus []GPU
	for _, node := range nodes {
		devPath, err := filepath.EvalSymlinks(filepath.Join(node, "device"))
		if err != nil {
			continue
		}
		addr := filepath.Base(devPath)
		if !pciAddress.MatchString(addr) {
			continue
		}
		driverPath, err := filepath.EvalSymlinks(filepath.Join(devPath, "driver"))
		if err != nil {
			continue
		}
		vendor, err := readTrimmed(filepath.Join(devPath, "vendor"))
		if err != nil {
			continue
		}
		device, err := readTrimmed(filepath.Join(devPath, "device"))
		if err != nil {
			continue
		}
		gpus = append(gpus, GPU{
			RenderNode:   filepath.Base(node),
			PCIAddress:   addr,
			VendorID:     vendor,
			DeviceID:     device,
			KernelDriver: filepath.Base(driverPath),
			DevicePath:   devPath,
		})
	}
	sort.Slice(gpus, func(i, j int) bool { return gpus[i].PCIAddress < gpus[j].PCIAddress })
	return gpus, nil
}

func readTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

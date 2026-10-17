// Package sysfstest builds fake sysfs trees for tests.
package sysfstest

import (
	"os"
	"path/filepath"
	"testing"
)

// FakeGPU describes one render node and its PCI device.
type FakeGPU struct {
	RenderNode string            // e.g. "renderD128"
	PCIAddress string            // e.g. "0000:06:00.0"; may be any name for non-PCI nodes
	VendorID   string            // e.g. "0x1002"; empty = no vendor file
	DeviceID   string            // e.g. "0x6798"
	Driver     string            // bound kernel driver; empty = unbound
	Files      map[string]string // extra files in the device dir
}

// AddGPU creates the device directory, its driver link and the render node
// link under root, laid out like /sys. It returns the device directory.
func AddGPU(t testing.TB, root string, g FakeGPU) string {
	t.Helper()
	dev := filepath.Join(root, "devices", "pci0000:00", g.PCIAddress)
	mkdir(t, dev)
	if g.VendorID != "" {
		write(t, filepath.Join(dev, "vendor"), g.VendorID+"\n")
	}
	write(t, filepath.Join(dev, "device"), g.DeviceID+"\n")
	for name, content := range g.Files {
		write(t, filepath.Join(dev, name), content)
	}
	if g.Driver != "" {
		drv := filepath.Join(root, "bus", "pci", "drivers", g.Driver)
		mkdir(t, drv)
		symlink(t, drv, filepath.Join(dev, "driver"))
	}
	node := filepath.Join(root, "class", "drm", g.RenderNode)
	mkdir(t, node)
	symlink(t, dev, filepath.Join(node, "device"))
	return dev
}

// SetFile replaces one file in a device directory created by AddGPU.
func SetFile(t testing.TB, devDir, name, content string) {
	t.Helper()
	write(t, filepath.Join(devDir, name), content)
}

func mkdir(t testing.TB, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t testing.TB, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlink(t testing.TB, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

package sysfs_test

import (
	"path/filepath"
	"testing"

	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfs"
	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfstest"
)

func resolved(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestScanTwoGPUsSorted(t *testing.T) {
	root := t.TempDir()
	devB := sysfstest.AddGPU(t, root, sysfstest.FakeGPU{RenderNode: "renderD128", PCIAddress: "0000:06:00.0", VendorID: "0x1002", DeviceID: "0x6798", Driver: "amdgpu"})
	devA := sysfstest.AddGPU(t, root, sysfstest.FakeGPU{RenderNode: "renderD129", PCIAddress: "0000:02:00.0", VendorID: "0x1002", DeviceID: "0x6798", Driver: "amdgpu"})

	got, err := sysfs.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []sysfs.GPU{
		{RenderNode: "renderD129", PCIAddress: "0000:02:00.0", VendorID: "0x1002", DeviceID: "0x6798", KernelDriver: "amdgpu", DevicePath: resolved(t, devA)},
		{RenderNode: "renderD128", PCIAddress: "0000:06:00.0", VendorID: "0x1002", DeviceID: "0x6798", KernelDriver: "amdgpu", DevicePath: resolved(t, devB)},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d GPUs, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("GPU %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestScanSkipsUnusableNodes(t *testing.T) {
	root := t.TempDir()
	sysfstest.AddGPU(t, root, sysfstest.FakeGPU{RenderNode: "renderD128", PCIAddress: "0000:06:00.0", VendorID: "0x1002", DeviceID: "0x6798"})         // unbound
	sysfstest.AddGPU(t, root, sysfstest.FakeGPU{RenderNode: "renderD129", PCIAddress: "vgem", VendorID: "0x0000", DeviceID: "0x0000", Driver: "vgem"}) // not PCI
	sysfstest.AddGPU(t, root, sysfstest.FakeGPU{RenderNode: "renderD130", PCIAddress: "0000:07:00.0", DeviceID: "0x6798", Driver: "amdgpu"})           // no vendor file
	ok := sysfstest.AddGPU(t, root, sysfstest.FakeGPU{RenderNode: "renderD131", PCIAddress: "0000:08:00.0", VendorID: "0x1002", DeviceID: "0x6798", Driver: "amdgpu"})

	got, err := sysfs.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].DevicePath != resolved(t, ok) {
		t.Fatalf("got %+v, want only 0000:08:00.0", got)
	}
}

func TestScanEmptyRoot(t *testing.T) {
	got, err := sysfs.Scan(t.TempDir())
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want no GPUs and no error", got, err)
	}
}

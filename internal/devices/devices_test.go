package devices_test

import (
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/deviceattribute"

	"github.com/ipsupport-llc/dra-driver-dri/internal/devices"
	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfs"
	"github.com/ipsupport-llc/dra-driver-dri/internal/vram"
)

const mib = int64(1) << 20

func gpu(addr, node string) sysfs.GPU {
	return sysfs.GPU{RenderNode: node, PCIAddress: addr, VendorID: "0x1002", DeviceID: "0x6798", KernelDriver: "amdgpu"}
}

func cfg() devices.Config {
	return devices.Config{NodeName: "node-a", ReserveBytes: 256 * mib, StepBytes: 64 * mib}
}

func TestDeviceNameAndVendor(t *testing.T) {
	if got := devices.DeviceName("0000:06:00.0"); got != "pci-0000-06-00-0" {
		t.Errorf("DeviceName = %q", got)
	}
	for id, want := range map[string]string{"0x1002": "amd", "0x8086": "intel", "0x10DE": "nvidia", "0x1af4": "unknown"} {
		if got := devices.VendorName(id); got != want {
			t.Errorf("VendorName(%s) = %q, want %q", id, got, want)
		}
	}
}

func TestBuildPublishesRoundedVRAM(t *testing.T) {
	det := vram.Detector{Overrides: vram.Overrides{
		"0000:02:00.0": 6144 * mib,        // D700: 6144 - 256 = 5888 MiB (multiple of 64)
		"0000:06:00.0": 6144*mib + 100000, // odd size rounds down to 5888 MiB
	}}
	pubs, skipped := devices.Build(cfg(), []sysfs.GPU{gpu("0000:02:00.0", "renderD129"), gpu("0000:06:00.0", "renderD128")}, det)
	if len(skipped) != 0 || len(pubs) != 2 {
		t.Fatalf("pubs=%+v skipped=%+v", pubs, skipped)
	}
	for _, p := range pubs {
		if p.VRAMPublished != 5888*mib || p.Vendor != "amd" {
			t.Errorf("%s: published %d vendor %s", p.Name, p.VRAMPublished, p.Vendor)
		}
	}
	if pubs[0].Name != "pci-0000-02-00-0" || pubs[0].VRAMTotal != 6144*mib {
		t.Errorf("first = %+v", pubs[0])
	}
}

func TestBuildSkips(t *testing.T) {
	det := vram.Detector{Overrides: vram.Overrides{"0000:07:00.0": 300 * mib}} // 300-256 = 44 MiB < one step
	pubs, skipped := devices.Build(cfg(), []sysfs.GPU{gpu("0000:06:00.0", "renderD128"), gpu("0000:07:00.0", "renderD130")}, det)
	if len(pubs) != 0 || len(skipped) != 2 {
		t.Fatalf("pubs=%+v skipped=%+v", pubs, skipped)
	}
	if !strings.Contains(skipped[1].Reason.Error(), "reserve") {
		t.Errorf("reason = %v", skipped[1].Reason)
	}
}

func TestResources(t *testing.T) {
	pubs := []devices.Published{{Name: "pci-0000-06-00-0", GPU: gpu("0000:06:00.0", "renderD128"), Vendor: "amd", VRAMTotal: 6144 * mib, VRAMPublished: 5888 * mib}}
	res := devices.Resources(cfg(), pubs)

	pool, ok := res.Pools["node-a"]
	if !ok || len(pool.Slices) != 1 || len(pool.Slices[0].Devices) != 1 {
		t.Fatalf("pools = %+v", res.Pools)
	}
	d := pool.Slices[0].Devices[0]
	if d.Name != "pci-0000-06-00-0" || d.AllowMultipleAllocations == nil || !*d.AllowMultipleAllocations {
		t.Fatalf("device = %+v", d)
	}
	attrs := map[resourceapi.QualifiedName]string{
		"vendor": "amd", "kernelDriver": "amdgpu", "deviceID": "0x6798", "renderNode": "renderD128",
		deviceattribute.StandardDeviceAttributePCIBusID: "0000:06:00.0",
	}
	for k, want := range attrs {
		if a, ok := d.Attributes[k]; !ok || a.StringValue == nil || *a.StringValue != want {
			t.Errorf("attribute %s = %+v, want %q", k, d.Attributes[k], want)
		}
	}
	c, ok := d.Capacity[devices.CapacityVRAM]
	if !ok || c.Value.Value() != 5888*mib {
		t.Fatalf("capacity = %+v", d.Capacity)
	}
	p := c.RequestPolicy
	if p == nil || p.Default.Value() != 5888*mib || p.ValidRange == nil ||
		p.ValidRange.Min.Value() != 64*mib || p.ValidRange.Step.Value() != 64*mib || p.ValidRange.Max.Value() != 5888*mib {
		t.Fatalf("request policy = %+v", p)
	}
}

func TestResourcesNoGPUs(t *testing.T) {
	res := devices.Resources(cfg(), nil)
	if pool, ok := res.Pools["node-a"]; !ok || len(pool.Slices) != 1 || len(pool.Slices[0].Devices) != 0 {
		t.Fatalf("pools = %+v", res.Pools)
	}
}

package metrics_test

import (
	"testing"

	"github.com/ipsupport-llc/dra-driver-dri/internal/devices"
	"github.com/ipsupport-llc/dra-driver-dri/internal/metrics"
	"github.com/ipsupport-llc/dra-driver-dri/internal/prepare"
	"github.com/ipsupport-llc/dra-driver-dri/internal/procvram"
	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfs"
)

const gib = int64(1) << 30

func TestAttributeSumsPodProcessesOnClaimedDevice(t *testing.T) {
	pubs := []devices.Published{{Name: "gpu-0", GPU: sysfs.GPU{PCIAddress: "0000:06:00.0"}}}
	claims := []prepare.ClaimPod{{Device: "gpu-0", PodUID: "uid-a", PodNamespace: "llm", PodName: "frog-0", ClaimedBytes: 3 * gib}}
	usages := []procvram.Usage{
		{PID: 1, PCI: "0000:06:00.0", PodUID: "uid-a", VRAMBytes: 3 * gib},     // the claiming pod
		{PID: 2, PCI: "0000:06:00.0", PodUID: "uid-a", VRAMBytes: 1 * gib},     // same pod, another process
		{PID: 3, PCI: "0000:06:00.0", PodUID: "uid-other", VRAMBytes: 1 * gib}, // a different pod
	}

	got := metrics.Attribute(claims, pubs, usages)

	if len(got) != 1 {
		t.Fatalf("got %d, want 1: %+v", len(got), got)
	}
	c := got[0]
	// Claimed 3 GiB, actually uses 4 GiB (over-claim) — the catch of this feature.
	if c.Device != "gpu-0" || c.Namespace != "llm" || c.Pod != "frog-0" || c.UsedBytes != 4*gib || c.ClaimedBytes != 3*gib {
		t.Fatalf("got %+v; want gpu-0/llm/frog-0 used 4GiB claimed 3GiB", c)
	}
}

// A claimed pod not using any VRAM is still reported at 0 (claimed but idle).
func TestAttributeClaimedButIdleIsZero(t *testing.T) {
	pubs := []devices.Published{{Name: "gpu-0", GPU: sysfs.GPU{PCIAddress: "0000:06:00.0"}}}
	claims := []prepare.ClaimPod{{Device: "gpu-0", PodUID: "uid-a", PodNamespace: "llm", PodName: "frog-0"}}
	got := metrics.Attribute(claims, pubs, nil)
	if len(got) != 1 || got[0].UsedBytes != 0 {
		t.Fatalf("got %+v; want one entry at 0", got)
	}
}

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/dynamic-resource-allocation/resourceslice"

	"github.com/ipsupport-llc/dra-driver-dri/internal/cdi"
	"github.com/ipsupport-llc/dra-driver-dri/internal/devices"
	"github.com/ipsupport-llc/dra-driver-dri/internal/metrics"
	"github.com/ipsupport-llc/dra-driver-dri/internal/prepare"
	"github.com/ipsupport-llc/dra-driver-dri/internal/state"
	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfstest"
	"github.com/ipsupport-llc/dra-driver-dri/internal/vram"
)

type fakePublisher struct {
	calls []resourceslice.DriverResources
	err   error
}

func (f *fakePublisher) PublishResources(_ context.Context, r resourceslice.DriverResources) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, r)
	return nil
}

func testDriver(t *testing.T) (*Driver, string, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "sys")
	dev := sysfstest.AddGPU(t, root, sysfstest.FakeGPU{RenderNode: "renderD128", PCIAddress: "0000:06:00.0", VendorID: "0x1002", DeviceID: "0x6798", Driver: "amdgpu",
		Files: map[string]string{"mem_info_vram_total": "6442450944\n", "mem_info_vram_used": "0\n"}})
	m := metrics.New()
	d := newDriver(devices.Config{NodeName: "node-a", ReserveBytes: 256 << 20}, root, vram.NewDetector(nil), m, func(error) {})
	w, err := cdi.NewWriter(filepath.Join(dir, "cdi"), "gpu.example.com")
	if err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(filepath.Join(dir, "checkpoint.json"))
	if err != nil {
		t.Fatal(err)
	}
	d.prep = &prepare.Preparer{DriverName: "gpu.example.com", Inventory: d.Inventory, CDI: w, Store: s}
	return d, root, dev
}

func TestRescanPublishesOnlyOnChange(t *testing.T) {
	d, _, dev := testDriver(t)
	pub := &fakePublisher{}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := d.Rescan(ctx, pub); err != nil {
			t.Fatal(err)
		}
	}
	if len(pub.calls) != 1 {
		t.Fatalf("publishes = %d, want 1", len(pub.calls))
	}
	sysfstest.SetFile(t, dev, "mem_info_vram_total", "8589934592\n")
	if err := d.Rescan(ctx, pub); err != nil {
		t.Fatal(err)
	}
	if len(pub.calls) != 2 {
		t.Fatalf("publishes = %d, want 2 after a VRAM change", len(pub.calls))
	}
}

func TestRescanRetriesFailedPublish(t *testing.T) {
	d, _, _ := testDriver(t)
	pub := &fakePublisher{err: errors.New("api down")}
	if err := d.Rescan(context.Background(), pub); err == nil {
		t.Fatal("want the publish error")
	}
	pub.err = nil
	if err := d.Rescan(context.Background(), pub); err != nil || len(pub.calls) != 1 {
		t.Fatalf("retry: %v, calls=%d", err, len(pub.calls))
	}
}

func TestRefreshFillsInventoryWithoutPublishing(t *testing.T) {
	d, _, _ := testDriver(t)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Inventory()["pci-0000-06-00-0"]; !ok {
		t.Fatalf("inventory = %v", d.Inventory())
	}
	pub := &fakePublisher{}
	if err := d.Rescan(context.Background(), pub); err != nil || len(pub.calls) != 1 {
		t.Fatalf("first Rescan after Refresh must publish: %v, calls=%d", err, len(pub.calls))
	}
}

func TestRescanCountsSkipped(t *testing.T) {
	d, root, _ := testDriver(t)
	sysfstest.AddGPU(t, root, sysfstest.FakeGPU{RenderNode: "renderD129", PCIAddress: "0000:03:00.0", VendorID: "0x8086", DeviceID: "0xe20b", Driver: "xe"})
	if err := d.Rescan(context.Background(), &fakePublisher{}); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(d.skippedGauge()); got != 1 {
		t.Fatalf("dri_gpus_skipped = %v", got)
	}
}

func TestPrepareResourceClaimsReportsErrors(t *testing.T) {
	d, _, _ := testDriver(t)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	bad := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "c", UID: "u1"}}
	res, err := d.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{bad})
	if err != nil {
		t.Fatal(err)
	}
	if res["u1"].Err == nil {
		t.Fatal("want a per-claim error")
	}
	if got := testutil.ToFloat64(d.metrics.PrepareErrors); got != 1 {
		t.Fatalf("prepare errors = %v", got)
	}
}

// A GPU that disappears after the last periodic scan must not be prepared.
func TestPrepareRescansBeforePreparing(t *testing.T) {
	d, root, _ := testDriver(t)
	if err := d.Rescan(context.Background(), &fakePublisher{}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "class", "drm", "renderD128")); err != nil {
		t.Fatal(err)
	}
	c := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "c", UID: "u1"},
		Status: resourceapi.ResourceClaimStatus{Allocation: &resourceapi.AllocationResult{
			Devices: resourceapi.DeviceAllocationResult{Results: []resourceapi.DeviceRequestAllocationResult{
				{Request: "gpu", Driver: "gpu.example.com", Pool: "node-a", Device: "pci-0000-06-00-0"},
			}},
		}},
	}
	res, err := d.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{c})
	if err != nil {
		t.Fatal(err)
	}
	if res["u1"].Err == nil {
		t.Fatalf("prepared a GPU that is gone: %+v", res["u1"])
	}
	if d.prep.Count() != 0 {
		t.Fatal("checkpoint entry written for a missing GPU")
	}
}

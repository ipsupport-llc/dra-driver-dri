package prepare_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/ipsupport-llc/dra-driver-dri/internal/cdi"
	"github.com/ipsupport-llc/dra-driver-dri/internal/devices"
	"github.com/ipsupport-llc/dra-driver-dri/internal/prepare"
	"github.com/ipsupport-llc/dra-driver-dri/internal/state"
	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfs"
)

const (
	driver = "gpu.example.com"
	gib    = int64(1) << 30
)

type env struct {
	cdiRoot, checkpoint string
	inv                 map[string]devices.Published
	p                   *prepare.Preparer
}

func published(name, addr, node string) devices.Published {
	return devices.Published{Name: name, Vendor: "amd", VRAMTotal: 6 * gib, VRAMPublished: 5888 << 20,
		GPU: sysfs.GPU{RenderNode: node, PCIAddress: addr, VendorID: "0x1002", DeviceID: "0x6798", KernelDriver: "amdgpu"}}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{cdiRoot: filepath.Join(dir, "cdi"), checkpoint: filepath.Join(dir, "checkpoint.json"),
		inv: map[string]devices.Published{"pci-0000-06-00-0": published("pci-0000-06-00-0", "0000:06:00.0", "renderD128")}}
	e.reopen(t)
	return e
}

// reopen simulates a plugin restart: fresh writer and store on the same paths.
func (e *env) reopen(t *testing.T) {
	t.Helper()
	w, err := cdi.NewWriter(e.cdiRoot, driver)
	if err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(e.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	e.p = &prepare.Preparer{DriverName: driver, Inventory: func() map[string]devices.Published { return e.inv }, CDI: w, Store: s}
}

func (e *env) specs(t *testing.T) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(e.cdiRoot, "*"))
	return m
}

func result(device string, share string, vram int64) resourceapi.DeviceRequestAllocationResult {
	r := resourceapi.DeviceRequestAllocationResult{Request: "gpu", Driver: driver, Pool: "node-a", Device: device}
	if share != "" {
		r.ShareID = ptr.To(types.UID(share))
	}
	if vram > 0 {
		r.ConsumedCapacity = map[resourceapi.QualifiedName]resource.Quantity{devices.CapacityVRAM: *resource.NewQuantity(vram, resource.BinarySI)}
	}
	return r
}

func claim(uid string, results ...resourceapi.DeviceRequestAllocationResult) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "claim-" + uid, UID: types.UID(uid)},
		Status: resourceapi.ResourceClaimStatus{Allocation: &resourceapi.AllocationResult{
			Devices: resourceapi.DeviceAllocationResult{Results: results},
		}},
	}
}

func readSpec(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPrepareWholeGPU(t *testing.T) {
	e := newEnv(t)
	got, err := e.p.Prepare(claim("u1", result("pci-0000-06-00-0", "", 0)))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].DeviceName != "pci-0000-06-00-0" || got[0].PoolName != "node-a" || got[0].ShareID != nil ||
		len(got[0].CDIDeviceIDs) != 1 || got[0].CDIDeviceIDs[0] != "k8s.gpu.example.com/gpu=u1-pci-0000-06-00-0" {
		t.Fatalf("devices = %+v", got)
	}
	specs := e.specs(t)
	if len(specs) != 1 || !strings.Contains(readSpec(t, specs[0]), "DRA_GPU_VRAM_BYTES=6174015488") {
		t.Fatalf("spec = %v", specs)
	}
	if e.p.Allocated()["pci-0000-06-00-0"] != 5888<<20 || e.p.Count() != 1 {
		t.Fatalf("allocated = %v count = %d", e.p.Allocated(), e.p.Count())
	}
}

func TestPrepareTwoSharesOfOneDevice(t *testing.T) {
	e := newEnv(t)
	a, err := e.p.Prepare(claim("u1", result("pci-0000-06-00-0", "s1", 2*gib)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.p.Prepare(claim("u2", result("pci-0000-06-00-0", "s2", 3*gib)))
	if err != nil {
		t.Fatal(err)
	}
	if a[0].CDIDeviceIDs[0] == b[0].CDIDeviceIDs[0] || *a[0].ShareID != "s1" || *b[0].ShareID != "s2" {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
	if got := e.p.Allocated()["pci-0000-06-00-0"]; got != 5*gib {
		t.Fatalf("allocated = %d", got)
	}
	if err := e.p.Unprepare("u1"); err != nil {
		t.Fatal(err)
	}
	specs := e.specs(t)
	if len(specs) != 1 || !strings.Contains(readSpec(t, specs[0]), "u2-pci-0000-06-00-0-s2") {
		t.Fatalf("specs after unprepare = %v", specs)
	}
	if got := e.p.Allocated()["pci-0000-06-00-0"]; got != 3*gib {
		t.Fatalf("allocated after unprepare = %d", got)
	}
}

func TestPrepareIgnoresOtherDrivers(t *testing.T) {
	e := newEnv(t)
	other := result("nic-0", "", 0)
	other.Driver = "net.example.com"
	got, err := e.p.Prepare(claim("u1", other, result("pci-0000-06-00-0", "", 0)))
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := e.p.Prepare(claim("u2", other)); err == nil {
		t.Fatal("a claim without devices of this driver must fail")
	}
}

func TestPrepareUnknownDevice(t *testing.T) {
	e := newEnv(t)
	_, err := e.p.Prepare(claim("u1", result("pci-0000-09-00-0", "", 0)))
	if err == nil || !strings.Contains(err.Error(), "pci-0000-09-00-0") {
		t.Fatalf("err = %v", err)
	}
	if len(e.specs(t)) != 0 || e.p.Count() != 0 {
		t.Fatal("a failed prepare left state behind")
	}
}

func TestPrepareNoAllocation(t *testing.T) {
	e := newEnv(t)
	c := claim("u1")
	c.Status.Allocation = nil
	if _, err := e.p.Prepare(c); err == nil {
		t.Fatal("want an error")
	}
}

func TestPrepareIsIdempotent(t *testing.T) {
	e := newEnv(t)
	c := claim("u1", result("pci-0000-06-00-0", "s1", 2*gib))
	first, err := e.p.Prepare(c)
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.p.Prepare(c)
	if err != nil {
		t.Fatal(err)
	}
	if first[0].CDIDeviceIDs[0] != second[0].CDIDeviceIDs[0] || len(e.specs(t)) != 1 {
		t.Fatalf("first=%+v second=%+v specs=%v", first, second, e.specs(t))
	}
}

func TestPrepareRestoresAfterReboot(t *testing.T) {
	e := newEnv(t)
	c := claim("u1", result("pci-0000-06-00-0", "s1", 2*gib))
	if _, err := e.p.Prepare(c); err != nil {
		t.Fatal(err)
	}
	// Reboot: /var/run/cdi is empty, the render node of the same PCI device changed.
	if err := os.RemoveAll(e.cdiRoot); err != nil {
		t.Fatal(err)
	}
	e.inv["pci-0000-06-00-0"] = published("pci-0000-06-00-0", "0000:06:00.0", "renderD129")
	e.reopen(t)

	got, err := e.p.Prepare(c)
	if err != nil {
		t.Fatal(err)
	}
	specs := e.specs(t)
	if len(specs) != 1 {
		t.Fatalf("specs = %v", specs)
	}
	body := readSpec(t, specs[0])
	if !strings.Contains(body, "/dev/dri/renderD129") || !strings.Contains(body, "DRA_GPU_VRAM_BYTES=2147483648") {
		t.Fatalf("restored spec:\n%s", body)
	}
	if *got[0].ShareID != "s1" {
		t.Fatalf("devices = %+v", got)
	}
}

func TestRestoreDeviceGone(t *testing.T) {
	e := newEnv(t)
	c := claim("u1", result("pci-0000-06-00-0", "", 0))
	if _, err := e.p.Prepare(c); err != nil {
		t.Fatal(err)
	}
	delete(e.inv, "pci-0000-06-00-0")
	if _, err := e.p.Prepare(c); err == nil {
		t.Fatal("restoring onto a missing device must fail")
	}
}

func TestUnprepareUnknownClaim(t *testing.T) {
	if err := newEnv(t).p.Unprepare("nope"); err != nil {
		t.Fatal(err)
	}
}

// Two plugin instances overlap during a DaemonSet rolling update and share
// the checkpoint file; neither may resurrect or drop the other's claims.
func TestTwoInstancesShareTheCheckpoint(t *testing.T) {
	a := newEnv(t)
	b := &env{cdiRoot: a.cdiRoot, checkpoint: a.checkpoint, inv: a.inv}
	if _, err := a.p.Prepare(claim("z", result("pci-0000-06-00-0", "sz", gib))); err != nil {
		t.Fatal(err)
	}
	b.reopen(t) // B starts and loads a checkpoint that contains z
	if err := a.p.Unprepare("z"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.p.Prepare(claim("x", result("pci-0000-06-00-0", "sx", gib))); err != nil {
		t.Fatal(err)
	}
	if _, err := b.p.Prepare(claim("y", result("pci-0000-06-00-0", "sy", gib))); err != nil {
		t.Fatal(err)
	}
	final, err := state.Open(a.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	got := final.All()
	if _, ok := got["z"]; ok {
		t.Error("z was resurrected")
	}
	for _, uid := range []string{"x", "y"} {
		if _, ok := got[uid]; !ok {
			t.Errorf("%s was dropped", uid)
		}
	}
}

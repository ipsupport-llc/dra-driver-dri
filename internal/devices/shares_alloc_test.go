package devices_test

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/cel"
	"k8s.io/dynamic-resource-allocation/structured"

	"github.com/ipsupport-llc/dra-driver-dri/internal/devices"
)

type classes []*resourceapi.DeviceClass

func (c classes) List() ([]*resourceapi.DeviceClass, error) { return c, nil }
func (c classes) Get(name string) (*resourceapi.DeviceClass, error) {
	for _, dc := range c {
		if dc.Name == name {
			return dc, nil
		}
	}
	return nil, nil
}

// claim asks for one GPU with the given capacity requests (none: the defaults).
func claim(name string, requests map[resourceapi.QualifiedName]string) *resourceapi.ResourceClaim {
	r := resourceapi.ExactDeviceRequest{DeviceClassName: "gpu", AllocationMode: resourceapi.DeviceAllocationModeExactCount, Count: 1}
	if len(requests) > 0 {
		r.Capacity = &resourceapi.CapacityRequirements{Requests: map[resourceapi.QualifiedName]resource.Quantity{}}
		for k, v := range requests {
			r.Capacity.Requests[k] = resource.MustParse(v)
		}
	}
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID(name)},
		Spec:       resourceapi.ResourceClaimSpec{Devices: resourceapi.DeviceClaim{Requests: []resourceapi.DeviceRequest{{Name: "gpu", Exactly: &r}}}},
	}
}

// allocate runs the scheduler's allocator for claims on one node with one
// 6080Mi GPU published with shares, and reports whether all of them fit.
func allocate(t *testing.T, shares int64, claims ...*resourceapi.ResourceClaim) bool {
	t.Helper()
	c := cfg()
	c.Shares = shares
	pubs := []devices.Published{{Name: "pci-0000-02-00-0", GPU: gpu("0000:02:00.0", "renderD128"), Vendor: "amd", VRAMTotal: 6144 * mib, VRAMPublished: 6080 * mib}}
	res := devices.Resources(c, pubs)
	var slices []*resourceapi.ResourceSlice
	for pool, p := range res.Pools {
		for _, s := range p.Slices {
			slices = append(slices, &resourceapi.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{Name: pool + "-slice"},
				Spec: resourceapi.ResourceSliceSpec{
					Driver: "gpu.example.com", NodeName: &c.NodeName, Devices: s.Devices,
					Pool: resourceapi.ResourcePool{Name: pool, ResourceSliceCount: 1},
				},
			})
		}
	}
	dc := &resourceapi.DeviceClass{ObjectMeta: metav1.ObjectMeta{Name: "gpu"},
		Spec: resourceapi.DeviceClassSpec{Selectors: []resourceapi.DeviceSelector{{CEL: &resourceapi.CELDeviceSelector{Expression: `device.driver == "gpu.example.com"`}}}}}
	features := structured.Features{ConsumableCapacity: true}
	a, err := structured.NewAllocator(context.Background(), features, structured.AllocatedState{},
		classes{dc}, slices, cel.NewCache(10, cel.Features{}))
	if err != nil {
		t.Fatal(err)
	}
	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: c.NodeName}}
	results, err := a.Allocate(context.Background(), node, claims)
	return err == nil && len(results) == len(claims)
}

// The scheduler, not the driver, enforces shares: a claim that does not ask
// takes the default one share, a claim can ask for more.
func TestSharesCapClaimsPerGPU(t *testing.T) {
	chat := func(n string) *resourceapi.ResourceClaim {
		return claim(n, map[resourceapi.QualifiedName]string{"vram": "4608Mi"})
	}
	embed := func(n string) *resourceapi.ResourceClaim {
		return claim(n, map[resourceapi.QualifiedName]string{"vram": "1Gi", "shares": "2"})
	}
	small := func(n string) *resourceapi.ResourceClaim {
		return claim(n, map[resourceapi.QualifiedName]string{"vram": "1Gi"})
	}
	cases := []struct {
		name   string
		shares int64
		claims []*resourceapi.ResourceClaim
		fit    bool
	}{
		{"chat and embeddings share a GPU", 3, []*resourceapi.ResourceClaim{chat("c"), embed("e")}, true},
		{"two embeddings do not", 3, []*resourceapi.ResourceClaim{embed("e1"), embed("e2")}, false},
		{"three small claims fit three shares", 3, []*resourceapi.ResourceClaim{small("s1"), small("s2"), small("s3")}, true},
		{"a fourth does not", 3, []*resourceapi.ResourceClaim{small("s1"), small("s2"), small("s3"), small("s4")}, false},
		{"without shares only VRAM counts", 0, []*resourceapi.ResourceClaim{small("s1"), small("s2"), small("s3"), small("s4")}, true},
		{"a whole-GPU claim takes one share and all VRAM", 3, []*resourceapi.ResourceClaim{claim("w", nil), small("s")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := allocate(t, tc.shares, tc.claims...); got != tc.fit {
				t.Fatalf("fit = %v, want %v", got, tc.fit)
			}
		})
	}
}

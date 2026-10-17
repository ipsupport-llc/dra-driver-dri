// Package devices turns discovered GPUs into DRA devices.
package devices

import (
	"fmt"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/utils/ptr"

	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfs"
	"github.com/ipsupport-llc/dra-driver-dri/internal/vram"
)

// CapacityVRAM is the consumable capacity a claim requests.
const CapacityVRAM resourceapi.QualifiedName = "vram"

// DefaultStep is the request granularity and the minimum request.
const DefaultStep int64 = 64 << 20

// Config holds the node-level publishing settings.
type Config struct {
	NodeName     string
	ReserveBytes int64 // subtracted from each GPU's VRAM
	StepBytes    int64 // request granularity; published VRAM is rounded down to it
}

func (c Config) step() int64 {
	if c.StepBytes > 0 {
		return c.StepBytes
	}
	return DefaultStep
}

// Published is a GPU as offered to the scheduler.
type Published struct {
	Name          string
	GPU           sysfs.GPU
	Vendor        string
	VRAMTotal     int64
	VRAMPublished int64
}

// Skipped is a GPU left out of the ResourceSlice, with the reason.
type Skipped struct {
	GPU    sysfs.GPU
	Reason error
}

// DeviceName derives a stable device name from the PCI address.
func DeviceName(pciAddress string) string {
	return "pci-" + strings.NewReplacer(":", "-", ".", "-").Replace(pciAddress)
}

// VendorName maps a PCI vendor ID to a short name.
func VendorName(vendorID string) string {
	switch strings.ToLower(vendorID) {
	case "0x1002":
		return "amd"
	case "0x8086":
		return "intel"
	case "0x10de":
		return "nvidia"
	default:
		return "unknown"
	}
}

// Build decides which GPUs to publish and how much VRAM each offers.
func Build(cfg Config, gpus []sysfs.GPU, det vram.Detector) ([]Published, []Skipped) {
	step := cfg.step()
	var pubs []Published
	var skipped []Skipped
	for _, g := range gpus {
		total, err := det.Total(g)
		if err != nil {
			skipped = append(skipped, Skipped{GPU: g, Reason: err})
			continue
		}
		published := (total - cfg.ReserveBytes) / step * step
		if published < step {
			skipped = append(skipped, Skipped{GPU: g, Reason: fmt.Errorf(
				"%s: %d bytes of VRAM minus the %d-byte reserve is less than one %d-byte step",
				g.PCIAddress, total, cfg.ReserveBytes, step)})
			continue
		}
		pubs = append(pubs, Published{
			Name:          DeviceName(g.PCIAddress),
			GPU:           g,
			Vendor:        VendorName(g.VendorID),
			VRAMTotal:     total,
			VRAMPublished: published,
		})
	}
	return pubs, skipped
}

// Resources builds the content of this node's ResourceSlice.
func Resources(cfg Config, pubs []Published) resourceslice.DriverResources {
	step := cfg.step()
	devs := make([]resourceapi.Device, 0, len(pubs))
	for _, p := range pubs {
		value := *resource.NewQuantity(p.VRAMPublished, resource.BinarySI)
		def, maxQ := value.DeepCopy(), value.DeepCopy()
		minQ := *resource.NewQuantity(step, resource.BinarySI)
		stepQ := minQ.DeepCopy()
		devs = append(devs, resourceapi.Device{
			Name: p.Name,
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"vendor":       {StringValue: ptr.To(p.Vendor)},
				"kernelDriver": {StringValue: ptr.To(p.GPU.KernelDriver)},
				"deviceID":     {StringValue: ptr.To(p.GPU.DeviceID)},
				"renderNode":   {StringValue: ptr.To(p.GPU.RenderNode)},
				deviceattribute.StandardDeviceAttributePCIBusID: {StringValue: ptr.To(p.GPU.PCIAddress)},
			},
			Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
				CapacityVRAM: {
					Value: value,
					RequestPolicy: &resourceapi.CapacityRequestPolicy{
						Default: &def,
						ValidRange: &resourceapi.CapacityRequestPolicyRange{
							Min:  &minQ,
							Step: &stepQ,
							Max:  &maxQ,
						},
					},
				},
			},
			AllowMultipleAllocations: ptr.To(true),
		})
	}
	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			cfg.NodeName: {Slices: []resourceslice.Slice{{Devices: devs}}},
		},
	}
}

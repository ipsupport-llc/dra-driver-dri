package metrics

import (
	"github.com/ipsupport-llc/dra-driver-dri/internal/devices"
	"github.com/ipsupport-llc/dra-driver-dri/internal/prepare"
	"github.com/ipsupport-llc/dra-driver-dri/internal/procvram"
)

// ClaimVRAM is a pod's actual VRAM on a device it claimed, next to what the
// claim promised.
type ClaimVRAM struct {
	Device, Namespace, Pod string
	UsedBytes              int64
	ClaimedBytes           int64
}

// Attribute matches per-process VRAM to the pods that claimed each device: a
// pod's VRAM on a GPU is the sum of its processes' VRAM on that GPU's PCI
// address. A claimed pod with no GPU process is reported at zero, so the gap
// between what it claimed and what it uses is visible either way.
func Attribute(claims []prepare.ClaimPod, pubs []devices.Published, usages []procvram.Usage) []ClaimVRAM {
	pci := make(map[string]string, len(pubs))
	for _, p := range pubs {
		pci[p.Name] = p.GPU.PCIAddress
	}
	type key struct{ pod, pci string }
	used := map[key]int64{}
	for _, u := range usages {
		if u.PodUID == "" {
			continue
		}
		used[key{u.PodUID, u.PCI}] += u.VRAMBytes
	}
	out := make([]ClaimVRAM, 0, len(claims))
	for _, c := range claims {
		out = append(out, ClaimVRAM{
			Device:       c.Device,
			Namespace:    c.PodNamespace,
			Pod:          c.PodName,
			UsedBytes:    used[key{c.PodUID, pci[c.Device]}],
			ClaimedBytes: c.ClaimedBytes,
		})
	}
	return out
}

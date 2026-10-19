// Package prepare turns allocated ResourceClaims into CDI specs.
package prepare

import (
	"fmt"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/utils/ptr"

	"github.com/ipsupport-llc/dra-driver-dri/internal/cdi"
	"github.com/ipsupport-llc/dra-driver-dri/internal/devices"
	"github.com/ipsupport-llc/dra-driver-dri/internal/state"
)

// Inventory returns the devices this node publishes, keyed by device name.
type Inventory func() map[string]devices.Published

// Preparer prepares and unprepares claims of one driver on one node.
type Preparer struct {
	DriverName string
	Inventory  Inventory
	CDI        *cdi.Writer
	Store      *state.Store
}

// Prepare writes the CDI spec of a claim and records it. Preparing a claim
// that is already recorded rewrites its spec from the record (the spec
// directory is tmpfs and is empty after a reboot) and returns the same
// devices.
func (p *Preparer) Prepare(claim *resourceapi.ResourceClaim) ([]kubeletplugin.Device, error) {
	uid := string(claim.UID)
	if err := p.Store.Reload(); err != nil {
		return nil, err
	}
	inv := p.Inventory()
	if recorded, ok := p.Store.Get(uid); ok {
		edits, err := editsFor(recorded, inv)
		if err != nil {
			return nil, fmt.Errorf("restore claim %s/%s: %w", claim.Namespace, claim.Name, err)
		}
		if err := p.CDI.WriteClaim(uid, edits); err != nil {
			return nil, err
		}
		return toKubelet(recorded), nil
	}
	if claim.Status.Allocation == nil {
		return nil, fmt.Errorf("claim %s/%s is not allocated", claim.Namespace, claim.Name)
	}
	// The pod the claim is reserved for (same namespace as the claim), for
	// per-pod VRAM attribution. Empty when not yet reserved.
	var podUID, podName string
	for _, ref := range claim.Status.ReservedFor {
		if ref.Resource == "pods" {
			podUID, podName = string(ref.UID), ref.Name
			break
		}
	}
	var recs []state.Device
	for _, r := range claim.Status.Allocation.Devices.Results {
		if r.Driver != p.DriverName {
			continue
		}
		pub, ok := inv[r.Device]
		if !ok {
			return nil, fmt.Errorf("claim %s/%s: device %s is not available on this node", claim.Namespace, claim.Name, r.Device)
		}
		vram := pub.VRAMPublished
		if q, ok := r.ConsumedCapacity[devices.CapacityVRAM]; ok {
			vram = q.Value()
		}
		share := ""
		if r.ShareID != nil {
			share = string(*r.ShareID)
		}
		rec := state.Device{Request: r.Request, Pool: r.Pool, Device: r.Device, ShareID: share, VRAMBytes: vram,
			PodUID: podUID, PodNamespace: claim.Namespace, PodName: podName}
		rec.CDIDeviceIDs = []string{p.CDI.QualifiedID(uid, edit(rec, pub))}
		recs = append(recs, rec)
	}
	if len(recs) == 0 {
		return nil, fmt.Errorf("claim %s/%s has no devices of driver %s", claim.Namespace, claim.Name, p.DriverName)
	}
	edits, err := editsFor(recs, inv)
	if err != nil {
		return nil, err
	}
	if err := p.CDI.WriteClaim(uid, edits); err != nil {
		return nil, err
	}
	if err := p.Store.Put(uid, recs); err != nil {
		_ = p.CDI.DeleteClaim(uid)
		return nil, err
	}
	return toKubelet(recs), nil
}

// Unprepare removes the claim's spec and record. Unknown claims are a no-op.
func (p *Preparer) Unprepare(uid string) error {
	if err := p.Store.Reload(); err != nil {
		return err
	}
	if err := p.CDI.DeleteClaim(uid); err != nil {
		return err
	}
	return p.Store.Delete(uid)
}

// Allocated sums the VRAM of all prepared claims per device name.
func (p *Preparer) Allocated() map[string]int64 {
	out := map[string]int64{}
	for _, recs := range p.Store.All() {
		for _, r := range recs {
			out[r.Device] += r.VRAMBytes
		}
	}
	return out
}

// Count is the number of prepared claims.
func (p *Preparer) Count() int {
	return len(p.Store.All())
}

// ClaimPod ties a prepared device to the pod that claimed it, with the VRAM
// the claim promised.
type ClaimPod struct {
	Device       string
	PodUID       string
	PodNamespace string
	PodName      string
	ClaimedBytes int64
}

// ClaimPods lists the pod behind every prepared device whose pod is known.
func (p *Preparer) ClaimPods() []ClaimPod {
	var out []ClaimPod
	for _, recs := range p.Store.All() {
		for _, r := range recs {
			if r.PodUID == "" {
				continue
			}
			out = append(out, ClaimPod{Device: r.Device, PodUID: r.PodUID, PodNamespace: r.PodNamespace, PodName: r.PodName, ClaimedBytes: r.VRAMBytes})
		}
	}
	return out
}

func edit(r state.Device, pub devices.Published) cdi.Edit {
	return cdi.Edit{Device: r.Device, ShareID: r.ShareID, RenderNode: pub.GPU.RenderNode,
		PCIAddress: pub.GPU.PCIAddress, Vendor: pub.Vendor, VRAMBytes: r.VRAMBytes}
}

func editsFor(recs []state.Device, inv map[string]devices.Published) ([]cdi.Edit, error) {
	edits := make([]cdi.Edit, 0, len(recs))
	for _, r := range recs {
		pub, ok := inv[r.Device]
		if !ok {
			return nil, fmt.Errorf("device %s is not available on this node", r.Device)
		}
		edits = append(edits, edit(r, pub))
	}
	return edits, nil
}

func toKubelet(recs []state.Device) []kubeletplugin.Device {
	out := make([]kubeletplugin.Device, 0, len(recs))
	for _, r := range recs {
		var share *types.UID
		if r.ShareID != "" {
			share = ptr.To(types.UID(r.ShareID))
		}
		out = append(out, kubeletplugin.Device{
			Requests:     []string{r.Request},
			PoolName:     r.Pool,
			DeviceName:   r.Device,
			CDIDeviceIDs: r.CDIDeviceIDs,
			ShareID:      share,
		})
	}
	return out
}

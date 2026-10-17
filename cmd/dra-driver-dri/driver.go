package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	"github.com/ipsupport-llc/dra-driver-dri/internal/devices"
	"github.com/ipsupport-llc/dra-driver-dri/internal/metrics"
	"github.com/ipsupport-llc/dra-driver-dri/internal/prepare"
	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfs"
	"github.com/ipsupport-llc/dra-driver-dri/internal/vram"
)

type publisher interface {
	PublishResources(ctx context.Context, resources resourceslice.DriverResources) error
}

// Driver is the kubelet plugin: it keeps the inventory current and
// prepares claims.
type Driver struct {
	cfg     devices.Config
	sysRoot string
	det     vram.Detector
	prep    *prepare.Preparer
	metrics *metrics.Metrics
	cancel  context.CancelCauseFunc

	mu        sync.RWMutex
	pubs      []devices.Published
	key       string
	published bool
}

func newDriver(cfg devices.Config, sysRoot string, det vram.Detector, m *metrics.Metrics, cancel context.CancelCauseFunc) *Driver {
	return &Driver{cfg: cfg, sysRoot: sysRoot, det: det, metrics: m, cancel: cancel}
}

func (d *Driver) scan() ([]devices.Published, []devices.Skipped, string, error) {
	gpus, err := sysfs.Scan(d.sysRoot)
	if err != nil {
		d.metrics.DiscoveryErrors.Inc()
		return nil, nil, "", fmt.Errorf("scan %s: %w", d.sysRoot, err)
	}
	pubs, skipped := devices.Build(d.cfg, gpus, d.det)
	var b strings.Builder
	for _, p := range pubs {
		fmt.Fprintf(&b, "%s|%s|%s|%s|%d;", p.Name, p.GPU.RenderNode, p.GPU.KernelDriver, p.GPU.DeviceID, p.VRAMPublished)
	}
	b.WriteString("#")
	for _, s := range skipped {
		fmt.Fprintf(&b, "%s|%v;", s.GPU.PCIAddress, s.Reason)
	}
	return pubs, skipped, b.String(), nil
}

// Refresh loads the inventory without publishing (before the kubelet
// plugin starts).
func (d *Driver) Refresh(_ context.Context) error {
	pubs, skipped, _, err := d.scan()
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.pubs = pubs
	d.mu.Unlock()
	d.metrics.SetSkipped(len(skipped))
	return nil
}

// Rescan refreshes the inventory and republishes it when it changed or was
// never published.
func (d *Driver) Rescan(ctx context.Context, pub publisher) error {
	logger := klog.FromContext(ctx)
	pubs, skipped, key, err := d.scan()
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.pubs = pubs
	changed := !d.published || key != d.key
	d.mu.Unlock()
	d.metrics.SetSkipped(len(skipped))
	if changed {
		for _, s := range skipped {
			logger.Info("GPU not published", "pciBusID", s.GPU.PCIAddress, "reason", s.Reason.Error())
		}
		logger.Info("Publishing GPUs", "count", len(pubs))
		if err := pub.PublishResources(ctx, devices.Resources(d.cfg, pubs)); err != nil {
			return fmt.Errorf("publish resources: %w", err)
		}
		d.mu.Lock()
		d.key, d.published = key, true
		d.mu.Unlock()
	}
	d.refreshMetrics()
	return nil
}

// Inventory returns the published devices by name.
func (d *Driver) Inventory() map[string]devices.Published {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make(map[string]devices.Published, len(d.pubs))
	for _, p := range d.pubs {
		out[p.Name] = p
	}
	return out
}

func (d *Driver) refreshMetrics() {
	pubs := slices.Collect(maps.Values(d.Inventory()))
	d.metrics.Update(pubs, d.prep.Allocated(), d.det.Used, d.prep.Count())
}

func (d *Driver) skippedGauge() prometheus.Collector { return d.metrics.SkippedGauge() }

// PrepareResourceClaims implements kubeletplugin.DRAPlugin.
func (d *Driver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	logger := klog.FromContext(ctx)
	out := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))
	// Check the GPUs as they are now, not as of the last periodic scan: a GPU
	// unbound since then must not be handed out, and render-node numbers
	// may have moved.
	if err := d.Refresh(ctx); err != nil {
		for _, c := range claims {
			d.metrics.PrepareErrors.Inc()
			out[c.UID] = kubeletplugin.PrepareResult{Err: fmt.Errorf("rescan GPUs: %w", err)}
		}
		return out, nil
	}
	for _, c := range claims {
		devs, err := d.prep.Prepare(c)
		if err != nil {
			d.metrics.PrepareErrors.Inc()
			logger.Error(err, "Prepare failed", "claim", klog.KObj(c))
			out[c.UID] = kubeletplugin.PrepareResult{Err: err}
			continue
		}
		out[c.UID] = kubeletplugin.PrepareResult{Devices: devs}
	}
	d.refreshMetrics()
	return out, nil
}

// UnprepareResourceClaims implements kubeletplugin.DRAPlugin.
func (d *Driver) UnprepareResourceClaims(_ context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	out := make(map[types.UID]error, len(claims))
	for _, c := range claims {
		out[c.UID] = d.prep.Unprepare(string(c.UID))
	}
	d.refreshMetrics()
	return out, nil
}

// HandleError implements kubeletplugin.DRAPlugin: unrecoverable background
// errors stop the process so the DaemonSet restarts it.
func (d *Driver) HandleError(ctx context.Context, err error, msg string) {
	utilruntime.HandleErrorWithContext(ctx, err, msg)
	if !errors.Is(err, kubeletplugin.ErrRecoverable) {
		d.cancel(fmt.Errorf("fatal background error: %s: %w", msg, err))
	}
}

// WatchHealthStatus implements kubeletplugin.DRAPlugin; device health is
// not reported in v0.1.
func (d *Driver) WatchHealthStatus(context.Context, chan<- kubeletplugin.DeviceHealthReport) error {
	return kubeletplugin.ErrHealthNotSupported
}

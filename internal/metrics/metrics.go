// Package metrics exports per-GPU VRAM gauges and driver counters.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ipsupport-llc/dra-driver-dri/internal/devices"
	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfs"
)

var labels = []string{"device", "vendor", "pci_bus_id"}

// Metrics holds the driver's collectors in their own registry.
type Metrics struct {
	reg                               *prometheus.Registry
	total, published, allocated, used *prometheus.GaugeVec
	claims, skipped                   prometheus.Gauge
	PrepareErrors, DiscoveryErrors    prometheus.Counter
}

func gauge(name, help string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
}

// New creates and registers all collectors.
func New() *Metrics {
	m := &Metrics{
		reg:             prometheus.NewRegistry(),
		total:           gauge("dri_vram_total_bytes", "VRAM reported by the GPU."),
		published:       gauge("dri_vram_published_bytes", "VRAM offered to the scheduler."),
		allocated:       gauge("dri_vram_allocated_bytes", "VRAM promised to prepared claims."),
		used:            gauge("dri_vram_used_bytes", "VRAM in use according to the kernel driver."),
		claims:          prometheus.NewGauge(prometheus.GaugeOpts{Name: "dri_claims_prepared", Help: "Claims prepared on this node."}),
		skipped:         prometheus.NewGauge(prometheus.GaugeOpts{Name: "dri_gpus_skipped", Help: "GPUs found but not published (unknown VRAM, below the reserve)."}),
		PrepareErrors:   prometheus.NewCounter(prometheus.CounterOpts{Name: "dri_prepare_errors_total", Help: "Failed claim preparations."}),
		DiscoveryErrors: prometheus.NewCounter(prometheus.CounterOpts{Name: "dri_discovery_errors_total", Help: "Failed GPU scans."}),
	}
	m.reg.MustRegister(m.total, m.published, m.allocated, m.used, m.claims, m.skipped, m.PrepareErrors, m.DiscoveryErrors)
	return m
}

// Registry is served on the metrics endpoint.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// Update replaces the per-GPU series with the current state.
func (m *Metrics) Update(pubs []devices.Published, allocated map[string]int64, used func(sysfs.GPU) (int64, bool), claims int) {
	for _, g := range []*prometheus.GaugeVec{m.total, m.published, m.allocated, m.used} {
		g.Reset()
	}
	for _, p := range pubs {
		l := prometheus.Labels{"device": p.Name, "vendor": p.Vendor, "pci_bus_id": p.GPU.PCIAddress}
		m.total.With(l).Set(float64(p.VRAMTotal))
		m.published.With(l).Set(float64(p.VRAMPublished))
		m.allocated.With(l).Set(float64(allocated[p.Name]))
		if u, ok := used(p.GPU); ok {
			m.used.With(l).Set(float64(u))
		}
	}
	m.claims.Set(float64(claims))
}

// SetSkipped records how many GPUs the last scan left out.
func (m *Metrics) SetSkipped(n int) { m.skipped.Set(float64(n)) }

// SkippedGauge exposes the dri_gpus_skipped gauge for tests.
func (m *Metrics) SkippedGauge() prometheus.Gauge { return m.skipped }

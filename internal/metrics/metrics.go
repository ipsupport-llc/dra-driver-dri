// Package metrics exports per-GPU gauges and driver counters.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ipsupport-llc/dra-driver-dri/internal/devices"
	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfs"
	"github.com/ipsupport-llc/dra-driver-dri/internal/vram"
)

var labels = []string{"device", "vendor", "pci_bus_id"}

// Metrics holds the driver's collectors in their own registry.
type Metrics struct {
	reg                            *prometheus.Registry
	total, published, allocated    *prometheus.GaugeVec
	stats                          map[vram.Stat]*prometheus.GaugeVec
	claims, skipped                prometheus.Gauge
	PrepareErrors, DiscoveryErrors prometheus.Counter
}

func gauge(name, help string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
}

// New creates and registers all collectors.
func New() *Metrics {
	m := &Metrics{
		reg:       prometheus.NewRegistry(),
		total:     gauge("dri_vram_total_bytes", "VRAM reported by the GPU."),
		published: gauge("dri_vram_published_bytes", "VRAM offered to the scheduler."),
		allocated: gauge("dri_vram_allocated_bytes", "VRAM promised to prepared claims."),
		stats: map[vram.Stat]*prometheus.GaugeVec{
			vram.VRAMUsed:    gauge("dri_vram_used_bytes", "VRAM in use according to the kernel driver."),
			vram.GTTUsed:     gauge("dri_gtt_used_bytes", "System RAM mapped for the GPU (GTT); grows when VRAM is evicted to RAM."),
			vram.Temperature: gauge("dri_gpu_temperature_celsius", "GPU temperature."),
			vram.CoreClock:   gauge("dri_gpu_core_clock_hertz", "GPU core clock."),
			vram.MemoryClock: gauge("dri_gpu_memory_clock_hertz", "GPU memory clock."),
		},
		claims:          prometheus.NewGauge(prometheus.GaugeOpts{Name: "dri_claims_prepared", Help: "Claims prepared on this node."}),
		skipped:         prometheus.NewGauge(prometheus.GaugeOpts{Name: "dri_gpus_skipped", Help: "GPUs found but not published (unknown VRAM, below the reserve)."}),
		PrepareErrors:   prometheus.NewCounter(prometheus.CounterOpts{Name: "dri_prepare_errors_total", Help: "Failed claim preparations."}),
		DiscoveryErrors: prometheus.NewCounter(prometheus.CounterOpts{Name: "dri_discovery_errors_total", Help: "Failed GPU scans."}),
	}
	m.reg.MustRegister(m.total, m.published, m.allocated, m.claims, m.skipped, m.PrepareErrors, m.DiscoveryErrors)
	for _, g := range m.stats {
		m.reg.MustRegister(g)
	}
	return m
}

// Registry is served on the metrics endpoint.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// Update replaces the per-GPU series with the current state. A reading the
// backend cannot take leaves its series out.
func (m *Metrics) Update(pubs []devices.Published, allocated map[string]int64, stats func(sysfs.GPU) vram.Stats, claims int) {
	for _, g := range []*prometheus.GaugeVec{m.total, m.published, m.allocated} {
		g.Reset()
	}
	for _, g := range m.stats {
		g.Reset()
	}
	for _, p := range pubs {
		l := prometheus.Labels{"device": p.Name, "vendor": p.Vendor, "pci_bus_id": p.GPU.PCIAddress}
		m.total.With(l).Set(float64(p.VRAMTotal))
		m.published.With(l).Set(float64(p.VRAMPublished))
		m.allocated.With(l).Set(float64(allocated[p.Name]))
		for k, v := range stats(p.GPU) {
			if g, ok := m.stats[k]; ok {
				g.With(l).Set(v)
			}
		}
	}
	m.claims.Set(float64(claims))
}

// SetSkipped records how many GPUs the last scan left out.
func (m *Metrics) SetSkipped(n int) { m.skipped.Set(float64(n)) }

// SkippedGauge exposes the dri_gpus_skipped gauge for tests.
func (m *Metrics) SkippedGauge() prometheus.Gauge { return m.skipped }

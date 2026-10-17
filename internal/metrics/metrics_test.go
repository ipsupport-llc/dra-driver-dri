package metrics_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ipsupport-llc/dra-driver-dri/internal/devices"
	"github.com/ipsupport-llc/dra-driver-dri/internal/metrics"
	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfs"
	"github.com/ipsupport-llc/dra-driver-dri/internal/vram"
)

func TestUpdate(t *testing.T) {
	m := metrics.New()
	pubs := []devices.Published{{Name: "pci-0000-06-00-0", Vendor: "amd", VRAMTotal: 6442450944, VRAMPublished: 6174015488,
		GPU: sysfs.GPU{PCIAddress: "0000:06:00.0", KernelDriver: "amdgpu"}}}
	stats := func(sysfs.GPU) vram.Stats {
		return vram.Stats{vram.VRAMUsed: 1000, vram.GTTUsed: 2000, vram.Temperature: 49, vram.CoreClock: 850e6, vram.MemoryClock: 1370e6}
	}
	m.Update(pubs, map[string]int64{"pci-0000-06-00-0": 2147483648}, stats, 1)
	m.SetSkipped(2)
	m.PrepareErrors.Inc()

	want := `
# HELP dri_claims_prepared Claims prepared on this node.
# TYPE dri_claims_prepared gauge
dri_claims_prepared 1
# HELP dri_gpu_core_clock_hertz GPU core clock.
# TYPE dri_gpu_core_clock_hertz gauge
dri_gpu_core_clock_hertz{device="pci-0000-06-00-0",pci_bus_id="0000:06:00.0",vendor="amd"} 8.5e+08
# HELP dri_gpu_memory_clock_hertz GPU memory clock.
# TYPE dri_gpu_memory_clock_hertz gauge
dri_gpu_memory_clock_hertz{device="pci-0000-06-00-0",pci_bus_id="0000:06:00.0",vendor="amd"} 1.37e+09
# HELP dri_gpu_temperature_celsius GPU temperature.
# TYPE dri_gpu_temperature_celsius gauge
dri_gpu_temperature_celsius{device="pci-0000-06-00-0",pci_bus_id="0000:06:00.0",vendor="amd"} 49
# HELP dri_gpus_skipped GPUs found but not published (unknown VRAM, below the reserve).
# TYPE dri_gpus_skipped gauge
dri_gpus_skipped 2
# HELP dri_gtt_used_bytes System RAM mapped for the GPU (GTT); grows when VRAM is evicted to RAM.
# TYPE dri_gtt_used_bytes gauge
dri_gtt_used_bytes{device="pci-0000-06-00-0",pci_bus_id="0000:06:00.0",vendor="amd"} 2000
# HELP dri_prepare_errors_total Failed claim preparations.
# TYPE dri_prepare_errors_total counter
dri_prepare_errors_total 1
# HELP dri_vram_allocated_bytes VRAM promised to prepared claims.
# TYPE dri_vram_allocated_bytes gauge
dri_vram_allocated_bytes{device="pci-0000-06-00-0",pci_bus_id="0000:06:00.0",vendor="amd"} 2.147483648e+09
# HELP dri_vram_published_bytes VRAM offered to the scheduler.
# TYPE dri_vram_published_bytes gauge
dri_vram_published_bytes{device="pci-0000-06-00-0",pci_bus_id="0000:06:00.0",vendor="amd"} 6.174015488e+09
# HELP dri_vram_total_bytes VRAM reported by the GPU.
# TYPE dri_vram_total_bytes gauge
dri_vram_total_bytes{device="pci-0000-06-00-0",pci_bus_id="0000:06:00.0",vendor="amd"} 6.442450944e+09
# HELP dri_vram_used_bytes VRAM in use according to the kernel driver.
# TYPE dri_vram_used_bytes gauge
dri_vram_used_bytes{device="pci-0000-06-00-0",pci_bus_id="0000:06:00.0",vendor="amd"} 1000
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want),
		"dri_claims_prepared", "dri_gpus_skipped", "dri_gtt_used_bytes",
		"dri_gpu_temperature_celsius", "dri_gpu_core_clock_hertz", "dri_gpu_memory_clock_hertz", "dri_prepare_errors_total",
		"dri_vram_allocated_bytes", "dri_vram_published_bytes", "dri_vram_total_bytes", "dri_vram_used_bytes"); err != nil {
		t.Fatal(err)
	}

	// A GPU that disappears drops out of the gauges.
	m.Update(nil, nil, stats, 0)
	for _, name := range []string{"dri_vram_total_bytes", "dri_gtt_used_bytes", "dri_gpu_temperature_celsius"} {
		if n, err := testutil.GatherAndCount(m.Registry(), name); err != nil || n != 0 {
			t.Fatalf("stale %s series: %d, %v", name, n, err)
		}
	}
}

func TestUpdateUnknownReadings(t *testing.T) {
	m := metrics.New()
	pubs := []devices.Published{{Name: "pci-0000-03-00-0", Vendor: "intel", VRAMTotal: 1 << 34, VRAMPublished: 1 << 34,
		GPU: sysfs.GPU{PCIAddress: "0000:03:00.0", KernelDriver: "xe"}}}
	m.Update(pubs, nil, func(sysfs.GPU) vram.Stats { return vram.Stats{} }, 0)
	// A reading the backend cannot take is absent, not zero.
	for _, name := range []string{"dri_vram_used_bytes", "dri_gtt_used_bytes", "dri_gpu_temperature_celsius", "dri_gpu_core_clock_hertz", "dri_gpu_memory_clock_hertz"} {
		if n, err := testutil.GatherAndCount(m.Registry(), name); err != nil || n != 0 {
			t.Fatalf("%s: %d series, %v; want none", name, n, err)
		}
	}
	if n, _ := testutil.GatherAndCount(m.Registry(), "dri_vram_total_bytes"); n != 1 {
		t.Fatalf("dri_vram_total_bytes: %d series, want 1", n)
	}
}

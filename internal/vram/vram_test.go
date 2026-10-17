package vram_test

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfs"
	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfstest"
	"github.com/ipsupport-llc/dra-driver-dri/internal/vram"
)

const gib = int64(1) << 30

func scanOne(t *testing.T, g sysfstest.FakeGPU) sysfs.GPU {
	t.Helper()
	root := t.TempDir()
	sysfstest.AddGPU(t, root, g)
	gpus, err := sysfs.Scan(root)
	if err != nil || len(gpus) != 1 {
		t.Fatalf("scan: %v %v", gpus, err)
	}
	return gpus[0]
}

func d700(files map[string]string) sysfstest.FakeGPU {
	return sysfstest.FakeGPU{RenderNode: "renderD128", PCIAddress: "0000:06:00.0", VendorID: "0x1002", DeviceID: "0x6798", Driver: "amdgpu", Files: files}
}

func TestAMDGPUTotalAndUsed(t *testing.T) {
	g := scanOne(t, d700(map[string]string{"mem_info_vram_total": "6442450944\n", "mem_info_vram_used": "123456\n"}))
	d := vram.NewDetector(nil)
	total, err := d.Total(g)
	if err != nil || total != 6*gib {
		t.Fatalf("Total = %d, %v; want %d", total, err, 6*gib)
	}
	if s := d.Stats(g); s[vram.VRAMUsed] != 123456 {
		t.Fatalf("Stats = %v; want VRAMUsed 123456", s)
	}
}

func TestAMDGPUStats(t *testing.T) {
	g := scanOne(t, d700(map[string]string{
		"mem_info_vram_total":      "6442450944\n",
		"mem_info_vram_used":       "2168455168\n",
		"mem_info_gtt_used":        "300941312\n",
		"hwmon/hwmon3/name":        "amdgpu\n",
		"hwmon/hwmon3/temp1_input": "49000\n",
		"hwmon/hwmon3/freq1_label": "mclk\n",
		"hwmon/hwmon3/freq1_input": "1370000000\n",
		"hwmon/hwmon3/freq2_label": "sclk\n",
		"hwmon/hwmon3/freq2_input": "850000000\n",
		"hwmon/hwmon3/freq3_label": "fclk\n",
		"hwmon/hwmon3/freq3_input": "1\n",
	}))
	want := vram.Stats{vram.VRAMUsed: 2168455168, vram.GTTUsed: 300941312, vram.Temperature: 49, vram.CoreClock: 850e6, vram.MemoryClock: 1370e6}
	if got := vram.NewDetector(nil).Stats(g); !maps.Equal(got, want) {
		t.Fatalf("Stats = %v, want %v", got, want)
	}
}

func TestAMDGPUStatsPartial(t *testing.T) {
	// No hwmon directory, unreadable GTT counter: only what can be read.
	g := scanOne(t, d700(map[string]string{"mem_info_vram_used": "1024\n", "mem_info_gtt_used": "n/a\n"}))
	want := vram.Stats{vram.VRAMUsed: 1024}
	if got := vram.NewDetector(nil).Stats(g); !maps.Equal(got, want) {
		t.Fatalf("Stats = %v, want %v", got, want)
	}
}

func TestOverrideWins(t *testing.T) {
	g := scanOne(t, d700(map[string]string{"mem_info_vram_total": "6442450944\n"}))
	d := vram.NewDetector(vram.Overrides{"0000:06:00.0": 4 * gib})
	if total, err := d.Total(g); err != nil || total != 4*gib {
		t.Fatalf("Total = %d, %v; want %d", total, err, 4*gib)
	}
}

func TestUnknownVRAM(t *testing.T) {
	cases := map[string]sysfstest.FakeGPU{
		"no backend for driver": {RenderNode: "renderD128", PCIAddress: "0000:03:00.0", VendorID: "0x8086", DeviceID: "0xe20b", Driver: "xe"},
		"file missing":          d700(nil),
		"zero":                  d700(map[string]string{"mem_info_vram_total": "0\n"}),
		"garbage":               d700(map[string]string{"mem_info_vram_total": "lots\n"}),
	}
	for name, fake := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := vram.NewDetector(nil).Total(scanOne(t, fake))
			if !errors.Is(err, vram.ErrUnknown) {
				t.Fatalf("err = %v, want ErrUnknown", err)
			}
		})
	}
}

func TestStatsWithoutBackend(t *testing.T) {
	g := scanOne(t, sysfstest.FakeGPU{RenderNode: "renderD128", PCIAddress: "0000:03:00.0", VendorID: "0x8086", DeviceID: "0xe20b", Driver: "xe"})
	if s := vram.NewDetector(vram.Overrides{"0000:03:00.0": 16 * gib}).Stats(g); len(s) != 0 {
		t.Fatalf("Stats reported %v without a backend", s)
	}
}

func TestLoadOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overrides.yaml")
	if err := os.WriteFile(path, []byte("\"0000:03:00.0\": 16Gi\n\"0000:0A:00.0\": 512Mi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := vram.LoadOverrides(path)
	if err != nil {
		t.Fatal(err)
	}
	if got["0000:03:00.0"] != 16*gib || got["0000:0a:00.0"] != 512<<20 || len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestLoadOverridesEmptyPath(t *testing.T) {
	got, err := vram.LoadOverrides("")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestLoadOverridesInvalid(t *testing.T) {
	for name, body := range map[string]string{"bad quantity": "\"0000:03:00.0\": sixteen\n", "negative": "\"0000:03:00.0\": -1Gi\n", "not a map": "- 1\n"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "o.yaml")
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := vram.LoadOverrides(path); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

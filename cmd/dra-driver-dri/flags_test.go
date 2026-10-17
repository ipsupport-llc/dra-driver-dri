package main

import (
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseFlagsDefaultsFromEnv(t *testing.T) {
	f, err := parseFlags(nil, env(map[string]string{"DRIVER_NAME": "gpu.example.com", "NODE_NAME": "node-a", "POD_UID": "pod-1"}))
	if err != nil {
		t.Fatal(err)
	}
	if f.DriverName != "gpu.example.com" || f.NodeName != "node-a" || f.PodUID != "pod-1" ||
		f.ReserveBytes != 256<<20 || f.RescanInterval != 30*time.Second || f.MetricsAddr != ":8080" ||
		f.SysRoot != "/sys" || f.CDIRoot != "/var/run/cdi" ||
		f.RegistrarDir != "/var/lib/kubelet/plugins_registry" || f.PluginsDir != "/var/lib/kubelet/plugins" {
		t.Fatalf("flags = %+v", f)
	}
}

func TestParseFlagsOverrideEnv(t *testing.T) {
	f, err := parseFlags([]string{"--vram-reserve=512Mi", "--rescan-interval=10s"},
		env(map[string]string{"DRIVER_NAME": "gpu.example.com", "NODE_NAME": "node-a", "VRAM_RESERVE": "1Gi"}))
	if err != nil {
		t.Fatal(err)
	}
	if f.ReserveBytes != 512<<20 || f.RescanInterval != 10*time.Second {
		t.Fatalf("flags = %+v", f)
	}
}

func TestParseFlagsErrors(t *testing.T) {
	base := map[string]string{"DRIVER_NAME": "gpu.example.com", "NODE_NAME": "node-a"}
	cases := map[string]struct {
		args []string
		env  map[string]string
	}{
		"no driver name":   {nil, map[string]string{"NODE_NAME": "node-a"}},
		"bad driver name":  {[]string{"--driver-name=Not_A_Domain"}, base},
		"no node name":     {nil, map[string]string{"DRIVER_NAME": "gpu.example.com"}},
		"bad reserve":      {[]string{"--vram-reserve=lots"}, base},
		"negative reserve": {[]string{"--vram-reserve=-1Mi"}, base},
		"too-fast rescan":  {[]string{"--rescan-interval=100ms"}, base},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseFlags(c.args, env(c.env)); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

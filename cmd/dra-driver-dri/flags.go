package main

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

// Flags is the plugin configuration. Every flag falls back to an
// environment variable, which is how the DaemonSet sets them.
type Flags struct {
	DriverName     string
	NodeName       string
	PodUID         string
	Overrides      string
	MetricsAddr    string
	SysRoot        string
	CDIRoot        string
	RegistrarDir   string
	PluginsDir     string
	ReserveBytes   int64
	RescanInterval time.Duration
}

func parseFlags(args []string, getenv func(string) string) (Flags, error) {
	or := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	var f Flags
	var reserve, interval string
	fs := flag.NewFlagSet("dra-driver-dri", flag.ContinueOnError)
	fs.StringVar(&f.DriverName, "driver-name", or("DRIVER_NAME", ""), "DRA driver name, a DNS subdomain owned by the operator (DRIVER_NAME)")
	fs.StringVar(&f.NodeName, "node-name", or("NODE_NAME", ""), "node this plugin runs on (NODE_NAME)")
	fs.StringVar(&f.PodUID, "pod-uid", or("POD_UID", ""), "UID of this pod, enables seamless DaemonSet updates (POD_UID)")
	fs.StringVar(&reserve, "vram-reserve", or("VRAM_RESERVE", "256Mi"), "VRAM held back per GPU (VRAM_RESERVE)")
	fs.StringVar(&interval, "rescan-interval", or("RESCAN_INTERVAL", "30s"), "sysfs rescan period (RESCAN_INTERVAL)")
	fs.StringVar(&f.Overrides, "overrides", or("OVERRIDES", ""), "YAML file of VRAM per PCI address (OVERRIDES)")
	fs.StringVar(&f.MetricsAddr, "metrics-addr", or("METRICS_ADDR", ":8080"), "Prometheus endpoint (METRICS_ADDR)")
	fs.StringVar(&f.SysRoot, "sys-root", or("SYS_ROOT", "/sys"), "sysfs mount (SYS_ROOT)")
	fs.StringVar(&f.CDIRoot, "cdi-root", or("CDI_ROOT", "/var/run/cdi"), "CDI spec directory (CDI_ROOT)")
	fs.StringVar(&f.RegistrarDir, "kubelet-registrar-directory-path", or("KUBELET_REGISTRAR_DIRECTORY_PATH", kubeletplugin.KubeletRegistryDir), "kubelet plugin registration directory")
	fs.StringVar(&f.PluginsDir, "kubelet-plugins-directory-path", or("KUBELET_PLUGINS_DIRECTORY_PATH", kubeletplugin.KubeletPluginsDir), "kubelet plugin data directory")
	if err := fs.Parse(args); err != nil {
		return Flags{}, err
	}

	if f.DriverName == "" {
		return Flags{}, fmt.Errorf("--driver-name is required")
	}
	if errs := validation.IsDNS1123Subdomain(f.DriverName); len(errs) > 0 {
		return Flags{}, fmt.Errorf("--driver-name %q: %s", f.DriverName, strings.Join(errs, "; "))
	}
	if f.NodeName == "" {
		return Flags{}, fmt.Errorf("--node-name is required")
	}
	q, err := resource.ParseQuantity(reserve)
	if err != nil {
		return Flags{}, fmt.Errorf("--vram-reserve: %w", err)
	}
	if q.Sign() < 0 {
		return Flags{}, fmt.Errorf("--vram-reserve must not be negative")
	}
	f.ReserveBytes = q.Value()
	if f.RescanInterval, err = time.ParseDuration(interval); err != nil {
		return Flags{}, fmt.Errorf("--rescan-interval: %w", err)
	}
	if f.RescanInterval < time.Second {
		return Flags{}, fmt.Errorf("--rescan-interval must be at least 1s")
	}
	return f, nil
}

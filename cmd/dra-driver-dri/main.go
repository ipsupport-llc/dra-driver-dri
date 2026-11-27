// Command dra-driver-dri is a Kubernetes DRA kubelet plugin for GPUs that
// expose a DRM render node.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	"github.com/ipsupport-llc/dra-driver-dri/internal/cdi"
	"github.com/ipsupport-llc/dra-driver-dri/internal/devices"
	"github.com/ipsupport-llc/dra-driver-dri/internal/hang"
	"github.com/ipsupport-llc/dra-driver-dri/internal/metrics"
	"github.com/ipsupport-llc/dra-driver-dri/internal/prepare"
	"github.com/ipsupport-llc/dra-driver-dri/internal/state"
	"github.com/ipsupport-llc/dra-driver-dri/internal/vram"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		klog.ErrorS(err, "dra-driver-dri stopped")
		klog.Flush()
		os.Exit(1)
	}
}

func run() error {
	f, err := parseFlags(os.Args[1:], os.Getenv)
	if err != nil {
		return err
	}
	logger := klog.Background().WithValues("driver", f.DriverName, "node", f.NodeName)
	logger.Info("Starting", "version", version)

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancelCause(klog.NewContext(sigCtx, logger))
	defer cancel(nil)

	overrides, err := vram.LoadOverrides(f.Overrides)
	if err != nil {
		return err
	}
	writer, err := cdi.NewWriter(f.CDIRoot, f.DriverName)
	if err != nil {
		return err
	}
	pluginDir := filepath.Join(f.PluginsDir, f.DriverName)
	if err := os.MkdirAll(pluginDir, 0o750); err != nil {
		return fmt.Errorf("create plugin dir: %w", err)
	}
	store, err := state.Open(filepath.Join(pluginDir, "checkpoint.json"))
	if err != nil {
		return err
	}

	m := metrics.New()
	d := newDriver(devices.Config{NodeName: f.NodeName, DriverName: f.DriverName, ReserveBytes: f.ReserveBytes, Shares: f.DeviceShares}, f.SysRoot, vram.NewDetector(overrides), m, cancel)
	d.prep = &prepare.Preparer{DriverName: f.DriverName, Inventory: d.Inventory, CDI: writer, Store: store}
	// GPUs latched earlier in this boot stay tainted whether or not the
	// kernel log is watched now.
	bootID, err := hang.BootID("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	if d.latch, err = hang.OpenLatch(filepath.Join(pluginDir, "hung.json"), bootID); err != nil {
		if d.latch == nil {
			return err
		}
		logger.Error(err, "Ignoring the undecodable hung-GPU latch; the next hang overwrites it")
	}
	if hung := d.latch.Hung(); len(hung) > 0 {
		logger.Info("GPUs hung earlier in this boot", "pciBusIDs", hung)
	}
	if f.KernelLog != "" {
		go watchKernelLog(ctx, d, f.KernelLog, f.SysRoot)
	}
	if err := d.Refresh(ctx); err != nil {
		return err
	}

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("in-cluster config: %w", err)
	}
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}

	opts := []kubeletplugin.Option{
		kubeletplugin.KubeClient(client),
		kubeletplugin.NodeName(f.NodeName),
		kubeletplugin.DriverName(f.DriverName),
		kubeletplugin.RegistrarDirectoryPath(f.RegistrarDir),
		kubeletplugin.PluginDataDirectoryPath(pluginDir),
		kubeletplugin.HealthService(false),
	}
	if f.PodUID != "" {
		opts = append(opts, kubeletplugin.RollingUpdate(types.UID(f.PodUID)))
	}
	helper, err := kubeletplugin.Start(ctx, d, opts...)
	if err != nil {
		return fmt.Errorf("start kubelet plugin: %w", err)
	}
	defer helper.Stop()

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.Registry(), promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := &http.Server{Addr: f.MetricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			cancel(fmt.Errorf("metrics server: %w", err))
		}
	}()
	defer func() {
		shutdownCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := d.Rescan(ctx, helper); err != nil {
		return err
	}
	ticker := time.NewTicker(f.RescanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
				return cause
			}
			logger.Info("Stopping")
			return nil
		case <-ticker.C:
			if err := d.Rescan(ctx, helper); err != nil {
				logger.Error(err, "Rescan failed")
			}
		case <-d.kick:
			if err := d.Rescan(ctx, helper); err != nil {
				logger.Error(err, "Rescan failed")
			}
		}
	}
}

// watchKernelLog follows the kernel log for GPU hangs until ctx ends,
// reopening it after an error. It starts only once amdgpu runs with
// gpu_recovery=0: with recovery on, the kernel resets a hung GPU itself.
// Without access to the kernel log (an unprivileged container cannot open
// /dev/kmsg) the driver still runs and reports dri_kernel_log_watched 0.
func watchKernelLog(ctx context.Context, d *Driver, path, sysRoot string) {
	logger := klog.FromContext(ctx)
	// amdgpu may load after the plugin starts, and gpu_recovery is a
	// writable module parameter: check until it reads 0.
	for {
		off, err := hang.RecoveryDisabled(sysRoot)
		if err == nil && off {
			break
		}
		logger.Info("GPU hang detection waits: amdgpu does not run with gpu_recovery=0", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute):
		}
	}
	d.metrics.SetKernelLogWatched(false)
	for {
		err := followKernelLog(ctx, d, path)
		d.metrics.SetKernelLogWatched(false)
		if ctx.Err() != nil {
			return
		}
		logger.Error(err, "GPU hang detection interrupted, retrying", "path", path)
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

func followKernelLog(ctx context.Context, d *Driver, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = f.Close() })
	defer func() {
		if stop() {
			_ = f.Close()
		}
	}()
	d.metrics.SetKernelLogWatched(true)
	return hang.Watch(f, func(pci string) { d.OnHang(ctx, pci) })
}

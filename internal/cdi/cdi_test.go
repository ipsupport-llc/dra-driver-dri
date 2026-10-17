package cdi_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"sigs.k8s.io/yaml"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"github.com/ipsupport-llc/dra-driver-dri/internal/cdi"
)

func specFiles(t *testing.T, root string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(root, "*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func edit(share string) cdi.Edit {
	return cdi.Edit{Device: "pci-0000-06-00-0", ShareID: share, RenderNode: "renderD128", PCIAddress: "0000:06:00.0", Vendor: "amd", VRAMBytes: 2 << 30}
}

func TestWriteClaim(t *testing.T) {
	root := t.TempDir()
	w, err := cdi.NewWriter(root, "gpu.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteClaim("uid-1", []cdi.Edit{edit("share-a")}); err != nil {
		t.Fatal(err)
	}
	files := specFiles(t, root)
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var spec cdispec.Spec
	if err := yaml.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Kind != "k8s.gpu.example.com/gpu" || len(spec.Devices) != 1 {
		t.Fatalf("spec = %+v", spec)
	}
	d := spec.Devices[0]
	if d.Name != "uid-1-pci-0000-06-00-0-share-a" {
		t.Errorf("name = %q", d.Name)
	}
	if len(d.ContainerEdits.DeviceNodes) != 1 {
		t.Fatalf("device nodes = %+v", d.ContainerEdits.DeviceNodes)
	}
	n := d.ContainerEdits.DeviceNodes[0]
	if n.Path != "/dev/dri/renderD128" || n.Permissions != "rw" || n.FileMode == nil || *n.FileMode != 0o666 {
		t.Errorf("device node = %+v", n)
	}
	for _, want := range []string{"DRA_GPU_PCI_BUS_ID=0000:06:00.0", "DRA_GPU_VENDOR=amd", "DRA_GPU_VRAM_BYTES=2147483648"} {
		if !slices.Contains(d.ContainerEdits.Env, want) {
			t.Errorf("env %v lacks %q", d.ContainerEdits.Env, want)
		}
	}
	if got := w.QualifiedID("uid-1", edit("share-a")); got != "k8s.gpu.example.com/gpu=uid-1-pci-0000-06-00-0-share-a" {
		t.Errorf("QualifiedID = %q", got)
	}
}

func TestUnsharedNameAndDelete(t *testing.T) {
	root := t.TempDir()
	w, err := cdi.NewWriter(root, "gpu.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got := w.QualifiedID("uid-2", edit("")); got != "k8s.gpu.example.com/gpu=uid-2-pci-0000-06-00-0" {
		t.Errorf("QualifiedID = %q", got)
	}
	if err := w.WriteClaim("uid-2", []cdi.Edit{edit("")}); err != nil {
		t.Fatal(err)
	}
	if err := w.DeleteClaim("uid-2"); err != nil {
		t.Fatal(err)
	}
	if files := specFiles(t, root); len(files) != 0 {
		t.Fatalf("files left: %v", files)
	}
	if err := w.DeleteClaim("uid-2"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestNewWriterCreatesRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "run", "cdi")
	if _, err := cdi.NewWriter(root, "gpu.example.com"); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		t.Fatalf("root not created: %v", err)
	}
}

package procvram_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ipsupport-llc/dra-driver-dri/internal/procvram"
)

// writeProc lays out a fake /proc: per PID, an optional cgroup file and a set
// of fdinfo files keyed by fd number.
func writeProc(t *testing.T, root string, pid int, cgroup string, fdinfo map[string]string) {
	t.Helper()
	base := filepath.Join(root, "proc", itoa(pid))
	if cgroup != "" {
		mustWrite(t, filepath.Join(base, "cgroup"), cgroup)
	}
	for fd, body := range fdinfo {
		mustWrite(t, filepath.Join(base, "fdinfo", fd), body)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string { return string(rune('0'+n/1000%10)) + string(rune('0'+n/100%10)) + string(rune('0'+n/10%10)) + string(rune('0'+n%10)) }

const amdgpuFdinfo = `pos:	0
flags:	02000002
drm-driver:	amdgpu
drm-pdev:	0000:06:00.0
drm-client-id:	42
drm-memory-cpu:	0 KiB
drm-memory-gtt:	1024 KiB
drm-memory-vram:	524288 KiB
`

func TestScanReadsAmdgpuVRAMPerProcess(t *testing.T) {
	root := t.TempDir()
	// A GPU process on PCI 0000:06:00.0 using 512 MiB VRAM.
	writeProc(t, root, 1234, "", map[string]string{"3": amdgpuFdinfo})
	// A process with a non-DRM fd — ignored.
	writeProc(t, root, 5678, "", map[string]string{"7": "pos:\t0\nflags:\t0\n"})

	got, err := procvram.Scan(filepath.Join(root, "proc"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d usages, want 1: %+v", len(got), got)
	}
	u := got[0]
	if u.PID != 1234 || u.PCI != "0000:06:00.0" || u.VRAMBytes != 524288*1024 {
		t.Fatalf("got %+v; want PID 1234, PCI 0000:06:00.0, VRAM %d", u, 524288*1024)
	}
}

// systemd cgroup driver (k3s default): the pod UID is in a `*-pod<uid>.slice`
// segment with underscores instead of dashes.
const systemdCgroup = "0::/kubepods.slice/kubepods-besteffort.slice/" +
	"kubepods-besteffort-pod3b2ef00d_1a2b_4c3d_8e9f_0011aabbccdd.slice/" +
	"cri-containerd-deadbeef.scope\n"

func TestScanMapsProcessToPodUID(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, 1234, systemdCgroup, map[string]string{"3": amdgpuFdinfo})

	got, err := procvram.Scan(filepath.Join(root, "proc"))
	if err != nil || len(got) != 1 {
		t.Fatalf("Scan: %v, %+v", err, got)
	}
	if want := "3b2ef00d-1a2b-4c3d-8e9f-0011aabbccdd"; got[0].PodUID != want {
		t.Fatalf("PodUID = %q; want %q", got[0].PodUID, want)
	}
}

// cgroupfs driver: `pod<uid>` with dashes already.
func TestScanPodUIDCgroupfs(t *testing.T) {
	root := t.TempDir()
	cg := "0::/kubepods/besteffort/pod3b2ef00d-1a2b-4c3d-8e9f-0011aabbccdd/deadbeef\n"
	writeProc(t, root, 1234, cg, map[string]string{"3": amdgpuFdinfo})

	got, err := procvram.Scan(filepath.Join(root, "proc"))
	if err != nil || len(got) != 1 {
		t.Fatalf("Scan: %v, %+v", err, got)
	}
	if want := "3b2ef00d-1a2b-4c3d-8e9f-0011aabbccdd"; got[0].PodUID != want {
		t.Fatalf("PodUID = %q; want %q", got[0].PodUID, want)
	}
}

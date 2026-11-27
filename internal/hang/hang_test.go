package hang_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ipsupport-llc/dra-driver-dri/internal/hang"
)

func TestMatch(t *testing.T) {
	for _, tc := range []struct {
		record, pci string
	}{
		// Real lines from a FirePro D700 that hung and never recovered.
		{"3,123456,85032000000,-;amdgpu 0000:02:00.0: ring gfx timeout, signaled seq=795476, emitted seq=795478", "0000:02:00.0"},
		{"3,123460,85277000000,-;amdgpu 0000:02:00.0: ring sdma1 timeout, signaled seq=5902, emitted seq=5904", "0000:02:00.0"},
		{"3,1,2,-;amdgpu 0000:83:00.1: ring gfx_0.0.0 timeout, signaled seq=1, emitted seq=3", "0000:83:00.1"},
		// Newer kernels repeat the driver name after the device prefix.
		{"3,1,2,-;amdgpu 0000:83:00.0: amdgpu: ring gfx_0.0.0 timeout, signaled seq=1, emitted seq=3", "0000:83:00.0"},
		// Written to /dev/kmsg by a process (facility user, 1*8+3): the
		// kernel never lets userspace log as facility kern.
		{"11,5,6,-;amdgpu 0000:02:00.0: ring gfx timeout, signaled seq=1, emitted seq=3", ""},
		// Printed at every boot with amdgpu.gpu_recovery=0, and after a hang:
		// not a hang by itself.
		{"6,99,1000,-;amdgpu 0000:02:00.0: GPU recovery disabled.", ""},
		{"4,7,8,-;amdgpu 0000:02:00.0:  Process llama-server pid 958660 thread llama-server pid 958660", ""},
		{"3,9,9,-;INFO: task kworker/u97:0:1060269 blocked for more than 122 seconds.", ""},
		// Dictionary continuation line of a record.
		{" DEVICE=+pci:0000:02:00.0", ""},
		{"", ""},
	} {
		pci, ok := hang.Match(tc.record)
		if pci != tc.pci || ok != (tc.pci != "") {
			t.Errorf("Match(%q) = %q, %v; want %q", tc.record, pci, ok, tc.pci)
		}
	}
}

// chunks returns one record per Read, like /dev/kmsg, with an error in the
// middle of the stream.
type chunks struct {
	reads []string
	errs  []error
}

func (c *chunks) Read(p []byte) (int, error) {
	if len(c.reads) == 0 {
		return 0, os.ErrClosed
	}
	r, err := c.reads[0], c.errs[0]
	c.reads, c.errs = c.reads[1:], c.errs[1:]
	return copy(p, r), err
}

func TestWatchReportsHungGPUsAndSkipsOverwrittenRecords(t *testing.T) {
	src := &chunks{
		reads: []string{
			"6,1,1,-;amdgpu 0000:02:00.0: GPU recovery disabled.\n",
			"3,2,2,-;amdgpu 0000:02:00.0: ring gfx timeout, signaled seq=1, emitted seq=3\n SUBSYSTEM=pci\n DEVICE=+pci:0000:02:00.0\n",
			"", // records were overwritten before they were read
			"3,9,9,-;amdgpu 0000:06:00.0: ring sdma0 timeout, signaled seq=1, emitted seq=3\n",
		},
		errs: []error{nil, nil, syscall.EPIPE, nil},
	}
	var got []string
	err := hang.Watch(src, func(pci string) { got = append(got, pci) })
	if err == nil || !strings.Contains(err.Error(), os.ErrClosed.Error()) {
		t.Fatalf("Watch error = %v, want the reader's closing error", err)
	}
	if strings.Join(got, ",") != "0000:02:00.0,0000:06:00.0" {
		t.Errorf("hung = %v", got)
	}
}

func TestLatchPersistsWithinOneBoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hung.json")
	l, err := hang.OpenLatch(path, "boot-1")
	if err != nil {
		t.Fatal(err)
	}
	if added, err := l.Add("0000:02:00.0"); err != nil || !added {
		t.Fatalf("Add = %v, %v", added, err)
	}
	if added, err := l.Add("0000:02:00.0"); err != nil || added {
		t.Fatalf("second Add = %v, %v; want not added", added, err)
	}

	same, err := hang.OpenLatch(path, "boot-1")
	if err != nil {
		t.Fatal(err)
	}
	if !same.Has("0000:02:00.0") || same.Has("0000:06:00.0") {
		t.Errorf("after restart, same boot: hung = %v", same.Hung())
	}

	next, err := hang.OpenLatch(path, "boot-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Hung()) != 0 {
		t.Errorf("after reboot: hung = %v, want none", next.Hung())
	}
}

func TestLatchOverwritesACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hung.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := hang.OpenLatch(path, "boot-1")
	if err == nil || l == nil {
		t.Fatalf("OpenLatch = %v, %v; want an empty latch and the decode error", l, err)
	}
	if _, err := l.Add("0000:02:00.0"); err != nil {
		t.Fatal(err)
	}
	again, err := hang.OpenLatch(path, "boot-1")
	if err != nil || !again.Has("0000:02:00.0") {
		t.Fatalf("after overwrite: %v, %v", again.Hung(), err)
	}
}

func TestTwoInstancesKeepEachOthersHangs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hung.json")
	a, err := hang.OpenLatch(path, "boot-1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := hang.OpenLatch(path, "boot-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Add("0000:02:00.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Add("0000:06:00.0"); err != nil {
		t.Fatal(err)
	}
	c, err := hang.OpenLatch(path, "boot-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.Hung(), ","); got != "0000:02:00.0,0000:06:00.0" {
		t.Fatalf("file after both instances = %s", got)
	}
	if err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	if !a.Has("0000:06:00.0") {
		t.Fatal("Reload did not pick up the other instance's hang")
	}
}

func TestLatchRetriesAFailedSave(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "hung.json")
	l, err := hang.OpenLatch(path, "boot-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Add("0000:02:00.0"); err == nil {
		t.Fatal("Add saved into a read-only directory")
	}
	if !l.Has("0000:02:00.0") {
		t.Fatal("a failed save must still latch the GPU in memory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Add("0000:02:00.0"); err != nil {
		t.Fatal(err)
	}
	again, err := hang.OpenLatch(path, "boot-1")
	if err != nil || !again.Has("0000:02:00.0") {
		t.Fatalf("the retry did not save: %v, %v", again.Hung(), err)
	}
}

func TestRecoveryDisabled(t *testing.T) {
	root := t.TempDir()
	if off, err := hang.RecoveryDisabled(root); err == nil || off {
		t.Fatalf("without amdgpu: %v, %v; want false and an error", off, err)
	}
	dir := filepath.Join(root, "module", "amdgpu", "parameters")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for value, want := range map[string]bool{"0\n": true, "-1\n": false, "1\n": false} {
		if err := os.WriteFile(filepath.Join(dir, "gpu_recovery"), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		if off, err := hang.RecoveryDisabled(root); err != nil || off != want {
			t.Errorf("gpu_recovery=%q: %v, %v; want %v", value, off, err, want)
		}
	}
}

func TestBootID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boot_id")
	if err := os.WriteFile(path, []byte("6f1c0a8e-0b7e-4c3a-9d55-2a1f2b3c4d5e\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := hang.BootID(path)
	if err != nil || id != "6f1c0a8e-0b7e-4c3a-9d55-2a1f2b3c4d5e" {
		t.Errorf("BootID = %q, %v", id, err)
	}
}

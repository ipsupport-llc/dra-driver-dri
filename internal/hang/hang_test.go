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

func TestLatchRejectsACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hung.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := hang.OpenLatch(path, "boot-1"); err == nil {
		t.Fatal("OpenLatch accepted a corrupt file")
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

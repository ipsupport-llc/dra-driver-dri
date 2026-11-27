// Package hang finds GPUs that the kernel reports as hung and remembers
// them until the node reboots.
//
// The signal is the kernel log: amdgpu prints "ring <name> timeout" when a
// job does not finish. With amdgpu.gpu_recovery=0 (common on older GPUs,
// whose reset can take the host down) the GPU stays hung until a reboot,
// and every process using it is stuck in the kernel. A hung GPU is
// therefore latched for the rest of the boot, across plugin restarts.
package hang

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
)

// ringTimeout matches the message part of a kernel log record.
var ringTimeout = regexp.MustCompile(`^amdgpu ([0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]): ring \S+ timeout`)

// Match returns the PCI address of the GPU that a /dev/kmsg record
// ("prio,seq,usec,flags;message") reports as hung.
func Match(record string) (string, bool) {
	_, msg, ok := strings.Cut(record, ";")
	if !ok || strings.HasPrefix(record, " ") {
		return "", false
	}
	m := ringTimeout.FindStringSubmatch(msg)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Watch reads kernel log records from r (an open /dev/kmsg, which returns
// one record per read, oldest first) and calls onHang for every hang
// record. It returns the first read error other than EPIPE, which only
// means that records were overwritten before they were read.
func Watch(r io.Reader, onHang func(pci string)) error {
	buf := make([]byte, 16<<10)
	for {
		n, err := r.Read(buf)
		for _, line := range strings.Split(string(buf[:n]), "\n") {
			if pci, ok := Match(line); ok {
				onHang(pci)
			}
		}
		if errors.Is(err, syscall.EPIPE) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read kernel log: %w", err)
		}
	}
}

// BootID reads the kernel's boot ID (/proc/sys/kernel/random/boot_id).
func BootID(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read boot id: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

type latchFile struct {
	BootID string   `json:"bootID"`
	Hung   []string `json:"hung"`
}

// Latch is the set of hung GPUs of the current boot, kept in a file so a
// restarted plugin still knows them after the kernel log has moved on. It
// is safe for concurrent use.
type Latch struct {
	mu     sync.Mutex
	path   string
	bootID string
	hung   map[string]bool
}

// OpenLatch loads the latch at path. A missing file, or one written during
// another boot, is an empty latch.
func OpenLatch(path, bootID string) (*Latch, error) {
	l := &Latch{path: path, bootID: bootID, hung: map[string]bool{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read hang latch: %w", err)
	}
	var f latchFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("decode hang latch %s: %w", path, err)
	}
	if f.BootID == bootID {
		for _, pci := range f.Hung {
			l.hung[pci] = true
		}
	}
	return l, nil
}

// Add latches a GPU and saves the latch. It reports whether the GPU was
// new.
func (l *Latch) Add(pci string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hung[pci] {
		return false, nil
	}
	l.hung[pci] = true
	f := latchFile{BootID: l.bootID, Hung: slices.Sorted(maps.Keys(l.hung))}
	b, err := json.Marshal(f)
	if err != nil {
		return true, err
	}
	// Two plugin instances overlap during a rolling update; each writes its
	// own temporary file and renames it into place.
	tmp, err := os.CreateTemp(filepath.Dir(l.path), filepath.Base(l.path)+".*")
	if err != nil {
		return true, fmt.Errorf("write hang latch: %w", err)
	}
	_, err = tmp.Write(b)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), l.path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return true, fmt.Errorf("write hang latch: %w", err)
	}
	_ = syncDir(filepath.Dir(l.path))
	return true, nil
}

// Has reports whether a GPU is latched.
func (l *Latch) Has(pci string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hung[pci]
}

// Hung returns the latched GPUs.
func (l *Latch) Hung() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Sorted(maps.Keys(l.hung))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

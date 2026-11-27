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
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// ringTimeout matches the message part of a kernel log record. Newer
// kernels repeat the driver name ("amdgpu 0000:02:00.0: amdgpu: ring ...").
var ringTimeout = regexp.MustCompile(`^amdgpu ([0-9a-f]{4,}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]): (?:amdgpu: )?ring \S+ timeout`)

// Match returns the PCI address of the GPU that a /dev/kmsg record
// ("prio,seq,usec,flags;message") reports as hung. Only kernel records
// count: prio is facility*8+level, and the kernel rewrites facility 0
// (kern) to 1 (user) for anything a process writes to /dev/kmsg, so a
// process cannot fake a hang.
func Match(record string) (string, bool) {
	header, msg, ok := strings.Cut(record, ";")
	if !ok || strings.HasPrefix(record, " ") {
		return "", false
	}
	prio, _, _ := strings.Cut(header, ",")
	if p, err := strconv.Atoi(prio); err != nil || p>>3 != 0 {
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

// RecoveryDisabled reports whether amdgpu runs with gpu_recovery=0, read
// from sysfs. Only then does a GPU stay hung after a ring timeout; with
// recovery on, the kernel resets the GPU and it returns to service.
func RecoveryDisabled(sysRoot string) (bool, error) {
	b, err := os.ReadFile(filepath.Join(sysRoot, "module", "amdgpu", "parameters", "gpu_recovery"))
	if err != nil {
		return false, fmt.Errorf("read amdgpu gpu_recovery: %w", err)
	}
	return strings.TrimSpace(string(b)) == "0", nil
}

type latchFile struct {
	BootID string   `json:"bootID"`
	Hung   []string `json:"hung"`
}

// Latch is the set of hung GPUs of the current boot, kept in a file so a
// restarted plugin still knows them after the kernel log has moved on.
// Two plugin instances overlap during a rolling update and share the file:
// every save merges what is on disk under an flock, so neither drops the
// other's hangs. It is safe for concurrent use.
type Latch struct {
	path   string
	bootID string

	mu    sync.Mutex
	hung  map[string]bool
	dirty bool // a GPU is latched in memory but not saved yet
}

// OpenLatch loads the latch at path. A missing file, or one written during
// another boot, is an empty latch. A file that cannot be decoded also
// gives an empty latch, with the error, and is overwritten by the next
// save; only a failed read returns no latch.
func OpenLatch(path, bootID string) (*Latch, error) {
	l := &Latch{path: path, bootID: bootID, hung: map[string]bool{}}
	saved, err := l.load()
	if saved == nil {
		return nil, err
	}
	l.merge(saved)
	return l, err
}

// load returns the GPUs saved during this boot. A decode error returns an
// empty set with the error.
func (l *Latch) load() ([]string, error) {
	b, err := os.ReadFile(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read hang latch: %w", err)
	}
	var f latchFile
	if err := json.Unmarshal(b, &f); err != nil {
		return []string{}, fmt.Errorf("decode hang latch %s: %w", l.path, err)
	}
	if f.BootID != l.bootID || f.Hung == nil {
		return []string{}, nil
	}
	return f.Hung, nil
}

func (l *Latch) merge(pcis []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, pci := range pcis {
		l.hung[pci] = true
	}
}

// Reload adds the GPUs another plugin instance saved, and saves again
// after a failed save. It never forgets a GPU.
func (l *Latch) Reload() error {
	saved, err := l.load()
	if saved == nil {
		return err
	}
	l.merge(saved)
	l.mu.Lock()
	dirty := l.dirty
	l.mu.Unlock()
	if dirty {
		return l.save()
	}
	return nil
}

// Add latches a GPU and saves the latch. It reports whether the GPU was
// new. A GPU latched by an Add whose save failed is saved again by the
// next Add, even of the same GPU, or by the next Reload.
func (l *Latch) Add(pci string) (bool, error) {
	l.mu.Lock()
	added := !l.hung[pci]
	l.hung[pci] = true
	if added {
		l.dirty = true
	}
	dirty := l.dirty
	l.mu.Unlock()
	if !dirty {
		return false, nil
	}
	return added, l.save()
}

// save writes the union of the file and memory, under an flock.
func (l *Latch) save() error {
	lock, err := os.OpenFile(l.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("lock hang latch: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock hang latch: %w", err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	// An undecodable file is replaced by memory, which already holds
	// everything this instance knows; an unreadable one is left alone, as
	// it may hold another instance's hangs.
	saved, err := l.load()
	if saved == nil {
		return err
	}
	l.merge(saved)
	l.mu.Lock()
	f := latchFile{BootID: l.bootID, Hung: slices.Sorted(maps.Keys(l.hung))}
	l.mu.Unlock()
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(l.path), filepath.Base(l.path)+".*")
	if err != nil {
		return fmt.Errorf("write hang latch: %w", err)
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
		return fmt.Errorf("write hang latch: %w", err)
	}
	_ = syncDir(filepath.Dir(l.path))
	l.mu.Lock()
	// Only what this save wrote is clean; a GPU added meanwhile keeps
	// dirty set through its own Add.
	if len(l.hung) == len(f.Hung) {
		l.dirty = false
	}
	l.mu.Unlock()
	return nil
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
	defer func() { _ = d.Close() }()
	return d.Sync()
}

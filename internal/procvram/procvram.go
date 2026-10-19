// Package procvram attributes amdgpu VRAM to the processes using each GPU,
// read from DRM fdinfo under /proc. DRA accounts for claims but the kernel does
// not cap a process's VRAM, so comparing a claim's actual use to what it
// claimed is the only way to catch a workload that sized for weights only.
package procvram

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// A Kubernetes pod UID in a cgroup path: `pod<uid>` with dashes (cgroupfs) or
// underscores (systemd).
var podUIDRe = regexp.MustCompile(`pod([0-9a-fA-F]{8}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{12})`)

// Usage is one process's VRAM on one GPU.
type Usage struct {
	PID       int
	PCI       string // GPU PCI address (from drm-pdev)
	PodUID    string // Kubernetes pod UID (from the cgroup); empty if unknown
	VRAMBytes int64
}

// Scan reads every process's DRM fdinfo under procRoot and returns its amdgpu
// VRAM use per GPU. A process with no GPU fd is omitted. Unreadable processes
// (gone, or not permitted) are skipped rather than failing the scan.
func Scan(procRoot string) ([]Usage, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}

	type key struct {
		pid int
		pci string
	}
	sum := map[key]int64{}
	var order []key

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a PID directory
		}
		fdinfoDir := filepath.Join(procRoot, e.Name(), "fdinfo")
		fds, err := os.ReadDir(fdinfoDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			pci, vram, ok := parseFdinfo(filepath.Join(fdinfoDir, fd.Name()))
			if !ok {
				continue
			}
			k := key{pid, pci}
			if _, seen := sum[k]; !seen {
				order = append(order, k)
			}
			sum[k] += vram
		}
	}

	uidCache := map[int]string{}
	out := make([]Usage, 0, len(order))
	for _, k := range order {
		uid, ok := uidCache[k.pid]
		if !ok {
			uid = podUID(procRoot, k.pid)
			uidCache[k.pid] = uid
		}
		out = append(out, Usage{PID: k.pid, PCI: k.pci, PodUID: uid, VRAMBytes: sum[k]})
	}
	return out, nil
}

// podUID reads a process's cgroup and returns the Kubernetes pod UID, with
// systemd's underscores normalised to dashes. Empty when the process is not in
// a pod cgroup (or the cgroup is unreadable).
func podUID(procRoot string, pid int) string {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return ""
	}
	m := podUIDRe.FindSubmatch(b)
	if m == nil {
		return ""
	}
	return strings.ReplaceAll(string(m[1]), "_", "-")
}

// parseFdinfo returns the GPU PCI address and VRAM bytes of one DRM fd. ok is
// false for a non-DRM fd or one without a VRAM line.
func parseFdinfo(path string) (pci string, vram int64, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, false
	}
	defer func() { _ = f.Close() }()

	var isDRM, hasVRAM bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		field, val, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue
		}
		switch strings.TrimSpace(field) {
		case "drm-driver":
			isDRM = true
		case "drm-pdev":
			pci = strings.TrimSpace(val)
		case "drm-memory-vram":
			if b, parsed := parseMemory(strings.TrimSpace(val)); parsed {
				vram = b
				hasVRAM = true
			}
		}
	}
	return pci, vram, isDRM && hasVRAM && pci != ""
}

// parseMemory parses a DRM fdinfo memory value like "524288 KiB".
func parseMemory(v string) (int64, bool) {
	fields := strings.Fields(v)
	if len(fields) == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, false
	}
	unit := "KiB"
	if len(fields) > 1 {
		unit = fields[1]
	}
	switch unit {
	case "B":
		return n, true
	case "KiB":
		return n << 10, true
	case "MiB":
		return n << 20, true
	case "GiB":
		return n << 30, true
	}
	return 0, false
}

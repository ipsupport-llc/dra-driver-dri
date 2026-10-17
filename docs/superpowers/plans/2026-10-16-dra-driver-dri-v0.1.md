# dra-driver-dri v0.1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship v0.1.0 of a DRA kubelet plugin that publishes every DRM render-node GPU with a consumable `vram` capacity, hands each claim exactly one render node through CDI, and run it on the operator's Mac Pro (2× FirePro D700) nodes.

**Architecture:** One Go binary (kubelet plugin, DaemonSet). Packages: `sysfs` (find render nodes), `vram` (per-kernel-driver VRAM readers + overrides), `devices` (DRA device model), `cdi` (per-claim specs), `state` (JSON checkpoint), `prepare` (claim → CDI), `metrics`; `cmd/dra-driver-dri` wires them to `k8s.io/dynamic-resource-allocation/kubeletplugin`. No controller, no CRD. Deployment in the operator's cluster goes through the platform engine (Terraform) and Ansible.

**Tech Stack:** Go 1.26, `k8s.io/{api,apimachinery,client-go,dynamic-resource-allocation}` v0.37.1, `tags.cncf.io/container-device-interface` v1.1.1, `github.com/prometheus/client_golang` v1.24.0, `sigs.k8s.io/yaml` v1.6.0; GitHub Actions (GitHub-hosted runners); image `ghcr.io/ipsupport-llc/dra-driver-dri`.

**Spec:** `docs/design.md` (this repository, PR #1).

## Global Constraints

- English only in this repository (code, comments, docs, commits, PRs).
- Public repository and public image: no operator hostnames, IPs, node names or domains in this repo; examples use `gpu.example.com` as the driver name.
- Kubernetes ≥ 1.36 (`DRAConsumableCapacity` beta, on by default); target cluster runs k3s v1.37.0, containerd 2.3.4 (CDI on by default, spec dirs `/etc/cdi`, `/var/run/cdi`).
- Capacity name: `vram`. Device name: `pci-` + PCI address with `:` and `.` replaced by `-` (e.g. `pci-0000-06-00-0`).
- `--vram-reserve` default `256Mi`; request granularity (min and step) `64Mi`; published VRAM = total − reserve, rounded **down** to 64 MiB.
- A claim that names no `vram` amount gets the whole published VRAM of the device.
- CDI edits per device: device node `/dev/dri/renderDNNN`, permissions `rw`, file mode `0666`; env `DRA_GPU_PCI_BUS_ID`, `DRA_GPU_VENDOR`, `DRA_GPU_VRAM_BYTES`.
- No VRAM enforcement (out of scope); over-use is reported by metrics.
- Commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`; PR bodies end with `🤖 Generated with [Claude Code](https://claude.com/claude-code)`.
- Changes to a live cluster go through that cluster's infrastructure-as-code; no permanent objects applied by hand.

## Review Focus

1. **Node reboot:** `/var/run/cdi` is tmpfs and is wiped, the checkpoint (under `/var/lib/kubelet/plugins`) survives, and render-node numbers may swap (`renderD128` ↔ `renderD129`). Re-preparing a checkpointed claim must rewrite its CDI spec with the *current* render node of the same PCI device → test in Task 7 (`TestPrepareRestoresAfterReboot`).
2. **GPU gone between allocation and prepare** (reset, driver unbind): prepare must fail with a clear error and leave no CDI file and no checkpoint entry → Task 7 (`TestPrepareUnknownDevice`).
3. **Two shares of one GPU** (two claims on the same device, consumable capacity): CDI device names and IDs must differ per `ShareID`, and unpreparing one must not touch the other → Task 7 (`TestPrepareTwoSharesOfOneDevice`).
4. **Corrupt or foreign checkpoint file:** the plugin must refuse to start rather than silently forget prepared claims → Task 6 (`TestOpenCorrupt`, `TestOpenWrongVersion`).
5. **GPU with less VRAM than the reserve, or unreadable VRAM:** not published, reason logged, `dri_gpus_skipped` set → Task 4 (`TestBuildSkips`) and Task 9 (`TestRescanCountsSkipped`).

---

## File Structure

```
go.mod, go.sum                        module github.com/ipsupport-llc/dra-driver-dri
Makefile                              test / lint / build / image
Dockerfile                            golang:1.26.9 build → distroless static
.golangci.yml                         golangci-lint v2 config
.github/workflows/ci.yml              vet, test -race, lint, image build, kubeconform
.github/workflows/release.yml         tag v* → push ghcr image
cmd/dra-driver-dri/main.go            process wiring, signals, metrics server, rescan loop
cmd/dra-driver-dri/flags.go           flags with env defaults + validation
cmd/dra-driver-dri/flags_test.go
cmd/dra-driver-dri/driver.go          kubeletplugin.DRAPlugin impl, Rescan/Refresh/Inventory
cmd/dra-driver-dri/driver_test.go
internal/sysfstest/sysfstest.go       fake sysfs tree builder (test helper, importable)
internal/sysfs/scan.go                find render-node PCI GPUs
internal/sysfs/scan_test.go
internal/vram/vram.go                 Backend iface, AMDGPU backend, Overrides, Detector
internal/vram/vram_test.go
internal/devices/devices.go           Build (publish/skip), Resources (ResourceSlice content)
internal/devices/devices_test.go
internal/cdi/cdi.go                   per-claim CDI spec writer
internal/cdi/cdi_test.go
internal/state/state.go               JSON checkpoint store
internal/state/state_test.go
internal/prepare/prepare.go           claim → CDI + checkpoint, restore, unprepare
internal/prepare/prepare_test.go
internal/metrics/metrics.go           Prometheus gauges/counters
internal/metrics/metrics_test.go
deploy/*.yaml                         reference manifests (namespace, RBAC, DaemonSet, DeviceClass)
deploy/examples/*.yaml                claim template, test pod, alert rules
docs/design.md                        spec (updated where this plan refines it)
docs/e2e.md                           hardware test results (Phase B)
```


---

## Phase A — the driver repository

### Task 1: Scaffold, toolchain and CI

**Files:**
- Create: `go.mod`, `go.sum`, `Makefile`, `Dockerfile`, `.golangci.yml`, `.github/workflows/ci.yml`, `cmd/dra-driver-dri/main.go` (stub)
- Modify: `.gitignore` (create)

**Interfaces:**
- Produces: module path `github.com/ipsupport-llc/dra-driver-dri`; `main.version` string set by `-ldflags`.

- [ ] **Step 1: Branch and module**

```bash
cd dra-driver-dri
git checkout main && git pull --ff-only
git checkout -b v0.1
go mod init github.com/ipsupport-llc/dra-driver-dri
go mod edit -go=1.26.0
```

- [ ] **Step 2: Stub binary**

`cmd/dra-driver-dri/main.go`:

```go
// Command dra-driver-dri is a Kubernetes DRA kubelet plugin for GPUs that
// expose a DRM render node.
package main

import "fmt"

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	fmt.Println("dra-driver-dri", version)
}
```

`.gitignore`:

```
/bin/
*.test
*.out
```

- [ ] **Step 3: Makefile, Dockerfile, lint config**

`Makefile`:

```make
VERSION ?= $(shell git describe --tags --always --dirty)
IMAGE   ?= ghcr.io/ipsupport-llc/dra-driver-dri

.PHONY: test lint build image
test:
	go vet ./...
	go test -race ./...
lint:
	golangci-lint run ./...
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/dra-driver-dri ./cmd/dra-driver-dri
image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .
```

`Dockerfile` (the plugin runs as root: it writes the kubelet plugin socket and CDI specs into root-owned host directories):

```dockerfile
FROM golang:1.26.9 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/dra-driver-dri ./cmd/dra-driver-dri

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/dra-driver-dri /dra-driver-dri
ENTRYPOINT ["/dra-driver-dri"]
```

`.golangci.yml`:

```yaml
version: "2"
linters:
  default: standard
  enable:
    - misspell
```

- [ ] **Step 4: CI workflow**

`.github/workflows/ci.yml`:

```yaml
name: ci
on:
  pull_request:
  push:
    branches: [main]
permissions:
  contents: read
jobs:
  test:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v7
      - uses: actions/setup-go@v7
        with:
          go-version-file: go.mod
      - run: go vet ./...
      - run: go test -race ./...
  lint:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v7
      - uses: actions/setup-go@v7
        with:
          go-version-file: go.mod
      - uses: golangci/golangci-lint-action@v9
        with:
          version: v2.15.0
  image:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v7
      - uses: docker/setup-buildx-action@v4
      - uses: docker/build-push-action@v7
        with:
          context: .
          push: false
          build-args: VERSION=ci
```

- [ ] **Step 5: Verify locally**

Run: `go mod tidy && make test && make build && ./bin/dra-driver-dri`
Expected: tests pass (no test files yet → `ok`/`no test files`), binary prints `dra-driver-dri <git describe>`.

- [ ] **Step 6: Commit and open the PR**

```bash
git add -A
git commit -m "Scaffold module, Makefile, Dockerfile, CI

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
git push -u origin v0.1
gh pr create --draft --title "v0.1: DRA driver for DRM render-node GPUs" --body "Implements docs/design.md per docs/superpowers/plans/2026-10-16-dra-driver-dri-v0.1.md.

🤖 Generated with [Claude Code](https://claude.com/claude-code)"
```

Expected: PR checks `test`, `lint`, `image` green.

---

### Task 2: Render-node discovery (`sysfs`)

**Files:**
- Create: `internal/sysfstest/sysfstest.go`, `internal/sysfs/scan.go`, `internal/sysfs/scan_test.go`

**Interfaces:**
- Produces:
  - `sysfs.GPU{RenderNode, PCIAddress, VendorID, DeviceID, KernelDriver, DevicePath string}`
  - `func sysfs.Scan(root string) ([]sysfs.GPU, error)` — sorted by `PCIAddress`
  - `sysfstest.FakeGPU{RenderNode, PCIAddress, VendorID, DeviceID, Driver string; Files map[string]string}`
  - `func sysfstest.AddGPU(t testing.TB, root string, g sysfstest.FakeGPU) string` — returns the PCI device dir

- [ ] **Step 1: Test helper (fake sysfs)**

`internal/sysfstest/sysfstest.go`:

```go
// Package sysfstest builds fake sysfs trees for tests.
package sysfstest

import (
	"os"
	"path/filepath"
	"testing"
)

// FakeGPU describes one render node and its PCI device.
type FakeGPU struct {
	RenderNode string            // e.g. "renderD128"
	PCIAddress string            // e.g. "0000:06:00.0"; may be any name for non-PCI nodes
	VendorID   string            // e.g. "0x1002"; empty = no vendor file
	DeviceID   string            // e.g. "0x6798"
	Driver     string            // bound kernel driver; empty = unbound
	Files      map[string]string // extra files in the device dir
}

// AddGPU creates the device directory, its driver link and the render node
// link under root, laid out like /sys. It returns the device directory.
func AddGPU(t testing.TB, root string, g FakeGPU) string {
	t.Helper()
	dev := filepath.Join(root, "devices", "pci0000:00", g.PCIAddress)
	mkdir(t, dev)
	if g.VendorID != "" {
		write(t, filepath.Join(dev, "vendor"), g.VendorID+"\n")
	}
	write(t, filepath.Join(dev, "device"), g.DeviceID+"\n")
	for name, content := range g.Files {
		write(t, filepath.Join(dev, name), content)
	}
	if g.Driver != "" {
		drv := filepath.Join(root, "bus", "pci", "drivers", g.Driver)
		mkdir(t, drv)
		symlink(t, drv, filepath.Join(dev, "driver"))
	}
	node := filepath.Join(root, "class", "drm", g.RenderNode)
	mkdir(t, node)
	symlink(t, dev, filepath.Join(node, "device"))
	return dev
}

// SetFile replaces one file in a device directory created by AddGPU.
func SetFile(t testing.TB, devDir, name, content string) {
	t.Helper()
	write(t, filepath.Join(devDir, name), content)
}

func mkdir(t testing.TB, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t testing.TB, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlink(t testing.TB, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Write the failing tests**

`internal/sysfs/scan_test.go`:

```go
package sysfs_test

import (
	"path/filepath"
	"testing"

	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfs"
	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfstest"
)

func resolved(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestScanTwoGPUsSorted(t *testing.T) {
	root := t.TempDir()
	devB := sysfstest.AddGPU(t, root, sysfstest.FakeGPU{RenderNode: "renderD128", PCIAddress: "0000:06:00.0", VendorID: "0x1002", DeviceID: "0x6798", Driver: "amdgpu"})
	devA := sysfstest.AddGPU(t, root, sysfstest.FakeGPU{RenderNode: "renderD129", PCIAddress: "0000:02:00.0", VendorID: "0x1002", DeviceID: "0x6798", Driver: "amdgpu"})

	got, err := sysfs.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []sysfs.GPU{
		{RenderNode: "renderD129", PCIAddress: "0000:02:00.0", VendorID: "0x1002", DeviceID: "0x6798", KernelDriver: "amdgpu", DevicePath: resolved(t, devA)},
		{RenderNode: "renderD128", PCIAddress: "0000:06:00.0", VendorID: "0x1002", DeviceID: "0x6798", KernelDriver: "amdgpu", DevicePath: resolved(t, devB)},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d GPUs, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("GPU %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestScanSkipsUnusableNodes(t *testing.T) {
	root := t.TempDir()
	sysfstest.AddGPU(t, root, sysfstest.FakeGPU{RenderNode: "renderD128", PCIAddress: "0000:06:00.0", VendorID: "0x1002", DeviceID: "0x6798"})              // unbound
	sysfstest.AddGPU(t, root, sysfstest.FakeGPU{RenderNode: "renderD129", PCIAddress: "vgem", VendorID: "0x0000", DeviceID: "0x0000", Driver: "vgem"})       // not PCI
	sysfstest.AddGPU(t, root, sysfstest.FakeGPU{RenderNode: "renderD130", PCIAddress: "0000:07:00.0", DeviceID: "0x6798", Driver: "amdgpu"})                   // no vendor file
	ok := sysfstest.AddGPU(t, root, sysfstest.FakeGPU{RenderNode: "renderD131", PCIAddress: "0000:08:00.0", VendorID: "0x1002", DeviceID: "0x6798", Driver: "amdgpu"})

	got, err := sysfs.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].DevicePath != resolved(t, ok) {
		t.Fatalf("got %+v, want only 0000:08:00.0", got)
	}
}

func TestScanEmptyRoot(t *testing.T) {
	got, err := sysfs.Scan(t.TempDir())
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want no GPUs and no error", got, err)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/sysfs/`
Expected: FAIL — `undefined: sysfs.Scan`.

- [ ] **Step 4: Implement**

`internal/sysfs/scan.go`:

```go
// Package sysfs finds GPUs that expose a DRM render node.
package sysfs

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// GPU is one DRM render node backed by a PCI device.
type GPU struct {
	RenderNode   string // e.g. "renderD128"
	PCIAddress   string // e.g. "0000:06:00.0"
	VendorID     string // e.g. "0x1002"
	DeviceID     string // e.g. "0x6798"
	KernelDriver string // e.g. "amdgpu"
	DevicePath   string // resolved sysfs directory of the PCI device
}

var pciAddress = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)

// Scan returns the render-node GPUs under root (normally "/sys"), sorted by
// PCI address. Render nodes that are not PCI devices, have no bound kernel
// driver or lack vendor/device IDs are skipped.
func Scan(root string) ([]GPU, error) {
	nodes, err := filepath.Glob(filepath.Join(root, "class", "drm", "renderD*"))
	if err != nil {
		return nil, fmt.Errorf("list render nodes: %w", err)
	}
	var gpus []GPU
	for _, node := range nodes {
		devPath, err := filepath.EvalSymlinks(filepath.Join(node, "device"))
		if err != nil {
			continue
		}
		addr := filepath.Base(devPath)
		if !pciAddress.MatchString(addr) {
			continue
		}
		driverPath, err := filepath.EvalSymlinks(filepath.Join(devPath, "driver"))
		if err != nil {
			continue
		}
		vendor, err := readTrimmed(filepath.Join(devPath, "vendor"))
		if err != nil {
			continue
		}
		device, err := readTrimmed(filepath.Join(devPath, "device"))
		if err != nil {
			continue
		}
		gpus = append(gpus, GPU{
			RenderNode:   filepath.Base(node),
			PCIAddress:   addr,
			VendorID:     vendor,
			DeviceID:     device,
			KernelDriver: filepath.Base(driverPath),
			DevicePath:   devPath,
		})
	}
	sort.Slice(gpus, func(i, j int) bool { return gpus[i].PCIAddress < gpus[j].PCIAddress })
	return gpus, nil
}

func readTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
```

- [ ] **Step 5: Run tests**

Run: `go test ./internal/sysfs/ -v`
Expected: PASS (3 tests).

- [ ] **Step 6: Commit**

```bash
git add internal/sysfs internal/sysfstest
git commit -m "sysfs: find PCI GPUs that expose a DRM render node

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: VRAM detection (`vram`)

**Files:**
- Create: `internal/vram/vram.go`, `internal/vram/vram_test.go`

**Interfaces:**
- Consumes: `sysfs.GPU` (Task 2), `sysfstest.AddGPU` (Task 2).
- Produces:
  - `var vram.ErrUnknown error`
  - `type vram.Backend interface { Total(sysfs.GPU) (int64, error); Used(sysfs.GPU) (int64, error) }`
  - `type vram.AMDGPU struct{}` (implements Backend)
  - `type vram.Overrides map[string]int64`; `func vram.LoadOverrides(path string) (vram.Overrides, error)`
  - `type vram.Detector struct { Backends map[string]vram.Backend; Overrides vram.Overrides }`
  - `func vram.NewDetector(o vram.Overrides) vram.Detector`
  - `func (vram.Detector) Total(sysfs.GPU) (int64, error)`; `func (vram.Detector) Used(sysfs.GPU) (int64, bool)`

- [ ] **Step 1: Add the YAML and quantity dependencies**

Run: `go get sigs.k8s.io/yaml@v1.6.0 k8s.io/apimachinery@v0.37.1`

- [ ] **Step 2: Write the failing tests**

`internal/vram/vram_test.go`:

```go
package vram_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfs"
	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfstest"
	"github.com/ipsupport-llc/dra-driver-dri/internal/vram"
)

const gib = int64(1) << 30

func scanOne(t *testing.T, g sysfstest.FakeGPU) sysfs.GPU {
	t.Helper()
	root := t.TempDir()
	sysfstest.AddGPU(t, root, g)
	gpus, err := sysfs.Scan(root)
	if err != nil || len(gpus) != 1 {
		t.Fatalf("scan: %v %v", gpus, err)
	}
	return gpus[0]
}

func d700(files map[string]string) sysfstest.FakeGPU {
	return sysfstest.FakeGPU{RenderNode: "renderD128", PCIAddress: "0000:06:00.0", VendorID: "0x1002", DeviceID: "0x6798", Driver: "amdgpu", Files: files}
}

func TestAMDGPUTotalAndUsed(t *testing.T) {
	g := scanOne(t, d700(map[string]string{"mem_info_vram_total": "6442450944\n", "mem_info_vram_used": "123456\n"}))
	d := vram.NewDetector(nil)
	total, err := d.Total(g)
	if err != nil || total != 6*gib {
		t.Fatalf("Total = %d, %v; want %d", total, err, 6*gib)
	}
	used, ok := d.Used(g)
	if !ok || used != 123456 {
		t.Fatalf("Used = %d, %v; want 123456, true", used, ok)
	}
}

func TestOverrideWins(t *testing.T) {
	g := scanOne(t, d700(map[string]string{"mem_info_vram_total": "6442450944\n"}))
	d := vram.NewDetector(vram.Overrides{"0000:06:00.0": 4 * gib})
	if total, err := d.Total(g); err != nil || total != 4*gib {
		t.Fatalf("Total = %d, %v; want %d", total, err, 4*gib)
	}
}

func TestUnknownVRAM(t *testing.T) {
	cases := map[string]sysfstest.FakeGPU{
		"no backend for driver": {RenderNode: "renderD128", PCIAddress: "0000:03:00.0", VendorID: "0x8086", DeviceID: "0xe20b", Driver: "xe"},
		"file missing":          d700(nil),
		"zero":                  d700(map[string]string{"mem_info_vram_total": "0\n"}),
		"garbage":               d700(map[string]string{"mem_info_vram_total": "lots\n"}),
	}
	for name, fake := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := vram.NewDetector(nil).Total(scanOne(t, fake))
			if !errors.Is(err, vram.ErrUnknown) {
				t.Fatalf("err = %v, want ErrUnknown", err)
			}
		})
	}
}

func TestUsedWithoutBackend(t *testing.T) {
	g := scanOne(t, sysfstest.FakeGPU{RenderNode: "renderD128", PCIAddress: "0000:03:00.0", VendorID: "0x8086", DeviceID: "0xe20b", Driver: "xe"})
	if _, ok := vram.NewDetector(vram.Overrides{"0000:03:00.0": 16 * gib}).Used(g); ok {
		t.Fatal("Used reported a value without a backend")
	}
}

func TestLoadOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overrides.yaml")
	if err := os.WriteFile(path, []byte("\"0000:03:00.0\": 16Gi\n\"0000:0A:00.0\": 512Mi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := vram.LoadOverrides(path)
	if err != nil {
		t.Fatal(err)
	}
	if got["0000:03:00.0"] != 16*gib || got["0000:0a:00.0"] != 512<<20 || len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestLoadOverridesEmptyPath(t *testing.T) {
	got, err := vram.LoadOverrides("")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestLoadOverridesInvalid(t *testing.T) {
	for name, body := range map[string]string{"bad quantity": "\"0000:03:00.0\": sixteen\n", "negative": "\"0000:03:00.0\": -1Gi\n", "not a map": "- 1\n"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "o.yaml")
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := vram.LoadOverrides(path); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/vram/`
Expected: FAIL — `undefined: vram.NewDetector`.

- [ ] **Step 4: Implement**

`internal/vram/vram.go`:

```go
// Package vram reads the video memory of render-node GPUs.
package vram

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfs"
)

// ErrUnknown means the VRAM size of a GPU could not be determined.
var ErrUnknown = errors.New("VRAM size unknown")

// Backend reads VRAM for GPUs bound to one kernel driver.
type Backend interface {
	Total(gpu sysfs.GPU) (int64, error)
	Used(gpu sysfs.GPU) (int64, error)
}

// AMDGPU reads the amdgpu driver's sysfs counters.
type AMDGPU struct{}

// Total returns mem_info_vram_total.
func (AMDGPU) Total(g sysfs.GPU) (int64, error) {
	return readInt(filepath.Join(g.DevicePath, "mem_info_vram_total"))
}

// Used returns mem_info_vram_used.
func (AMDGPU) Used(g sysfs.GPU) (int64, error) {
	return readInt(filepath.Join(g.DevicePath, "mem_info_vram_used"))
}

func readInt(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

// Overrides sets the VRAM size per PCI address (lower-case).
type Overrides map[string]int64

// LoadOverrides reads a YAML map of PCI address to quantity, for example:
//
//	"0000:03:00.0": 16Gi
//
// An empty path yields no overrides.
func LoadOverrides(path string) (Overrides, error) {
	out := Overrides{}
	if path == "" {
		return out, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read overrides: %w", err)
	}
	raw := map[string]string{}
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse overrides %s: %w", path, err)
	}
	for addr, q := range raw {
		qty, err := resource.ParseQuantity(q)
		if err != nil {
			return nil, fmt.Errorf("overrides %s: %s: %w", path, addr, err)
		}
		if qty.Sign() <= 0 {
			return nil, fmt.Errorf("overrides %s: %s: VRAM must be positive", path, addr)
		}
		out[strings.ToLower(addr)] = qty.Value()
	}
	return out, nil
}

// Detector picks the VRAM source for a GPU: an override first, then the
// backend of its kernel driver.
type Detector struct {
	Backends  map[string]Backend // keyed by kernel driver name
	Overrides Overrides
}

// NewDetector returns a Detector with every built-in backend.
func NewDetector(o Overrides) Detector {
	return Detector{Backends: map[string]Backend{"amdgpu": AMDGPU{}}, Overrides: o}
}

// Total returns the GPU's VRAM size in bytes, or an error wrapping ErrUnknown.
func (d Detector) Total(g sysfs.GPU) (int64, error) {
	if v, ok := d.Overrides[g.PCIAddress]; ok {
		return v, nil
	}
	b, ok := d.Backends[g.KernelDriver]
	if !ok {
		return 0, fmt.Errorf("%s: no VRAM backend for kernel driver %q: %w", g.PCIAddress, g.KernelDriver, ErrUnknown)
	}
	v, err := b.Total(g)
	if err != nil {
		return 0, fmt.Errorf("%s: %w: %v", g.PCIAddress, ErrUnknown, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("%s: backend reported %d bytes: %w", g.PCIAddress, v, ErrUnknown)
	}
	return v, nil
}

// Used returns the VRAM in use; ok is false when no backend can tell.
func (d Detector) Used(g sysfs.GPU) (int64, bool) {
	b, ok := d.Backends[g.KernelDriver]
	if !ok {
		return 0, false
	}
	v, err := b.Used(g)
	if err != nil {
		return 0, false
	}
	return v, true
}
```

- [ ] **Step 5: Run tests**

Run: `go mod tidy && go test ./internal/vram/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/vram
git commit -m "vram: amdgpu backend, per-PCI overrides, detector

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: DRA device model (`devices`)

**Files:**
- Create: `internal/devices/devices.go`, `internal/devices/devices_test.go`
- Modify: `docs/design.md` §5 (request granularity)

**Interfaces:**
- Consumes: `sysfs.GPU`, `vram.Detector` (Tasks 2–3).
- Produces:
  - `const devices.CapacityVRAM resourceapi.QualifiedName = "vram"`; `const devices.DefaultStep int64 = 64 << 20`
  - `type devices.Config struct { NodeName string; ReserveBytes int64; StepBytes int64 }`
  - `type devices.Published struct { Name string; GPU sysfs.GPU; Vendor string; VRAMTotal, VRAMPublished int64 }`
  - `type devices.Skipped struct { GPU sysfs.GPU; Reason error }`
  - `func devices.DeviceName(pciAddress string) string`; `func devices.VendorName(vendorID string) string`
  - `func devices.Build(cfg devices.Config, gpus []sysfs.GPU, det vram.Detector) ([]devices.Published, []devices.Skipped)`
  - `func devices.Resources(cfg devices.Config, pubs []devices.Published) resourceslice.DriverResources`

- [ ] **Step 1: Add dependencies**

Run: `go get k8s.io/api@v0.37.1 k8s.io/dynamic-resource-allocation@v0.37.1 k8s.io/utils@v0.0.0-20260626114624-be93311217bd`

- [ ] **Step 2: Write the failing tests**

`internal/devices/devices_test.go`:

```go
package devices_test

import (
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/deviceattribute"

	"github.com/ipsupport-llc/dra-driver-dri/internal/devices"
	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfs"
	"github.com/ipsupport-llc/dra-driver-dri/internal/vram"
)

const mib = int64(1) << 20

func gpu(addr, node string) sysfs.GPU {
	return sysfs.GPU{RenderNode: node, PCIAddress: addr, VendorID: "0x1002", DeviceID: "0x6798", KernelDriver: "amdgpu"}
}

func cfg() devices.Config {
	return devices.Config{NodeName: "node-a", ReserveBytes: 256 * mib, StepBytes: 64 * mib}
}

func TestDeviceNameAndVendor(t *testing.T) {
	if got := devices.DeviceName("0000:06:00.0"); got != "pci-0000-06-00-0" {
		t.Errorf("DeviceName = %q", got)
	}
	for id, want := range map[string]string{"0x1002": "amd", "0x8086": "intel", "0x10DE": "nvidia", "0x1af4": "unknown"} {
		if got := devices.VendorName(id); got != want {
			t.Errorf("VendorName(%s) = %q, want %q", id, got, want)
		}
	}
}

func TestBuildPublishesRoundedVRAM(t *testing.T) {
	det := vram.Detector{Overrides: vram.Overrides{
		"0000:02:00.0": 6144 * mib,        // D700: 6144 - 256 = 5888 MiB (multiple of 64)
		"0000:06:00.0": 6144*mib + 100000, // odd size rounds down to 5888 MiB
	}}
	pubs, skipped := devices.Build(cfg(), []sysfs.GPU{gpu("0000:02:00.0", "renderD129"), gpu("0000:06:00.0", "renderD128")}, det)
	if len(skipped) != 0 || len(pubs) != 2 {
		t.Fatalf("pubs=%+v skipped=%+v", pubs, skipped)
	}
	for _, p := range pubs {
		if p.VRAMPublished != 5888*mib || p.Vendor != "amd" {
			t.Errorf("%s: published %d vendor %s", p.Name, p.VRAMPublished, p.Vendor)
		}
	}
	if pubs[0].Name != "pci-0000-02-00-0" || pubs[0].VRAMTotal != 6144*mib {
		t.Errorf("first = %+v", pubs[0])
	}
}

func TestBuildSkips(t *testing.T) {
	det := vram.Detector{Overrides: vram.Overrides{"0000:07:00.0": 300 * mib}} // 300-256 = 44 MiB < one step
	pubs, skipped := devices.Build(cfg(), []sysfs.GPU{gpu("0000:06:00.0", "renderD128"), gpu("0000:07:00.0", "renderD130")}, det)
	if len(pubs) != 0 || len(skipped) != 2 {
		t.Fatalf("pubs=%+v skipped=%+v", pubs, skipped)
	}
	if !strings.Contains(skipped[1].Reason.Error(), "reserve") {
		t.Errorf("reason = %v", skipped[1].Reason)
	}
}

func TestResources(t *testing.T) {
	pubs := []devices.Published{{Name: "pci-0000-06-00-0", GPU: gpu("0000:06:00.0", "renderD128"), Vendor: "amd", VRAMTotal: 6144 * mib, VRAMPublished: 5888 * mib}}
	res := devices.Resources(cfg(), pubs)

	pool, ok := res.Pools["node-a"]
	if !ok || len(pool.Slices) != 1 || len(pool.Slices[0].Devices) != 1 {
		t.Fatalf("pools = %+v", res.Pools)
	}
	d := pool.Slices[0].Devices[0]
	if d.Name != "pci-0000-06-00-0" || d.AllowMultipleAllocations == nil || !*d.AllowMultipleAllocations {
		t.Fatalf("device = %+v", d)
	}
	attrs := map[resourceapi.QualifiedName]string{
		"vendor": "amd", "kernelDriver": "amdgpu", "deviceID": "0x6798", "renderNode": "renderD128",
		deviceattribute.StandardDeviceAttributePCIBusID: "0000:06:00.0",
	}
	for k, want := range attrs {
		if a, ok := d.Attributes[k]; !ok || a.StringValue == nil || *a.StringValue != want {
			t.Errorf("attribute %s = %+v, want %q", k, d.Attributes[k], want)
		}
	}
	c, ok := d.Capacity[devices.CapacityVRAM]
	if !ok || c.Value.Value() != 5888*mib {
		t.Fatalf("capacity = %+v", d.Capacity)
	}
	p := c.RequestPolicy
	if p == nil || p.Default.Value() != 5888*mib || p.ValidRange == nil ||
		p.ValidRange.Min.Value() != 64*mib || p.ValidRange.Step.Value() != 64*mib || p.ValidRange.Max.Value() != 5888*mib {
		t.Fatalf("request policy = %+v", p)
	}
}

func TestResourcesNoGPUs(t *testing.T) {
	res := devices.Resources(cfg(), nil)
	if pool, ok := res.Pools["node-a"]; !ok || len(pool.Slices) != 1 || len(pool.Slices[0].Devices) != 0 {
		t.Fatalf("pools = %+v", res.Pools)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/devices/`
Expected: FAIL — `undefined: devices.Build`.

- [ ] **Step 4: Implement**

`internal/devices/devices.go`:

```go
// Package devices turns discovered GPUs into DRA devices.
package devices

import (
	"fmt"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/utils/ptr"

	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfs"
	"github.com/ipsupport-llc/dra-driver-dri/internal/vram"
)

// CapacityVRAM is the consumable capacity a claim requests.
const CapacityVRAM resourceapi.QualifiedName = "vram"

// DefaultStep is the request granularity and the minimum request.
const DefaultStep int64 = 64 << 20

// Config holds the node-level publishing settings.
type Config struct {
	NodeName     string
	ReserveBytes int64 // subtracted from each GPU's VRAM
	StepBytes    int64 // request granularity; published VRAM is rounded down to it
}

func (c Config) step() int64 {
	if c.StepBytes > 0 {
		return c.StepBytes
	}
	return DefaultStep
}

// Published is a GPU as offered to the scheduler.
type Published struct {
	Name          string
	GPU           sysfs.GPU
	Vendor        string
	VRAMTotal     int64
	VRAMPublished int64
}

// Skipped is a GPU left out of the ResourceSlice, with the reason.
type Skipped struct {
	GPU    sysfs.GPU
	Reason error
}

// DeviceName derives a stable device name from the PCI address.
func DeviceName(pciAddress string) string {
	return "pci-" + strings.NewReplacer(":", "-", ".", "-").Replace(pciAddress)
}

// VendorName maps a PCI vendor ID to a short name.
func VendorName(vendorID string) string {
	switch strings.ToLower(vendorID) {
	case "0x1002":
		return "amd"
	case "0x8086":
		return "intel"
	case "0x10de":
		return "nvidia"
	default:
		return "unknown"
	}
}

// Build decides which GPUs to publish and how much VRAM each offers.
func Build(cfg Config, gpus []sysfs.GPU, det vram.Detector) ([]Published, []Skipped) {
	step := cfg.step()
	var pubs []Published
	var skipped []Skipped
	for _, g := range gpus {
		total, err := det.Total(g)
		if err != nil {
			skipped = append(skipped, Skipped{GPU: g, Reason: err})
			continue
		}
		published := (total - cfg.ReserveBytes) / step * step
		if published < step {
			skipped = append(skipped, Skipped{GPU: g, Reason: fmt.Errorf(
				"%s: %d bytes of VRAM minus the %d-byte reserve is less than one %d-byte step",
				g.PCIAddress, total, cfg.ReserveBytes, step)})
			continue
		}
		pubs = append(pubs, Published{
			Name:          DeviceName(g.PCIAddress),
			GPU:           g,
			Vendor:        VendorName(g.VendorID),
			VRAMTotal:     total,
			VRAMPublished: published,
		})
	}
	return pubs, skipped
}

// Resources builds the content of this node's ResourceSlice.
func Resources(cfg Config, pubs []Published) resourceslice.DriverResources {
	step := cfg.step()
	devs := make([]resourceapi.Device, 0, len(pubs))
	for _, p := range pubs {
		value := *resource.NewQuantity(p.VRAMPublished, resource.BinarySI)
		def, maxQ := value.DeepCopy(), value.DeepCopy()
		minQ := *resource.NewQuantity(step, resource.BinarySI)
		stepQ := minQ.DeepCopy()
		devs = append(devs, resourceapi.Device{
			Name: p.Name,
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"vendor":       {StringValue: ptr.To(p.Vendor)},
				"kernelDriver": {StringValue: ptr.To(p.GPU.KernelDriver)},
				"deviceID":     {StringValue: ptr.To(p.GPU.DeviceID)},
				"renderNode":   {StringValue: ptr.To(p.GPU.RenderNode)},
				deviceattribute.StandardDeviceAttributePCIBusID: {StringValue: ptr.To(p.GPU.PCIAddress)},
			},
			Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
				CapacityVRAM: {
					Value: value,
					RequestPolicy: &resourceapi.CapacityRequestPolicy{
						Default: &def,
						ValidRange: &resourceapi.CapacityRequestPolicyRange{
							Min:  &minQ,
							Step: &stepQ,
							Max:  &maxQ,
						},
					},
				},
			},
			AllowMultipleAllocations: ptr.To(true),
		})
	}
	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			cfg.NodeName: {Slices: []resourceslice.Slice{{Devices: devs}}},
		},
	}
}
```

- [ ] **Step 5: Run tests**

Run: `go mod tidy && go test ./internal/devices/ -v`
Expected: PASS.

- [ ] **Step 6: Update the design (request granularity)**

In `docs/design.md` §5, replace the bullet

```
  - Request policy: `default` = the whole published amount, `validRange.min`
    = `64Mi`. A claim that does not name an amount gets **the whole GPU**;
    sharing happens only when a claim asks for a specific `vram`.
```

with

```
  - Request policy: `default` = the whole published amount; `validRange`
    `min` = `step` = `64Mi`, `max` = the published amount. Requests round up
    to a multiple of 64 MiB, and the published amount is rounded **down** to
    one. A claim that does not name an amount gets **the whole GPU**;
    sharing happens only when a claim asks for a specific `vram`.
```

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/devices docs/design.md
git commit -m "devices: publish GPUs with consumable vram capacity

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Per-claim CDI specs (`cdi`)

**Files:**
- Create: `internal/cdi/cdi.go`, `internal/cdi/cdi_test.go`
- Modify: `docs/design.md` §7 (spec file name)

**Interfaces:**
- Produces:
  - `type cdi.Edit struct { Device, ShareID, RenderNode, PCIAddress, Vendor string; VRAMBytes int64 }`
  - `func cdi.NewWriter(root, driverName string) (*cdi.Writer, error)`
  - `func (*cdi.Writer) QualifiedID(claimUID string, e cdi.Edit) string`
  - `func (*cdi.Writer) WriteClaim(claimUID string, edits []cdi.Edit) error`
  - `func (*cdi.Writer) DeleteClaim(claimUID string) error` (missing spec → nil)

- [ ] **Step 1: Add dependencies**

Run: `go get tags.cncf.io/container-device-interface@v1.1.1 tags.cncf.io/container-device-interface/specs-go@v1.1.1`

- [ ] **Step 2: Write the failing tests**

`internal/cdi/cdi_test.go`:

```go
package cdi_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	cdispec "tags.cncf.io/container-device-interface/specs-go"
	"sigs.k8s.io/yaml"

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
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/cdi/`
Expected: FAIL — `undefined: cdi.NewWriter`.

- [ ] **Step 4: Implement**

`internal/cdi/cdi.go`:

```go
// Package cdi writes per-claim CDI specs that hand a container one render node.
//
// The layout follows kubernetes-sigs/dra-example-driver (Apache-2.0):
// one transient spec per claim, kind "k8s.<driver-name>/gpu".
package cdi

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdiparser "tags.cncf.io/container-device-interface/pkg/parser"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

const class = "gpu"

// Edit is one GPU handed to a claim.
type Edit struct {
	Device     string // DRA device name
	ShareID    string // empty when the allocation is not shared
	RenderNode string // e.g. "renderD128"
	PCIAddress string
	Vendor     string
	VRAMBytes  int64
}

// Writer writes and removes claim specs in one CDI spec directory.
type Writer struct {
	cache  *cdiapi.Cache
	vendor string
}

// NewWriter creates root if needed and returns a Writer for driverName.
func NewWriter(root, driverName string) (*Writer, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create CDI spec dir: %w", err)
	}
	cache, err := cdiapi.NewCache(cdiapi.WithSpecDirs(root), cdiapi.WithAutoRefresh(false))
	if err != nil {
		return nil, fmt.Errorf("CDI cache: %w", err)
	}
	return &Writer{cache: cache, vendor: "k8s." + driverName}, nil
}

func (w *Writer) deviceName(claimUID string, e Edit) string {
	n := claimUID + "-" + e.Device
	if e.ShareID != "" {
		n += "-" + e.ShareID
	}
	return n
}

// QualifiedID is the CDI device ID the kubelet passes to the runtime.
func (w *Writer) QualifiedID(claimUID string, e Edit) string {
	return cdiparser.QualifiedName(w.vendor, class, w.deviceName(claimUID, e))
}

func (w *Writer) specName(claimUID string) string {
	return cdiapi.GenerateTransientSpecName(w.vendor, class, claimUID)
}

// WriteClaim writes (or replaces) the spec of one claim.
func (w *Writer) WriteClaim(claimUID string, edits []Edit) error {
	spec := &cdispec.Spec{Kind: w.vendor + "/" + class}
	for _, e := range edits {
		mode := os.FileMode(0o666)
		spec.Devices = append(spec.Devices, cdispec.Device{
			Name: w.deviceName(claimUID, e),
			ContainerEdits: cdispec.ContainerEdits{
				DeviceNodes: []*cdispec.DeviceNode{{
					Path:        "/dev/dri/" + e.RenderNode,
					Permissions: "rw",
					FileMode:    &mode,
				}},
				Env: []string{
					"DRA_GPU_PCI_BUS_ID=" + e.PCIAddress,
					"DRA_GPU_VENDOR=" + e.Vendor,
					fmt.Sprintf("DRA_GPU_VRAM_BYTES=%d", e.VRAMBytes),
				},
			},
		})
	}
	version, err := cdispec.MinimumRequiredVersion(spec)
	if err != nil {
		return fmt.Errorf("CDI spec version: %w", err)
	}
	spec.Version = version
	if err := w.cache.WriteSpec(spec, w.specName(claimUID)); err != nil {
		return fmt.Errorf("write CDI spec for claim %s: %w", claimUID, err)
	}
	return nil
}

// DeleteClaim removes the claim's spec; a missing spec is not an error.
func (w *Writer) DeleteClaim(claimUID string) error {
	err := w.cache.RemoveSpec(w.specName(claimUID))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove CDI spec for claim %s: %w", claimUID, err)
	}
	return nil
}
```

- [ ] **Step 5: Run tests**

Run: `go mod tidy && go test ./internal/cdi/ -v`
Expected: PASS. If `TestUnsharedNameAndDelete` fails on the second delete because the library wraps the error differently, print the error type in the test, adjust the `errors.Is` check to match it, and keep the "second delete returns nil" assertion.

- [ ] **Step 6: Update the design (spec file name)**

In `docs/design.md` §7, replace

```
Per prepared claim, `/var/run/cdi/<driver-name>-<claim-uid>.json`, kind
`<driver-name>/gpu`:
```

with

```
Per prepared claim, one transient spec in `/var/run/cdi` named by the CDI
library from the driver name and the claim UID, kind `k8s.<driver-name>/gpu`.
Device names are `<claim-uid>-<device>[-<share-id>]`:
```

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/cdi docs/design.md
git commit -m "cdi: per-claim specs with one render node and DRA_GPU_* env

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: Checkpoint (`state`)

**Files:**
- Create: `internal/state/state.go`, `internal/state/state_test.go`
- Modify: `docs/design.md` §12 (corrupt checkpoint row)

**Interfaces:**
- Produces:
  - `type state.Device struct { Request, Pool, Device, ShareID string; VRAMBytes int64; CDIDeviceIDs []string }` (JSON tags `request,pool,device,shareID,vramBytes,cdiDeviceIDs`)
  - `func state.Open(path string) (*state.Store, error)`
  - `func (*state.Store) Get(uid string) ([]state.Device, bool)`
  - `func (*state.Store) Put(uid string, devs []state.Device) error`
  - `func (*state.Store) Delete(uid string) error`
  - `func (*state.Store) All() map[string][]state.Device` (deep copy)

- [ ] **Step 1: Write the failing tests**

`internal/state/state_test.go`:

```go
package state_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ipsupport-llc/dra-driver-dri/internal/state"
)

func dev(name string) state.Device {
	return state.Device{Request: "gpu", Pool: "node-a", Device: name, VRAMBytes: 1 << 30, CDIDeviceIDs: []string{"k8s.x/gpu=" + name}}
}

func TestOpenMissing(t *testing.T) {
	s, err := state.Open(filepath.Join(t.TempDir(), "checkpoint.json"))
	if err != nil || len(s.All()) != 0 {
		t.Fatalf("got %v, %v", s.All(), err)
	}
}

func TestPutGetPersistDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	s, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("uid-1", []state.Device{dev("a")}); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Get("uid-1"); !ok || len(got) != 1 || got[0].Device != "a" {
		t.Fatalf("Get = %v, %v", got, ok)
	}
	reopened, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reopened.Get("uid-1"); !ok || got[0].CDIDeviceIDs[0] != "k8s.x/gpu=a" {
		t.Fatalf("after reopen: %v, %v", got, ok)
	}
	if err := reopened.Delete("uid-1"); err != nil {
		t.Fatal(err)
	}
	again, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again.Get("uid-1"); ok {
		t.Fatal("claim survived Delete")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
}

func TestAllIsACopy(t *testing.T) {
	s, err := state.Open(filepath.Join(t.TempDir(), "c.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("uid-1", []state.Device{dev("a")}); err != nil {
		t.Fatal(err)
	}
	all := s.All()
	all["uid-1"][0].Device = "mutated"
	if got, _ := s.Get("uid-1"); got[0].Device != "a" {
		t.Fatal("All returned shared memory")
	}
}

func TestOpenCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Open(path); err == nil {
		t.Fatal("want an error for a corrupt checkpoint")
	}
}

func TestOpenWrongVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(path, []byte(`{"version": 99, "claims": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Open(path); err == nil {
		t.Fatal("want an error for an unknown version")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/state/`
Expected: FAIL — `undefined: state.Open`.

- [ ] **Step 3: Implement**

`internal/state/state.go`:

```go
// Package state keeps the claims this node prepared, across plugin restarts.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sync"
)

const version = 1

// Device is one prepared device of a claim.
type Device struct {
	Request      string   `json:"request"`
	Pool         string   `json:"pool"`
	Device       string   `json:"device"`
	ShareID      string   `json:"shareID,omitempty"`
	VRAMBytes    int64    `json:"vramBytes"`
	CDIDeviceIDs []string `json:"cdiDeviceIDs"`
}

type checkpoint struct {
	Version int                 `json:"version"`
	Claims  map[string][]Device `json:"claims"`
}

// Store is a JSON checkpoint file. It is safe for concurrent use.
type Store struct {
	mu     sync.Mutex
	path   string
	claims map[string][]Device
}

// Open loads the checkpoint at path. A missing file is an empty checkpoint;
// an unreadable or foreign one is an error, so prepared claims are never
// forgotten silently.
func Open(path string) (*Store, error) {
	s := &Store{path: path, claims: map[string][]Device{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read checkpoint: %w", err)
	}
	var cp checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, fmt.Errorf("decode checkpoint %s: %w", path, err)
	}
	if cp.Version != version {
		return nil, fmt.Errorf("checkpoint %s: unsupported version %d", path, cp.Version)
	}
	if cp.Claims != nil {
		s.claims = cp.Claims
	}
	return s, nil
}

// Get returns a copy of the claim's devices.
func (s *Store) Get(uid string) ([]Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.claims[uid]
	return cloneDevices(d), ok
}

// Put records a claim and writes the checkpoint.
func (s *Store) Put(uid string, devs []Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.claims[uid]
	s.claims[uid] = cloneDevices(devs)
	if err := s.save(); err != nil {
		if had {
			s.claims[uid] = prev
		} else {
			delete(s.claims, uid)
		}
		return err
	}
	return nil
}

// Delete forgets a claim and writes the checkpoint.
func (s *Store) Delete(uid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.claims[uid]
	if !had {
		return nil
	}
	delete(s.claims, uid)
	if err := s.save(); err != nil {
		s.claims[uid] = prev
		return err
	}
	return nil
}

// All returns a deep copy of every recorded claim.
func (s *Store) All() map[string][]Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]Device, len(s.claims))
	for uid, d := range s.claims {
		out[uid] = cloneDevices(d)
	}
	return out
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(checkpoint{Version: version, Claims: s.claims}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode checkpoint: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write checkpoint: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace checkpoint: %w", err)
	}
	return nil
}

func cloneDevices(in []Device) []Device {
	if in == nil {
		return nil
	}
	out := make([]Device, len(in))
	for i, d := range in {
		d.CDIDeviceIDs = slices.Clone(d.CDIDeviceIDs)
		out[i] = d
	}
	return out
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/state/ -race -v`
Expected: PASS.

- [ ] **Step 5: Update the design (checkpoint)**

In `docs/design.md` §12, add a row to the table after "Plugin restarts":

```
| Checkpoint unreadable (corrupt, unknown version) | The plugin refuses to start (crash loop, alert) instead of forgetting prepared claims. Recovery: inspect `<plugins dir>/<driver-name>/checkpoint.json`; deleting it is safe only when no pod on the node uses a claim of this driver. |
```

- [ ] **Step 6: Commit**

```bash
git add internal/state docs/design.md
git commit -m "state: JSON checkpoint of prepared claims

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: Claim preparation (`prepare`)

**Files:**
- Create: `internal/prepare/prepare.go`, `internal/prepare/prepare_test.go`

**Interfaces:**
- Consumes: `devices.Published`, `devices.CapacityVRAM` (Task 4); `cdi.Writer`, `cdi.Edit` (Task 5); `state.Store`, `state.Device` (Task 6).
- Produces:
  - `type prepare.Inventory func() map[string]devices.Published`
  - `type prepare.Preparer struct { DriverName string; Inventory prepare.Inventory; CDI *cdi.Writer; Store *state.Store }`
  - `func (*prepare.Preparer) Prepare(claim *resourceapi.ResourceClaim) ([]kubeletplugin.Device, error)`
  - `func (*prepare.Preparer) Unprepare(uid string) error`
  - `func (*prepare.Preparer) Allocated() map[string]int64` (bytes per device name)
  - `func (*prepare.Preparer) Count() int` (prepared claims)

- [ ] **Step 1: Write the failing tests**

`internal/prepare/prepare_test.go`:

```go
package prepare_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/ipsupport-llc/dra-driver-dri/internal/cdi"
	"github.com/ipsupport-llc/dra-driver-dri/internal/devices"
	"github.com/ipsupport-llc/dra-driver-dri/internal/prepare"
	"github.com/ipsupport-llc/dra-driver-dri/internal/state"
	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfs"
)

const (
	driver = "gpu.example.com"
	gib    = int64(1) << 30
)

type env struct {
	cdiRoot, checkpoint string
	inv                 map[string]devices.Published
	p                   *prepare.Preparer
}

func published(name, addr, node string) devices.Published {
	return devices.Published{Name: name, Vendor: "amd", VRAMTotal: 6 * gib, VRAMPublished: 5888 << 20,
		GPU: sysfs.GPU{RenderNode: node, PCIAddress: addr, VendorID: "0x1002", DeviceID: "0x6798", KernelDriver: "amdgpu"}}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{cdiRoot: filepath.Join(dir, "cdi"), checkpoint: filepath.Join(dir, "checkpoint.json"),
		inv: map[string]devices.Published{"pci-0000-06-00-0": published("pci-0000-06-00-0", "0000:06:00.0", "renderD128")}}
	e.reopen(t)
	return e
}

// reopen simulates a plugin restart: fresh writer and store on the same paths.
func (e *env) reopen(t *testing.T) {
	t.Helper()
	w, err := cdi.NewWriter(e.cdiRoot, driver)
	if err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(e.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	e.p = &prepare.Preparer{DriverName: driver, Inventory: func() map[string]devices.Published { return e.inv }, CDI: w, Store: s}
}

func (e *env) specs(t *testing.T) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(e.cdiRoot, "*"))
	return m
}

func result(device string, share string, vram int64) resourceapi.DeviceRequestAllocationResult {
	r := resourceapi.DeviceRequestAllocationResult{Request: "gpu", Driver: driver, Pool: "node-a", Device: device}
	if share != "" {
		r.ShareID = ptr.To(types.UID(share))
	}
	if vram > 0 {
		r.ConsumedCapacity = map[resourceapi.QualifiedName]resource.Quantity{devices.CapacityVRAM: *resource.NewQuantity(vram, resource.BinarySI)}
	}
	return r
}

func claim(uid string, results ...resourceapi.DeviceRequestAllocationResult) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "claim-" + uid, UID: types.UID(uid)},
		Status: resourceapi.ResourceClaimStatus{Allocation: &resourceapi.AllocationResult{
			Devices: resourceapi.DeviceAllocationResult{Results: results},
		}},
	}
}

func readSpec(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPrepareWholeGPU(t *testing.T) {
	e := newEnv(t)
	got, err := e.p.Prepare(claim("u1", result("pci-0000-06-00-0", "", 0)))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].DeviceName != "pci-0000-06-00-0" || got[0].PoolName != "node-a" || got[0].ShareID != nil ||
		len(got[0].CDIDeviceIDs) != 1 || got[0].CDIDeviceIDs[0] != "k8s.gpu.example.com/gpu=u1-pci-0000-06-00-0" {
		t.Fatalf("devices = %+v", got)
	}
	specs := e.specs(t)
	if len(specs) != 1 || !strings.Contains(readSpec(t, specs[0]), "DRA_GPU_VRAM_BYTES=6174015488") {
		t.Fatalf("spec = %v", specs)
	}
	if e.p.Allocated()["pci-0000-06-00-0"] != 5888<<20 || e.p.Count() != 1 {
		t.Fatalf("allocated = %v count = %d", e.p.Allocated(), e.p.Count())
	}
}

func TestPrepareTwoSharesOfOneDevice(t *testing.T) {
	e := newEnv(t)
	a, err := e.p.Prepare(claim("u1", result("pci-0000-06-00-0", "s1", 2*gib)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.p.Prepare(claim("u2", result("pci-0000-06-00-0", "s2", 3*gib)))
	if err != nil {
		t.Fatal(err)
	}
	if a[0].CDIDeviceIDs[0] == b[0].CDIDeviceIDs[0] || *a[0].ShareID != "s1" || *b[0].ShareID != "s2" {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
	if got := e.p.Allocated()["pci-0000-06-00-0"]; got != 5*gib {
		t.Fatalf("allocated = %d", got)
	}
	if err := e.p.Unprepare("u1"); err != nil {
		t.Fatal(err)
	}
	specs := e.specs(t)
	if len(specs) != 1 || !strings.Contains(readSpec(t, specs[0]), "u2-pci-0000-06-00-0-s2") {
		t.Fatalf("specs after unprepare = %v", specs)
	}
	if got := e.p.Allocated()["pci-0000-06-00-0"]; got != 3*gib {
		t.Fatalf("allocated after unprepare = %d", got)
	}
}

func TestPrepareIgnoresOtherDrivers(t *testing.T) {
	e := newEnv(t)
	other := result("nic-0", "", 0)
	other.Driver = "net.example.com"
	got, err := e.p.Prepare(claim("u1", other, result("pci-0000-06-00-0", "", 0)))
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := e.p.Prepare(claim("u2", other)); err == nil {
		t.Fatal("a claim without devices of this driver must fail")
	}
}

func TestPrepareUnknownDevice(t *testing.T) {
	e := newEnv(t)
	_, err := e.p.Prepare(claim("u1", result("pci-0000-09-00-0", "", 0)))
	if err == nil || !strings.Contains(err.Error(), "pci-0000-09-00-0") {
		t.Fatalf("err = %v", err)
	}
	if len(e.specs(t)) != 0 || e.p.Count() != 0 {
		t.Fatal("a failed prepare left state behind")
	}
}

func TestPrepareNoAllocation(t *testing.T) {
	e := newEnv(t)
	c := claim("u1")
	c.Status.Allocation = nil
	if _, err := e.p.Prepare(c); err == nil {
		t.Fatal("want an error")
	}
}

func TestPrepareIsIdempotent(t *testing.T) {
	e := newEnv(t)
	c := claim("u1", result("pci-0000-06-00-0", "s1", 2*gib))
	first, err := e.p.Prepare(c)
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.p.Prepare(c)
	if err != nil {
		t.Fatal(err)
	}
	if first[0].CDIDeviceIDs[0] != second[0].CDIDeviceIDs[0] || len(e.specs(t)) != 1 {
		t.Fatalf("first=%+v second=%+v specs=%v", first, second, e.specs(t))
	}
}

func TestPrepareRestoresAfterReboot(t *testing.T) {
	e := newEnv(t)
	c := claim("u1", result("pci-0000-06-00-0", "s1", 2*gib))
	if _, err := e.p.Prepare(c); err != nil {
		t.Fatal(err)
	}
	// Reboot: /var/run/cdi is empty, the render node of the same PCI device changed.
	if err := os.RemoveAll(e.cdiRoot); err != nil {
		t.Fatal(err)
	}
	e.inv["pci-0000-06-00-0"] = published("pci-0000-06-00-0", "0000:06:00.0", "renderD129")
	e.reopen(t)

	got, err := e.p.Prepare(c)
	if err != nil {
		t.Fatal(err)
	}
	specs := e.specs(t)
	if len(specs) != 1 {
		t.Fatalf("specs = %v", specs)
	}
	body := readSpec(t, specs[0])
	if !strings.Contains(body, "/dev/dri/renderD129") || !strings.Contains(body, "DRA_GPU_VRAM_BYTES=2147483648") {
		t.Fatalf("restored spec:\n%s", body)
	}
	if *got[0].ShareID != "s1" {
		t.Fatalf("devices = %+v", got)
	}
}

func TestRestoreDeviceGone(t *testing.T) {
	e := newEnv(t)
	c := claim("u1", result("pci-0000-06-00-0", "", 0))
	if _, err := e.p.Prepare(c); err != nil {
		t.Fatal(err)
	}
	delete(e.inv, "pci-0000-06-00-0")
	if _, err := e.p.Prepare(c); err == nil {
		t.Fatal("restoring onto a missing device must fail")
	}
}

func TestUnprepareUnknownClaim(t *testing.T) {
	if err := newEnv(t).p.Unprepare("nope"); err != nil {
		t.Fatal(err)
	}
}
```

Note: `DRA_GPU_VRAM_BYTES=6174015488` is 5888 MiB.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/prepare/`
Expected: FAIL — `undefined: prepare.Preparer`.

- [ ] **Step 3: Implement**

`internal/prepare/prepare.go`:

```go
// Package prepare turns allocated ResourceClaims into CDI specs.
package prepare

import (
	"fmt"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/utils/ptr"

	"github.com/ipsupport-llc/dra-driver-dri/internal/cdi"
	"github.com/ipsupport-llc/dra-driver-dri/internal/devices"
	"github.com/ipsupport-llc/dra-driver-dri/internal/state"
)

// Inventory returns the devices this node publishes, keyed by device name.
type Inventory func() map[string]devices.Published

// Preparer prepares and unprepares claims of one driver on one node.
type Preparer struct {
	DriverName string
	Inventory  Inventory
	CDI        *cdi.Writer
	Store      *state.Store
}

// Prepare writes the CDI spec of a claim and records it. Preparing a claim
// that is already recorded rewrites its spec from the record (the spec
// directory is tmpfs and is empty after a reboot) and returns the same
// devices.
func (p *Preparer) Prepare(claim *resourceapi.ResourceClaim) ([]kubeletplugin.Device, error) {
	uid := string(claim.UID)
	inv := p.Inventory()
	if recorded, ok := p.Store.Get(uid); ok {
		edits, err := editsFor(recorded, inv)
		if err != nil {
			return nil, fmt.Errorf("restore claim %s/%s: %w", claim.Namespace, claim.Name, err)
		}
		if err := p.CDI.WriteClaim(uid, edits); err != nil {
			return nil, err
		}
		return toKubelet(recorded), nil
	}
	if claim.Status.Allocation == nil {
		return nil, fmt.Errorf("claim %s/%s is not allocated", claim.Namespace, claim.Name)
	}
	var recs []state.Device
	for _, r := range claim.Status.Allocation.Devices.Results {
		if r.Driver != p.DriverName {
			continue
		}
		pub, ok := inv[r.Device]
		if !ok {
			return nil, fmt.Errorf("claim %s/%s: device %s is not available on this node", claim.Namespace, claim.Name, r.Device)
		}
		vram := pub.VRAMPublished
		if q, ok := r.ConsumedCapacity[devices.CapacityVRAM]; ok {
			vram = q.Value()
		}
		share := ""
		if r.ShareID != nil {
			share = string(*r.ShareID)
		}
		rec := state.Device{Request: r.Request, Pool: r.Pool, Device: r.Device, ShareID: share, VRAMBytes: vram}
		rec.CDIDeviceIDs = []string{p.CDI.QualifiedID(uid, edit(rec, pub))}
		recs = append(recs, rec)
	}
	if len(recs) == 0 {
		return nil, fmt.Errorf("claim %s/%s has no devices of driver %s", claim.Namespace, claim.Name, p.DriverName)
	}
	edits, err := editsFor(recs, inv)
	if err != nil {
		return nil, err
	}
	if err := p.CDI.WriteClaim(uid, edits); err != nil {
		return nil, err
	}
	if err := p.Store.Put(uid, recs); err != nil {
		_ = p.CDI.DeleteClaim(uid)
		return nil, err
	}
	return toKubelet(recs), nil
}

// Unprepare removes the claim's spec and record. Unknown claims are a no-op.
func (p *Preparer) Unprepare(uid string) error {
	if err := p.CDI.DeleteClaim(uid); err != nil {
		return err
	}
	return p.Store.Delete(uid)
}

// Allocated sums the VRAM of all prepared claims per device name.
func (p *Preparer) Allocated() map[string]int64 {
	out := map[string]int64{}
	for _, recs := range p.Store.All() {
		for _, r := range recs {
			out[r.Device] += r.VRAMBytes
		}
	}
	return out
}

// Count is the number of prepared claims.
func (p *Preparer) Count() int {
	return len(p.Store.All())
}

func edit(r state.Device, pub devices.Published) cdi.Edit {
	return cdi.Edit{Device: r.Device, ShareID: r.ShareID, RenderNode: pub.GPU.RenderNode,
		PCIAddress: pub.GPU.PCIAddress, Vendor: pub.Vendor, VRAMBytes: r.VRAMBytes}
}

func editsFor(recs []state.Device, inv map[string]devices.Published) ([]cdi.Edit, error) {
	edits := make([]cdi.Edit, 0, len(recs))
	for _, r := range recs {
		pub, ok := inv[r.Device]
		if !ok {
			return nil, fmt.Errorf("device %s is not available on this node", r.Device)
		}
		edits = append(edits, edit(r, pub))
	}
	return edits, nil
}

func toKubelet(recs []state.Device) []kubeletplugin.Device {
	out := make([]kubeletplugin.Device, 0, len(recs))
	for _, r := range recs {
		var share *types.UID
		if r.ShareID != "" {
			share = ptr.To(types.UID(r.ShareID))
		}
		out = append(out, kubeletplugin.Device{
			Requests:     []string{r.Request},
			PoolName:     r.Pool,
			DeviceName:   r.Device,
			CDIDeviceIDs: r.CDIDeviceIDs,
			ShareID:      share,
		})
	}
	return out
}
```

- [ ] **Step 4: Run tests**

Run: `go mod tidy && go test ./internal/prepare/ -race -v`
Expected: PASS (9 tests).

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/prepare
git commit -m "prepare: claims to CDI specs, restore after reboot, unprepare

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: Metrics (`metrics`)

**Files:**
- Create: `internal/metrics/metrics.go`, `internal/metrics/metrics_test.go`
- Modify: `docs/design.md` §11 (add `dri_gpus_skipped`)

**Interfaces:**
- Consumes: `devices.Published` (Task 4), `sysfs.GPU` (Task 2).
- Produces:
  - `func metrics.New() *metrics.Metrics`
  - `func (*metrics.Metrics) Registry() *prometheus.Registry`
  - `func (*metrics.Metrics) Update(pubs []devices.Published, allocated map[string]int64, used func(sysfs.GPU) (int64, bool), claims int)`
  - `func (*metrics.Metrics) SetSkipped(n int)`
  - fields `PrepareErrors, DiscoveryErrors prometheus.Counter`

- [ ] **Step 1: Add dependency**

Run: `go get github.com/prometheus/client_golang@v1.24.0`

- [ ] **Step 2: Write the failing test**

`internal/metrics/metrics_test.go`:

```go
package metrics_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ipsupport-llc/dra-driver-dri/internal/devices"
	"github.com/ipsupport-llc/dra-driver-dri/internal/metrics"
	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfs"
)

func TestUpdate(t *testing.T) {
	m := metrics.New()
	pubs := []devices.Published{{Name: "pci-0000-06-00-0", Vendor: "amd", VRAMTotal: 6442450944, VRAMPublished: 6174015488,
		GPU: sysfs.GPU{PCIAddress: "0000:06:00.0", KernelDriver: "amdgpu"}}}
	used := func(sysfs.GPU) (int64, bool) { return 1000, true }
	m.Update(pubs, map[string]int64{"pci-0000-06-00-0": 2147483648}, used, 1)
	m.SetSkipped(2)
	m.PrepareErrors.Inc()

	want := `
# HELP dri_claims_prepared Claims prepared on this node.
# TYPE dri_claims_prepared gauge
dri_claims_prepared 1
# HELP dri_gpus_skipped GPUs found but not published (unknown VRAM, below the reserve).
# TYPE dri_gpus_skipped gauge
dri_gpus_skipped 2
# HELP dri_prepare_errors_total Failed claim preparations.
# TYPE dri_prepare_errors_total counter
dri_prepare_errors_total 1
# HELP dri_vram_allocated_bytes VRAM promised to prepared claims.
# TYPE dri_vram_allocated_bytes gauge
dri_vram_allocated_bytes{device="pci-0000-06-00-0",pci_bus_id="0000:06:00.0",vendor="amd"} 2.147483648e+09
# HELP dri_vram_published_bytes VRAM offered to the scheduler.
# TYPE dri_vram_published_bytes gauge
dri_vram_published_bytes{device="pci-0000-06-00-0",pci_bus_id="0000:06:00.0",vendor="amd"} 6.174015488e+09
# HELP dri_vram_total_bytes VRAM reported by the GPU.
# TYPE dri_vram_total_bytes gauge
dri_vram_total_bytes{device="pci-0000-06-00-0",pci_bus_id="0000:06:00.0",vendor="amd"} 6.442450944e+09
# HELP dri_vram_used_bytes VRAM in use according to the kernel driver.
# TYPE dri_vram_used_bytes gauge
dri_vram_used_bytes{device="pci-0000-06-00-0",pci_bus_id="0000:06:00.0",vendor="amd"} 1000
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want),
		"dri_claims_prepared", "dri_gpus_skipped", "dri_prepare_errors_total",
		"dri_vram_allocated_bytes", "dri_vram_published_bytes", "dri_vram_total_bytes", "dri_vram_used_bytes"); err != nil {
		t.Fatal(err)
	}

	// A GPU that disappears drops out of the gauges.
	m.Update(nil, nil, used, 0)
	n, err := testutil.GatherAndCount(m.Registry(), "dri_vram_total_bytes")
	if err != nil || n != 0 {
		t.Fatalf("stale series: %d, %v", n, err)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/metrics/`
Expected: FAIL — `undefined: metrics.New`.

- [ ] **Step 4: Implement**

`internal/metrics/metrics.go`:

```go
// Package metrics exports per-GPU VRAM gauges and driver counters.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ipsupport-llc/dra-driver-dri/internal/devices"
	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfs"
)

var labels = []string{"device", "vendor", "pci_bus_id"}

// Metrics holds the driver's collectors in their own registry.
type Metrics struct {
	reg                               *prometheus.Registry
	total, published, allocated, used *prometheus.GaugeVec
	claims, skipped                   prometheus.Gauge
	PrepareErrors, DiscoveryErrors    prometheus.Counter
}

func gauge(name, help string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
}

// New creates and registers all collectors.
func New() *Metrics {
	m := &Metrics{
		reg:             prometheus.NewRegistry(),
		total:           gauge("dri_vram_total_bytes", "VRAM reported by the GPU."),
		published:       gauge("dri_vram_published_bytes", "VRAM offered to the scheduler."),
		allocated:       gauge("dri_vram_allocated_bytes", "VRAM promised to prepared claims."),
		used:            gauge("dri_vram_used_bytes", "VRAM in use according to the kernel driver."),
		claims:          prometheus.NewGauge(prometheus.GaugeOpts{Name: "dri_claims_prepared", Help: "Claims prepared on this node."}),
		skipped:         prometheus.NewGauge(prometheus.GaugeOpts{Name: "dri_gpus_skipped", Help: "GPUs found but not published (unknown VRAM, below the reserve)."}),
		PrepareErrors:   prometheus.NewCounter(prometheus.CounterOpts{Name: "dri_prepare_errors_total", Help: "Failed claim preparations."}),
		DiscoveryErrors: prometheus.NewCounter(prometheus.CounterOpts{Name: "dri_discovery_errors_total", Help: "Failed GPU scans."}),
	}
	m.reg.MustRegister(m.total, m.published, m.allocated, m.used, m.claims, m.skipped, m.PrepareErrors, m.DiscoveryErrors)
	return m
}

// Registry is served on the metrics endpoint.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// Update replaces the per-GPU series with the current state.
func (m *Metrics) Update(pubs []devices.Published, allocated map[string]int64, used func(sysfs.GPU) (int64, bool), claims int) {
	for _, g := range []*prometheus.GaugeVec{m.total, m.published, m.allocated, m.used} {
		g.Reset()
	}
	for _, p := range pubs {
		l := prometheus.Labels{"device": p.Name, "vendor": p.Vendor, "pci_bus_id": p.GPU.PCIAddress}
		m.total.With(l).Set(float64(p.VRAMTotal))
		m.published.With(l).Set(float64(p.VRAMPublished))
		m.allocated.With(l).Set(float64(allocated[p.Name]))
		if u, ok := used(p.GPU); ok {
			m.used.With(l).Set(float64(u))
		}
	}
	m.claims.Set(float64(claims))
}

// SetSkipped records how many GPUs the last scan left out.
func (m *Metrics) SetSkipped(n int) { m.skipped.Set(float64(n)) }
```

- [ ] **Step 5: Run test**

Run: `go mod tidy && go test ./internal/metrics/ -v`
Expected: PASS.

- [ ] **Step 6: Update the design (metrics list)**

In `docs/design.md` §11, replace

```
- `dri_claims_prepared`, `dri_prepare_errors_total`, `dri_discovery_errors_total`.
```

with

```
- `dri_claims_prepared`, `dri_gpus_skipped`, `dri_prepare_errors_total`,
  `dri_discovery_errors_total`.
```

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/metrics docs/design.md
git commit -m "metrics: per-GPU VRAM gauges and driver counters

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 9: Driver and flags (`cmd/dra-driver-dri`)

**Files:**
- Create: `cmd/dra-driver-dri/flags.go`, `cmd/dra-driver-dri/flags_test.go`, `cmd/dra-driver-dri/driver.go`, `cmd/dra-driver-dri/driver_test.go`

**Interfaces:**
- Consumes: everything from Tasks 2–8.
- Produces:
  - `type Flags struct { DriverName, NodeName, PodUID, Overrides, MetricsAddr, SysRoot, CDIRoot, RegistrarDir, PluginsDir string; ReserveBytes int64; RescanInterval time.Duration }`
  - `func parseFlags(args []string, getenv func(string) string) (Flags, error)`
  - `type publisher interface { PublishResources(context.Context, resourceslice.DriverResources) error }`
  - `func newDriver(cfg devices.Config, sysRoot string, det vram.Detector, m *metrics.Metrics, cancel context.CancelCauseFunc) *Driver`
  - `func (*Driver) Refresh(ctx context.Context) error`, `func (*Driver) Rescan(ctx context.Context, pub publisher) error`, `func (*Driver) Inventory() map[string]devices.Published`
  - DRAPlugin methods on `*Driver`.

- [ ] **Step 1: Write the failing tests**

`cmd/dra-driver-dri/flags_test.go`:

```go
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
		"no driver name":    {nil, map[string]string{"NODE_NAME": "node-a"}},
		"bad driver name":   {[]string{"--driver-name=Not_A_Domain"}, base},
		"no node name":      {nil, map[string]string{"DRIVER_NAME": "gpu.example.com"}},
		"bad reserve":       {[]string{"--vram-reserve=lots"}, base},
		"negative reserve":  {[]string{"--vram-reserve=-1Mi"}, base},
		"too-fast rescan":   {[]string{"--rescan-interval=100ms"}, base},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseFlags(c.args, env(c.env)); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}
```

`cmd/dra-driver-dri/driver_test.go`:

```go
package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/dynamic-resource-allocation/resourceslice"

	"github.com/ipsupport-llc/dra-driver-dri/internal/cdi"
	"github.com/ipsupport-llc/dra-driver-dri/internal/devices"
	"github.com/ipsupport-llc/dra-driver-dri/internal/metrics"
	"github.com/ipsupport-llc/dra-driver-dri/internal/prepare"
	"github.com/ipsupport-llc/dra-driver-dri/internal/state"
	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfstest"
	"github.com/ipsupport-llc/dra-driver-dri/internal/vram"
)

type fakePublisher struct {
	calls []resourceslice.DriverResources
	err   error
}

func (f *fakePublisher) PublishResources(_ context.Context, r resourceslice.DriverResources) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, r)
	return nil
}

func testDriver(t *testing.T) (*Driver, string, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "sys")
	dev := sysfstest.AddGPU(t, root, sysfstest.FakeGPU{RenderNode: "renderD128", PCIAddress: "0000:06:00.0", VendorID: "0x1002", DeviceID: "0x6798", Driver: "amdgpu",
		Files: map[string]string{"mem_info_vram_total": "6442450944\n", "mem_info_vram_used": "0\n"}})
	m := metrics.New()
	d := newDriver(devices.Config{NodeName: "node-a", ReserveBytes: 256 << 20}, root, vram.NewDetector(nil), m, func(error) {})
	w, err := cdi.NewWriter(filepath.Join(dir, "cdi"), "gpu.example.com")
	if err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(filepath.Join(dir, "checkpoint.json"))
	if err != nil {
		t.Fatal(err)
	}
	d.prep = &prepare.Preparer{DriverName: "gpu.example.com", Inventory: d.Inventory, CDI: w, Store: s}
	return d, root, dev
}

func TestRescanPublishesOnlyOnChange(t *testing.T) {
	d, _, dev := testDriver(t)
	pub := &fakePublisher{}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := d.Rescan(ctx, pub); err != nil {
			t.Fatal(err)
		}
	}
	if len(pub.calls) != 1 {
		t.Fatalf("publishes = %d, want 1", len(pub.calls))
	}
	sysfstest.SetFile(t, dev, "mem_info_vram_total", "8589934592\n")
	if err := d.Rescan(ctx, pub); err != nil {
		t.Fatal(err)
	}
	if len(pub.calls) != 2 {
		t.Fatalf("publishes = %d, want 2 after a VRAM change", len(pub.calls))
	}
}

func TestRescanRetriesFailedPublish(t *testing.T) {
	d, _, _ := testDriver(t)
	pub := &fakePublisher{err: errors.New("api down")}
	if err := d.Rescan(context.Background(), pub); err == nil {
		t.Fatal("want the publish error")
	}
	pub.err = nil
	if err := d.Rescan(context.Background(), pub); err != nil || len(pub.calls) != 1 {
		t.Fatalf("retry: %v, calls=%d", err, len(pub.calls))
	}
}

func TestRefreshFillsInventoryWithoutPublishing(t *testing.T) {
	d, _, _ := testDriver(t)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Inventory()["pci-0000-06-00-0"]; !ok {
		t.Fatalf("inventory = %v", d.Inventory())
	}
	pub := &fakePublisher{}
	if err := d.Rescan(context.Background(), pub); err != nil || len(pub.calls) != 1 {
		t.Fatalf("first Rescan after Refresh must publish: %v, calls=%d", err, len(pub.calls))
	}
}

func TestRescanCountsSkipped(t *testing.T) {
	d, root, _ := testDriver(t)
	sysfstest.AddGPU(t, root, sysfstest.FakeGPU{RenderNode: "renderD129", PCIAddress: "0000:03:00.0", VendorID: "0x8086", DeviceID: "0xe20b", Driver: "xe"})
	if err := d.Rescan(context.Background(), &fakePublisher{}); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(d.skippedGauge()); got != 1 {
		t.Fatalf("dri_gpus_skipped = %v", got)
	}
}

func TestPrepareResourceClaimsReportsErrors(t *testing.T) {
	d, _, _ := testDriver(t)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	bad := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "c", UID: "u1"}}
	res, err := d.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{bad})
	if err != nil {
		t.Fatal(err)
	}
	if res["u1"].Err == nil {
		t.Fatal("want a per-claim error")
	}
	if got := testutil.ToFloat64(d.metrics.PrepareErrors); got != 1 {
		t.Fatalf("prepare errors = %v", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/dra-driver-dri/`
Expected: FAIL — `undefined: parseFlags`, `undefined: newDriver`.

- [ ] **Step 3: Implement flags**

`cmd/dra-driver-dri/flags.go`:

```go
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
```

- [ ] **Step 4: Implement the driver**

`cmd/dra-driver-dri/driver.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	"github.com/ipsupport-llc/dra-driver-dri/internal/devices"
	"github.com/ipsupport-llc/dra-driver-dri/internal/metrics"
	"github.com/ipsupport-llc/dra-driver-dri/internal/prepare"
	"github.com/ipsupport-llc/dra-driver-dri/internal/sysfs"
	"github.com/ipsupport-llc/dra-driver-dri/internal/vram"
)

type publisher interface {
	PublishResources(ctx context.Context, resources resourceslice.DriverResources) error
}

// Driver is the kubelet plugin: it keeps the inventory current and
// prepares claims.
type Driver struct {
	cfg     devices.Config
	sysRoot string
	det     vram.Detector
	prep    *prepare.Preparer
	metrics *metrics.Metrics
	cancel  context.CancelCauseFunc

	mu        sync.RWMutex
	pubs      []devices.Published
	key       string
	published bool
}

func newDriver(cfg devices.Config, sysRoot string, det vram.Detector, m *metrics.Metrics, cancel context.CancelCauseFunc) *Driver {
	return &Driver{cfg: cfg, sysRoot: sysRoot, det: det, metrics: m, cancel: cancel}
}

func (d *Driver) scan() ([]devices.Published, []devices.Skipped, string, error) {
	gpus, err := sysfs.Scan(d.sysRoot)
	if err != nil {
		d.metrics.DiscoveryErrors.Inc()
		return nil, nil, "", fmt.Errorf("scan %s: %w", d.sysRoot, err)
	}
	pubs, skipped := devices.Build(d.cfg, gpus, d.det)
	var b strings.Builder
	for _, p := range pubs {
		fmt.Fprintf(&b, "%s|%s|%s|%s|%d;", p.Name, p.GPU.RenderNode, p.GPU.KernelDriver, p.GPU.DeviceID, p.VRAMPublished)
	}
	b.WriteString("#")
	for _, s := range skipped {
		fmt.Fprintf(&b, "%s|%v;", s.GPU.PCIAddress, s.Reason)
	}
	return pubs, skipped, b.String(), nil
}

// Refresh loads the inventory without publishing (before the kubelet
// plugin starts).
func (d *Driver) Refresh(_ context.Context) error {
	pubs, skipped, _, err := d.scan()
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.pubs = pubs
	d.mu.Unlock()
	d.metrics.SetSkipped(len(skipped))
	return nil
}

// Rescan refreshes the inventory and republishes it when it changed or was
// never published.
func (d *Driver) Rescan(ctx context.Context, pub publisher) error {
	logger := klog.FromContext(ctx)
	pubs, skipped, key, err := d.scan()
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.pubs = pubs
	changed := !d.published || key != d.key
	d.mu.Unlock()
	d.metrics.SetSkipped(len(skipped))
	if changed {
		for _, s := range skipped {
			logger.Info("GPU not published", "pciBusID", s.GPU.PCIAddress, "reason", s.Reason.Error())
		}
		logger.Info("Publishing GPUs", "count", len(pubs))
		if err := pub.PublishResources(ctx, devices.Resources(d.cfg, pubs)); err != nil {
			return fmt.Errorf("publish resources: %w", err)
		}
		d.mu.Lock()
		d.key, d.published = key, true
		d.mu.Unlock()
	}
	d.refreshMetrics()
	return nil
}

// Inventory returns the published devices by name.
func (d *Driver) Inventory() map[string]devices.Published {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make(map[string]devices.Published, len(d.pubs))
	for _, p := range d.pubs {
		out[p.Name] = p
	}
	return out
}

func (d *Driver) refreshMetrics() {
	pubs := slices.Collect(maps.Values(d.Inventory()))
	d.metrics.Update(pubs, d.prep.Allocated(), d.det.Used, d.prep.Count())
}

func (d *Driver) skippedGauge() prometheus.Collector { return d.metrics.SkippedGauge() }

// PrepareResourceClaims implements kubeletplugin.DRAPlugin.
func (d *Driver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	logger := klog.FromContext(ctx)
	out := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))
	for _, c := range claims {
		devs, err := d.prep.Prepare(c)
		if err != nil {
			d.metrics.PrepareErrors.Inc()
			logger.Error(err, "Prepare failed", "claim", klog.KObj(c))
			out[c.UID] = kubeletplugin.PrepareResult{Err: err}
			continue
		}
		out[c.UID] = kubeletplugin.PrepareResult{Devices: devs}
	}
	d.refreshMetrics()
	return out, nil
}

// UnprepareResourceClaims implements kubeletplugin.DRAPlugin.
func (d *Driver) UnprepareResourceClaims(_ context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	out := make(map[types.UID]error, len(claims))
	for _, c := range claims {
		out[c.UID] = d.prep.Unprepare(string(c.UID))
	}
	d.refreshMetrics()
	return out, nil
}

// HandleError implements kubeletplugin.DRAPlugin: unrecoverable background
// errors stop the process so the DaemonSet restarts it.
func (d *Driver) HandleError(ctx context.Context, err error, msg string) {
	utilruntime.HandleErrorWithContext(ctx, err, msg)
	if !errors.Is(err, kubeletplugin.ErrRecoverable) {
		d.cancel(fmt.Errorf("fatal background error: %s: %w", msg, err))
	}
}

// WatchHealthStatus implements kubeletplugin.DRAPlugin; device health is
// not reported in v0.1.
func (d *Driver) WatchHealthStatus(context.Context, chan<- kubeletplugin.DeviceHealthReport) error {
	return kubeletplugin.ErrHealthNotSupported
}
```

Add the accessor the test uses to `internal/metrics/metrics.go`:

```go
// SkippedGauge exposes the dri_gpus_skipped gauge for tests.
func (m *Metrics) SkippedGauge() prometheus.Gauge { return m.skipped }
```

- [ ] **Step 5: Run tests**

Run: `go mod tidy && go test ./... -race`
Expected: PASS (all packages).

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum cmd internal/metrics
git commit -m "driver: flags, inventory, publish-on-change, DRAPlugin methods

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 10: Process wiring (`main.go`)

**Files:**
- Modify: `cmd/dra-driver-dri/main.go` (replace the stub)

**Interfaces:**
- Consumes: `parseFlags`, `newDriver`, `Driver.Refresh/Rescan` (Task 9); `vram.LoadOverrides`, `vram.NewDetector` (Task 3); `cdi.NewWriter` (Task 5); `state.Open` (Task 6); `prepare.Preparer` (Task 7); `metrics.New` (Task 8).

- [ ] **Step 1: Implement**

`cmd/dra-driver-dri/main.go`:

```go
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
	d := newDriver(devices.Config{NodeName: f.NodeName, ReserveBytes: f.ReserveBytes}, f.SysRoot, vram.NewDetector(overrides), m, cancel)
	d.prep = &prepare.Preparer{DriverName: f.DriverName, Inventory: d.Inventory, CDI: writer, Store: store}
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
		}
	}
}
```

- [ ] **Step 2: Build and test**

Run: `go mod tidy && make test && make build && ./bin/dra-driver-dri --driver-name=gpu.example.com --node-name=x; echo "exit=$?"`
Expected: tests PASS; the binary exits 1 with `in-cluster config: unable to load in-cluster configuration` (not in a cluster), proving flag parsing, CDI dir creation (`/var/run/cdi` → run as a user without permission: expect `create CDI spec dir: permission denied` instead; either error is acceptable — both come after flag parsing).

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum cmd/dra-driver-dri/main.go
git commit -m "main: wire discovery, CDI, checkpoint, kubelet plugin, metrics

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 11: Reference manifests, examples, release workflow

**Files:**
- Create: `deploy/namespace.yaml`, `deploy/rbac.yaml`, `deploy/daemonset.yaml`, `deploy/deviceclass.yaml`, `deploy/examples/claim-template.yaml`, `deploy/examples/vulkan-pod.yaml`, `deploy/examples/alerts.yaml`, `.github/workflows/release.yml`
- Modify: `.github/workflows/ci.yml` (manifest validation job), `README.md` (usage)

- [ ] **Step 1: Manifests**

`deploy/namespace.yaml`:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: dra-driver-dri
```

`deploy/rbac.yaml` (driver name `gpu.example.com` — replace with your own):

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: dra-driver-dri
  namespace: dra-driver-dri
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: dra-driver-dri
rules:
  - apiGroups: ["resource.k8s.io"]
    resources: ["resourceclaims"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["resource.k8s.io"]
    resources: ["resourceclaims/status"]
    verbs: ["update"]
  - apiGroups: ["resource.k8s.io"]
    resources: ["resourceclaims/driver"]
    verbs: ["associated-node:update", "associated-node:patch"]
    resourceNames: ["gpu.example.com"]
  - apiGroups: ["resource.k8s.io"]
    resources: ["resourceslices"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: dra-driver-dri
subjects:
  - kind: ServiceAccount
    name: dra-driver-dri
    namespace: dra-driver-dri
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: dra-driver-dri
```

`deploy/daemonset.yaml` (runs on nodes labelled `gpu=dri`):

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: dra-driver-dri
  namespace: dra-driver-dri
spec:
  selector:
    matchLabels: {app.kubernetes.io/name: dra-driver-dri}
  updateStrategy:
    type: RollingUpdate
    rollingUpdate: {maxSurge: 1, maxUnavailable: 0}
  template:
    metadata:
      labels: {app.kubernetes.io/name: dra-driver-dri}
    spec:
      serviceAccountName: dra-driver-dri
      priorityClassName: system-node-critical
      nodeSelector: {gpu: dri}
      containers:
        - name: plugin
          image: ghcr.io/ipsupport-llc/dra-driver-dri:0.1.0
          env:
            - {name: DRIVER_NAME, value: gpu.example.com}
            - name: NODE_NAME
              valueFrom: {fieldRef: {fieldPath: spec.nodeName}}
            - name: POD_UID
              valueFrom: {fieldRef: {fieldPath: metadata.uid}}
          ports:
            - {name: metrics, containerPort: 8080}
          livenessProbe:
            httpGet: {path: /healthz, port: metrics}
            periodSeconds: 30
          resources:
            requests: {cpu: 10m, memory: 32Mi}
            limits: {memory: 128Mi}
          securityContext:
            runAsUser: 0
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: ["ALL"]}
          volumeMounts:
            - {name: plugins-registry, mountPath: /var/lib/kubelet/plugins_registry}
            - {name: plugins, mountPath: /var/lib/kubelet/plugins}
            - {name: cdi, mountPath: /var/run/cdi}
            - {name: sys, mountPath: /sys, readOnly: true}
      volumes:
        - name: plugins-registry
          hostPath: {path: /var/lib/kubelet/plugins_registry, type: Directory}
        - name: plugins
          hostPath: {path: /var/lib/kubelet/plugins, type: DirectoryOrCreate}
        - name: cdi
          hostPath: {path: /var/run/cdi, type: DirectoryOrCreate}
        - name: sys
          hostPath: {path: /sys, type: Directory}
```

`deploy/deviceclass.yaml`:

```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: gpu.example.com
spec:
  selectors:
    - cel:
        expression: device.driver == "gpu.example.com"
```

`deploy/examples/claim-template.yaml`:

```yaml
# 2 GiB of VRAM on any GPU of the class; add a selector to pin a vendor/model.
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: vram-2gi
spec:
  spec:
    devices:
      requests:
        - name: gpu
          exactly:
            deviceClassName: gpu.example.com
            capacity:
              requests:
                vram: 2Gi
```

`deploy/examples/vulkan-pod.yaml`:

```yaml
# Lists the Vulkan devices the pod sees: exactly one GPU is expected.
apiVersion: v1
kind: Pod
metadata:
  name: vulkan-list-devices
spec:
  restartPolicy: Never
  resourceClaims:
    - name: gpu
      resourceClaimTemplateName: vram-2gi
  containers:
    - name: llama
      image: ghcr.io/ggml-org/llama.cpp:server-vulkan
      args: ["--list-devices"]
      resources:
        claims:
          - name: gpu
```

`deploy/examples/alerts.yaml` (prometheus-operator `PrometheusRule`):

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: dra-driver-dri
spec:
  groups:
    - name: dra-driver-dri
      rules:
        - alert: DRIPrepareErrors
          expr: increase(dri_prepare_errors_total[15m]) > 0
          labels: {severity: warning}
          annotations:
            summary: "{{ $labels.instance }}: GPU claims fail to prepare"
        - alert: DRIVRAMOverUse
          expr: dri_vram_used_bytes > dri_vram_allocated_bytes and dri_vram_allocated_bytes > 0
          for: 10m
          labels: {severity: warning}
          annotations:
            summary: "{{ $labels.device }}: workloads use more VRAM than they claimed"
        - alert: DRIVRAMNearlyFull
          expr: dri_vram_used_bytes / dri_vram_total_bytes > 0.95
          for: 10m
          labels: {severity: warning}
          annotations:
            summary: "{{ $labels.device }}: VRAM over 95 % used"
        - alert: DRIGPUsSkipped
          expr: dri_gpus_skipped > 0
          for: 30m
          labels: {severity: info}
          annotations:
            summary: "{{ $labels.instance }}: GPUs found but not published (see plugin log)"
```

(The "plugin not running on a GPU node" alert depends on how the cluster labels nodes and is left to the deployment (Phase B).)

- [ ] **Step 2: Validate manifests in CI**

Append to `.github/workflows/ci.yml` under `jobs:`:

```yaml
  manifests:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v7
      - name: kubeconform
        run: |
          curl -sSL https://github.com/yannh/kubeconform/releases/download/v0.8.0/kubeconform-linux-amd64.tar.gz | tar xz kubeconform
          ./kubeconform -strict -summary -ignore-missing-schemas -kubernetes-version 1.36.0 deploy deploy/examples
```

Run locally the same command (download kubeconform into `bin/`).
Expected: `Summary: N resources found ... Invalid: 0, Errors: 0` (DeviceClass/ResourceClaimTemplate/PrometheusRule may be "skipped" for missing schemas).

- [ ] **Step 3: Release workflow**

`.github/workflows/release.yml`:

```yaml
name: release
on:
  push:
    tags: ["v*"]
permissions:
  contents: read
  packages: write
jobs:
  image:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v7
      - uses: docker/setup-buildx-action@v4
      - uses: docker/login-action@v4
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - id: meta
        uses: docker/metadata-action@v6
        with:
          images: ghcr.io/ipsupport-llc/dra-driver-dri
          tags: type=semver,pattern={{version}}
      - uses: docker/build-push-action@v7
        with:
          context: .
          push: true
          tags: ${{ steps.meta.outputs.tags }}
          labels: ${{ steps.meta.outputs.labels }}
          build-args: VERSION=${{ github.ref_name }}
```

- [ ] **Step 4: README usage section**

Append to `README.md`:

```markdown
## Usage

1. Label GPU nodes `gpu=dri`.
2. Pick a driver name you own (e.g. `gpu.example.com`) and set it in
   `deploy/rbac.yaml`, `deploy/daemonset.yaml` and `deploy/deviceclass.yaml`.
3. `kubectl apply -f deploy/`.
4. Request VRAM with a ResourceClaim(Template) — see `deploy/examples/`.

Inside the container: one `/dev/dri/renderD*`, plus `DRA_GPU_PCI_BUS_ID`,
`DRA_GPU_VENDOR`, `DRA_GPU_VRAM_BYTES`. VRAM is not enforced: size models
and contexts to `DRA_GPU_VRAM_BYTES`.
```

- [ ] **Step 5: Commit, push, mark the PR ready**

```bash
git add deploy .github README.md
git commit -m "Reference manifests, examples, alerts, release workflow

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
git push
gh pr ready
gh pr checks --watch
```

Expected: `test`, `lint`, `image`, `manifests` green. Merge per the operator's rule (self-merge when all CI is green): `gh pr merge --squash --delete-branch`, then confirm the state is `MERGED` before going on.

- [ ] **Step 6: Tag v0.1.0 and check the image is public**

```bash
git checkout main && git pull --ff-only
git tag -a v0.1.0 -m "v0.1.0" && git push origin v0.1.0
gh run watch "$(gh run list --workflow release --limit 1 --json databaseId -q '.[0].databaseId')" --exit-status
docker logout ghcr.io >/dev/null 2>&1; docker pull ghcr.io/ipsupport-llc/dra-driver-dri:0.1.0
```

Expected: the pull succeeds without login. If it is denied, the package was created private: ask the operator to set it public (GitHub → ipsupport-llc → Packages → dra-driver-dri → Package settings → Change visibility → Public), then pull again.

---

## Phase B — deployment in the operator's cluster

Tracked in the operator's private infrastructure repositories (platform
engine module installing the DaemonSet, RBAC, DeviceClass, PodMonitor and
alerts; node labelling; rollout; hardware scenarios from spec §13). Results
of the hardware test go to `docs/e2e.md` in this repository, without host
names or addresses.

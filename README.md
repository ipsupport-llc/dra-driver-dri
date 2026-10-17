# dra-driver-dri — share a GPU between Kubernetes pods by VRAM

**dra-driver-dri** is a Kubernetes [Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
driver for GPUs that expose a Linux DRM render node (`/dev/dri/renderD*`).
Pods ask for an amount of **video memory** — `vram: 2Gi` — instead of a whole
GPU, and the Kubernetes scheduler places them on a node and a GPU that still
has that much room. Several pods share one GPU; each gets exactly one render
node through [CDI](https://github.com/cncf-tags/container-device-interface),
with no privileged containers and no `hostPath`.

It is built for **inference on Vulkan**: [llama.cpp](https://github.com/ggml-org/llama.cpp),
[whisper.cpp](https://github.com/ggml-org/whisper.cpp) and other GGML/Vulkan
workloads on GPUs that the vendor stacks do not cover — including older AMD
cards without ROCm support (GCN 1.0 "Tahiti", e.g. the FirePro D700 in the
2013 Mac Pro).

## Why

- **Device plugins hand out whole GPUs.** A 6 GiB card running a 1.5 GiB
  speech model is mostly idle memory, and you cannot ask Kubernetes for
  "2 GiB of some GPU".
- **Vendor DRA drivers need the vendor compute stack.** AMD's ROCm DRA driver
  finds GPUs through KFD and shares them by hardware partitions of datacenter
  GPUs; consumer and older GPUs that run fine on Vulkan are invisible to it.
- **Kubernetes now has the missing piece.** DRA *consumable capacity*
  (`DRAConsumableCapacity`, beta and on by default since Kubernetes 1.36) lets
  one device be allocated to many claims, each consuming part of a named
  capacity. This driver publishes every GPU with a `vram` capacity and lets the
  scheduler do the bookkeeping.

## Features

- **Fractional GPU by VRAM:** many pods per GPU, packed by the memory they
  claim; a claim without an amount gets the whole GPU.
- **Placement by node and GPU:** the scheduler picks both, across all GPU
  nodes.
- **One render node per pod via CDI:** the container sees only its GPU; no
  privileges, no host mounts in the workload.
- **Vendor-neutral device model:** any DRM render-node GPU; VRAM is read per
  kernel driver (`amdgpu` today), or set per PCI address in an override file.
- **Survives restarts and reboots:** prepared claims are checkpointed;
  after a reboot the CDI specs are rebuilt against the current render-node
  numbers. DaemonSet rolling updates do not disturb running pods.
- **Prometheus metrics and alerts:** VRAM published, claimed and actually used
  per GPU; alerts for over-use and failed preparation.

## How it works

```
 pod ── ResourceClaim (vram: 2Gi) ──► kube-scheduler ──► node + GPU with ≥ 2 GiB free
                                           ▲
                         ResourceSlice     │  (one device per render node,
                                           │   capacity vram, multi-allocation)
 node: dra-driver-dri kubelet plugin ──────┘
        ├─ scans /sys/class/drm/renderD*   (vendor, device, VRAM)
        ├─ NodePrepareResources → CDI spec: /dev/dri/renderDNNN + DRA_GPU_* env
        └─ /metrics
```

Only a kubelet plugin, run as a DaemonSet on GPU nodes: no controller, no
CRDs, no scheduler extender.

## Requirements

- Kubernetes ≥ 1.36 (DRA with consumable capacity on by default); tested on
  k3s v1.37.
- containerd 2.x (CDI enabled by default) or another CDI-capable runtime.
- Linux GPU nodes with a DRM render node per GPU. VRAM detection built in for
  `amdgpu`; other drivers via the override file.
- For Vulkan workloads: a Vulkan driver in the workload image (e.g. Mesa RADV
  for AMD).

## Installation

### 1. Node prerequisites

On every GPU node:

- **The GPU's kernel driver is loaded and exposes a render node.** Check:
  `ls -l /dev/dri/` shows `renderD128`, `renderD129`, …; for AMD,
  `cat /sys/class/drm/renderD128/device/mem_info_vram_total` prints the VRAM
  size in bytes.
- **The container runtime has CDI enabled.**
  - containerd 2.x (and k3s/RKE2 releases that bundle it): enabled by
    default, spec dirs `/etc/cdi` and `/var/run/cdi`. Nothing to do.
  - containerd 1.7: in the CRI plugin section of `config.toml` set
    `enable_cdi = true` and `cdi_spec_dirs = ["/etc/cdi", "/var/run/cdi"]`,
    then restart containerd.
  - CRI-O: enabled by default.
- **Nothing GPU-specific in the node's userspace.** Vulkan drivers (e.g. Mesa
  RADV) belong in the workload image, not on the node.

Label the GPU nodes:

```sh
kubectl label node <gpu-node> gpu=dri
```

### 2. Cluster prerequisites

- **Kubernetes 1.36 or newer:** DRA and consumable capacity are on by
  default; nothing to enable.
- **Kubernetes 1.34–1.35:** enable the `DRAConsumableCapacity` feature gate on
  the API server, scheduler, controller manager and kubelet.
- The node-scoping policy (`deploy/validatingadmissionpolicy.yaml`) relies on
  ServiceAccount tokens that carry the node name (on by default in current
  Kubernetes). On a cluster without it, leave that file out.

### 3. k3s

k3s runs the API server, scheduler and kubelet in one binary, so everything
above is set in `/etc/rancher/k3s/config.yaml` (or as `k3s server` /
`k3s agent` flags):

```yaml
# GPU agent node: label at registration (also re-applied on agent restart)
node-label:
  - gpu=dri
```

```yaml
# Only for Kubernetes 1.34–1.35, on the servers (and kubelet-arg on agents too)
kube-apiserver-arg:
  - feature-gates=DRAConsumableCapacity=true
kube-scheduler-arg:
  - feature-gates=DRAConsumableCapacity=true
kube-controller-manager-arg:
  - feature-gates=DRAConsumableCapacity=true
kubelet-arg:
  - feature-gates=DRAConsumableCapacity=true
```

The k3s kubelet uses the standard `/var/lib/kubelet`, and current k3s bundles
containerd 2.x, so the reference manifests work unchanged.

### 4. Other distributions

If the kubelet root is not `/var/lib/kubelet` (MicroK8s, for example, uses
`/var/snap/microk8s/common/var/lib/kubelet`), point both the hostPath volumes
in `deploy/daemonset.yaml` and the plugin at it:
`KUBELET_PLUGINS_DIRECTORY_PATH=<root>/plugins`,
`KUBELET_REGISTRAR_DIRECTORY_PATH=<root>/plugins_registry`.

### 5. Install the driver

Pick a driver name in a domain you own (e.g. `gpu.example.com`). Set it in
`deploy/daemonset.yaml` (`DRIVER_NAME`) and `deploy/deviceclass.yaml`, and the
plugin's namespace/ServiceAccount in `deploy/validatingadmissionpolicy.yaml`
if you change them. Then:

```sh
kubectl apply -f deploy/namespace.yaml
kubectl apply -f deploy/
```

The image is `ghcr.io/ipsupport-llc/dra-driver-dri` (public).

### 6. Check it

```sh
kubectl -n dra-driver-dri get pods -o wide          # one plugin pod per GPU node
kubectl -n dra-driver-dri logs ds/dra-driver-dri    # "Publishing GPUs" count=N
kubectl get resourceslices                         # one slice per GPU node
kubectl get resourceslice -o yaml | grep -A3 vram  # VRAM offered per GPU
```

Then run the pod from [`deploy/examples/vulkan-pod.yaml`](deploy/examples/vulkan-pod.yaml)
(with its claim template): its log lists exactly one Vulkan device.

### Claim VRAM from a pod

```yaml
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
---
apiVersion: v1
kind: Pod
metadata:
  name: llama
spec:
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

More in [`deploy/examples/`](deploy/examples/) and the recipes below.

## Placement recipes

The driver name below is `gpu.example.com`; the DeviceClass has the same
name. The driver's own attributes are read in CEL as
`device.attributes["gpu.example.com"].<name>` (`vendor`, `kernelDriver`,
`deviceID`, `renderNode`); the PCI address is the standard
`device.attributes["resource.kubernetes.io"].pciBusID`.

### Several small models per GPU (share by VRAM)

Ask for the VRAM the model needs — weights, KV cache and compute buffers.
Every pod of a Deployment gets its own claim from the template, and the
scheduler packs them onto GPUs until the VRAM runs out:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: whisper-1500mi
spec:
  spec:
    devices:
      requests:
        - name: gpu
          exactly:
            deviceClassName: gpu.example.com
            capacity:
              requests:
                vram: 1500Mi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: whisper
spec:
  replicas: 6
  selector:
    matchLabels: {app: whisper}
  template:
    metadata:
      labels: {app: whisper}
    spec:
      resourceClaims:
        - name: gpu
          resourceClaimTemplateName: whisper-1500mi   # one claim per pod
      containers:
        - name: whisper
          image: <your whisper.cpp Vulkan image>
          resources:
            claims:
              - name: gpu
```

### One model per GPU (spread over GPUs)

Placement is first-fit, so shared claims fill one GPU before the next. To
give each replica a GPU of its own, claim **whole GPUs** — no `vram` amount.
A whole-GPU claim cannot share, so N replicas land on N different GPUs:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: whole-gpu
spec:
  spec:
    devices:
      requests:
        - name: gpu
          exactly:
            deviceClassName: gpu.example.com   # no capacity → the whole GPU
```

### Only some GPUs (vendor, model)

```yaml
          exactly:
            deviceClassName: gpu.example.com
            selectors:
              - cel:
                  expression: >-
                    device.attributes["gpu.example.com"].vendor == "amd" &&
                    device.attributes["gpu.example.com"].deviceID == "0x6798"
            capacity:
              requests:
                vram: 2Gi
```

### One specific GPU

```yaml
            selectors:
              - cel:
                  expression: device.attributes["resource.kubernetes.io"].pciBusID == "0000:02:00.0"
```

Combine with a `nodeSelector` on the pod when several nodes have a GPU at that
address.

### One model on two GPUs (split across GPUs)

Both devices of one claim are always on the same node:

```yaml
          exactly:
            deviceClassName: gpu.example.com
            allocationMode: ExactCount
            count: 2                          # two whole GPUs
```

### Replicas on different nodes

GPU placement and node spreading combine: add a standard topology spread (or
pod anti-affinity) on `kubernetes.io/hostname`:

```yaml
    spec:
      topologySpreadConstraints:
        - maxSkew: 1
          topologyKey: kubernetes.io/hostname
          whenUnsatisfiable: ScheduleAnyway
          labelSelector:
            matchLabels: {app: whisper}
```

### Do not share one claim between replicas

`resourceClaimTemplateName` gives every pod its own claim. A pre-created
`ResourceClaim` referenced by `resourceClaimName` is **one** allocation: every
pod that references it lands on the same GPU and shares that one VRAM amount.

## Inside the container

| Variable | Meaning |
|---|---|
| `DRA_GPU_PCI_BUS_ID` | PCI address of the GPU, e.g. `0000:02:00.0` |
| `DRA_GPU_VENDOR` | `amd`, `intel`, `nvidia` or `unknown` |
| `DRA_GPU_VRAM_BYTES` | VRAM granted to this claim — size models and contexts to it |
| `DRA_GPU_<DEVICE>_*` | the same per GPU (e.g. `DRA_GPU_PCI_0000_02_00_0_VRAM_BYTES`) for containers with several GPUs |

## Configuration

| Flag / env | Default | Meaning |
|---|---|---|
| `--driver-name` / `DRIVER_NAME` | — | DRA driver name and DeviceClass name |
| `--vram-reserve` / `VRAM_RESERVE` | `256Mi` | VRAM held back per GPU for the kernel driver |
| `--rescan-interval` / `RESCAN_INTERVAL` | `30s` | how often GPUs are rescanned |
| `--overrides` / `OVERRIDES` | — | YAML of VRAM per PCI address, for GPUs without a backend |
| `--metrics-addr` / `METRICS_ADDR` | `:8080` | Prometheus endpoint |

Requests are rounded up to 64 MiB; published VRAM is total minus the reserve,
rounded down to 64 MiB.

## Metrics

Per GPU (labels `device`, `vendor`, `pci_bus_id`), refreshed on every rescan:

| Metric | Meaning |
|---|---|
| `dri_vram_total_bytes` | VRAM the GPU reports |
| `dri_vram_published_bytes` | VRAM offered to the scheduler (total minus the reserve) |
| `dri_vram_allocated_bytes` | VRAM promised to prepared claims |
| `dri_vram_used_bytes` | VRAM in use, from the kernel driver |
| `dri_gtt_used_bytes` | system RAM mapped for the GPU (GTT); grows when VRAM is full and buffers are evicted to RAM |
| `dri_gpu_temperature_celsius` | GPU temperature (hwmon) |
| `dri_gpu_core_clock_hertz`, `dri_gpu_memory_clock_hertz` | current core and memory clocks (hwmon) |

Per node: `dri_claims_prepared`, `dri_gpus_skipped`,
`dri_prepare_errors_total`, `dri_discovery_errors_total`. A reading the
backend cannot take is left out rather than reported as zero.

Ready-made monitoring in [`deploy/examples/`](deploy/examples/):

- [`podmonitor.yaml`](deploy/examples/podmonitor.yaml): Prometheus Operator
  scrape config that adds a `node` label (device names repeat across
  identical machines);
- [`alerts.yaml`](deploy/examples/alerts.yaml): prepare errors, a workload
  using more VRAM than it claimed, VRAM nearly full, VRAM evicted to RAM,
  skipped GPUs;
- [`grafana-dashboard.json`](deploy/examples/grafana-dashboard.json): per
  node and GPU: claimed, used and offered VRAM, eviction to RAM,
  temperature, clocks.

**Why GTT matters:** a VRAM claim is not enforced. When workloads together
use more VRAM than the GPU has, the kernel moves buffers to system RAM and
everything on that GPU slows down. On a FirePro D700, four llama.cpp
instances that need ~2 GiB each on one 6 GiB GPU pushed 3.4 GiB into GTT and
cut total generation speed from 41 to 16 tokens/s; a single instance that
fits keeps ~0.3 GiB in GTT.

## Comparison

| | Device plugin | Vendor DRA driver (ROCm) | Time-slicing / vGPU tools | **dra-driver-dri** |
|---|---|---|---|---|
| Unit of allocation | whole GPU | whole GPU or hardware partition | whole GPU, oversubscribed | **VRAM amount** |
| Several pods per GPU | no | partitions only (datacenter GPUs) | yes, without memory accounting | **yes, accounted by VRAM** |
| Needs vendor compute stack | depends | yes (ROCm/KFD) | usually | **no — any DRM render node** |
| Older / consumer GPUs (Vulkan) | yes | no | depends | **yes** |
| Scheduler knows free memory | no | partially | no | **yes** |

## Limitations

- **VRAM is not enforced.** A claim is a scheduling agreement; the kernel does
  not cap a process. Size workloads to `DRA_GPU_VRAM_BYTES` and watch the
  over-use alert.
- **Placement is first-fit.** Kubernetes fills one GPU before the next; when
  you run at most one model per GPU, claim whole GPUs (no `vram` amount).
- **Compute is time-sliced** between pods on one GPU (generation roughly halves
  for each of two busy pods).
- VRAM detection is built in for `amdgpu` only so far; Intel (`xe`, `i915`)
  is planned.

## Performance on FirePro D700

llama.cpp (Vulkan, Mesa RADV) on one D700, tokens/s:

| Model | Prompt (pp512) | Generation |
|---|---|---|
| LFM2.5-1.2B, Q4_K_M | 865 | 84 |
| LFM2.5-2.6B, Q4_K_M | 386 | 43 |

Full numbers and GPU-sharing measurements: [docs/benchmarks/firepro-d700-llama-cpp.md](docs/benchmarks/firepro-d700-llama-cpp.md),
[docs/e2e.md](docs/e2e.md).

## Documentation

- [Design](docs/design.md)
- [Hardware test](docs/e2e.md)
- [Benchmarks on FirePro D700](docs/benchmarks/firepro-d700-llama-cpp.md)

## Building

```sh
make test     # go vet + unit tests (race detector)
make build    # bin/dra-driver-dri
make image    # container image
```

## License

Apache-2.0. Parts of the CDI and plugin layout follow
[kubernetes-sigs/dra-example-driver](https://github.com/kubernetes-sigs/dra-example-driver)
(Apache-2.0).

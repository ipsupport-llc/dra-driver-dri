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

## Quick start

1. Label your GPU nodes:

   ```sh
   kubectl label node <gpu-node> gpu=dri
   ```

2. Pick a driver name in a domain you own (e.g. `gpu.example.com`), set it in
   `deploy/daemonset.yaml` and `deploy/deviceclass.yaml`, then install:

   ```sh
   kubectl apply -f deploy/namespace.yaml
   kubectl apply -f deploy/
   ```

3. Check the published GPUs:

   ```sh
   kubectl get resourceslices
   ```

4. Claim VRAM from a pod:

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

   More in [`deploy/examples/`](deploy/examples/).

The image is `ghcr.io/ipsupport-llc/dra-driver-dri` (public).

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

`dri_vram_total_bytes`, `dri_vram_published_bytes`, `dri_vram_allocated_bytes`,
`dri_vram_used_bytes` (per GPU), `dri_claims_prepared`, `dri_gpus_skipped`,
`dri_prepare_errors_total`, `dri_discovery_errors_total`. Example alerts:
[`deploy/examples/alerts.yaml`](deploy/examples/alerts.yaml).

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

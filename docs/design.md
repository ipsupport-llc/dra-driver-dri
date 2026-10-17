# dra-driver-dri — design

Status: **draft for review** (2026-10-16).

## 1. Goal

Let Kubernetes place GPU workloads by **video memory**: several pods share one
GPU, each with the amount of VRAM it asked for, and the scheduler picks both
the node and the GPU that still has room. The first users are small
inference servers (whisper.cpp, llama.cpp with the Vulkan backend) on older
AMD GPUs (FirePro D700, GCN 1.0 "Tahiti", 6 GiB each, two per node).

The driver uses Dynamic Resource Allocation (DRA) with **consumable
capacity** (`DRAConsumableCapacity`, beta and on by default since
Kubernetes 1.36). It hands a pod exactly one DRM render node
(`/dev/dri/renderD*`) through CDI, so the workload needs neither privileges
nor `hostPath`.

## 2. Scope

In scope:

- Any GPU that exposes a DRM render node. The device model is
  vendor-neutral; VRAM detection is per kernel driver ("backend").
- Backend `amdgpu` in the first version.
- VRAM as the only consumable capacity.
- Linux nodes with containerd 2.x (CDI enabled by default) and Kubernetes
  ≥ 1.36.

Out of scope (non-goals):

- **Enforcing** VRAM limits. A claim is a scheduling agreement; the kernel
  does not cap a process's VRAM. The driver reports over-use (section 9).
- Compute shares, time-slicing, MIG-like partitioning.
- Display/KMS access (`/dev/dri/card*`), video encode/decode as separate
  resources.
- ROCm/KFD devices. Vulkan (Mesa RADV, ANV) is the target API.

Backends for Intel (`xe`, `i915`) come later and only add a VRAM reader.

## 3. Prior art

| Option | Why not |
|---|---|
| AMD's ROCm DRA driver (`ROCm/k8s-gpu-dra-driver`) | Discovers GPUs through the KFD topology; GCN 1.0 GPUs have no KFD node, so it sees nothing. Its sharing model is hardware partitions (compute/memory modes of datacenter GPUs), not VRAM amounts. |
| Device plugin (`squat/generic-device-plugin` and similar) | Whole devices only; a pod cannot ask for "2 GiB of some GPU". |
| Static partitions (`DRAPartitionableDevices`, e.g. 1/2/4 GiB slices) | Fixed slice sizes waste VRAM and must be tuned to the models in advance. |
| `kubernetes-sigs/dra-example-driver` | Not a driver for real hardware, but it already demonstrates consumable capacity, checkpointing and CDI. **This project starts from it.** |

## 4. Architecture

One binary: the **kubelet plugin**, run as a DaemonSet on GPU nodes. There
is no controller and no CRD; the scheduler allocates devices from the
published `ResourceSlice`s.

```
                      kube-scheduler
                (allocates claim → node + GPU,
                 subtracts the requested vram)
                           ▲
            ResourceSlice  │                 ResourceClaim
                           │
 node ┌────────────────────┴──────────────┐
      │ dra-driver-dri kubelet plugin     │
      │  discovery ─ backends (amdgpu,…)  │──► /var/run/cdi/<claim>.json
      │  prepare/unprepare, checkpoint    │
      │  /metrics                         │
      └───────────────────────────────────┘
          ▲ sysfs (/sys/class/drm)          kubelet ─► containerd ─► container
                                                        (one /dev/dri/renderD*)
```

## 5. Device model

One device per render node, in the `ResourceSlice` of its node.

- **Name:** derived from the PCI address, stable across reboots
  (`pci-0000-06-00-0`). Render node numbers can change; PCI addresses do not.
- **Attributes** (qualified with the driver name):
  - `vendor` (string): `amd`, `intel`, … from the PCI vendor ID;
  - `kernelDriver` (string): e.g. `amdgpu`;
  - `deviceID` (string): PCI device ID, e.g. `0x6798`;
  - `renderNode` (string): e.g. `renderD128` (informational only);
  - standard `resource.kubernetes.io/pciBusID`.
- **Capacity `vram`:** total VRAM minus a reserve (flag `--vram-reserve`,
  default `256Mi`) for the kernel driver and firmware.
  - `allowMultipleAllocations: true`.
  - Request policy: `default` = the whole published amount; `validRange`
    `min` = `step` = `64Mi`, `max` = the published amount. Requests round up
    to a multiple of 64 MiB, and the published amount is rounded **down** to
    one. A claim that does not name an amount gets **the whole GPU**;
    sharing happens only when a claim asks for a specific `vram`.
- A GPU whose VRAM cannot be determined is **not published** (no device with
  zero or unknown capacity).

## 6. Request flow

1. A pod references a ResourceClaim (usually from a ResourceClaimTemplate)
   asking the driver's DeviceClass for, e.g., `capacity.requests.vram: 2Gi`.
   Selectors on the attributes narrow it down (vendor, device ID).
2. The scheduler finds a node with a device that still has ≥ 2 GiB of
   unallocated `vram` and allocates it.
3. The kubelet calls the plugin's `NodePrepareResources`. The plugin writes
   a CDI spec for the claim and records it in its checkpoint.
4. containerd applies the CDI edits: the container gets the one render node
   and the environment variables below. Inside, Vulkan sees exactly one GPU,
   so applications use device 0.
5. When the pod goes away, `NodeUnprepareResources` deletes the CDI spec;
   the scheduler returns the capacity.

## 7. CDI spec

Per prepared claim, one transient spec in `/var/run/cdi` named by the CDI
library from the driver name and the claim UID, kind `k8s.<driver-name>/gpu`.
Device names are `<claim-uid>-<device>[-<share-id>]`:

- `deviceNodes`: `path: /dev/dri/renderDNNN`, `permissions: rw`,
  `fileMode: 0666`. The render group's GID differs between hosts, so the
  node is made accessible inside the container instead of relying on
  supplemental groups. The container sees no other GPU.
- `env`:
  - `DRA_GPU_PCI_BUS_ID` (e.g. `0000:06:00.0`),
  - `DRA_GPU_VENDOR` (e.g. `amd`),
  - `DRA_GPU_VRAM_BYTES` (the allocated amount; workloads can size their
    context/batch from it);
  - the same three as `DRA_GPU_<DEVICE>_*` (device name upper-cased, `-` →
    `_`), so a container with several GPUs sees each one; the unsuffixed set
    then names one of them.

## 8. Discovery and backends

- Every 30 s (and at start) the plugin walks `/sys/class/drm/renderD*`,
  resolves each to its PCI device, reads vendor/device IDs and the bound
  kernel driver, and asks the matching backend for VRAM.
- Backend interface: `Total(dev) (bytes, error)` and `Stats(dev)` (the
  live readings it can take: VRAM and GTT in use, temperature, clocks).
  - `amdgpu`: `mem_info_vram_total`, `mem_info_vram_used`,
    `mem_info_gtt_used` in the PCI device's sysfs directory; `temp1_input`
    and the `freqN_input` labelled `sclk` / `mclk` in its `hwmon` directory.
- **Override:** a config file may set VRAM per PCI address for GPUs without
  a backend. A GPU with neither a backend nor an override is skipped and
  logged.
- The slice is republished only when the device set or a capacity changes.

## 9. Configuration

Flags (or environment variables):

| Flag | Default | Meaning |
|---|---|---|
| `--driver-name` | (required) | DRA driver name, a DNS subdomain owned by the operator, e.g. `gpu.example.com` |
| `--node-name` | `$NODE_NAME` | node the plugin runs on |
| `--vram-reserve` | `256Mi` | subtracted from each GPU's VRAM |
| `--rescan-interval` | `30s` | sysfs rescan period |
| `--overrides` | (none) | path to the per-PCI-address VRAM override file |
| `--metrics-addr` | `:8080` | Prometheus endpoint |

## 10. Deployment

The repository ships the image and reference manifests (`deploy/`); a
cluster's own infrastructure code deploys it.

- **DaemonSet** on labelled GPU nodes. Host mounts:
  `/var/lib/kubelet/plugins`, `/var/lib/kubelet/plugins_registry`,
  `/var/run/cdi`, `/sys` (read-only). No privileges beyond what the plugin
  registration and CDI directory require.
- **RBAC:** read `resourceclaims` (the plugin never writes claim status),
  read `nodes`, write `resourceslices`; a ValidatingAdmissionPolicy limits
  slice writes to the node in the plugin pod's ServiceAccount token.
- **DeviceClass** selecting `device.driver == "<driver-name>"`.
- **Image:** `ghcr.io/ipsupport-llc/dra-driver-dri`, public, built by GitHub
  Actions on each PR (lint, tests, build) and pushed on tags.
- **Upgrades:** prepared claims survive a plugin restart through the
  checkpoint and the CDI files on disk; running pods are not touched.

## 11. Observability

Prometheus metrics, per GPU (`device`, `vendor`, `pci_bus_id` labels):

- `dri_vram_total_bytes`, `dri_vram_published_bytes`,
  `dri_vram_allocated_bytes` (sum of prepared claims),
  `dri_vram_used_bytes` (backend reading);
- `dri_gtt_used_bytes` (system RAM mapped for the GPU; grows when VRAM is
  evicted), `dri_gpu_temperature_celsius`, `dri_gpu_core_clock_hertz`,
  `dri_gpu_memory_clock_hertz` (hwmon); a backend reports what it can read,
  the rest is left out;
- `dri_claims_prepared`, `dri_gpus_skipped`, `dri_prepare_errors_total`,
  `dri_discovery_errors_total`.

Suggested alerts (shipped as examples):

- plugin not running on a labelled GPU node;
- prepare errors increasing;
- `used > allocated + reserve` for 10 minutes (a workload uses more than it
  claimed; the reserve covers kernel and firmware use);
- `used > 95 %` of total;
- VRAM evicted to RAM: GTT above 1 GiB while VRAM is over 90 % used.

## 12. Failure handling

| Event | Behaviour |
|---|---|
| Plugin restarts | Restores prepared claims from the checkpoint; republishes the slice; running pods are unaffected. |
| Checkpoint unreadable (corrupt, unknown version) | The plugin refuses to start (crash loop, alert) instead of forgetting prepared claims. Recovery: inspect `<plugins dir>/<driver-name>/checkpoint.json`; deleting it is safe only when no pod on the node uses a claim of this driver. |
| GPU disappears (driver failure, reset, unplug) | Next scan drops it from the slice; no new allocations. Pods on it fail on their own and are recreated elsewhere by their controller. |
| VRAM unreadable | GPU not published; error logged and counted. |
| CDI directory not writable | Prepare fails with a clear error; the pod stays in `ContainerCreating` with an event. |

## 13. Testing

- **Unit:** discovery against fake sysfs trees (two AMD GPUs, unknown vendor,
  missing VRAM file, override file), CDI spec generation, reserve and
  request-policy arithmetic.
- **CI:** `golangci-lint`, unit tests, image build on each PR.
- **On real hardware** (a node with two D700s):
  1. a pod sees exactly one GPU (`vulkaninfo --summary`);
  2. llama.cpp and whisper.cpp run inside such pods;
  3. several 2 GiB claims fill both GPUs without over-allocation; one more
     stays `Pending` with a scheduler message naming the missing capacity;
  4. deleting a pod frees its capacity and the pending pod starts;
  5. restarting the plugin does not disturb running pods.

## 14. Risks and open items

- **Consumable capacity is beta** (stable targeted for 1.38). Small API
  changes between minor versions are possible; pin the `k8s.io/*`
  libraries to the cluster's version.
- **No VRAM enforcement** (section 2). Mitigated by metrics/alerts and by
  sizing workloads from `DRA_GPU_VRAM_BYTES`.
- **Consumers' tooling:** Terraform's `kubernetes` provider may not model
  `resourceClaims` in pod specs; consumers might need `kubernetes_manifest`
  or Helm. To check before the first integration.
- Intel backends (`xe`, `i915`): where their sysfs exposes VRAM totals is to
  be verified when they are added.

## 15. Decisions

- Own driver with consumable `vram` capacity, starting from
  `dra-example-driver` (chosen over static partitions and device plugins).
- Vendor-neutral device model with per-driver backends; only `amdgpu` now.
- No claim amount → whole GPU (sharing only on explicit request).
- Public repository and public image; CI on GitHub-hosted runners.

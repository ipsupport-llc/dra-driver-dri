# dra-driver-dri

Kubernetes [Dynamic Resource Allocation](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
driver for GPUs that expose a DRM render node (`/dev/dri/renderD*`).

Pods ask for an amount of video memory (`vram: 2Gi`); the scheduler places
them on a node and a GPU that still has that much room, so several pods share
one GPU. Each pod gets exactly one render node through CDI — no privileges,
no `hostPath`. Built on DRA consumable capacity (Kubernetes ≥ 1.36).

Status: design phase — see [docs/design.md](docs/design.md).

License: Apache-2.0.

## Usage

1. Label GPU nodes `gpu=dri`.
2. Pick a driver name you own (e.g. `gpu.example.com`) and set it in
   `deploy/rbac.yaml`, `deploy/daemonset.yaml` and `deploy/deviceclass.yaml`.
3. `kubectl apply -f deploy/`.
4. Request VRAM with a ResourceClaim(Template) — see `deploy/examples/`.

Inside the container: one `/dev/dri/renderD*`, plus `DRA_GPU_PCI_BUS_ID`,
`DRA_GPU_VENDOR`, `DRA_GPU_VRAM_BYTES`. VRAM is not enforced: size models
and contexts to `DRA_GPU_VRAM_BYTES`.

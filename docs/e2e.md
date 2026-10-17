# Hardware test — v0.1.0

2026-10-17, one node with two AMD FirePro D700 (Mac Pro 6,1), k3s v1.37.0,
containerd 2.3.4 (CDI on by default), Mesa 26.0.8 RADV, driver image
`ghcr.io/ipsupport-llc/dra-driver-dri:0.1.0`. Test pods used
`ghcr.io/ggml-org/llama.cpp:full-vulkan` and a ResourceClaimTemplate asking
for `vram: 2Gi`; model `LFM2.5-2.6B` Q4_K_M (1.74 GiB).

Published: two devices (`pci-0000-02-00-0`, `pci-0000-06-00-0`), `vram`
5888 MiB each (6 GiB − 256 MiB reserve), request policy default = max =
5888 MiB, min = step = 64 MiB, `allowMultipleAllocations: true`.

| # | Scenario | Result |
|---|---|---|
| 1 | Pod with a 2 GiB claim | One `/dev/dri/renderD128` (`crw-rw-rw-`), `DRA_GPU_PCI_BUS_ID=0000:02:00.0`, `DRA_GPU_VENDOR=amd`, `DRA_GPU_VRAM_BYTES=2147483648`; Vulkan lists exactly one device. Claim allocated with a `shareID` and `consumedCapacity.vram: 2Gi`. **Pass** |
| 2 | llama.cpp in that pod | pp512 385, tg128 42.9 tokens/s — same as on the host. **Pass** |
| 3 | Five pods × 2 GiB | Four run, two per GPU (2 × 2 GiB ≤ 5888 MiB); the fifth stays `Pending`: "cannot allocate all claims". **Pass** |
| 4 | Delete a running pod | The pending pod starts on the GPU that was freed. **Pass** |
| 5 | `kubectl rollout restart` of the plugin | Test pods untouched (same restart counts and start times); after the restart `dri_claims_prepared` = 4 and `dri_vram_allocated_bytes` = 4 GiB per GPU, restored from the checkpoint. **Pass** |
| 6 | Metrics | `dri_vram_allocated_bytes` matched the running pods per GPU; back to 0 (and no CDI specs left) after the test namespace was deleted. **Pass** |

## Sharing one GPU

LFM2.5-2.6B, tokens/s:

| | pp512 | tg |
|---|---|---|
| One instance on a GPU | 385 | 42.9 |
| Two instances on the same GPU, each | 362 / 373 | 19.3 / 20.5 |
| Two instances on the same GPU, total | 735 | 39.7 |
| Two instances on different GPUs, each | 385 / 385 | 41.0 / 41.0 |

Generation is time-sliced between processes on one GPU (each gets about
half, 93 % in total); prompt processing overlaps well (1.9× in total).

## Observations

- **Placement is first-fit, not spread:** the first two 2 GiB claims both
  went to the first GPU while the second was idle. Kubernetes 1.37 has no
  device-level scoring that spreads shared allocations. When there are no
  more model instances than GPUs, claim whole GPUs (no `vram` amount) to get
  one GPU each.
- VRAM is not enforced; claims must include weights, KV cache and compute
  buffers.

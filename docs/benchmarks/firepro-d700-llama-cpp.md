# llama.cpp on FirePro D700 (Mac Pro 6,1) — measurements

Measured 2026-10-16 on one Mac Pro 6,1 node. Reference numbers for sizing
VRAM claims and for deciding which models are worth running on these GPUs.

## Environment

| | |
|---|---|
| Machine | Mac Pro 6,1 (late 2013), Xeon E5-2697 v2 (12 cores / 24 threads), 64 GB RAM |
| GPUs | 2 × AMD FirePro D700 ("Tahiti XT", GCN 1.0), 6 GiB GDDR5 each |
| OS | Ubuntu Server 26.04.1, kernel 7.0.0-38-generic, `amdgpu` kernel driver |
| Vulkan | Mesa 26.0.8 RADV (`RADV TAHITI`), Vulkan 1.3 |
| llama.cpp | release `b11700`, prebuilt `llama-b11700-bin-ubuntu-vulkan-x64` |
| Models | GGUF builds from Hugging Face account `roman220220`, sha256-verified |

GPU clocks under load: DPM level 3 = 850 MHz core / 1370 MHz memory, 1.1 V
(idle: 300 / 150 MHz). Clocks were read from
`/sys/kernel/debug/dri/<pci>/amdgpu_pm_info` during the runs.

## Method

```sh
# one GPU (pp = prompt processing, tg = generation; tokens/s)
llama-bench -m <model.gguf> -ngl 99 -dev Vulkan0 -p 512 -n 128 -r 3
# CPU only, for comparison
llama-bench -m <model.gguf> -ngl 0 -dev none -t 12 -p 512 -n 128 -r 2
# both GPUs at once, one process per GPU
llama-bench -m <model.gguf> -ngl 99 -dev Vulkan0 -p 0 -n 256 -r 3 &
llama-bench -m <model.gguf> -ngl 99 -dev Vulkan1 -p 0 -n 256 -r 3 &
```

`Vulkan0` was the GPU on PCI bus 06, `Vulkan1` the one on bus 02.

## Results (tokens/s)

| Model (file, size) | Quant | One D700: pp512 | One D700: tg | CPU 12 t: pp512 | CPU 12 t: tg | Both D700 at once: tg per GPU |
|---|---|---|---|---|---|---|
| LFM2.5-1.2B-Thinking (`lfm2.5-1.2b-thinking-jang.gguf`, 797 MiB) | Q4_K_M, imatrix | **865** | **84.2** | 117 | 30.3 | 84.3 + 83.7 |
| LFM2.5-2.6B (`lfm2.5-2.6b-jang.gguf`, 1.74 GiB) | Q4_K_M, imatrix | **386** | **42.9** | 47.7 | 16.1 | 42.8 + 42.4 |
| Gemma 4 E2B (`gemma-4-E2B-it-qat-phone-Q4_0.gguf`, 2.65 GiB) | Q4_0 (QAT) | 437 | 19.6 | 33.2 | 18.8 | 18.6 + 18.4 |

tg was `tg128` on one GPU and on CPU, `tg256`/`tg2048` for the parallel runs.

## Observations

- **LFM2.5 suits these GPUs:** generation is 2.7–2.8× the CPU, prompt
  processing 7–8×. Gemma 4 E2B generates no faster than the CPU on RADV/GCN 1.0
  (its prompt processing is still 13× the CPU).
- **The two GPUs do not slow each other down:** per-GPU numbers in parallel
  equal the single-GPU numbers, so a node gives roughly twice the single-GPU
  throughput with one model instance per GPU.
- **VRAM sizing** (weights + a modest context): LFM2.5-2.6B ≈ 2–2.5 GiB,
  LFM2.5-1.2B ≈ 1–1.5 GiB, Gemma 4 E2B Q4_0 ≈ 3–3.5 GiB. With 5.75 GiB published
  per D700 (6 GiB minus the 256 MiB reserve), that is two 2.6B instances or three to
  four 1.2B instances per GPU. Measure real use (`dri_vram_used_bytes`) before
  settling claim sizes.
- **Thermals:** CPU and both GPUs share one heatsink and one fan. Under Linux
  the SMC does not raise the fan for GPU load by itself; with a fan controller
  that follows the hotter of CPU and GPU temperatures, both GPUs under continuous
  load reached 66 / 70 °C after 3 minutes at ~1450 rpm.
- Idle GPU clocks are already the lowest DPM level (300 / 150 MHz); idle
  power is not measured yet.

## Several instances on one GPU: VRAM and GTT

Same node, all instances on the GPU on bus 06 (`-dev Vulkan0`), VRAM and GTT
read from `mem_info_vram_used` / `mem_info_gtt_used` while they ran.

| Load | VRAM used | GTT used | Generation, tokens/s |
|---|---|---|---|
| idle | 6 MiB | 5 MiB | |
| 1 × LFM2.5-2.6B | 2068 MiB | 287 MiB | 41.1 |
| 3 × Gemma 4 E2B | 5211 MiB | 998 MiB | 3 × 6.9 = 20.8 (one alone: 19.6) |
| 4 × LFM2.5-2.6B | 5942 MiB (of 6144) | 3448 MiB | 4 × 4.0 = 15.9 (one alone: 41.1) |

- Each llama.cpp process keeps roughly 0.3 GiB in GTT even when its model
  fits in VRAM, so GTT alone is not a sign of trouble.
- Overcommitting VRAM does not fail: the kernel evicts buffers to system RAM
  (GTT) and the GPU slows down. Four 2.6B instances ran at 39 % of the
  throughput of one. The example alert fires on GTT above 1 GiB while VRAM
  is over 90 % used: the last row trips it, the others do not.

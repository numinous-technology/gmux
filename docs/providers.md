# Providers

gmux runs in any container that can see a GPU. It needs no host access, so it
works on providers that only give you a container.

## What a provider needs to offer

- A GPU visible in the container (the vendor's runtime, e.g. `--gpus all`).
- The vendor's query tool: `nvidia-smi`, `rocm-smi`, or `xpu-smi`.
- For NVIDIA compute and memory caps: `nvidia-cuda-mps-control` (in the CUDA
  images) and permission to run it as your own user, which is the default.

## Notes per vendor

- **NVIDIA:** the common case. MPS runs as your user with no root. Memory caps
  need a recent driver; `cuda-checkpoint` (for preempt-and-resume) needs a
  driver from 2024 on.
- **AMD (ROCm):** device selection and compute-unit masking work without root.
  Memory caps use the preload shim, so build `libgmux.so` into your image and
  set `GMUX_SHIM`.
- **Intel (Level Zero):** device selection works; sharing is by admission only.

## Verified end to end

- **DigitalOcean GPU droplet, NVIDIA L40S.** A KVM VM with the card passed
  through and a normal kernel. Real detection, MPS compute and memory caps,
  the benchmark in the README, `--deny-net`, and the full `--allow` fence all
  work. Transcript: [evidence/l40s-real-gpu.txt](evidence/l40s-real-gpu.txt).

## Hosts that are themselves sandboxes

Some providers run your container inside their own seccomp or gVisor sandbox
(Modal, Thunder Compute). The scheduler, admission, accounting and
`--deny-net` work there, but the kernel will not hand a second seccomp
listener to a process that already sits under one, so `--allow` cannot arm.
MPS may also be unavailable through a virtualised GPU. `gmux cards` reports
what the host allows.

## Tested

The vendor tool parsers are tested against recorded output from H100, A100,
L4, RTX 4090, T4 (NVIDIA); MI300X, MI250X, RX 7900 XTX (AMD); Max 1550, Flex
170, Arc A770 (Intel). End-to-end runs on each provider are being collected;
open an issue with your provider and card and we will add it.

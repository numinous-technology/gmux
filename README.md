# gmux

tmux for GPUs. Share one GPU across many jobs, in space and in time, resize
them while they run, and see exactly who used what.

No root and no host access needed. If you can run Docker, you can run gmux, so
it works on almost any GPU provider: RunPod, Vast, Lambda, a cloud VM, a
university node, or your own box.

Works with NVIDIA, AMD and Intel GPUs. What each one can enforce differs;
platform support is documented in [Isolation](docs/isolation.md).

## Why

Most GPU jobs do not use the whole card. A notebook idles, an eval runs in
bursts, a small model serves a few requests a minute. Renting a card per job
wastes most of it. Sharing one by hand means jobs running out of memory,
fighting over compute, and no record of who used what.

gmux splits the card for you and keeps the books.

## Quick start

```bash
# in a container that has a GPU
docker run -d --gpus all --name gmux ghcr.io/numinous-technology/gmux serve
docker exec gmux gmux run --share 0.25 --mem 20G -- python train.py
docker exec gmux gmux run --share 0.5 -- python serve.py
docker exec gmux gmux top
```

No GPU handy? Try the whole thing against a pretend card:

```bash
gmux serve --fake 1xH100:80G &
gmux run --share 0.25 --name a -- sleep 20
gmux run --share 0.25 --name b -- sleep 20
gmux top
gmux usage --since 1h --by job
```

## What it does

**Spatial sharing.** Several jobs run on one card at the same time, each with
its own slice of compute and memory.

**Temporal sharing.** Idle guaranteed capacity is lent to jobs that can use it,
and low priority jobs can wait for a free slot instead of competing.

```bash
gmux run --share 0.25 --burst -- python eval.py          # use idle compute above the guarantee
gmux run --share 0.5 --priority 1 --preemptible -- python sweep.py
gmux run --share 0.5 --priority 9 -- python urgent.py    # can push the low job off
```

**Live resize.** Grow or shrink a running job's share.

```bash
gmux resize j1234 --share 0.75
```

**Admission.** gmux refuses a job the card cannot fit, instead of letting it
start and crash its neighbours.

**Accounting.** Every second of every share is recorded, by job, owner, card
or vendor.

```bash
gmux usage --since 24h --by owner
```

**Network fence.** Limit what a job can reach, per job, enforced in the kernel
with seccomp. No root needed.

```bash
gmux run --share 0.25 --allow api.openai.com --allow pypi.org -- python agent.py
gmux run --share 0.25 --deny-net -- python offline.py
```

## Where it runs

Anywhere you get a container with a GPU. gmux never installs drivers, loads
kernel modules, or reconfigures the host. It detects whatever vendor tools are
present (`nvidia-smi`, `rocm-smi`, `xpu-smi`) and uses them. Tested providers
and their quirks are in [docs/providers.md](docs/providers.md).

## Run on a remote GPU host

A machine with no GPU can run a command on one that has gmux. The daemon syncs
your working directory (content addressed, so only changed files move), runs
the command on its GPU under a real share, streams the output back, and hands
you result files. The network boundary is the command, not the CUDA call, so it
tolerates latency and needs no driver interception.

On the GPU host:

```bash
gmux serve --addr :7070 --token $SECRET
```

From any machine, with no GPU of its own:

```bash
gmux run --remote $SECRET@gpuhost:7070 --share 0.25 \
    --pull 'out/*' -- python train.py
```

Repeated runs from the same directory reuse the workspace and sync only the
diff. This is how a fleet of cheap CPU machines shares a pool of GPU hosts:
the caps and the network fence run natively on the host, next to the card.

## Uses

A few things people do with a GPU multiplexer:

- **Batch many small evals or inferences** onto a few cards instead of renting
  one per job.
- **Pack fine-tuning and serving together**, giving serving the guaranteed
  share and letting training burst into the rest.
- **Give a team fair shares** of a shared box, with usage they can see.
- **Run untrusted or semi-trusted jobs** with a network allowlist and a memory
  cap, without handing out root.
- **Squeeze a fixed rented pod** by fitting more work onto the card you are
  already paying for by the hour.

gmux came out of batching GPU evaluations of AI agents: many short agent trials
across a handful of cards, each trial holding a slice, with per-second
accounting and a network allowlist per trial, and CPU-only machines dispatching
work to a GPU pool. It is a general multiplexer now.

## Isolation

gmux is for jobs you trust, or mostly trust. Without host access, sharing is
enforced in user space. On NVIDIA, MPS caps compute and the driver caps
memory; on AMD, compute units are masked and a preload library caps memory; on
Intel, sharing is by admission only. In every case a job you do not trust at
all can still affect its neighbours in ways only the driver or the hardware
could prevent. If you need hard isolation between strangers, use MIG or
separate cards. The full picture is in [docs/isolation.md](docs/isolation.md).

## Benchmark

Each job runs a sustained fp16 8192-square matmul for 20 seconds, long enough
that concurrent jobs genuinely overlap. Numbers are TFLOP/s per job, measured
through gmux on real cards. Reproduce with `bench/run.sh`.

| Card | Whole card, no gmux | 1/4 share alone | Four 1/4 shares at once, each | Memory cap on a 1/4 share |
|---|---|---|---|---|
| NVIDIA H100 80 GB | 662.0 | 215.1 | 157.7 to 158.1 | 19 GiB: 17 allocates, 20 refused |
| NVIDIA B300 288 GB | 1284.3 | 549.1 | 289.6 to 290.0 | 67 GiB: 60 allocates, 73 refused |
| NVIDIA L40S 46 GB | 171.6 | 81.1 | 32.7 | 10.6 GiB visible to the job |
| AMD MI355X 288 GB | 1264.1 | 452.0 to 457.8 | 242.5 to 242.8 | 71 GiB: 63 allocates, 78 refused |
| AMD MI350X 288 GB | 1024.4 | 416.5 to 419.0 | 194.1 to 194.4 | 71 GiB: 63 allocates, 78 refused |
| AMD MI325X 256 GB | 741.9 | 256.6 to 257.4 | 119.4 to 119.8 | 64 GB: 60 allocates, 70 refused |

NVIDIA shares are capped by MPS; AMD shares get their own shader engines on
every chiplet (the AMD rows show each of the four quarters alone) and a memory
cap through the preload shim. On every card the memory cap is hard, the split
between concurrent jobs is even, and a quarter share alone cannot take the
whole card. Without gmux, every over-limit allocation above succeeds.

Sharing pays when jobs do not fill the card on their own: notebooks, bursty
evals, small models serving a few requests. A job that already saturates the
card, like this matmul, runs fastest alone; four saturating quarter shares add
up to between 64% (MI325X) and 95% (H100) of one dedicated job. Transcripts
are in [docs/evidence](docs/evidence).

## How it works

gmux runs as a daemon in a container next to your jobs. A scheduler decides
who goes where, in space and time. For NVIDIA it runs one MPS daemon per card
and binds each job to its share; for AMD and Intel it sets the vendor's device
and compute-unit variables and, where possible, caps memory with a small
preload library. The network fence installs a seccomp filter on the job and
answers its connections from outside the filter, so the job cannot route around
the allowlist. Usage is a plain append-only ledger. Details in
[docs/how-it-works.md](docs/how-it-works.md).

## Build

```bash
go build -o gmux ./cmd/gmux          # the daemon and CLI
make -C shim                          # libgmux.so, the memory-cap shim
go test ./...                         # unit and integration tests
pip install -e sdk/python             # the Python client
```

## What works

Spatial and temporal sharing, admission, live resize, per-second accounting,
the network fence, remote command execution, and multi-vendor detection are
built and covered by tests on every build.

On real NVIDIA H100, B300 and L40S cards, gmux detects the card, starts MPS, applies each job's
compute and memory caps, splits the card fairly, and enforces the network
fence: `--deny-net` blocks everything, and `--allow` resolves and connects to
allowlisted hosts while refusing the rest.

On real AMD MI325X, MI350X and MI355X cards, gmux selects the card by UUID, gives each job its own
shader engines on every chiplet (checked with a compute-unit probe, see
`bench/cuprobe.hip`), and caps HIP allocations through the preload shim, with
PyTorch.

The Intel backend drives `xpu-smi`; its parser is tested against recorded
output from Max 1550, Flex 170 and Arc A770. `gmux cards` reports what each
card can enforce.

Preempt-and-resume through `cuda-checkpoint` is the next backend feature.

## License

Apache-2.0.

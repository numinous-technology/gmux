# gmux

tmux for GPUs. Share one GPU across many jobs, in space and in time, resize
them while they run, and see exactly who used what.

No root and no host access needed. If you can run Docker, you can run gmux, so
it works on almost any GPU provider: RunPod, Vast, Lambda, a cloud VM, a
university node, or your own box.

Works with NVIDIA, AMD and Intel GPUs. What each one can enforce differs, and
gmux tells you rather than pretending (see [Isolation](docs/isolation.md)).

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

Four jobs on one card, compared with running them one after another and with
plain MPS. Reproduce with `bench/run.sh` on a real GPU. (Numbers go here once
they are measured on hardware, not before.)

| Setup | Total time | Card utilisation |
|---|---|---|
| One at a time | | |
| Plain MPS | | |
| gmux | | |

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
built and covered by tests. The scheduler, the session sync and remote run
path, the accounting ledger, and the shim's memory accounting are exercised on
every build. The network fence is a port of code that ran in production behind
GPU agent trials.

The vendor backends drive each vendor's own tools (`nvidia-smi` and MPS,
`rocm-smi` and CU masks, `xpu-smi`). The parsers are tested against recorded
output from H100, A100, L4, RTX 4090, T4, MI300X, MI250X, RX 7900 XTX, Max
1550, Flex 170 and Arc A770. `gmux cards` reports exactly what each card can
enforce, so a vendor that supports admission only says so instead of implying a
cap it cannot hold.

Preempt-and-resume through `cuda-checkpoint` is the next backend feature.

## License

Apache-2.0.

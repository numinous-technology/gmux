# Isolation

gmux shares a GPU without host access. That buys wide compatibility and costs
hard isolation. This page says exactly what is enforced and what is not, so you
can decide whether gmux fits your trust model.

## What gmux enforces

| | NVIDIA | AMD | Intel |
|---|---|---|---|
| Jobs run at the same time | yes (MPS) | yes | yes |
| Compute share capped | yes (MPS thread %) | yes (whole shader engines per job) | no |
| Memory capped | yes (MPS pinned limit) | yes (preload shim) | no |
| Suspended job frees its memory | with cuda-checkpoint | no | no |
| Admission (refuse what will not fit) | yes | yes | yes |
| Network allowlist | yes (seccomp) | yes | yes |

Where a cap is not available, the share is still enforced by admission: gmux
will not place more work on a card than it has seats and memory for. What it
cannot do in that case is stop a job that ignores its share from using more
than its slice at runtime.

## How AMD compute partitions work

gmux gives each AMD job its own compute units through `HSA_CU_MASK`. The
runtime deals mask bits round robin across chiplets and then across each
chiplet's shader engines, and the hardware splits a kernel's work evenly across
every engine a job touches, so a job runs at the speed of its thinnest engine.
gmux therefore hands out whole engines: on an 8-chiplet MI300-class card, a
quarter share owns one shader engine on every chiplet, and an eighth owns half
of one. It reads the chiplet and engine counts from the kernel's KFD topology
(no root). Units that exist on only some engines are left out, so no job ever
has a lone unit on an engine; on an MI325X that is 16 of 304 units when the card
is fully partitioned.

## What gmux does not do

- **It is not a security boundary between strangers.** MPS is not isolation:
  a job on a shared MPS context can, in the worst case, affect others on the
  card. The memory-cap shim runs inside the job's own process and a determined
  job could bypass it. For untrusted tenants, use MIG (NVIDIA) or give each
  tenant a separate card.
- **It does not cap memory bandwidth, L2 or copy engines.** A compute
  percentage bounds SM time, not everything that makes a card fast, so a
  quarter card under contention is approximate.
- **It needs the vendor's user-space sharing to exist.** No MPS, no compute
  cap on NVIDIA; the daemon says so at start.

## When gmux is the right tool

- Your jobs are yours, or your team's, and you want them to coexist without
  stepping on each other, with accounting.
- You are packing many small or bursty jobs onto cards you already pay for.
- You want a network allowlist per job without root.

## When it is not

- You are running code you do not trust at all next to code you care about, on
  the same card. Use hardware partitioning.

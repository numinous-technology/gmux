# How gmux works

One daemon per container owns the cards, the scheduler, the ledger and the
jobs. The CLI and the Python SDK are clients of a small JSON API on a unix
socket.

## Cards and seats

Each card is divided into a fixed number of seats (eight by default). A request
for a fraction of a card is rounded up to whole seats, and the job is told the
share it actually got. A job's compute cap and its default memory cap both
follow from its seats.

## The scheduler

Pure and testable: it decides, the daemon acts.

- Placement is best fit, so large holes stay open for large jobs.
- A multi-card job gets all its cards at once or waits (gang placement).
- A higher priority job can push lower priority preemptible jobs off, the
  fewest and lowest and newest first, until it fits.
- A job that does not fit waits in a queue or is refused, as it asked.
- A burst job may use idle compute above its guarantee; its guarantee is still
  reserved, so bursting never takes a seat from anyone.

## Vendors

The scheduler only knows seats and memory. A backend turns that into what a
particular GPU enforces:

- **NVIDIA:** one MPS control daemon per card (no root). Each job gets an
  active-thread-percentage compute cap and a pinned-memory cap, both from MPS.
  `cuda-checkpoint`, when present, frees a suspended job's GPU memory.
- **AMD:** `HIP_VISIBLE_DEVICES` selects the card, `HSA_CU_MASK` masks compute
  units to the job's share (when the card's compute-unit count is known from
  `rocminfo`), and the preload shim caps HIP allocations.
- **Intel:** `ZE_AFFINITY_MASK` selects the card. There is no per-process
  compute or memory cap without host access, so a share is enforced by
  admission only.
- **fake:** pretend cards for trying gmux without hardware.

A machine with two vendors gets both; every card carries its vendor.

## Running a job

The daemon builds the job's GPU environment from its backend, then launches the
command in its own process group so the whole tree can be signalled. With a
network allowlist the command runs through the fence.

## The network fence

Two halves. A seccomp filter on the job traps every socket, connect and send,
and a supervisor outside the filter answers them: it owns every internet socket
in the job, and at connect time it replaces the job's socket with one it has
already connected to a local proxy. The proxy decides what to allow from the
name the job itself sends (the TLS server name, or the HTTP Host header), so
the job cannot get a socket to an address of its own choosing. Nothing is
decrypted. This is a Go port of code that ran in production.

Because the filter must survive `execve`, gmux runs the fenced command through
a tiny re-exec of itself that installs the filter on the thread that then
execs.

## Accounting

Every stretch of a job holding a share is one interval, appended to a JSON
lines file. A usage report clips intervals to the window and groups them by
job, owner, card or vendor. The ledger survives a daemon restart.

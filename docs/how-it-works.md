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

## Remote GPUs

A machine without a GPU runs commands on gmux hosts. The design is
command-level remoting: the network boundary is the command, not the CUDA call.

- `gmux serve --addr :7070` opens a TLS listener next to the local unix socket.
  On first start it makes a self-signed certificate and a random token and
  keeps both in the state directory, then prints the target a client needs:
  `TOKEN@HOST:PORT#FINGERPRINT`. Clients pin the certificate by that
  fingerprint and send the token on every request; the local socket stays
  trusted and unauthenticated.
- Clients keep their hosts in `~/.config/gmux/remotes.json`. With no local
  daemon, every command goes to them. For a run, the client asks every host
  for its cards in parallel and picks the one with the most seats left after
  the job fits, skipping hosts that do not answer; ties go to the default.
- A session is a workspace on the host. The client lists its working directory
  (path, sha256, size, exec bit), taking hashes of unchanged files from a local
  cache keyed by device, inode, size and mtime. The host replies with the
  blobs it lacks, the client streams only those, and the host writes each to a
  temp file while hashing it and publishes it only if the hash matches. The
  host then lays the tree into the workspace, skipping files it wrote before
  that are unchanged, and removing files the client no longer has.
- Exec runs the command in that workspace as an ordinary gmux job, so it takes
  a real share, its caps and its fence, applied on the host next to the card.
  Output streams back as NDJSON (`{"type":"o"/"e","d":line}`) ending in
  `{"type":"exit","code":n}`. If the client disconnects, the host stops the job.
- Fetch returns a gzip tar of workspace files matching a set of globs.

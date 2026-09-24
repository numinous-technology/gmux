# Contributing

gmux is Go plus a small C shim. No external Go dependencies on purpose: the
binary should drop into any container.

- `go test ./...` must pass. The fence has an end-to-end test that skips on
  hosts which forbid a nested seccomp listener (including some CI sandboxes);
  run it on a normal Linux host or in the gmux container.
- `make -C shim test` covers the memory-cap accounting core.
- Vendor parsers are tested against recorded tool output in
  `internal/gpu/testdata`. Adding a card means adding a fixture.
- If a cap is not enforced on a vendor, `gmux cards` must say so rather than
  imply otherwise.

Design notes are in `docs/`. The network fence is a port of a production
Python implementation; keep the two in agreement if you touch it.

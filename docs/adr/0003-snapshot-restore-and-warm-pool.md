# ADR 0003: Snapshot restore and a warm pool for starts

- **Status:** Accepted
- **Date:** 2026-09-29

## Context

A cold boot runs the guest kernel and init before the agent can answer. That
cost is paid on every sandbox create, inside an agent's interactive loop.
Firecracker can instead load a snapshot of a VM that has already booted,
mapping its memory lazily from a file. A pool of sandboxes restored ahead of
time removes even that from the request path.

## Decision

- Build each template by cold booting once and taking a snapshot when the
  agent is ready and file systems are synced.
- Serve creates from a per-template warm pool; fall back to restoring the
  template snapshot when the pool is empty or the requested shape differs.
- Keep cold boot for building templates and as the measured baseline.
- Handle clone uniqueness explicitly (design.md section 7.3): VMGenID in the
  guest kernel, host-supplied entropy after restore, correct wall clock, and
  per-sandbox network namespaces for identical addressing.

## Consequences

- Three start paths to measure and compare (design.md section 15).
- Templates are bound to the Firecracker version, kernels, and CPU model; a
  manifest records them and mismatches are rejected.
- Pooled sandboxes consume memory while idle; most of it is shared page cache
  from the snapshot file, and density is reported as proportional set size.
- Restoring many VMs from one memory image is exactly the case Firecracker
  warns about; the containment suite tests uniqueness on every build.

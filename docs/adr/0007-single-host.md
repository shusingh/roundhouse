# ADR 0007: Single host in v1, with a node boundary kept

- **Status:** Accepted
- **Date:** 2026-09-29

## Context

Production sandbox services run many hosts: a control plane places sandboxes
on nodes, and each node runs an orchestrator for its VMs. The hard problems
this project is about (isolation, start latency, cleanup, containment) all
live on one node. A scheduler adds placement, routing, and state replication,
none of which can be measured meaningfully on one development machine.

## Decision

v1 runs on one host. The packages that manage VMs (`sandbox`, `pool`, `vmm`,
`netns`, `storage`) are written as one node's orchestrator and assume nothing
about the API layer above them; only admission's view of capacity is
host-wide.

## Consequences

- Scope stays on the node-level problems, which are the ones benchmarks and the
  containment suite can measure.
- Adding a scheduler later means a routing layer in front of per-node APIs,
  not a rewrite of the node.
- Capacity numbers are per host and are reported that way.

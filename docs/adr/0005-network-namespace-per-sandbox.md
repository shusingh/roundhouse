# ADR 0005: One network namespace per sandbox, identical guest addressing

- **Status:** Accepted
- **Date:** 2026-09-29

## Context

Sandboxes restored from one snapshot share the guest's network configuration:
the same IP and MAC address. Changing the guest's address after restore is
possible but adds work to the start path and a window where the guest's view
and the host's disagree. Firecracker's documentation describes the standard
answer for clones: a network namespace per clone, so identical addresses never
meet.

## Decision

- Each sandbox runs its Firecracker process in its own network namespace
  containing a tap device with a fixed name and address.
- When egress is allowed, a veth pair connects the namespace to the host with a
  unique /30 per sandbox, and nftables on the host applies NAT and the egress
  policy (design.md section 8).
- With egress `none`, no veth pair is created: the namespace has no route out.
- All names carry the `rh-` prefix and the sandbox ID for reconciliation.

## Consequences

- Restored clones need no network reconfiguration.
- Isolation between sandboxes' networks is a kernel namespace boundary, not
  only firewall rules.
- Namespace, veth, and rule setup is on the create path and is measured; with
  egress `none` the cost is smallest.
- Host-side address space is a /30 per sandbox with egress, allocated from a
  configurable private range.

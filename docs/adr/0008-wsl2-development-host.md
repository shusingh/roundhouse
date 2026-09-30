# ADR 0008: Develop on WSL2 with nested KVM

- **Status:** Accepted
- **Date:** 2026-09-29

## Context

Firecracker needs Linux and KVM. The development machine runs Windows 11 with
WSL2, which runs its Linux kernel under Hyper-V. With nested virtualization,
WSL2 exposes `/dev/kvm`, and its kernel (6.18) is a Firecracker-supported host
kernel. The alternatives are a bare-metal or nested-capable cloud host, which
costs money for the whole build, or gVisor, which would change the isolation
design.

## Decision

Develop, test, and benchmark on WSL2 with nested KVM. Source code stays on the
Windows volume; runtime state (images, jails, snapshots) lives on WSL's ext4
volume. Every published number states that it was measured under nested
virtualization.

## Consequences

- No cloud cost, and the full design (jailer, cgroup v2, namespaces, nftables,
  snapshots) runs as it would on a Linux host.
- Absolute latency and density under nested virtualization differ from bare
  metal, likely for the worse; results are presented as measured on this host,
  with an optional later validation on bare metal.
- Some production hardening (disabling SMT, the host's kernel command line) is
  outside the development host's control and is documented rather than
  applied.

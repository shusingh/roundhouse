# ADR 0001: Firecracker microVMs for isolation

- **Status:** Accepted
- **Date:** 2026-09-29

## Context

Sandboxes run code written by a model, which anything in the model's context
can influence. The isolation boundary must hold against code that runs as
root and actively tries to escape. Three classes of engine are in common use
for this (design.md section 4):

- **OCI containers** (runc) share the host kernel. Every kernel vulnerability
  reachable from a container is a potential escape, and the attack surface is
  the whole system call interface.
- **User-space kernels** (gVisor) intercept system calls in a user-space
  kernel. The attack surface is much smaller than a container's, at a cost in
  system call performance and compatibility.
- **MicroVMs** (Firecracker, Cloud Hypervisor) run a separate guest kernel on
  hardware virtualization. The boundary is KVM plus a small device model.

## Decision

Run every sandbox in its own Firecracker microVM, started through the
jailer. Firecracker was chosen over other microVM monitors because it is
built for this use (multi-tenant, short-lived, many per host), has a minimal
device model with default seccomp filters, supports snapshots and vsock, and
is what the closest prior art (E2B, AWS Lambda) runs in production.

## Consequences

- Hardware isolation and a full Linux guest: anything that runs on Linux runs
  in a sandbox, with no system call compatibility gaps.
- The host needs KVM. Development uses nested virtualization on WSL2
  (ADR 0008).
- Start latency is a real cost, which ADR 0003 addresses with snapshots.
- Each sandbox carries a guest kernel's memory overhead, which bounds density
  and is measured.

# Changelog

All notable changes are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[semantic versioning](https://semver.org/).

## [0.1.0] - 2026-09-30

First release.

### Added

- `roundhoused`, a single-host control plane with an HTTP API: create, list,
  get, and delete sandboxes; run commands with stdout and stderr streamed as
  NDJSON frames; write and read files. Bearer API keys stored as SHA-256
  digests and compared in constant time; another key's sandboxes are
  reported as not found.
- One Firecracker microVM per sandbox, launched by the jailer into a chroot,
  PID and network namespaces, a cgroup, and a per-sandbox unprivileged uid.
  Teardown kills the whole cgroup; startup reconciliation removes anything a
  previous run left behind.
- Guest agent (`roundhouse-agent`) as the guest's init: overlay of a
  per-sandbox scratch disk on a shared read-only root, protocol server over
  vsock, commands run in a guest workload cgroup below the guest's own
  memory and process limits.
- Templates (`roundhoused template build`): snapshots of a booted guest per
  image and shape, with a manifest checked against the host at startup.
  Restores assign each clone fresh host entropy with a forced kernel reseed,
  its identity, and the host's wall clock.
- Warm pools per image and shape: pooled sandboxes are started and probed
  ahead of time, get the caller's egress policy at claim, give way to callers
  when the host is full, and are recycled after a maximum age.
- Networking: no network by default; `internet` and address `allowlist`
  egress through a per-sandbox veth pair and NAT, with host nftables rules
  that drop traffic to the host, other sandboxes, private and link-local
  ranges, cloud metadata, and IPv6. Per-sandbox bandwidth limits.
- Limits: vCPUs and `cpu.max`, memory and `memory.max`, processes, disk size
  and I/O rate, network rate, console rate, command time enforced in the
  guest and by the host as a backstop, idle timeout, and lifetime. Host-wide
  admission budget for memory, vCPUs, and sandbox count, and per-key quotas.
- Prometheus metrics, `/readyz`, JSON logs that never contain commands,
  output, or file contents, capped per-sandbox console and VMM logs, and
  diagnostics kept for sandboxes that fail.
- `rhctl`, a command-line client, and `rhbench`, the benchmark driver, with
  `bench/` scripts and runc and gVisor baselines.
- Test suites: unit tests with the race detector, goroutine-leak checks, and
  fuzz targets; integration tests against real microVMs; and a containment
  suite of hostile workloads with neighbor latency and leak checks.
- Documentation: design, architecture decision records, operations guide,
  and benchmark results.

[0.1.0]: https://github.com/shusingh/roundhouse/releases/tag/v0.1.0

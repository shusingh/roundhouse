# Roundhouse

A service that runs untrusted code for LLM agents inside Firecracker microVMs.

An agent asks for a sandbox, runs commands and moves files across many steps,
and releases it. Each sandbox is its own microVM behind a hardware isolation
boundary, with hard limits on CPU, memory, disk, processes, time, and network,
and no network at all unless asked for. Sandboxes start by restoring a
snapshot of an already-booted guest, or come ready from a warm pool, instead
of booting a kernel.

| On the development host (nested KVM), through the API | p50 | p99 |
|---|---:|---:|
| Warm pool: create to first command answered | 3.2 ms | 8.0 ms |
| Snapshot restore: create to first command answered | 108 ms | 189 ms |
| Cold boot: create to first command answered | 730 ms | 795 ms |
| Command round trip in a ready sandbox | 1.24 ms | 1.94 ms |

1,000 idle 256 MiB sandboxes held on one host at 8.3 MiB proportional memory
each, and no measurable effect on a neighbor's latency from a fork bomb,
memory hog, disk fill, CPU spin, or console flood. Method, baselines (runc,
gVisor), and caveats: [docs/benchmarks.md](docs/benchmarks.md).

## How it works

```text
  client ── HTTP/JSON ──▶ roundhoused ─────────────────────────────────────────┐
                           API · auth · quotas · admission · warm pools · reaper│
                                 │ per sandbox                                  │
                                 ▼                                              │
      jailer: chroot · pid/net namespaces · cgroup (cpu, memory, pids) · uid   │
        └─ Firecracker VMM ── KVM ──▶ guest: kernel + roundhouse-agent (init) │
              vsock ◀── HTTP ── agent: exec, files, health, restore            │
              tap0 in the sandbox's network namespace ─▶ nftables egress policy│
```

- **One microVM per sandbox**, launched by Firecracker's jailer into a chroot,
  its own PID and network namespaces, a cgroup, and an unprivileged uid.
  Teardown kills the whole cgroup and removes every resource; startup
  reconciliation removes anything a crashed control plane left behind.
- **Snapshots and a warm pool.** `roundhoused template build` snapshots a
  booted guest; each sandbox restores from it into a fresh jail with its own
  copy of the disk. Every clone gets fresh host entropy, a forced kernel
  reseed, and the host's clock, so clones never share random state. Pools
  keep restored sandboxes ready and give way to callers when the host is
  full.
- **Guest agent** as PID 1 in the guest, speaking HTTP over vsock: commands
  stream stdout and stderr as NDJSON frames; files move in and out. Commands
  run in a guest cgroup below the guest's own limits, so a runaway workload
  is stopped while the agent keeps answering.
- **Networking:** no network by default. Egress `internet` or an allowlist
  adds a veth pair and NAT, and host nftables rules drop anything bound for
  the host, other sandboxes, private and link-local ranges, cloud metadata,
  or IPv6. Bandwidth is capped per sandbox.
- **Limits:** vCPUs and `cpu.max`, guest memory and `memory.max`, processes,
  disk size and I/O rate, network rate, console rate, command time (enforced
  in the guest and, as a backstop, on the host), idle timeout, lifetime, and
  a host-wide admission budget with per-key quotas.
- **Operations:** Prometheus metrics, `/readyz`, structured logs that never
  contain commands, output, or file contents, and failed sandboxes' console
  and VMM logs kept for diagnosis.

The full design, including the threat model and what the containment suite
proves: [docs/engineering/design.md](docs/engineering/design.md). Decisions
are recorded as [ADRs](docs/adr/README.md).

## Quick start

Needs Linux with KVM (or WSL2 with nested virtualization), cgroup v2, and
root. From a clone:

```bash
make host-setup                       # Firecracker, jailer, Go, nftables (pinned, checksummed; uses sudo)
sudo bash images/fetch-base.sh        # guest kernel and Ubuntu root file system
make build                            # bin/roundhoused, rhctl, rhbench, roundhouse-agent
sudo bash images/build-rootfs.sh bin/roundhouse-agent
```

Create a key, write a config from
[configs/roundhoused.example.yaml](configs/roundhoused.example.yaml) with its
digest, build a template for the default shape, and start the server:

```bash
bin/roundhoused hash-key
sudo bin/roundhoused template build -config /etc/roundhouse/roundhoused.yaml
sudo bin/roundhoused -config /etc/roundhouse/roundhoused.yaml
```

Then, as any user:

```bash
export ROUNDHOUSE_API_KEY=rh_...
id=$(bin/rhctl create -memory 512)
bin/rhctl exec "$id" -- python3 -c 'print(2 + 2)'
bin/rhctl put "$id" data.csv /root/data.csv
bin/rhctl exec "$id" -- wc -l /root/data.csv
bin/rhctl rm "$id"
```

The HTTP API (`POST /v1/sandboxes`, `/exec`, `/files`, and so on) is in
[design.md section 13](docs/engineering/design.md#13-api); a Go client is in
[internal/apiclient](internal/apiclient). Running it as a service, templates,
pools, capacity, metrics, and troubleshooting:
[docs/operations.md](docs/operations.md).

## Testing

- Unit tests with the race detector and goroutine-leak checks, and fuzz
  targets for every decoder of untrusted input: `make check`.
- Integration tests that boot real microVMs (sessions, egress policies,
  templates, pools, clone uniqueness, reaping, reconciliation):
  `make integration`.
- A containment suite that runs hostile workloads against real microVMs with
  a neighbor's latency sampled throughout and a leak scan after every test:
  `make containment`.

## Status and limits

v0.1.0 runs on a single host. It was developed and measured on WSL2 with
nested virtualization, not on bare metal. Templates are built from
Roundhouse's own guest images, not arbitrary container images, and egress
allowlists filter by address, not by domain name. See the non-goals and open
risks in the design document.

## License

[MIT](LICENSE)

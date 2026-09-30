# Roundhouse

A service that runs untrusted code for LLM agents inside Firecracker microVMs.

An agent asks for a sandbox, runs commands and moves files across many steps,
and releases it. Each sandbox is its own microVM with a hardware isolation
boundary, hard limits on CPU, memory, disk, processes, time, and network, and
no network at all unless asked for. Sandboxes start from snapshots and a warm
pool rather than booting a kernel.

**Status:** in development. The design, its decisions, and the benchmark
method are written first:

- [Design](docs/engineering/design.md): architecture, lifecycle, snapshots and
  clone safety, networking, threat model, and the benchmark method
- [Architecture decision records](docs/adr/README.md)
- [Coding standards](docs/engineering/coding-standards.md)

## Early measurements

On the development host (WSL2 on Windows 11, nested KVM, Linux 6.18 host,
Firecracker v1.17.0, 1 vCPU and 256 MiB guest), before any Roundhouse code:

| Path | Time |
|---|---:|
| Cold boot, InstanceStart to guest user space | about 620 ms |
| Snapshot load and resume, fresh Firecracker process | 11 to 21 ms |

These are Firecracker's own numbers under nested virtualization. Roundhouse's
benchmarks will measure the full path to a sandbox that answers its first
command (design section 15).

## Requirements

Linux with KVM (or WSL2 with nested virtualization), cgroup v2, and root for
the control plane. `make host-setup` installs Firecracker, the jailer, Go, and
the system packages; `images/fetch-base.sh` downloads the guest kernel and root
file system.

## License

[MIT](LICENSE)

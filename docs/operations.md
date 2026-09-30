# Operating Roundhouse

How to install, configure, run, observe, and troubleshoot `roundhoused` on
one host. The design behind it is in
[engineering/design.md](engineering/design.md).

## Host requirements

- Linux with KVM (`/dev/kvm`), or WSL2 with nested virtualization. Firecracker
  v1.17 supports host kernels up to 6.18.
- cgroup v2 with the `cpu`, `memory`, and `pids` controllers.
- nftables, iproute2, and root for the control plane. Each VMM runs as its own
  unprivileged uid inside a jail.
- A Linux file system for the data directory (default `/var/lib/roundhouse`):
  jails hard link images and templates, and scratch disks are sparse files.
  On WSL2 it must live on the Linux side, not under `/mnt/c` or `/mnt/d`.

`make host-setup` installs pinned, checksum-verified Firecracker, jailer, Go,
and golangci-lint, the system packages, and the data directories.

## Install

```bash
make host-setup
sudo bash images/fetch-base.sh
make build
sudo install -m 0755 bin/roundhoused bin/rhctl /usr/local/bin/
sudo bash images/build-rootfs.sh bin/roundhouse-agent
sudo install -d -m 0750 /etc/roundhouse
sudo install -m 0640 configs/roundhoused.example.yaml /etc/roundhouse/roundhoused.yaml
sudo install -m 0644 deploy/roundhoused.service /etc/systemd/system/
```

`images/build-rootfs.sh` writes the `default` image: `vmlinux`, a read-only
`rootfs.ext4` with the agent installed as the guest's init, and a sparse
`scratch.ext4` (2 GiB by default; its size is each sandbox's disk limit). Pass
an output directory and size to build other images.

## Configure

Every field, with its default, is in
[configs/roundhoused.example.yaml](../configs/roundhoused.example.yaml).
Unknown fields and invalid values are rejected at startup with a message
naming the field.

**API keys.** `roundhoused hash-key` prints a new key and its SHA-256
digest. Only the digest goes in the config, so a leaked config file leaks no
usable key. The key's `name` owns the sandboxes it creates; another key's
sandboxes are reported as not found. `max_sandboxes` on a key caps how many it
may hold at once.

**Capacity.** Three limits apply to every create, pooled sandboxes included:

- `sandbox.max_sandboxes`, and the uid range `host.uid_base` to
  `uid_base + max_sandboxes` (one past the range builds templates), which
  nothing else on the host may use;
- `host.memory_budget_mib`, charged guest memory plus `vmm_overhead_mib` per
  sandbox (default three quarters of host memory);
- `host.vcpu_budget` (default twice the host's CPUs; each jail's `cpu.max`
  still caps its own share).

A create that would exceed any of them first evicts idle pooled sandboxes,
then gets `429` with `Retry-After`. A key over its own quota gets `429` with
the code `quota` and no `Retry-After`.

**Limits per sandbox.** `sandbox.defaults` applies when a create does not ask,
and `sandbox.max` bounds what it may ask for. Disk and network rates apply to
every sandbox (`disk_mib_per_second`, `disk_iops`, `network.bandwidth_mbps`).

## Templates

A template is a snapshot of a booted guest for one image and shape (vCPUs and
memory). Sandboxes of that shape restore from it (about 100 ms) instead of
booting (about 700 ms). Build one per shape you serve, while `roundhoused` is
stopped or before restarting it, since templates are loaded at startup:

```bash
sudo roundhoused template build -config /etc/roundhouse/roundhoused.yaml -memory-mib 512 -vcpus 1
```

Templates live in `images/<image>/templates/<vcpus>cpu-<mib>mib/` with a
`manifest.json`. At startup each manifest is compared with the host: the
Firecracker version, CPU model, kernel and root file system digests, guest
kernel command line, and network and disk rate limits. A template that
differs in any of them is logged (`template rejected`, with the field) and
ignored, and sandboxes of its shape cold-boot. **Rebuild templates after
upgrading Firecracker, rebuilding the image, moving to another CPU model, or
changing a rate limit.**

## Warm pools

```yaml
sandbox:
  pools:
    - memory_mib: 512
      size: 8
```

The manager keeps `size` sandboxes of the shape started and idle, at most two
starting at a time per pool, and a create of the same image and shape claims
one in a few milliseconds. A claim applies the caller's egress policy and
health-checks the guest; one that fails is destroyed and the create restores
instead. Pools use only capacity callers are not using, and pooled sandboxes
are replaced after `pool_max_age`. Size a pool for the burst you want served
instantly: at 16 concurrent creates, a pool of 16 served about half of them
and the rest restored (see [benchmarks.md](benchmarks.md)).

## Run

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now roundhoused
curl -fs http://127.0.0.1:8480/readyz
```

At startup `roundhoused` removes every Roundhouse jail, cgroup, network
namespace, and sandbox directory left by a previous run, installs its
nftables table, loads templates, and only then answers `/readyz` and serves.
Sandboxes do not outlive the control plane: on `SIGTERM` it stops accepting
requests, lets requests in flight finish (`shutdown_timeout`), and destroys
every sandbox.

The API listens on `127.0.0.1:8480` by default. It speaks plain HTTP; put it
behind a TLS-terminating proxy before exposing it beyond the host.

## Observe

**Metrics** at `GET /metrics` (Prometheus text format, unauthenticated; it
carries no sandbox IDs or key names):

| Metric | What to watch |
|---|---|
| `roundhouse_sandbox_creates_total{path,outcome}` | Rising `capacity` or `quota` outcomes; `failed` at all |
| `roundhouse_sandbox_start_seconds{path}` | Start latency per path; `cold` where you expect `restore` means a rejected or missing template |
| `roundhouse_execs_total{outcome}` | `unresponsive` or `error`: guests that stopped answering |
| `roundhouse_sandbox_destroys_total{reason}` | `vm_exited` (guest crashed or panicked), `failed_claim`, `evicted` |
| `roundhouse_teardown_failures_total` | Any increase: resources a teardown could not release (retried by the reaper) |
| `roundhouse_pool_sandboxes{state}` | `idle` well below `target` for long means the pool cannot keep up |
| `roundhouse_pool_starts_total{outcome}` | `failed` starts |
| `roundhouse_budget_used` and `_limit{resource}` | Headroom |
| `roundhouse_http_requests_total{route,code}` | API errors by route |

**Logs** are JSON on stdout, one event per line, with the sandbox ID and route
on request lines. They never contain command lines, command output, file
contents, or environment values.

**Diagnostics.** When a sandbox's VM exits unexpectedly, fails its pool claim,
or fails to start, its console and Firecracker's own log are copied to
`<data_dir>/diagnostics/<time>-<sandbox>/` with a `reason.txt`, and a warning
names the directory. The newest twenty are kept. The console holds whatever
the guest wrote, so treat these files as sensitive.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Creates cold-boot although a template exists | The manifest no longer matches the host: look for `template rejected` in the startup log and rebuild the template. |
| A VM dies about 5 s after starting | The jail's file size limit is below the scratch disk's size, and the guest's first ext4 journal commit writes past it (`SIGXFSZ`). Roundhouse sizes the limit from the disk; this appears only with a scratch disk larger than expected. |
| Restored guests show the snapshot's time | Firecracker's `clock_realtime` has no effect under nested virtualization; the agent's restore call sets the clock from the host. Check the guest image carries a current agent. |
| `roundhoused` fails to start with a cgroup error | cgroup v2 is required, with `cpu`, `memory`, and `pids` enabled for the `roundhouse` parent. |
| Jail or disk errors mentioning links | The data directory must be on one Linux file system; images, templates, and jails are hard linked. |
| Leftover `rh-*` namespaces, links, or jails after a crash | Restart `roundhoused`; startup reconciliation removes them. |
| A sandbox is reaped as `vm_exited` | Read its directory under `diagnostics/`: a guest kernel panic or an out-of-memory kill of the VMM shows there. |

## Upgrade

1. Stop `roundhoused` (sandboxes are destroyed).
2. Install the new binaries. If the agent changed, rebuild the image with
   `images/build-rootfs.sh`; if Firecracker, the image, or a rate limit
   changed, rebuild each template.
3. Start `roundhoused` and check `/readyz`, the startup log for rejected
   templates, and `roundhouse_sandbox_creates_total` for the expected start
   paths.

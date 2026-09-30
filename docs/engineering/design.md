# Roundhouse: Design

Roundhouse is a service that runs untrusted code for LLM agents inside
Firecracker microVMs. An agent asks for a sandbox, runs commands and moves
files across many steps, and releases it; Roundhouse keeps each sandbox
hardware-isolated, bounded in every resource, and fast to start.

Status: design, before implementation. Decisions are recorded in
[docs/adr/](../adr/README.md); the phased plan is in plan.md.

## Contents

1. Problem
2. Goals and non-goals
3. Background: Firecracker
4. Prior art
5. Architecture
6. The sandbox lifecycle
7. Fast starts: snapshots and the warm pool
8. Networking
9. Storage
10. The guest agent and its protocol
11. Resource limits
12. Threat model and containment
13. API
14. Observability
15. Benchmark methodology
16. Testing strategy
17. Repository layout
18. Decision log
19. Open questions and risks
20. References

---

## 1. Problem

Agents write code and need to run it: a data analysis script, a test suite, a
shell pipeline. That code is untrusted by construction. It came from a model
that can be steered by anything in its context, including a web page or a
file it was asked to read. Running it on the host, or in an ordinary container
that shares the host kernel, turns every kernel bug into a way out.

Three requirements pull against each other:

- **Isolation.** A sandbox must not see or affect the host or other sandboxes.
  The strongest practical boundary is a hardware virtual machine.
- **Start latency.** Agents create sandboxes inside an interactive loop. A
  sandbox that takes seconds to boot is felt on every turn.
- **Statefulness.** An agent's work spans many steps: install a package, write
  a file, run it, inspect the output. The sandbox must keep its state between
  calls, then release every resource when the agent is done or goes away.

Firecracker microVMs give a hardware boundary with a device model small
enough to boot in about a hundred milliseconds. Restoring a pre-booted
snapshot is faster still. Roundhouse is the control plane around them:
lifecycle, a warm pool, networking, limits, and a guest agent that runs
commands.

## 2. Goals and non-goals

### Goals

- **One microVM per sandbox**, launched through Firecracker's jailer, each in
  its own cgroup, network namespace, and chroot, under its own unprivileged
  user.
- **An HTTP API** to create a sandbox, run commands with streamed output and a
  time limit, write and read files, list, and destroy. State persists across
  calls within a sandbox.
- **Fast starts** from template snapshots and a warm pool, with measured
  latency for three start paths: cold boot, snapshot restore, and warm pool
  claim.
- **Safe clones.** Many sandboxes restored from one snapshot must not share
  random state or network identity in ways that matter (section 7.3).
- **Hard resource limits** on vCPUs, memory, disk, processes, execution time,
  sandbox lifetime, idle time, and network bandwidth.
- **Egress control**: no network by default; opt-in internet access that still
  blocks the host, other sandboxes, private ranges, and cloud metadata.
- **Guaranteed cleanup.** Destroying or expiring a sandbox leaves no process,
  cgroup, namespace, device, or file behind, including after a crash of the
  control plane (reconciled at startup).
- **A containment test suite** that runs hostile workloads (fork bomb, memory
  hog, disk fill, CPU spin, network escape attempts) and asserts the host and
  neighbors are unaffected.
- **Published benchmarks** with the method written first: start latency
  percentiles per path, command round-trip latency, density, and noisy
  neighbor interference.

### Non-goals (for v1)

- Multiple hosts. The design keeps a node boundary (section 5) so a scheduler
  can be added, but v1 runs on one host.
- GPU passthrough, Windows guests, and nested virtualization inside sandboxes.
- Building templates from arbitrary container images. v1 builds templates
  from its own guest images (`roundhoused template build`); container images
  are future work.
- Domain-based egress allowlists. v1 filters by address; name-based filtering
  needs DNS or SNI inspection and is recorded as future work.
- Pause and resume of a user's sandbox across host restarts. Snapshots are
  used for fast starts; persisting a user's sandbox is a stretch goal.

## 3. Background: Firecracker

**Firecracker** is a virtual machine monitor built on Linux KVM for
multi-tenant serverless workloads. It emulates only a handful of devices
(virtio net, virtio block, virtio vsock, a serial console, a minimal keyboard
controller for reboot), which keeps its attack surface small and its boot
fast. One Firecracker process runs one microVM and is configured over a REST
API on a Unix socket.

**The jailer** starts Firecracker in production. It creates a chroot, joins
or creates namespaces (mount, PID, and an optional network namespace), places
the process in cgroups (v1 or v2), sets resource limits, drops to an
unprivileged uid and gid, and then execs Firecracker. The API socket ends up
inside the chroot.

**Snapshots** save a paused microVM as a guest memory file and a VM state
file; disks are the caller's responsibility. Loading a snapshot maps guest
memory from the file privately and lazily: pages are read on first touch, and
guests restored from the same file share unmodified pages through the page
cache. Snapshots are tied to the Firecracker version, CPU model, and host
kernel they were taken on. A `Uffd` memory backend lets an external process
serve page faults, which is how some providers stream memory over a network.

**Clones.** Firecracker's documentation is explicit that resuming many VMs
from one snapshot duplicates everything that was in memory: identifiers,
random number generator state, and network configuration. The VMGenID device
(guest Linux 5.18 and later) tells the guest kernel it was restored so it can
reseed its generator, but user-space state stays duplicated. Section 7.3
describes how Roundhouse handles this.

**vsock** gives host and guest a socket channel without a network. On the
host it appears as a Unix socket; the host connects and writes
`CONNECT <port>\n` to reach a guest port. Connections reset across snapshot
and restore, so the guest agent listens fresh after each resume.

**Kernel support.** Firecracker supports host and guest kernels 6.1 and 6.18
at the time of writing, with each version supported for at least two years.
The development host runs 6.18 (section 19).

## 4. Prior art

| System | Isolation | Start path | Notes |
|---|---|---|---|
| **E2B** (open-source runtime, Go) | Firecracker, one VM per sandbox, own cgroup and network namespace | Restore from template snapshot; templates are pre-booted VMs in object storage | In-VM daemon (envd) serves processes, files, and PTYs over Connect RPC; per-sandbox nftables egress firewall with domain allow and deny lists; idle sandboxes pause to object storage. Closest in shape to Roundhouse. |
| **AWS Lambda** | Firecracker | Snapshot restore (SnapStart) | Published work on restoring uniqueness after cloning snapshots. |
| **CodeSandbox** | Firecracker | Clone a running VM by copying memory with copy-on-write and serving pages with userfaultfd | Clones multi-gigabyte VMs in about two seconds. |
| **Modal Sandboxes** | gVisor containers | Container start | `exec` with streamed stdout and stderr, per-exec timeouts, sandbox lifetime and idle timeouts. A good model for the API surface. |
| **Vercel Sandbox, Fly Machines** | Firecracker | Boot or restore | Managed products; little published internals. |

A recent comparative study of code sandbox engines separates them into three
classes (microVM, user-space kernel such as gVisor, and OCI container) and
finds the classes differ cleanly on attack surface, while products within a
class do not; it also finds that downstream patch adoption, not the engine
itself, is the weakest link.

**What Roundhouse does differently.** It is not trying to be a product. It is
a single-host, fully measured implementation whose deliverables include the
evidence: a containment suite that runs hostile workloads against every build,
clone safety handled explicitly and tested, and start-latency and density
numbers taken with a method written down before the results.

## 5. Architecture

```text
agent / SDK ──HTTP──> roundhoused ──────────────────────────────────────────────┐
                        │ API (auth, validation, streaming)                     │
                        │ sandbox manager (lifecycle, reaper, reconcile)        │
                        │ warm pool (per template)        admission (capacity)  │
                        │ network manager (netns, tap, veth, nftables)          │
                        │ storage (template images, per-sandbox scratch disks)  │
                        │ vmm driver (jailer launch, Firecracker API over UDS)  │
                        └──────────┬────────────────────────────────────────────┘
                                   │ one per sandbox
                         jailer ─> firecracker (chroot, cgroup, netns, uid)
                                   │ KVM
                         ┌─────────┴──────────┐
                         │ guest kernel       │
                         │ roundhouse-agent ◄─┼── vsock: exec, files, health
                         │ user workload      │
                         └────────────────────┘
```

- **roundhoused** is the control plane. It runs on the host with the
  privileges it needs to create namespaces, tap devices, cgroups, and
  nftables rules (root in v1; a narrower capability set is future work), and
  it never executes sandbox code itself.
- **roundhouse-agent** is a static Go binary inside every guest. It is the only
  thing the host talks to inside the VM, over vsock.
- The **node boundary** is the set of packages under `internal/` that manage
  one host's VMs. A future scheduler would place sandboxes on nodes and route
  API calls to them; nothing below the API layer assumes a single node except
  the admission controller's view of capacity.

Package dependencies point one way: `api` depends on `sandbox`; `sandbox`
depends on `pool`, `vmm`, `netns`, `storage`, and `agentclient`; nothing
depends on `api`. `agentproto` holds wire types shared by host and guest and
depends on nothing internal.

## 6. The sandbox lifecycle

```text
          create
            │
   (warm pool claim, restore, or cold boot)
            ▼
 ┌──────► ready ──exec──► busy ──done──┐
 │          │  ▲                       │
 │          │  └───────────────────────┘
 │     timeout, idle, delete, VM exit
 │          ▼
 │      stopping ──► stopped (resources released, record kept briefly)
 │
 └── failed start: resources released, error returned
```

- Every sandbox has an absolute **deadline** (lifetime limit) and an **idle
  timeout** reset by each API call. A reaper goroutine enforces both.
- A sandbox that is `busy` counts active executions; idle time only accrues
  when none are running.
- `stopping` is idempotent and ordered: stop accepting calls, kill every
  process in the sandbox's cgroup by writing `1` to its `cgroup.kill` file
  (graceful shutdown is not needed; the sandbox is disposable), wait for the
  cgroup to empty, remove the cgroup, remove the network namespace and its
  devices, delete the jail directory and scratch disk. Each step runs even if
  an earlier one failed, and errors are collected.
- `cgroup.kill` rather than a signal to a PID: with `--new-pid-ns`, Firecracker
  is the init process of its PID namespace, and the kernel discards signals to
  a namespace's init that it has no handler for, so `SIGTERM` from the host is
  silently ignored. Killing by cgroup also catches any process the jailer
  left behind.
- If the Firecracker process exits unexpectedly, the sandbox moves to
  `stopping` and in-flight calls fail with a clear error.
- **Reconciliation at startup**: roundhoused scans for jails, cgroups, and
  network namespaces that carry its naming prefix but no live record, and
  removes them. A crash of the control plane therefore leaks nothing past its
  next start.

## 7. Fast starts: snapshots and the warm pool

### 7.1 Three start paths

| Path | What happens | Expected cost |
|---|---|---|
| Cold boot | Launch jailer and Firecracker, configure, boot the kernel, start the agent | Kernel boot plus user space; the baseline |
| Snapshot restore | Launch jailer and Firecracker, load the template snapshot, resume, re-establish vsock | Process start plus lazy memory mapping |
| Warm pool claim | Hand out a sandbox restored ahead of time | A map lookup and a health check |

All three are measured (section 15). Only restore and claim are used by
default; cold boot exists to build templates and as the benchmark baseline.

### 7.2 Templates

A template is a snapshot of a booted guest for one image and one VM shape
(vCPUs and memory): the VM state, the guest memory file, and the guest's
scratch disk as it was at the moment of the snapshot. The scratch disk is
part of the template, not the image's pristine disk: the snapshotted page
cache and ext4 journal describe that disk, and restoring them over any
other would corrupt the file system. Because the VM is paused before the
snapshot, disk and memory are a consistent pair without syncing, and no
process runs in the guest to sync them (section 7.3).

Templates are built by `roundhoused template build`, which cold-boots the
guest, waits for the agent to listen, closes every host connection, pauses,
snapshots, and installs the files under
`images/<image>/templates/<vcpus>cpu-<mib>mib/` by a directory rename, so a
reader never sees a partial template. A manifest records the Firecracker
version, CPU model, SHA-256 digests of the kernel and root file system, the
guest kernel command line, and the network rate limit (device configuration
is part of a snapshot). At startup each manifest is compared with the host;
a template that differs in any field is logged and ignored, and sandboxes of
its shape cold-boot instead of restoring into a host it was not built for.

Restoring requires the snapshot's disk paths to exist. Because each sandbox
runs in its own jail, the paths are jail-relative and identical for every
sandbox; each jail gets hard links to the shared read-only files (root file
system, VM state, memory file) and its own sparse copy of the template's
scratch disk.

### 7.3 Clone safety

Every sandbox restored from a template starts with the same memory. Roundhouse
deals with each consequence explicitly:

- **Kernel randomness**: the guest kernel is 6.1 or later with VMGenID
  enabled, so the kernel reseeds its generator on restore. Every VM also has a
  virtio-rng entropy device (Firecracker's `/entropy`), rate limited, which
  keeps feeding the guest kernel fresh host entropy after a restore. In addition the
  agent, on its first request after restore, receives fresh entropy from the
  host (from `crypto/rand`) and writes it to `/dev/urandom`, crediting none,
  so user space reading from the kernel afterwards is unique.
- **Agent state**: the agent generates nothing random before the snapshot is
  taken; any per-sandbox identity is assigned after restore.
- **Time**: a restored guest's wall clock reads the time its snapshot was
  taken. Snapshots are loaded with `clock_realtime`, which corrects this
  where KVM supports it, but it has no effect under nested virtualization
  (measured: a 3 s old snapshot restored 3 s behind). The agent's restore
  call therefore also carries the host's wall clock, and the agent sets
  `CLOCK_REALTIME` from it; the integration suite checks the skew of clones
  from an aged snapshot.
- **Network identity**: identical guest IP and MAC addresses are expected and
  safe, because each sandbox has its own network namespace (section 8).
- **User-space state in the template** (for example, a Python interpreter's
  hash seed) is avoided by snapshotting before any user process starts.

The containment suite includes a test that restores many sandboxes and checks
that `/proc/sys/kernel/random/uuid` and bytes from `/dev/urandom` differ
across them.

### 7.4 The warm pool

- A pool is configured per image and shape with a target size. The manager
  starts sandboxes (by restore, or by cold boot if no template matches)
  until each pool is full, at most two at a time per pool, and a create of
  that image and shape claims one in constant time.
- Pooled sandboxes are started with egress `none`. A claim applies the
  caller's egress policy to the running sandbox (the veth pair, routes, and
  rules are added to its namespace; the guest's configuration does not
  change) and sends the agent's restore call again, which reseeds the
  guest, sets its clock, and doubles as the claim's health check. A
  sandbox that fails its claim is destroyed and the create falls back to a
  fresh start.
- Pooled sandboxes hold uids and budget like any other, but callers come
  first: the filler never evicts and stops when the host is full, and a
  create that finds the host full evicts idle pooled sandboxes, oldest
  first, before answering `429`.
- Pooled sandboxes older than `pool_max_age` are recycled so the pool does
  not age, and a failed start pauses that pool's refill for a few seconds.
- Pooled sandboxes are running and idle. Their memory cost is mostly shared:
  guest memory is mapped privately from the snapshot file, so untouched pages
  are shared page cache. Density measurements report proportional set size,
  not resident size, for this reason.
- Limits a caller asks for (vCPUs, memory) must match a pool's shape to be
  claimed; otherwise the request falls back to a restore.

## 8. Networking

Each sandbox gets its own **network namespace** containing a tap device for
the VM. Because namespaces are separate, every sandbox can use the same tap
name, guest IP, and MAC address, which is exactly what restored clones need.

```text
guest eth0 172.16.0.2 ── tap0 172.16.0.1 [netns rh-<id>] veth-g 10.200.x.2
                                                              │
                                             host veth-h 10.200.x.1 ── nftables ── uplink
```

- A veth pair connects the namespace to the host. Host-side addresses come
  from a private /30 per sandbox, allocated and released by the network
  manager.
- **Egress policy**, enforced by nftables on the host:
  - `none` (default): the namespace has no route out; the veth pair is not
    created at all.
  - `internet`: NAT to the uplink, with drops for the host itself, other
    sandboxes' ranges, RFC 1918 and link-local ranges, and cloud metadata
    (`169.254.169.254`).
  - `allowlist`: `internet`, restricted to listed addresses and ports.
- **Ingress** is not offered in v1; sandboxes cannot accept connections from
  outside. Port forwarding to a sandbox is future work.
- **Bandwidth** is bounded with Firecracker's per-device rate limiter on the
  virtio net device, which also bounds packets per second.
- DNS inside the guest points at a public resolver only when egress is
  enabled; with egress `none` there is no resolver.

## 9. Storage

- **Root filesystem**: one read-only ext4 image per template, shared by all
  its sandboxes through hard links into each jail. The guest mounts it
  read-only.
- **Scratch disk**: a per-sandbox writable ext4 image, sparse, created by
  copying a pre-formatted pristine image with sparse-aware copying (fast,
  because the file is mostly holes). The guest overlays it on the root file
  system with overlayfs, so the sandbox sees a normal writable root.
- **Disk size limit** is the scratch image's size; a full disk fails writes in
  the guest and cannot grow the host file beyond its preallocated maximum.
  The jailer's `fsize` limit caps any file Firecracker itself writes. It
  bounds write offsets, so it is sized to the larger of the scratch disk and
  guest memory (a snapshot's memory file); below the disk size, the guest's
  first mid-disk ext4 journal commit would kill the VMM with SIGXFSZ.
- **Block I/O** is bounded with Firecracker's rate limiter on the virtio block
  device (bandwidth and operations per second).

## 10. The guest agent and its protocol

The agent runs as a service started by the guest's init, listens on a fixed
vsock port, and serves **HTTP/1.1 over vsock**. HTTP was chosen over a custom
framing because it gives streaming (chunked bodies), request bodies of any
size, and standard tooling for tests; the host dials the vsock Unix socket,
writes `CONNECT <port>\n`, reads `OK`, and then speaks HTTP on the connection.

Endpoints (all under `/agent/v1`):

- `POST /exec`: runs a command (argv, environment, working directory, optional
  stdin bytes, time limit). The response streams newline-delimited JSON
  frames: `stdout` and `stderr` chunks (base64, bounded size), then one `exit`
  frame with the exit code, whether the time limit killed it, and the
  duration. The process runs in its own process group; the whole group is
  killed on timeout or when the host cancels the request.
- `PUT /files?path=...` and `GET /files?path=...`: write and read a file,
  streamed, with a size limit.
- `GET /health`: liveness and the agent's view of its boot or restore.
- `POST /restore`: called once after a restore; receives entropy and the
  sandbox identity, and reseeds.

The agent is the only guest component Roundhouse trusts to be well-behaved,
and it trusts nothing it receives from processes in the guest. The host
treats everything the agent returns as untrusted input: sizes are bounded,
frames are validated, and a misbehaving agent is a failed sandbox, not a
host problem.

## 11. Resource limits

| Resource | Mechanism | Where |
|---|---|---|
| vCPUs | Firecracker machine config | VM |
| CPU time | cgroup v2 `cpu.max` on the jail | host |
| Memory | VM memory size; cgroup `memory.max` above it for VMM overhead | VM and host |
| Processes | guest workload cgroup `pids.max` (1024); cgroup `pids.max` for the VMM | guest and host |
| Guest memory for commands | guest workload cgroup `memory.max`, 32 MiB below guest memory | guest |
| Disk space | scratch image size | host file |
| Disk I/O | Firecracker block rate limiter on each drive (bytes and operations per second) | VMM |
| Network | Firecracker net rate limiter; egress policy | VMM and host |
| Execution time | per-exec limit enforced by the agent, and by the host as a backstop | guest and host |
| Console output | serial console written to a file in the jail through Firecracker's `/serial` device with a byte-rate limit; set again before every restore, because the output path is not part of a snapshot | VMM |
| Lifetime, idle | reaper | host |
| Host capacity | admission: total memory and vCPU budget, maximum sandboxes | host |

**Guest-side workload limits.** The agent, as init, mounts cgroup v2 and
creates a `workload` cgroup; every command starts directly inside it
(`clone3` with `CLONE_INTO_CGROUP`, so not even a moment is spent outside).
Its memory and process limits sit below the guest's own, so a command that
exhausts memory or processes is stopped by the guest kernel inside the
workload while the agent, outside it, keeps answering. The host's limits on
the VM are the security boundary; these keep a sandbox usable after its
workload misbehaves.

**Execution time** is enforced twice: the agent kills a command's process
group at its time limit, and the host abandons the call ten seconds after
that limit if the guest has not answered, so a hung guest cannot hold an
API call open.

Admission rejects a create that would exceed the host budget with `429` and
`Retry-After`, rather than overcommitting memory. Each sandbox is charged its
guest memory plus the configured VMM overhead and its vCPUs. By default the
memory budget is three quarters of host memory and the vCPU budget twice the
host's CPUs (idle agent sandboxes rarely use their vCPUs, and each jail's
`cpu.max` still caps its own); both are configurable.

Per-API-key quotas cap how many sandboxes one caller can hold
(`api_keys[].max_sandboxes`), so one caller cannot take the whole host. A
create over the quota gets `429` with the code `quota`, distinct from
`capacity`, because waiting does not help: the caller must delete one of its
own sandboxes first.

## 12. Threat model and containment

**Adversary**: arbitrary code running as root inside the guest, including
code that tries to exhaust resources, reach the network, attack the virtual
devices, or persist beyond the sandbox's life.

**Must hold**:

1. No access to the host's files, processes, or network services.
2. No access to other sandboxes' memory, disks, or network traffic.
3. No effect on other sandboxes beyond the configured share of CPU, memory,
   disk, and network.
4. Nothing survives destruction of the sandbox.
5. No egress unless the sandbox was created with it, and never to the host,
   private ranges, or metadata endpoints.

**Layers**: KVM hardware virtualization; Firecracker's minimal device model
and its default seccomp filters; the jailer's chroot, namespaces, cgroups,
and per-sandbox uid; nftables egress rules; rate limiters; timeouts; and the
reaper.

**Out of scope**: hardware side channels (production guidance is to disable
SMT and kernel same-page merging; the development host does not), host
kernel and KVM zero-days, and denial of service against the API itself beyond
admission limits.

**Containment suite** (`test/containment`, runs against real microVMs; each
test asserts the host and a neighbor sandbox stay healthy, with the
neighbor's command latency measured throughout, and ends with a leak scan).
Measured on the development host (nested KVM), where a neighbor's p95
latency for a trivial command stayed between 2 and 6 ms under every
attack:

| Attack | Expected outcome |
|---|---|
| Attack | Expected outcome | Measured |
|---|---|---|
| Fork bomb | Workload capped; host process count unchanged; sandbox usable afterwards | Workload peaked at its 1024-process cap; the agent answered right after |
| Memory hog | Guest OOM killer acts inside the workload; the host never OOM-kills the VMM | Killed in the guest in under 2 s; host `oom_kill` 0; jail `memory.peak` below `memory.max` |
| Disk fill | Writes fail with no space; host file never exceeds the image size | Stopped at 1931 MiB allocated of 2048 MiB; the sandbox wrote again after freeing space |
| CPU spin on all vCPUs | CPU use held to the jail's `cpu.max` | 12.0 s of CPU in 6.0 s with 2 vCPUs |
| Console flood | Console log growth bounded by its rate | 109 KB in 5 s at 16 KiB/s, within one bucket of the rate |
| Bandwidth flood | Throughput capped at the configured rate | Steady 0.999 MB/s at a 1.000 MB/s cap; the first second carries a burst of about 2.6 MB |
| Guest kernel panic | VM exits, sandbox reaped, resources released | Reaped within 3 s |
| Destroy, then create | Nothing written by a destroyed sandbox is visible to a later one, pooled or not | Holds |
| Egress with policy `none`; host, neighbor, metadata, private ranges with `internet` | No route; dropped | Integration suite (`network_test.go`) |
| Many restores from one snapshot | Random output and UUIDs differ across sandboxes | Integration suite (`pool_test.go`) |
| Destroy, then scan | No process, cgroup, namespace, tap, veth, nftables rule, or file left | Checked after every test |

## 13. API

JSON over HTTP, bearer API keys (compared in constant time), versioned under
`/v1`.

| Method and path | Purpose |
|---|---|
| `POST /v1/sandboxes` | Create: template, vCPUs, memory, disk, lifetime, idle timeout, egress policy |
| `GET /v1/sandboxes` | List the caller's sandboxes |
| `GET /v1/sandboxes/{id}` | State, limits, timestamps |
| `DELETE /v1/sandboxes/{id}` | Destroy (idempotent) |
| `POST /v1/sandboxes/{id}/exec` | Run a command; streams NDJSON frames as in section 10 |
| `PUT /v1/sandboxes/{id}/files?path=` | Write a file (body is the content) |
| `GET /v1/sandboxes/{id}/files?path=` | Read a file |
| `GET /healthz`, `GET /readyz`, `GET /metrics` | Operations |

Errors use one JSON shape with a stable machine-readable code. A request for
a sandbox that belongs to another key returns `404`, not `403`, so existence
is not revealed.

## 14. Observability

- Prometheus metrics with the `roundhouse_` prefix: sandbox creates by start
  path and outcome, start latency histogram per path, exec latency and
  outcome, active sandboxes by state, pool size and refill rate, admission
  rejections, reaper actions, cleanup failures, and host budget in use.
- Structured JSON logs with the sandbox ID on every line. **Command lines,
  file contents, and command output are never logged**; logs record sizes,
  exit codes, and durations. Agents send secrets through sandboxes.
- Firecracker's own logs and metrics are written inside each jail with a size
  cap and collected on failure for diagnosis.

## 15. Benchmark methodology

Written before any results. All numbers state the host (CPU model, cores,
memory, kernel, and whether it is nested virtualization) and are taken with
the service running normally, not with test hooks.

1. **Start latency** per path (cold boot, restore, warm claim): time from the
   API receiving `POST /v1/sandboxes` to the sandbox answering its first
   `exec` of `true`. 1,000 sequential creates, and 1,000 creates at 16-way
   concurrency; report p50, p90, p99, and maximum.
2. **Exec round trip**: `echo` in a ready sandbox, 10,000 calls; p50 and p99.
3. **Density**: sandboxes held idle until creation fails or host memory
   pressure appears; report the count and proportional set size per sandbox.
4. **Noisy neighbor**: exec round trip in sandbox A while sandbox B runs each
   containment workload; report p99 inflation against an idle neighbor.
5. **Baselines**, named and run on the same host: container start with `runc`
   (Docker) and, if available, with gVisor (`runsc`). These are context for
   the isolation cost, not claims of improvement over those systems.

Three runs per configuration; tables report the median run with the range.

## 16. Testing strategy

- **Unit tests** for everything with logic: lifecycle state machine, reaper,
  pool, admission, IP allocation, nftables rule rendering, protocol framing,
  config. The VMM and network layers sit behind small interfaces with fakes
  so the manager is tested without KVM.
- **Fuzz tests** for the agent protocol decoders on both sides and for path
  handling in file endpoints.
- **Integration tests** (build tag `integration`) boot real microVMs; they
  skip with a clear message when `/dev/kvm` or root is unavailable.
- **Containment suite** (build tag `containment`) as in section 12.
- **Leak checks**: goroutine leaks with goleak in unit tests; host resource
  leaks (processes, namespaces, cgroups, devices, rules, files) scanned after
  every integration and containment test.

## 17. Repository layout

```text
cmd/
  roundhoused/          control plane binary (API, manager, template build)
  roundhouse-agent/     guest agent (linux, static)
  rhctl/                command-line client for humans and scripts
  rhbench/              benchmark driver
internal/
  api/                  HTTP handlers, auth, streaming
  sandbox/              lifecycle, reaper, warm pools, templates, booter
  admission/            host capacity budget
  node/                 assembles one host's machinery from configuration
  vmm/                  jailer launch and the Firecracker API client
  netns/                namespaces, tap, veth, nftables, address allocation
  storage/              data layout: images, templates, scratch disks
  agentproto/           wire types shared by host and guest
  agentclient/          host side of the agent protocol over vsock
  agent/                guest agent implementation
  config/               configuration
  telemetry/            logging and metrics
images/                 guest kernel config and rootfs build scripts
deploy/                 host setup (packages, kvm access, Firecracker install)
test/
  integration/          real-VM tests
  containment/          hostile-workload suite
bench/                  benchmark scripts and committed results
docs/
  engineering/          design and coding standards
  adr/                  architecture decision records
```

## 18. Decision log

| ADR | Decision |
|---|---|
| 0001 | Firecracker microVMs, not containers or gVisor, for isolation |
| 0002 | Go for the control plane and the guest agent |
| 0003 | Snapshot restore and a warm pool for starts; cold boot as baseline only |
| 0004 | HTTP/1.1 over vsock for the agent protocol |
| 0005 | One network namespace per sandbox, identical guest addressing |
| 0006 | Shared read-only rootfs plus a per-sandbox sparse scratch disk |
| 0007 | Single host in v1, with a node boundary kept in the code |
| 0008 | Develop on WSL2 with nested KVM; state it on every benchmark |

## 19. Open questions and risks

| Item | Type | Plan |
|---|---|---|
| Nested virtualization (WSL2 on Hyper-V) makes absolute latency and density unrepresentative of bare metal | Risk | State the environment on every number; optional validation on a bare-metal cloud host later |
| Snapshot restore latency under nested virtualization is unknown | Question | Measure in the Phase 0 spike before committing to targets |
| Guest kernel with VMGenID and the virtio drivers needed | Question | Start from Firecracker's CI kernel configs for 6.1 or 6.18; verify VMGenID in the spike |
| The control plane runs as root in v1 | Risk | Isolate privileged operations in `netns` and `vmm`; document the capability set a narrower deployment needs |
| Hard links into jails require the data directory and jail root on one file system | Constraint | Keep both under one directory on the WSL ext4 volume; check at startup |
| Clone safety relies on the guest kernel honoring VMGenID | Risk | Host-supplied entropy as a second mechanism; containment test verifies uniqueness |

## 20. References

- Firecracker: [design](https://github.com/firecracker-microvm/firecracker/blob/main/docs/design.md),
  [getting started](https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md),
  [jailer](https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md),
  [production host setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md),
  [snapshot support](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md),
  [network for clones](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/network-for-clones.md),
  [vsock](https://github.com/firecracker-microvm/firecracker/blob/main/docs/vsock.md),
  [network setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/network-setup.md),
  [kernel support policy](https://github.com/firecracker-microvm/firecracker/blob/main/docs/kernel-policy.md)
- E2B runtime: https://github.com/e2b-dev/runtime
- CodeSandbox: [cloning microVMs with userfaultfd](https://codesandbox.io/blog/cloning-microvms-using-userfaultfd),
  [cloning a running VM in 2 seconds](https://codesandbox.io/blog/how-we-clone-a-running-vm-in-2-seconds)
- Restoring uniqueness in microVM snapshots: https://arxiv.org/abs/2102.12892
- Benchmarking serverless function snapshots: https://arxiv.org/abs/2101.09355
- Modal Sandboxes: https://modal.com/docs/guide/sandboxes
- A comparative security study of code sandbox engines: https://arxiv.org/abs/2606.08433

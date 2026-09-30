# Benchmarks

Measured on 2026-09-30 with the method in
[design.md section 15](engineering/design.md#15-benchmark-methodology), which
was written before any results. Raw results, one JSON line per measurement,
are in [bench/results/2026-09-30/](../bench/results/2026-09-30/); rerun them
with `sudo bash bench/setup-baselines.sh && sudo bash bench/run.sh`.

## Host, and what it means for the numbers

| | |
|---|---|
| CPU | Intel Core i9-13900K, 32 logical CPUs visible to the Linux VM |
| Memory | 31 GiB visible to the Linux VM |
| Kernel | 6.18.33.2-microsoft-standard-WSL2 |
| Virtualization | **Nested**: Firecracker runs under KVM inside WSL2, which is itself a Hyper-V VM on Windows 11 |
| Firecracker | v1.17.0, guest kernel 6.1, Ubuntu 24.04 root file system |
| Baselines | runc 1.5.2 and gVisor release-20260921.0 (runsc), same root file system |

Nested virtualization makes every guest page fault and VM exit more
expensive than on bare metal, so these numbers are an upper bound for the
same hardware without the Windows layer. They were not measured on bare
metal, and no comparison with other sandbox services is claimed.

Every sandbox is 1 vCPU and 256 MiB with egress `none`, created through the
public API by `rhbench` on the same host. Each configuration ran three times;
tables show the median run with the range across runs in parentheses.
Latencies are in milliseconds.

## Start latency

Time from sending `POST /v1/sandboxes` to the sandbox answering its first
`exec` of `true`: what a caller waits for before it can run anything. 1,000
creates per run, each deleted afterwards (not timed).

| Path | Concurrency | p50 | p90 | p99 | max |
|---|---|---|---|---|---|
| Warm pool claim | 1 | 3.2 (3.2–3.2) | 4.5 (4.0–4.8) | 8.0 (6.3–9.4) | 46 (34–48) |
| Snapshot restore | 1 | 108 (107–111) | 125 (123–129) | 189 (141–190) | 359 (230–387) |
| Cold boot | 1 | 730 (720–733) | 779 (777–781) | 795 (794–797) | 835 (825–894) |
| Pool, drained by load | 16 | 78 (78–80) | 110 (110–111) | 133 (128–133) | 207 (145–236) |
| Snapshot restore | 16 | 88 (83–89) | 118 (116–121) | 236 (224–533) | 249 (233–780) |
| Cold boot | 16 | 724 (712–745) | 771 (762–776) | 874 (840–876) | 896 (868–930) |

- **Warm pool claim** was measured with the pool refilled before each create
  (`-wait-pool`), so every create is a claim: the latency of the path, not
  of the refill rate.
- **Pool, drained by load**: at 16-way concurrency the 16-sandbox pool
  empties faster than it refills, and about 47% of creates were claims and
  53% fell back to a restore. This is the honest steady state for a pool
  sized below the arrival rate.
- **Restore** includes starting a fresh jailed VMM, loading the snapshot,
  assigning the clone's identity and entropy, and one probe command (below).
  Concurrency 16 has a lower median than sequential because the restores
  overlap, while the tail grows.
- **Cold boot** is the baseline and the template builder's path; it is not
  used when a template matches.
- No create failed in any of these runs.

### What the restore path pays for, and a fix it prompted

The first command in a restored guest faults in the memory its exec path
touches, about 25 ms on this host. And after a restore, the guest resets
every vsock connection open when it handles Firecracker's transport reset
event, which can lag the restore; in the first full run this lost the first
agent call of about one restore in a thousand at 16-way concurrency. Every
restored guest now runs one probe command before it is handed over, retried
once if its connection is lost, and the identity call retries the same way.
That removed the failures (none in 12,000 restore-path creates across the
final runs) at a cost of about 10 ms on the restore path's median, and it
takes the fault cost off warm pool claims entirely.

## Exec round trip

`echo hi` in one ready sandbox, 10,000 calls per run, through the API.

| p50 | p99 | max |
|---|---|---|
| 1.24 (1.22–1.27) | 1.94 (1.80–2.15) | 31 (8–56) |

## Density

Sandboxes created one after another and held idle, until the server refused
or the host's available memory fell to 4 GiB. The server's admission budget
was raised above what the host holds so that memory, not admission, would be
the limit.

| Sandboxes held | Proportional set size per sandbox | Resident set size per sandbox | Host available memory used per sandbox |
|---|---|---|---|
| 1,000 in every run | 8.3 MiB | 31.0 MiB | 6.8 (6.8–7.2) MiB |

All three runs stopped at the configured 1,000-sandbox cap, not at memory:
the host still had over 18 GiB available. Each guest has 256 MiB, but its
memory is mapped privately from the snapshot file, so pages a guest never
writes are shared page cache across all clones. Proportional set size
charges each shared page to its sharers, which is why it, not resident set
size, is the density figure (design.md section 7.4). An idle sandbox that
starts writing memory costs more; this measures idle sandboxes only.

## Noisy neighbor

Exec round trip in sandbox A for 10 seconds, while a fresh 2-vCPU sandbox B
runs each hostile workload from the containment suite, against A with an
idle neighbor. p99 inflation is A's p99 divided by its idle p99.

| Neighbor B runs | A's p50 | A's p99 | p99 inflation |
|---|---|---|---|
| nothing (idle) | 1.19 | 1.75 | 1.00 |
| CPU spin on all vCPUs | 1.19 | 1.66 | 0.93–1.13 |
| Fork bomb | 1.21 | 1.70 | 0.96–1.19 |
| Memory hog | 1.19 | 1.69 | 0.93–0.97 |
| Disk fill | 1.19 | 1.80 | 0.98–1.07 |
| Console flood | 1.18 | 1.70 | 0.96–0.98 |

No workload moved A's latency beyond run-to-run noise, and no call failed.
The host has far more CPUs than the two sandboxes use, so this shows that
each sandbox is confined to its own share; it does not show behavior on an
oversubscribed host.

## Baselines: containers on the same host

The same Ubuntu root file system run as an OCI bundle. Start is `runtime run`
of a container whose process is `true`, to exit; exec is `runtime exec echo
hi` in a running container. 1,000 each per run.

| Runtime | Start p50 | Start p99 | Exec p50 | Exec p99 |
|---|---|---|---|---|
| runc 1.5.2 | 9.5 (9.5–9.6) | 11.1 (10.9–11.2) | 7.5 (7.5–7.6) | 22.4 (22.1–25.8) |
| gVisor runsc | 28.4 (28.3–28.4) | 32.1 (31.7–32.9) | 6.0 (6.0–6.1) | 7.4 (7.3–7.7) |
| Roundhouse, warm pool | 3.2 | 8.0 | 1.24 | 1.94 |
| Roundhouse, restore | 108 | 189 | 1.24 | 1.94 |

These are context for the cost of VM isolation, not a claim of improvement
over either system, and the measurements differ in kind:

- Container start creates and runs a container from nothing, while a pool
  claim hands over a VM started earlier; the fair comparison for cold
  isolation cost is restore (108 ms) or cold boot (730 ms) against 9.5 ms
  for runc and 28 ms for gVisor.
- Container exec spawns the runtime binary per call, and Roundhouse's exec is
  a request to an agent already running in the guest; the gap mostly
  measures that design difference, not isolation.
- The containers share the host kernel (runc) or run gVisor's user-space
  kernel; Roundhouse gives each sandbox its own kernel behind KVM.

## Not measured

- Bare-metal (non-nested) performance.
- Guests that write much of their memory, which erodes the sharing that makes
  idle density high.
- Sustained throughput beyond 16-way concurrency, and hosts oversubscribed
  on CPU.
- Other sandbox services.

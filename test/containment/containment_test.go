//go:build containment

// Package containment runs hostile workloads in real microVMs and checks that
// the host and a neighbor sandbox are unaffected beyond the configured
// limits (docs/engineering/design.md section 12). Egress attacks are covered
// by the integration suite's network tests.
package containment

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/shusingh/roundhouse/internal/agentproto"
	"github.com/shusingh/roundhouse/internal/apiclient"
	"github.com/shusingh/roundhouse/internal/netns"
	"github.com/shusingh/roundhouse/test/harness"
)

// neighborBound is the worst p95 latency of a trivial command in a neighbor
// sandbox that any attack may cause. Nested virtualization is noisy, so the
// bound is loose; an unconfined attack degrades the neighbor far past it.
const neighborBound = 500 * time.Millisecond

func create(ctx context.Context, t *testing.T, s *harness.Stack, opts apiclient.CreateOptions) apiclient.Sandbox {
	t.Helper()
	sb, err := s.Client.Create(ctx, opts)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return sb
}

// jailCgroup is the host cgroup holding a sandbox's VMM.
func jailCgroup(id string) string { return "/sys/fs/cgroup/roundhouse/rh-" + id }

// cgroupValue reads a cgroup file's single number, or the value of key in a
// flat keyed file.
func cgroupValue(t *testing.T, file, key string) int64 {
	t.Helper()
	data, err := os.ReadFile(file) //nolint:gosec // G304: a cgroup file of the test's own sandbox
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if key == "" && len(fields) == 1 {
			if fields[0] == "max" {
				return 1 << 62
			}
			n, err := strconv.ParseInt(fields[0], 10, 64)
			if err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			return n
		}
		if len(fields) == 2 && fields[0] == key {
			n, _ := strconv.ParseInt(fields[1], 10, 64)
			return n
		}
	}
	t.Fatalf("%s has no value %q", file, key)
	return 0
}

// hostProcesses counts processes on the host.
func hostProcesses(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err == nil {
			n++
		}
	}
	return n
}

// latencySampler runs a trivial command in a sandbox over and over and
// records how long each takes.
type latencySampler struct {
	stop     chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	samples  []time.Duration
	failures int
}

func sampleLatency(ctx context.Context, c *apiclient.Client, id string) *latencySampler {
	s := &latencySampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for {
			select {
			case <-s.stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			start := time.Now()
			_, err := c.Exec(ctx, id, apiclient.ExecOptions{Argv: []string{"true"}}, nil)
			s.mu.Lock()
			if err != nil {
				s.failures++
			} else {
				s.samples = append(s.samples, time.Since(start))
			}
			s.mu.Unlock()
		}
	}()
	return s
}

// finish stops sampling and checks the neighbor stayed healthy.
func (s *latencySampler) finish(t *testing.T) {
	t.Helper()
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failures > 0 {
		t.Errorf("neighbor: %d of %d calls failed during the attack", s.failures, s.failures+len(s.samples))
	}
	if len(s.samples) == 0 {
		t.Error("neighbor: no successful calls during the attack")
		return
	}
	slices.Sort(s.samples)
	p50, p95 := s.samples[len(s.samples)/2], s.samples[len(s.samples)*95/100]
	t.Logf("neighbor latency during the attack: p50 %v, p95 %v, max %v over %d calls",
		p50, p95, s.samples[len(s.samples)-1], len(s.samples))
	if p95 > neighborBound {
		t.Errorf("neighbor p95 latency %v exceeds %v", p95, neighborBound)
	}
}

// waitResponsive waits for a sandbox to run a command again after an attack.
func waitResponsive(ctx context.Context, t *testing.T, c *apiclient.Client, id string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var out strings.Builder
		exit, err := c.Exec(ctx, id, apiclient.ExecOptions{Argv: []string{"echo", "ok"}, TimeoutMS: 2000}, func(f *agentproto.Frame) error {
			out.Write(f.Data)
			return nil
		})
		if err == nil && exit.ExitCode == 0 && out.String() == "ok\n" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("sandbox %s did not become responsive after the attack", id)
}

// attack runs a hostile command with a time limit and returns its output and
// exit, which may be a time-out or a kill.
func attack(ctx context.Context, t *testing.T, c *apiclient.Client, id string, limit time.Duration, script string) (string, *agentproto.Frame) {
	t.Helper()
	return harness.ExecWith(ctx, t, c, id, apiclient.ExecOptions{Argv: []string{"bash", "-c", script}, TimeoutMS: limit.Milliseconds()})
}

func TestForkBomb(t *testing.T) {
	s := harness.Start(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	victim := create(ctx, t, s, apiclient.CreateOptions{MemoryMiB: 256})
	neighbor := create(ctx, t, s, apiclient.CreateOptions{MemoryMiB: 256})
	procs := hostProcesses(t)

	sampler := sampleLatency(ctx, s.Client, neighbor.ID)
	_, exit := attack(ctx, t, s.Client, victim.ID, 8*time.Second, "f(){ f | f & }; f; sleep 60")
	sampler.finish(t)
	t.Logf("fork bomb ended: %+v", exit)

	if after := hostProcesses(t); after > procs+20 {
		t.Errorf("host processes grew from %d to %d", procs, after)
	}
	if peak, limit := cgroupValue(t, jailCgroup(victim.ID)+"/pids.peak", ""), cgroupValue(t, jailCgroup(victim.ID)+"/pids.max", ""); peak > limit {
		t.Errorf("jail pids.peak %d exceeds pids.max %d", peak, limit)
	}
	waitResponsive(ctx, t, s.Client, victim.ID)
	out, _ := harness.ExecOut(ctx, t, s.Client, victim.ID, "cat", "/sys/fs/cgroup/workload/pids.peak")
	t.Logf("guest workload pids.peak: %s", strings.TrimSpace(out))
}

func TestMemoryHog(t *testing.T) {
	s := harness.Start(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	victim := create(ctx, t, s, apiclient.CreateOptions{MemoryMiB: 256})
	neighbor := create(ctx, t, s, apiclient.CreateOptions{MemoryMiB: 256})

	sampler := sampleLatency(ctx, s.Client, neighbor.ID)
	_, exit := attack(ctx, t, s.Client, victim.ID, 30*time.Second,
		"python3 -c 'a = []\nwhile True: a.append(bytearray(8 << 20))'")
	sampler.finish(t)
	if exit.ExitCode == 0 || exit.TimedOut {
		t.Errorf("memory hog ended with %+v, want killed by the guest", exit)
	}

	cg := jailCgroup(victim.ID)
	if peak, limit := cgroupValue(t, cg+"/memory.peak", ""), cgroupValue(t, cg+"/memory.max", ""); peak > limit {
		t.Errorf("jail memory.peak %d exceeds memory.max %d", peak, limit)
	}
	if kills := cgroupValue(t, cg+"/memory.events", "oom_kill"); kills != 0 {
		t.Errorf("the host OOM-killed %d processes in the jail; the guest should have contained the hog", kills)
	}
	waitResponsive(ctx, t, s.Client, victim.ID)
	out, _ := harness.ExecOut(ctx, t, s.Client, victim.ID, "grep", "oom_kill", "/sys/fs/cgroup/workload/memory.events")
	if !strings.Contains(out, "oom_kill 1") && !strings.Contains(out, "oom_kill 2") {
		t.Errorf("guest workload memory.events = %q, want the guest's workload OOM killer to have acted", out)
	}
}

func TestDiskFill(t *testing.T) {
	s := harness.Start(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	victim := create(ctx, t, s, apiclient.CreateOptions{MemoryMiB: 256})

	out, exit := attack(ctx, t, s.Client, victim.ID, 2*time.Minute, "dd if=/dev/zero of=/root/fill bs=4M 2>&1; sync")
	if !strings.Contains(out, "No space left") {
		t.Errorf("disk fill ended with %q (%+v), want no space left", harness.Tail(out, 200), exit)
	}
	var st unix.Stat_t
	scratch := filepath.Join(s.Layout.SandboxDir(victim.ID), "scratch.ext4")
	if err := unix.Stat(scratch, &st); err != nil {
		t.Fatal(err)
	}
	if allocated := st.Blocks * 512; allocated > st.Size {
		t.Errorf("scratch disk occupies %d bytes on the host, more than its size %d", allocated, st.Size)
	}
	t.Logf("scratch disk: %d MiB allocated of %d MiB", st.Blocks*512>>20, st.Size>>20)
	// The sandbox recovers once its workload frees space.
	if _, exit := harness.ExecOut(ctx, t, s.Client, victim.ID, "rm", "/root/fill"); exit.ExitCode != 0 {
		t.Errorf("rm after the fill exited %d", exit.ExitCode)
	}
	if _, exit := harness.ExecOut(ctx, t, s.Client, victim.ID, "sh", "-c", "echo ok > /root/after"); exit.ExitCode != 0 {
		t.Error("the sandbox cannot write after freeing space")
	}
}

func TestCPUSpin(t *testing.T) {
	s := harness.Start(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	victim := create(ctx, t, s, apiclient.CreateOptions{VCPUs: 2, MemoryMiB: 256})
	neighbor := create(ctx, t, s, apiclient.CreateOptions{MemoryMiB: 256})

	cg := jailCgroup(victim.ID)
	before := cgroupValue(t, cg+"/cpu.stat", "usage_usec")
	start := time.Now()
	sampler := sampleLatency(ctx, s.Client, neighbor.ID)
	attack(ctx, t, s.Client, victim.ID, 6*time.Second, "for i in 1 2 3 4; do (while :; do :; done) & done; wait")
	sampler.finish(t)
	used := time.Duration(cgroupValue(t, cg+"/cpu.stat", "usage_usec")-before) * time.Microsecond
	elapsed := time.Since(start)
	t.Logf("jail used %v of CPU in %v (cpu.max allows 2 CPUs)", used, elapsed)
	if limit := 2 * elapsed * 11 / 10; used > limit {
		t.Errorf("jail used %v of CPU in %v, more than 2 CPUs allow", used, elapsed)
	}
	if throttled := cgroupValue(t, cg+"/cpu.stat", "nr_throttled"); throttled == 0 {
		t.Log("cpu.max never throttled the jail (the guest could not saturate 2 CPUs)")
	}
}

func TestConsoleFlood(t *testing.T) {
	s := harness.Start(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	victim := create(ctx, t, s, apiclient.CreateOptions{MemoryMiB: 256})
	console := filepath.Join(s.Layout.JailBase(), "firecracker", "rh-"+victim.ID, "root", "logs", "console.log")
	sizeOf := func() int64 {
		info, err := os.Stat(console)
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}
	before := sizeOf()
	start := time.Now()
	attack(ctx, t, s.Client, victim.ID, 5*time.Second, "yes flood-flood-flood-flood-flood-flood > /dev/console")
	grew := sizeOf() - before
	elapsed := time.Since(start)
	// 16 KiB/s, plus one full bucket at the start.
	limit := int64(16<<10) * (int64(elapsed/time.Second) + 2)
	t.Logf("console grew %d bytes in %v (limit %d)", grew, elapsed, limit)
	if grew > limit {
		t.Errorf("console grew %d bytes in %v, more than the rate limit allows (%d)", grew, elapsed, limit)
	}
	waitResponsive(ctx, t, s.Client, victim.ID)
}

// listenInNamespace listens on addr inside a network namespace. The thread
// that enters the namespace is locked and never returned to the scheduler's
// pool; the socket stays in the namespace it was created in.
func listenInNamespace(t *testing.T, ns, addr string) net.Listener {
	t.Helper()
	type result struct {
		ln  net.Listener
		err error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread() // not unlocked: the thread exits with the goroutine
		f, err := os.Open("/var/run/netns/" + ns)
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer func() { _ = f.Close() }()
		if err := unix.Setns(int(f.Fd()), unix.CLONE_NEWNET); err != nil {
			ch <- result{err: err}
			return
		}
		ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp4", addr)
		ch <- result{ln, err}
	}()
	r := <-ch
	if r.err != nil {
		t.Fatalf("listen in %s: %v", ns, r.err)
	}
	t.Cleanup(func() { _ = r.ln.Close() })
	return r.ln
}

func TestBandwidthCap(t *testing.T) {
	const mbps = 8 // 1 MB/s each way
	s := harness.Start(t, fmt.Sprintf("network:\n  bandwidth_mbps: %d\n", mbps))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	victim := create(ctx, t, s, apiclient.CreateOptions{MemoryMiB: 256})

	ln := listenInNamespace(t, netns.NamespaceName(victim.ID), netns.TapAddr+":0")
	port := ln.Addr().(*net.TCPAddr).Port
	// The receiver records when each byte arrived, so the steady rate can be
	// told apart from the initial burst the full token bucket allows.
	type sample struct {
		at    time.Time
		total int64
	}
	received := make(chan []sample, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			received <- nil
			return
		}
		defer func() { _ = conn.Close() }()
		var samples []sample
		var total int64
		buf := make([]byte, 64<<10)
		for {
			n, err := conn.Read(buf)
			total += int64(n)
			samples = append(samples, sample{time.Now(), total})
			if err != nil {
				received <- samples
				return
			}
		}
	}()
	bytesBy := func(samples []sample, at time.Time) int64 {
		var n int64
		for _, s := range samples {
			if s.at.After(at) {
				break
			}
			n = s.total
		}
		return n
	}

	const seconds = 6
	script := fmt.Sprintf(`import socket, time
s = socket.create_connection(("%s", %d))
buf = b"x" * 65536
end = time.time() + %d
while time.time() < end:
    s.sendall(buf)
s.close()
`, netns.TapAddr, port, seconds)
	if _, exit := harness.ExecOut(ctx, t, s.Client, victim.ID, "python3", "-c", script); exit.ExitCode != 0 {
		t.Fatalf("sender exited %d", exit.ExitCode)
	}
	var samples []sample
	select {
	case samples = <-received:
	case <-time.After(30 * time.Second):
		t.Fatal("the receiver never finished")
	}
	if len(samples) < 2 {
		t.Fatal("the receiver got nothing; the path through the tap device is not working")
	}
	first := samples[0].at
	for at := time.Duration(0); at <= seconds*time.Second; at += time.Second {
		t.Logf("  +%v: %d bytes", at, bytesBy(samples, first.Add(at)))
	}
	// Steady state: after the first two seconds, when the initial bucket is
	// long spent, until a second before the sender stops.
	from, to := first.Add(2*time.Second), first.Add((seconds-1)*time.Second)
	steady := float64(bytesBy(samples, to)-bytesBy(samples, from)) / to.Sub(from).Seconds()
	capBytes := float64(mbps) * 1e6 / 8
	t.Logf("steady rate %.3f MB/s, cap %.3f MB/s", steady/1e6, capBytes/1e6)
	if steady > capBytes*1.05 {
		t.Errorf("steady rate %.3f MB/s exceeds the %d Mbit/s cap", steady/1e6, mbps)
	}
	if steady < capBytes*0.5 {
		t.Errorf("steady rate %.3f MB/s is far below the cap; the path is not working", steady/1e6)
	}
}

func TestGuestKernelPanicIsReaped(t *testing.T) {
	s := harness.Start(t, "sandbox:\n  reap_interval: 200ms\n")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	victim := create(ctx, t, s, apiclient.CreateOptions{MemoryMiB: 256})
	neighbor := create(ctx, t, s, apiclient.CreateOptions{MemoryMiB: 256})

	// The call dies with the guest; its error is expected.
	_, _ = s.Client.Exec(ctx, victim.ID, apiclient.ExecOptions{Argv: []string{"sh", "-c", "echo c > /proc/sysrq-trigger"}, TimeoutMS: 5000}, nil)
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, err := s.Client.Get(ctx, victim.ID)
		if apiclient.IsNotFound(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the sandbox whose guest panicked was not reaped")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if harness.Exists(jailCgroup(victim.ID)) || harness.Exists(s.Layout.SandboxDir(victim.ID)) {
		t.Error("the panicked sandbox's cgroup or directory survived its reaping")
	}
	// The console of the dead guest is kept for diagnosis.
	kept, _ := filepath.Glob(filepath.Join(s.Layout.DiagnosticsDir(), "*-"+victim.ID, "console.log"))
	if len(kept) != 1 {
		t.Fatalf("diagnostics for %s: %v, want one kept console", victim.ID, kept)
	}
	console, _ := os.ReadFile(kept[0]) //nolint:gosec // G304: a file this test's stack wrote
	if !strings.Contains(string(console), "Kernel panic") {
		t.Errorf("the kept console lacks the panic:\n%s", harness.Tail(string(console), 1024))
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(kept[0])) })
	waitResponsive(ctx, t, s.Client, neighbor.ID)
}

func TestNothingSurvivesDestruction(t *testing.T) {
	s := harness.Start(t, "sandbox:\n  pools:\n    - memory_mib: 256\n      size: 1\n")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	first := create(ctx, t, s, apiclient.CreateOptions{MemoryMiB: 256})
	if _, exit := harness.ExecOut(ctx, t, s.Client, first.ID, "sh", "-c", "echo secret > /root/secret; echo secret > /tmp/secret; sync"); exit.ExitCode != 0 {
		t.Fatal("could not write the secret")
	}
	if err := s.Client.Delete(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		sb := create(ctx, t, s, apiclient.CreateOptions{MemoryMiB: 256})
		out, _ := harness.ExecOut(ctx, t, s.Client, sb.ID, "sh", "-c", "cat /root/secret /tmp/secret 2>/dev/null; true")
		if strings.Contains(out, "secret") {
			t.Errorf("sandbox %d (%s) read a file written by a destroyed sandbox", i, sb.StartPath)
		}
	}
}

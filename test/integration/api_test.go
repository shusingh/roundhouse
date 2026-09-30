//go:build integration

package integration

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shusingh/roundhouse/internal/agentproto"
	"github.com/shusingh/roundhouse/internal/apiclient"
	"github.com/shusingh/roundhouse/internal/storage"
	"github.com/shusingh/roundhouse/internal/vmm"
	"github.com/shusingh/roundhouse/test/harness"
)

const testKey = harness.Key

// startStack runs the control plane in-process; see harness.Start.
func startStack(t *testing.T, extraYAML string) *harness.Stack {
	t.Helper()
	return harness.Start(t, extraYAML)
}

func execOut(t *testing.T, ctx context.Context, c *apiclient.Client, id string, argv ...string) (string, *agentproto.Frame) {
	t.Helper()
	return harness.ExecOut(ctx, t, c, id, argv...)
}

func TestAgentSessionThroughAPI(t *testing.T) {
	s := startStack(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	start := time.Now()
	sb, err := s.Client.Create(ctx, apiclient.CreateOptions{MemoryMiB: 256})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Logf("API create (cold boot) to ready: %v", time.Since(start))

	// A multi-step session: upload data, process it across calls, fetch output.
	if err := s.Client.WriteFile(ctx, sb.ID, "/root/work/nums.txt", strings.NewReader("3\n4\n5\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	script := "import sys\nnums=[int(x) for x in open('/root/work/nums.txt')]\nopen('/root/work/sum.txt','w').write(str(sum(nums)))\nprint('ok')"
	if out, exit := execOut(t, ctx, s.Client, sb.ID, "python3", "-c", script); out != "ok\n" || exit.ExitCode != 0 {
		t.Fatalf("python step printed %q, exit %+v", out, exit)
	}
	var sum bytes.Buffer
	if _, err := s.Client.ReadFile(ctx, sb.ID, "/root/work/sum.txt", &sum); err != nil || sum.String() != "12" {
		t.Fatalf("ReadFile = %q, %v; want 12", sum.String(), err)
	}
	if _, exit := execOut(t, ctx, s.Client, sb.ID, "sh", "-c", "exit 9"); exit.ExitCode != 9 {
		t.Errorf("exit code = %d, want 9", exit.ExitCode)
	}
	exit, err := s.Client.Exec(ctx, sb.ID, apiclient.ExecOptions{Argv: []string{"sleep", "30"}, TimeoutMS: 300}, nil)
	if err != nil || !exit.TimedOut {
		t.Errorf("sleep with a 300 ms limit = %+v, %v; want timed out", exit, err)
	}

	if err := s.Client.Delete(ctx, sb.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Client.Get(ctx, sb.ID); !apiclient.IsNotFound(err) {
		t.Errorf("Get after Delete = %v, want 404", err)
	}
	if exists(s.Layout.SandboxDir(sb.ID)) {
		t.Error("sandbox directory survived Delete")
	}
}

func TestIdleSandboxIsReaped(t *testing.T) {
	s := startStack(t, "sandbox:\n  reap_interval: 200ms\n")
	ctx := context.Background()
	sb, err := s.Client.Create(ctx, apiclient.CreateOptions{MemoryMiB: 256, IdleTimeoutSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := s.Client.Get(ctx, sb.ID); apiclient.IsNotFound(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("idle sandbox was not reaped")
}

func TestCrashedVMIsReaped(t *testing.T) {
	s := startStack(t, "sandbox:\n  reap_interval: 200ms\n")
	ctx := context.Background()
	sb, err := s.Client.Create(ctx, apiclient.CreateOptions{MemoryMiB: 256})
	if err != nil {
		t.Fatal(err)
	}
	// Kill the VMM behind the manager's back, as a crash would.
	if err := os.WriteFile("/sys/fs/cgroup/roundhouse/rh-"+sb.ID+"/cgroup.kill", []byte("1"), 0); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := s.Client.Get(ctx, sb.ID); apiclient.IsNotFound(err) {
			// Its logs are kept for diagnosis.
			kept, _ := filepath.Glob(filepath.Join(s.Layout.DiagnosticsDir(), "*-"+sb.ID))
			if len(kept) != 1 || !exists(filepath.Join(kept[0], "reason.txt")) {
				t.Errorf("diagnostics for the crashed sandbox: %v, want one entry", kept)
			}
			for _, d := range kept {
				_ = os.RemoveAll(d)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("sandbox with a dead VM was not reaped")
}

func TestReconcileRemovesOrphans(t *testing.T) {
	requireGuestImage(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// An orphan: a jailed VMM and sandbox directory with no control plane.
	spec := jailSpec(t, "orphan")
	spec.ID = "rh-sb-deadbeef0001"
	if _, err := vmm.Launch(ctx, spec); err != nil {
		t.Fatal(err)
	}
	layout := storage.Layout{Root: "/var/lib/roundhouse"}
	if err := os.MkdirAll(layout.SandboxDir("sb-deadbeef0001"), 0o750); err != nil {
		t.Fatal(err)
	}

	startStack(t, "") // Run reconciles before serving
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (exists(spec.Dir()) || exists(spec.CgroupDir())) {
		time.Sleep(50 * time.Millisecond)
	}
	if exists(spec.Dir()) || exists(spec.CgroupDir()) || exists(layout.SandboxDir("sb-deadbeef0001")) {
		t.Fatal("reconciliation left the orphan behind")
	}
}

// TestLongSilentCommand covers a sandbox living past the guest's first ext4
// journal commits, which write mid-disk: a file size limit below the scratch
// disk's size once killed the VMM about five seconds after boot.
func TestLongSilentCommand(t *testing.T) {
	s := startStack(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sb, err := s.Client.Create(ctx, apiclient.CreateOptions{MemoryMiB: 256})
	if err != nil {
		t.Fatal(err)
	}
	out, exit := execOut(t, ctx, s.Client, sb.ID, "sh", "-c", "dd if=/dev/zero of=/root/f bs=1M count=16 2>/dev/null && sync && sleep 8 && echo done")
	if out != "done\n" || exit.ExitCode != 0 {
		t.Fatalf("long command printed %q, exit %+v", out, exit)
	}
}

func TestMetricsAndReadiness(t *testing.T) {
	s := startStack(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	get := func(path string) (int, string) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, _ := get("/readyz"); code != http.StatusOK {
		t.Errorf("readyz after Prepare: %d, want 200", code)
	}
	sb, err := s.Client.Create(ctx, apiclient.CreateOptions{MemoryMiB: 256})
	if err != nil {
		t.Fatal(err)
	}
	execOut(t, ctx, s.Client, sb.ID, "sh", "-c", "exit 3")
	_, body := get("/metrics")
	for _, want := range []string{
		`roundhouse_sandbox_creates_total{outcome="ok",path="cold"} 1`,
		`roundhouse_execs_total{outcome="nonzero"} 1`,
		`roundhouse_sandboxes{state="ready"} 1`,
		`roundhouse_budget_used{resource="sandboxes"} 1`,
		`roundhouse_sandbox_start_seconds_count{path="cold"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	if strings.Contains(body, sb.ID) {
		t.Error("metrics carry a sandbox ID")
	}
}

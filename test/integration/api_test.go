//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/hex"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shusingh/roundhouse/internal/agentproto"
	"github.com/shusingh/roundhouse/internal/api"
	"github.com/shusingh/roundhouse/internal/apiclient"
	"github.com/shusingh/roundhouse/internal/config"
	"github.com/shusingh/roundhouse/internal/node"
	"github.com/shusingh/roundhouse/internal/sandbox"
	"github.com/shusingh/roundhouse/internal/storage"
	"github.com/shusingh/roundhouse/internal/vmm"
)

const testKey = "rh_integration_key"

type stack struct {
	client  *apiclient.Client
	manager *sandbox.Manager
	layout  storage.Layout
	cancel  context.CancelFunc
}

// startStack runs the manager and API in-process against real Firecracker.
func startStack(t *testing.T, extraYAML string) *stack {
	t.Helper()
	requireGuestImage(t)
	d := config.Digest(testKey)
	cfg, err := config.Parse([]byte("api_keys:\n  - name: it\n    sha256: " + hex.EncodeToString(d[:]) + "\n" + extraYAML))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	layout := storage.Layout{Root: cfg.Host.DataDir}
	manager, err := node.New(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	apiServer, err := api.New(cfg, manager, logger)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := manager.Reconcile(ctx); err != nil {
		cancel()
		t.Fatalf("Reconcile: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := manager.Run(ctx); err != nil {
			t.Errorf("manager: %v", err)
		}
	}()
	srv := httptest.NewServer(apiServer.Handler())
	t.Cleanup(func() {
		if t.Failed() {
			consoles, _ := filepath.Glob(filepath.Join(layout.JailBase(), "firecracker", "rh-sb-*", "root", "console.log"))
			for _, c := range consoles {
				data, _ := os.ReadFile(c)
				t.Logf("console of %s (last 4 KiB):\n%s", filepath.Base(filepath.Dir(filepath.Dir(c))), tail(string(data), 4096))
			}
		}
		srv.Close()
		cancel()
		<-done
		shutdownCtx, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		if err := manager.Shutdown(shutdownCtx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		assertHostClean(t, layout)
	})
	return &stack{client: apiclient.New(srv.URL, testKey), manager: manager, layout: layout, cancel: cancel}
}

// assertHostClean fails if any Roundhouse jail, cgroup, VMM, or sandbox
// directory is left on the host.
func assertHostClean(t *testing.T, layout storage.Layout) {
	t.Helper()
	for _, dir := range []string{filepath.Join(layout.JailBase(), "firecracker"), "/sys/fs/cgroup/roundhouse", layout.SandboxesDir()} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() && (strings.HasPrefix(e.Name(), "rh-sb-") || strings.HasPrefix(e.Name(), "sb-")) {
				t.Errorf("leftover after shutdown: %s", filepath.Join(dir, e.Name()))
			}
		}
	}
}

func execOut(t *testing.T, ctx context.Context, c *apiclient.Client, id string, argv ...string) (string, *agentproto.Frame) {
	t.Helper()
	var out bytes.Buffer
	exit, err := c.Exec(ctx, id, apiclient.ExecOptions{Argv: argv}, func(f *agentproto.Frame) error {
		out.Write(f.Data)
		return nil
	})
	if err != nil {
		t.Fatalf("exec %v: %v", argv, err)
	}
	return out.String(), exit
}

func TestAgentSessionThroughAPI(t *testing.T) {
	s := startStack(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	start := time.Now()
	sb, err := s.client.Create(ctx, apiclient.CreateOptions{MemoryMiB: 256})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Logf("API create (cold boot) to ready: %v", time.Since(start))

	// A multi-step session: upload data, process it across calls, fetch output.
	if err := s.client.WriteFile(ctx, sb.ID, "/root/work/nums.txt", strings.NewReader("3\n4\n5\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	script := "import sys\nnums=[int(x) for x in open('/root/work/nums.txt')]\nopen('/root/work/sum.txt','w').write(str(sum(nums)))\nprint('ok')"
	if out, exit := execOut(t, ctx, s.client, sb.ID, "python3", "-c", script); out != "ok\n" || exit.ExitCode != 0 {
		t.Fatalf("python step printed %q, exit %+v", out, exit)
	}
	var sum bytes.Buffer
	if _, err := s.client.ReadFile(ctx, sb.ID, "/root/work/sum.txt", &sum); err != nil || sum.String() != "12" {
		t.Fatalf("ReadFile = %q, %v; want 12", sum.String(), err)
	}
	if _, exit := execOut(t, ctx, s.client, sb.ID, "sh", "-c", "exit 9"); exit.ExitCode != 9 {
		t.Errorf("exit code = %d, want 9", exit.ExitCode)
	}
	exit, err := s.client.Exec(ctx, sb.ID, apiclient.ExecOptions{Argv: []string{"sleep", "30"}, TimeoutMS: 300}, nil)
	if err != nil || !exit.TimedOut {
		t.Errorf("sleep with a 300 ms limit = %+v, %v; want timed out", exit, err)
	}

	if err := s.client.Delete(ctx, sb.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.client.Get(ctx, sb.ID); !apiclient.IsNotFound(err) {
		t.Errorf("Get after Delete = %v, want 404", err)
	}
	if exists(s.layout.SandboxDir(sb.ID)) {
		t.Error("sandbox directory survived Delete")
	}
}

func TestIdleSandboxIsReaped(t *testing.T) {
	s := startStack(t, "sandbox:\n  reap_interval: 200ms\n")
	ctx := context.Background()
	sb, err := s.client.Create(ctx, apiclient.CreateOptions{MemoryMiB: 256, IdleTimeoutSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := s.client.Get(ctx, sb.ID); apiclient.IsNotFound(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("idle sandbox was not reaped")
}

func TestCrashedVMIsReaped(t *testing.T) {
	s := startStack(t, "sandbox:\n  reap_interval: 200ms\n")
	ctx := context.Background()
	sb, err := s.client.Create(ctx, apiclient.CreateOptions{MemoryMiB: 256})
	if err != nil {
		t.Fatal(err)
	}
	// Kill the VMM behind the manager's back, as a crash would.
	if err := os.WriteFile("/sys/fs/cgroup/roundhouse/rh-"+sb.ID+"/cgroup.kill", []byte("1"), 0); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := s.client.Get(ctx, sb.ID); apiclient.IsNotFound(err) {
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
	sb, err := s.client.Create(ctx, apiclient.CreateOptions{MemoryMiB: 256})
	if err != nil {
		t.Fatal(err)
	}
	out, exit := execOut(t, ctx, s.client, sb.ID, "sh", "-c", "dd if=/dev/zero of=/root/f bs=1M count=16 2>/dev/null && sync && sleep 8 && echo done")
	if out != "done\n" || exit.ExitCode != 0 {
		t.Fatalf("long command printed %q, exit %+v", out, exit)
	}
}

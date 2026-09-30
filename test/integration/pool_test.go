//go:build integration

package integration

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shusingh/roundhouse/internal/apiclient"
	"github.com/shusingh/roundhouse/internal/config"
	"github.com/shusingh/roundhouse/internal/node"
	"github.com/shusingh/roundhouse/internal/sandbox"
	"github.com/shusingh/roundhouse/internal/storage"
	"github.com/shusingh/roundhouse/test/harness"
)

// templateMiB is a shape no other test uses, so the template built here
// changes no other test's start path.
const templateMiB = 320

// buildTemplate builds the template for the default image at 1 vCPU and
// templateMiB, and removes it when the test ends.
func buildTemplate(t *testing.T) storage.Template {
	t.Helper()
	requireGuestImage(t)
	d := config.Digest(testKey)
	cfg, err := config.Parse([]byte("api_keys:\n  - name: it\n    sha256: " + hex.EncodeToString(d[:]) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := node.New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	m, err := n.Booter.BuildTemplate(ctx, "default", sandbox.Limits{VCPUs: 1, MemoryMiB: templateMiB}, n.TemplateUID)
	if err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	t.Logf("template built in %v (%s)", time.Since(start), m.FirecrackerVersion)
	layout := storage.Layout{Root: cfg.Host.DataDir}
	tpl := storage.TemplateAt(layout.TemplateDir("default", 1, templateMiB))
	t.Cleanup(func() { _ = os.RemoveAll(tpl.Dir) })
	return tpl
}

func waitForPool(t *testing.T, s *harness.Stack, idle int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if p := s.Manager.Pools(); len(p) == 1 && p[0].Idle >= idle {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pool did not reach %d idle sandboxes: %+v", idle, s.Manager.Pools())
}

func TestTemplatePoolAndRestoredClones(t *testing.T) {
	buildTemplate(t)
	// Age the snapshot past the clock tolerance below, so a guest clock left at
	// snapshot time would be caught.
	time.Sleep(3 * time.Second)
	s := startStack(t, "sandbox:\n  pools:\n    - memory_mib: "+strconv.Itoa(templateMiB)+"\n      size: 2\n")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	waitForPool(t, s, 2)

	// Four creates: two claims from the full pool, then whatever the refill
	// has ready, restoring the rest. None may cold-boot: a template exists.
	var ids []string
	paths := map[string]int{}
	for i := range 4 {
		start := time.Now()
		sb, err := s.Client.Create(ctx, apiclient.CreateOptions{MemoryMiB: templateMiB})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		t.Logf("create %d: %s in %v", i, sb.StartPath, time.Since(start))
		if i < 2 && sb.StartPath != string(sandbox.StartPool) {
			t.Errorf("create %d with a full pool started by %s, want pool", i, sb.StartPath)
		}
		if sb.StartPath == string(sandbox.StartCold) {
			t.Errorf("create %d cold-booted although a template exists", i)
		}
		paths[sb.StartPath]++
		ids = append(ids, sb.ID)
	}
	t.Logf("start paths: %v", paths)

	// Every clone starts from the same memory; each must still be unique.
	seen := map[string]string{}
	for _, id := range ids {
		out, exit := execOut(t, ctx, s.Client, id, "sh", "-c",
			"cat /proc/sys/kernel/random/uuid; head -c 16 /dev/urandom | od -An -tx1; date +%s")
		if exit.ExitCode != 0 {
			t.Fatalf("probe in %s exited %d", id, exit.ExitCode)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 3 {
			t.Fatalf("probe in %s printed %q", id, out)
		}
		for _, v := range lines[:2] {
			if other, dup := seen[v]; dup {
				t.Errorf("sandboxes %s and %s produced the same random value %q", other, id, v)
			}
			seen[v] = id
		}
		// clock_realtime moves the restored guest.s wall clock to now.
		guest, _ := strconv.ParseInt(lines[2], 10, 64)
		if skew := time.Since(time.Unix(guest, 0)); skew < -2*time.Second || skew > 2*time.Second {
			t.Errorf("guest clock in %s is off by %v", id, skew)
		}
	}

	// The pool refills after the claims.
	waitForPool(t, s, 2)
}

func TestPooledSandboxGetsItsEgressAtClaim(t *testing.T) {
	requireInternet(t)
	buildTemplate(t)
	s := startStack(t, "sandbox:\n  pools:\n    - memory_mib: "+strconv.Itoa(templateMiB)+"\n      size: 1\n")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	waitForPool(t, s, 1)
	sb, err := s.Client.Create(ctx, apiclient.CreateOptions{MemoryMiB: templateMiB, Egress: &apiclient.Egress{Mode: "internet"}})
	if err != nil {
		t.Fatal(err)
	}
	if sb.StartPath != string(sandbox.StartPool) || sb.Egress != "internet" {
		t.Fatalf("create = start %s egress %s, want a pooled sandbox with internet egress", sb.StartPath, sb.Egress)
	}
	if got := probe(t, ctx, s.Client, sb.ID, "1.1.1.1", 443); got != "ok" {
		t.Error("a pooled sandbox claimed with internet egress cannot reach the internet")
	}
	if got := probe(t, ctx, s.Client, sb.ID, uplinkAddr(t), 22); got != "fail" {
		t.Error("a pooled sandbox claimed with internet egress reached the host")
	}
}

func TestMismatchedTemplateIsNotRestored(t *testing.T) {
	tpl := buildTemplate(t)
	data, err := os.ReadFile(tpl.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m["firecracker_version"] = "Firecracker v0.0.1"
	data, _ = json.Marshal(m)
	if err := os.WriteFile(tpl.Manifest, data, 0o644); err != nil { //nolint:gosec // G306: matches the template's own mode
		t.Fatal(err)
	}

	s := startStack(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sb, err := s.Client.Create(ctx, apiclient.CreateOptions{MemoryMiB: templateMiB})
	if err != nil {
		t.Fatal(err)
	}
	if sb.StartPath != string(sandbox.StartCold) {
		t.Errorf("a template built by another Firecracker was used: start path %s, want cold", sb.StartPath)
	}
}

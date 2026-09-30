//go:build integration || containment

// Package harness runs Roundhouse in-process against real Firecracker for the
// integration and containment suites: the node, the manager, and the API on
// a test HTTP server, with a leak scan when each test ends.
package harness

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
)

// Key is the API key every harness stack accepts.
const Key = "rh_test_key"

// Host paths the suites rely on.
const (
	GuestImageDir = "/var/lib/roundhouse/images/default"
	BaseImageDir  = "/var/lib/roundhouse/images/base"
)

// Stack is a running control plane.
type Stack struct {
	Client  *apiclient.Client
	Node    *node.Node
	Manager *sandbox.Manager
	Layout  storage.Layout
	Config  config.Config
	// URL is the API's base URL.
	URL string
}

// Start runs a control plane configured by extraYAML (appended to a config
// holding the harness key) until the test ends, then shuts it down and fails
// the test if anything is left on the host.
func Start(t *testing.T, extraYAML string) *Stack {
	t.Helper()
	RequireGuestImage(t)
	d := config.Digest(Key)
	cfg, err := config.Parse([]byte("api_keys:\n  - name: it\n    sha256: " + hex.EncodeToString(d[:]) + "\n" + extraYAML))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	layout := storage.Layout{Root: cfg.Host.DataDir}
	n, err := node.New(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	apiServer, err := api.New(cfg, n.Manager, n.Metrics, logger)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := n.Manager.Prepare(ctx); err != nil {
		cancel()
		t.Fatalf("Prepare: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := n.Manager.Run(ctx); err != nil {
			t.Errorf("manager: %v", err)
		}
	}()
	srv := httptest.NewServer(apiServer.Handler())
	t.Cleanup(func() {
		if t.Failed() {
			consoles, _ := filepath.Glob(filepath.Join(layout.JailBase(), "firecracker", "rh-sb-*", "root", "logs", "console.log"))
			for _, c := range consoles {
				data, _ := os.ReadFile(c) //nolint:gosec // G304: a console log under the test's own data directory
				t.Logf("console of %s (last 4 KiB):\n%s", filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(c)))), Tail(string(data), 4096))
			}
		}
		srv.Close()
		cancel()
		<-done
		shutdownCtx, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		if err := n.Manager.Shutdown(shutdownCtx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		AssertHostClean(t, layout)
	})
	return &Stack{Client: apiclient.New(srv.URL, Key), Node: n, Manager: n.Manager, Layout: layout, Config: cfg, URL: srv.URL}
}

// RequireHost skips the test unless it runs as root on a host with KVM, the
// jailer, and the base images.
func RequireHost(t *testing.T) {
	t.Helper()
	switch {
	case os.Geteuid() != 0:
		t.Skip("needs root: run with `make integration`")
	case !Exists("/dev/kvm"):
		t.Skip("needs /dev/kvm")
	case !Exists("/usr/local/bin/jailer"):
		t.Skip("needs the jailer: run `make host-setup`")
	case !Exists(filepath.Join(BaseImageDir, "vmlinux")):
		t.Skip("needs base images: run `sudo bash images/fetch-base.sh`")
	}
}

// RequireGuestImage also requires the guest image with the agent installed.
func RequireGuestImage(t *testing.T) {
	t.Helper()
	RequireHost(t)
	if !Exists(filepath.Join(GuestImageDir, "rootfs.ext4")) {
		t.Skip("needs the guest image: run `sudo bash images/build-rootfs.sh bin/roundhouse-agent`")
	}
}

// AssertHostClean fails if any Roundhouse jail, cgroup, VMM, network
// namespace, or sandbox directory is left on the host.
func AssertHostClean(t *testing.T, layout storage.Layout) {
	t.Helper()
	dirs := []string{filepath.Join(layout.JailBase(), "firecracker"), "/sys/fs/cgroup/roundhouse", layout.SandboxesDir(), "/var/run/netns"}
	for _, dir := range dirs {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "rh-sb-") || strings.HasPrefix(e.Name(), "sb-") {
				t.Errorf("leftover after shutdown: %s", filepath.Join(dir, e.Name()))
			}
		}
	}
}

// ExecOut runs argv in a sandbox and returns its combined output and exit
// frame, failing the test if the call itself fails.
func ExecOut(ctx context.Context, t *testing.T, c *apiclient.Client, id string, argv ...string) (string, *agentproto.Frame) {
	t.Helper()
	return ExecWith(ctx, t, c, id, apiclient.ExecOptions{Argv: argv})
}

// ExecWith is ExecOut with full options.
func ExecWith(ctx context.Context, t *testing.T, c *apiclient.Client, id string, opts apiclient.ExecOptions) (string, *agentproto.Frame) {
	t.Helper()
	var out bytes.Buffer
	exit, err := c.Exec(ctx, id, opts, func(f *agentproto.Frame) error {
		out.Write(f.Data)
		return nil
	})
	if err != nil {
		t.Fatalf("exec %v: %v", opts.Argv, err)
	}
	return out.String(), exit
}

// Exists reports whether a path exists.
func Exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Tail returns the last n bytes of s.
func Tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

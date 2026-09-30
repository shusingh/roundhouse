package vmm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// fakeAPI is a Firecracker API stand-in on a Unix socket that records every
// request and answers with a configurable status.
type fakeAPI struct {
	socket string

	mu       sync.Mutex
	requests []recorded // guarded by mu
	status   int        // guarded by mu
	fault    string     // guarded by mu
}

type recorded struct {
	Method string
	Path   string
	Body   map[string]any
}

// shortSocketPath returns a socket path short enough for sun_path (108 bytes
// on Linux), which t.TempDir can exceed on some systems.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rhvmm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "api.sock")
}

func startFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{socket: shortSocketPath(t), status: http.StatusNoContent}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", f.socket)
	if err != nil {
		t.Fatalf("listen on %s: %v", f.socket, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(f.serve), ReadHeaderTimeout: time.Second}
	go srv.Serve(ln) //nolint:errcheck // returns ErrServerClosed on shutdown
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	rec := recorded{Method: r.Method, Path: r.URL.Path}
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		_ = json.Unmarshal(raw, &rec.Body)
	}
	f.mu.Lock()
	f.requests = append(f.requests, rec)
	status, fault := f.status, f.fault
	f.mu.Unlock()
	if r.Method == http.MethodGet && status == http.StatusNoContent {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if fault != "" {
		_, _ = w.Write([]byte(fault))
	}
}

func (f *fakeAPI) last(t *testing.T) recorded {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("no request reached the fake API")
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeAPI) respond(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.fault = status, body
}

func TestClientRequests(t *testing.T) {
	t.Parallel()
	f := startFakeAPI(t)
	c := NewClient(f.socket)
	defer c.Close()
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
		want recorded
	}{
		{
			name: "machine config",
			call: func() error { return c.PutMachineConfig(ctx, MachineConfig{VCPUCount: 2, MemSizeMiB: 512}) },
			want: recorded{"PUT", "/machine-config", map[string]any{"vcpu_count": 2.0, "mem_size_mib": 512.0, "smt": false}},
		},
		{
			name: "boot source",
			call: func() error { return c.PutBootSource(ctx, BootSource{KernelImagePath: "vmlinux", BootArgs: "quiet"}) },
			want: recorded{"PUT", "/boot-source", map[string]any{"kernel_image_path": "vmlinux", "boot_args": "quiet"}},
		},
		{
			name: "drive with rate limiter",
			call: func() error {
				return c.PutDrive(ctx, Drive{
					DriveID: "scratch", PathOnHost: "scratch.ext4",
					RateLimiter: &RateLimiter{Bandwidth: &TokenBucket{Size: 1 << 20, RefillTimeMS: 100}},
				})
			},
			want: recorded{"PUT", "/drives/scratch", map[string]any{
				"drive_id": "scratch", "path_on_host": "scratch.ext4", "is_root_device": false, "is_read_only": false,
				"rate_limiter": map[string]any{"bandwidth": map[string]any{"size": float64(1 << 20), "refill_time": 100.0}},
			}},
		},
		{
			name: "network interface",
			call: func() error {
				return c.PutNetworkInterface(ctx, NetworkInterface{IfaceID: "eth0", HostDevName: "tap0", GuestMAC: "06:00:ac:10:00:02"})
			},
			want: recorded{"PUT", "/network-interfaces/eth0", map[string]any{
				"iface_id": "eth0", "host_dev_name": "tap0", "guest_mac": "06:00:ac:10:00:02",
			}},
		},
		{
			name: "vsock",
			call: func() error { return c.PutVsock(ctx, Vsock{GuestCID: 3, UDSPath: "/run/v.sock"}) },
			want: recorded{"PUT", "/vsock", map[string]any{"guest_cid": 3.0, "uds_path": "/run/v.sock"}},
		},
		{
			name: "serial",
			call: func() error {
				return c.PutSerial(ctx, Serial{OutputPath: "console.log", RateLimiter: &TokenBucket{Size: 8192, RefillTimeMS: 1000}})
			},
			want: recorded{"PUT", "/serial", map[string]any{
				"serial_out_path": "console.log", "rate_limiter": map[string]any{"size": 8192.0, "refill_time": 1000.0},
			}},
		},
		{
			name: "entropy",
			call: func() error { return c.PutEntropy(ctx, Entropy{}) },
			want: recorded{"PUT", "/entropy", map[string]any{}},
		},
		{
			name: "start",
			call: func() error { return c.Start(ctx) },
			want: recorded{"PUT", "/actions", map[string]any{"action_type": "InstanceStart"}},
		},
		{
			name: "pause",
			call: func() error { return c.Pause(ctx) },
			want: recorded{"PATCH", "/vm", map[string]any{"state": "Paused"}},
		},
		{
			name: "resume",
			call: func() error { return c.Resume(ctx) },
			want: recorded{"PATCH", "/vm", map[string]any{"state": "Resumed"}},
		},
		{
			name: "create snapshot",
			call: func() error {
				return c.CreateSnapshot(ctx, SnapshotCreate{SnapshotType: SnapshotFull, SnapshotPath: "vm.state", MemFilePath: "vm.mem"})
			},
			want: recorded{"PUT", "/snapshot/create", map[string]any{
				"snapshot_type": "Full", "snapshot_path": "vm.state", "mem_file_path": "vm.mem",
			}},
		},
		{
			name: "load snapshot",
			call: func() error {
				return c.LoadSnapshot(ctx, SnapshotLoad{
					SnapshotPath:  "vm.state",
					MemBackend:    MemoryBackend{BackendType: BackendFile, BackendPath: "vm.mem"},
					ResumeVM:      true,
					ClockRealtime: true,
					VsockOverride: &VsockOverride{UDSPath: "/run/v.sock"},
				})
			},
			want: recorded{"PUT", "/snapshot/load", map[string]any{
				"snapshot_path": "vm.state",
				"mem_backend":   map[string]any{"backend_type": "File", "backend_path": "vm.mem"},
				"resume_vm":     true, "clock_realtime": true,
				"vsock_override": map[string]any{"uds_path": "/run/v.sock"},
			}},
		},
	}
	// Subtests share the fake server, so they run sequentially.
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err != nil {
				t.Fatalf("call: %v", err)
			}
			if diff := cmp.Diff(tc.want, f.last(t)); diff != "" {
				t.Errorf("request mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestClientAPIError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		status    int
		body      string
		wantFault string
	}{
		{"json fault", http.StatusBadRequest, `{"fault_message":"Invalid vCPU count"}`, "Invalid vCPU count"},
		{"plain body", http.StatusInternalServerError, "boom", "boom"},
		{"empty body", http.StatusBadRequest, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startFakeAPI(t)
			f.respond(tc.status, tc.body)
			c := NewClient(f.socket)
			defer c.Close()

			err := c.PutMachineConfig(context.Background(), MachineConfig{VCPUCount: 3, MemSizeMiB: 128})
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("PutMachineConfig error = %v, want an *APIError", err)
			}
			if apiErr.StatusCode != tc.status || apiErr.Fault != tc.wantFault || apiErr.Path != "/machine-config" {
				t.Errorf("APIError = %+v, want status %d, fault %q, path /machine-config", apiErr, tc.status, tc.wantFault)
			}
		})
	}
}

func TestClientWaitReady(t *testing.T) {
	t.Parallel()
	socket := shortSocketPath(t)
	c := NewClient(socket)
	defer c.Close()

	// The socket appears only after WaitReady has started polling.
	ready := make(chan struct{})
	go func() {
		defer close(ready)
		time.Sleep(30 * time.Millisecond)
		ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", socket)
		if err != nil {
			t.Errorf("listen: %v", err)
			return
		}
		srv := &http.Server{
			Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
			ReadHeaderTimeout: time.Second,
		}
		t.Cleanup(func() { _ = srv.Close() })
		go srv.Serve(ln) //nolint:errcheck // returns ErrServerClosed on shutdown
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	<-ready
}

func TestClientWaitReadyTimesOut(t *testing.T) {
	t.Parallel()
	c := NewClient(shortSocketPath(t)) // nothing ever listens
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := c.WaitReady(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitReady error = %v, want context.DeadlineExceeded", err)
	}
}

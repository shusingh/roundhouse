//go:build integration

// Package integration boots real Firecracker microVMs. It needs Linux, KVM,
// root, the jailer, and the base images from images/fetch-base.sh; tests
// skip with a reason when any of these is missing.
package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shusingh/roundhouse/internal/vmm"
)

const (
	imageDir   = "/var/lib/roundhouse/images/base"
	chrootBase = "/var/lib/roundhouse/jail"
	scratchDir = "/var/lib/roundhouse/sandboxes"
)

// heartbeatInit prints a marker once, then a heartbeat every 50 ms, so a test
// can tell a booted guest from a restored one that kept running.
//
// The kernel command line understands double quotes only. The markers are
// computed by the shell (RH_READY_42, RH_HEARTBEAT_2) so they cannot match the
// command line itself, which the kernel echoes if init fails.
const heartbeatInit = `console=ttyS0 reboot=k panic=1 pci=off quiet ro init=/bin/sh -- -c ` +
	`"echo RH_READY_$((40+2)); while true; do echo RH_HEARTBEAT_$((1+1)); sleep 0.05; done"`

const (
	readyMarker     = "RH_READY_42"
	heartbeatMarker = "RH_HEARTBEAT_2"
)

func requireHost(t *testing.T) {
	t.Helper()
	switch {
	case os.Geteuid() != 0:
		t.Skip("needs root: run with `make integration`")
	case !exists("/dev/kvm"):
		t.Skip("needs /dev/kvm")
	case !exists("/usr/local/bin/jailer"):
		t.Skip("needs the jailer: run `make host-setup`")
	case !exists(filepath.Join(imageDir, "vmlinux")):
		t.Skip("needs base images: run `sudo bash images/fetch-base.sh`")
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// jailSpec returns a spec for a test jail with a unique ID and uid.
func jailSpec(t *testing.T, name string, files ...vmm.JailFile) vmm.JailSpec {
	t.Helper()
	id := fmt.Sprintf("rh-it-%s-%d", name, time.Now().UnixNano()%1_000_000)
	uid := 190_000 + int(time.Now().UnixNano()%10_000)
	kernel, err := filepath.EvalSymlinks(filepath.Join(imageDir, "vmlinux"))
	if err != nil {
		t.Fatal(err)
	}
	files = append([]vmm.JailFile{
		{Source: kernel, Name: "vmlinux"},
		{Source: filepath.Join(imageDir, "rootfs.ext4"), Name: "rootfs.ext4"},
	}, files...)
	return vmm.JailSpec{
		ID: id, JailerPath: "/usr/local/bin/jailer", FirecrackerPath: "/usr/local/bin/firecracker",
		ChrootBase: chrootBase, CgroupRoot: "/sys/fs/cgroup", CgroupParent: "roundhouse",
		UID: uid, GID: uid,
		MemoryMax: 512 << 20, CPUs: 1, PidsMax: 64, FileSizeLimit: 4 << 30, NoFileLimit: 1024,
		Files: files,
	}
}

func launch(ctx context.Context, t *testing.T, spec vmm.JailSpec) (*vmm.Jail, *vmm.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	jail, err := vmm.Launch(ctx, spec)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { assertGone(context.WithoutCancel(ctx), t, spec, jail) })
	client := vmm.NewClient(spec.SocketPath())
	t.Cleanup(client.Close)
	if err := client.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	return jail, client
}

// assertGone destroys the jail and checks that nothing is left behind.
func assertGone(ctx context.Context, t *testing.T, spec vmm.JailSpec, jail *vmm.Jail) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := jail.Destroy(ctx); err != nil {
		t.Errorf("Destroy: %v", err)
	}
	if err := jail.Destroy(ctx); err != nil {
		t.Errorf("second Destroy (must be idempotent): %v", err)
	}
	for _, p := range []string{spec.Dir(), spec.CgroupDir(), fmt.Sprintf("/proc/%d", jail.PID())} {
		if exists(p) {
			t.Errorf("%s still exists after Destroy", p)
		}
	}
}

func configureBoot(ctx context.Context, t *testing.T, c *vmm.Client) {
	t.Helper()
	steps := []func() error{
		func() error { return c.PutMachineConfig(ctx, vmm.MachineConfig{VCPUCount: 1, MemSizeMiB: 256}) },
		func() error {
			return c.PutBootSource(ctx, vmm.BootSource{KernelImagePath: "vmlinux", BootArgs: heartbeatInit})
		},
		func() error {
			return c.PutDrive(ctx, vmm.Drive{DriveID: "rootfs", PathOnHost: "rootfs.ext4", IsRootDevice: true, IsReadOnly: true})
		},
		func() error {
			return c.PutSerial(ctx, vmm.Serial{OutputPath: "console.log", RateLimiter: &vmm.TokenBucket{Size: 65536, RefillTimeMS: 1000}})
		},
		func() error { return c.PutEntropy(ctx, vmm.Entropy{}) },
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("configure step %d: %v", i, err)
		}
	}
}

// waitConsole waits until the jail's console log contains marker n times.
func waitConsole(t *testing.T, spec vmm.JailSpec, marker string, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(filepath.Join(spec.Root(), "console.log"))
		if strings.Count(string(data), marker) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("console never showed %q %d times within %v", marker, n, timeout)
}

func TestBootJailedVM(t *testing.T) {
	requireHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	spec := jailSpec(t, "boot")
	jail, client := launch(ctx, t, spec)

	configureBoot(ctx, t, client)
	start := time.Now()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitConsole(t, spec, readyMarker, 1, 5*time.Second)
	t.Logf("boot to guest user space: %v", time.Since(start))

	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", jail.PID()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(status), fmt.Sprintf("Uid:\t%d", spec.UID)) {
		t.Errorf("VMM does not run as uid %d", spec.UID)
	}
	cgroup, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", jail.PID()))
	if want := "0::/roundhouse/" + spec.ID; strings.TrimSpace(string(cgroup)) != want {
		t.Errorf("VMM cgroup = %q, want %q", strings.TrimSpace(string(cgroup)), want)
	}
	if exited, err := jail.Exited(); err != nil || exited {
		t.Errorf("Exited() = %v, %v while running; want false, nil", exited, err)
	}
}

func TestSnapshotRestoreIntoNewJail(t *testing.T) {
	requireHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Boot, then snapshot inside the first jail.
	first := jailSpec(t, "snap")
	firstJail, c1 := launch(ctx, t, first)
	configureBoot(ctx, t, c1)
	if err := c1.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitConsole(t, first, heartbeatMarker, 3, 5*time.Second)
	if err := c1.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := c1.CreateSnapshot(ctx, vmm.SnapshotCreate{SnapshotType: vmm.SnapshotFull, SnapshotPath: "vm.state", MemFilePath: "vm.mem"}); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	// Keep the snapshot files outside the jail, then destroy it.
	if err := os.MkdirAll(scratchDir, 0o750); err != nil {
		t.Fatal(err)
	}
	keep, err := os.MkdirTemp(scratchDir, "it-snap-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(keep) })
	for _, f := range []string{"vm.state", "vm.mem"} {
		if err := os.Link(filepath.Join(first.Root(), f), filepath.Join(keep, f)); err != nil {
			t.Fatal(err)
		}
	}
	if err := firstJail.Destroy(ctx); err != nil {
		t.Fatalf("Destroy first jail: %v", err)
	}

	// Restore into a fresh jail and check the guest keeps running.
	second := jailSpec(t, "restore",
		vmm.JailFile{Source: filepath.Join(keep, "vm.state"), Name: "vm.state"},
		vmm.JailFile{Source: filepath.Join(keep, "vm.mem"), Name: "vm.mem"},
	)
	start := time.Now()
	_, c2 := launch(ctx, t, second)
	if err := c2.PutSerial(ctx, vmm.Serial{OutputPath: "console.log", RateLimiter: &vmm.TokenBucket{Size: 65536, RefillTimeMS: 1000}}); err != nil {
		t.Fatalf("PutSerial before restore: %v", err)
	}
	if err := c2.LoadSnapshot(ctx, vmm.SnapshotLoad{
		SnapshotPath:  "vm.state",
		MemBackend:    vmm.MemoryBackend{BackendType: vmm.BackendFile, BackendPath: "vm.mem"},
		ResumeVM:      true,
		ClockRealtime: true,
	}); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	t.Logf("jail launch to resumed VM: %v", time.Since(start))
	waitConsole(t, second, heartbeatMarker, 3, 5*time.Second)
}

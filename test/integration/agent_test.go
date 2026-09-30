//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shusingh/roundhouse/internal/agentclient"
	"github.com/shusingh/roundhouse/internal/agentproto"
	"github.com/shusingh/roundhouse/internal/vmm"
)

const guestImageDir = "/var/lib/roundhouse/images/default"

const agentBootArgs = "console=ttyS0 reboot=k panic=1 pci=off quiet init=/sbin/roundhouse-agent"

func requireGuestImage(t *testing.T) {
	t.Helper()
	requireHost(t)
	if !exists(filepath.Join(guestImageDir, "rootfs.ext4")) {
		t.Skip("needs the guest image: run `sudo bash images/build-rootfs.sh bin/roundhouse-agent`")
	}
}

// guestSpec returns a jail spec with the guest image and a fresh scratch disk.
func guestSpec(ctx context.Context, t *testing.T, name string, extra ...vmm.JailFile) vmm.JailSpec {
	t.Helper()
	dir, err := os.MkdirTemp(scratchDir, "it-"+name+"-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	scratch := filepath.Join(dir, "scratch.ext4")
	out, err := exec.CommandContext(ctx, "cp", "--sparse=always", filepath.Join(guestImageDir, "scratch.ext4"), scratch).CombinedOutput() //nolint:gosec // G204: fixed program, test-owned paths
	if err != nil {
		t.Fatalf("copy scratch disk: %v: %s", err, out)
	}
	if err := os.Chmod(scratch, 0o600); err != nil {
		t.Fatal(err)
	}
	spec := jailSpec(t, name, extra...)
	spec.Files[1] = vmm.JailFile{Source: filepath.Join(guestImageDir, "rootfs.ext4"), Name: "rootfs.ext4"}
	spec.Files = append(spec.Files, vmm.JailFile{Source: scratch, Name: "scratch.ext4", Writable: true})
	return spec
}

func bootGuest(ctx context.Context, t *testing.T, spec vmm.JailSpec) (*vmm.Jail, *vmm.Client, *agentclient.Client) {
	t.Helper()
	jail, c := launch(ctx, t, spec)
	// Cleanups run last-registered first, so this reads the console before
	// the jail (and its console log) is destroyed.
	t.Cleanup(func() {
		if t.Failed() {
			console, _ := os.ReadFile(filepath.Join(spec.Root(), "console.log"))
			t.Logf("guest console (last 4 KiB):\n%s", tail(string(console), 4096))
		}
	})
	steps := []func() error{
		func() error { return c.PutMachineConfig(ctx, vmm.MachineConfig{VCPUCount: 1, MemSizeMiB: 256}) },
		func() error {
			return c.PutBootSource(ctx, vmm.BootSource{KernelImagePath: "vmlinux", BootArgs: agentBootArgs})
		},
		func() error {
			return c.PutDrive(ctx, vmm.Drive{DriveID: "rootfs", PathOnHost: "rootfs.ext4", IsRootDevice: true, IsReadOnly: true})
		},
		func() error { return c.PutDrive(ctx, vmm.Drive{DriveID: "scratch", PathOnHost: "scratch.ext4"}) },
		func() error { return c.PutVsock(ctx, vmm.Vsock{GuestCID: 3, UDSPath: "/v.sock"}) },
		func() error { return c.PutSerial(ctx, vmm.Serial{OutputPath: "console.log"}) },
		func() error { return c.PutEntropy(ctx, vmm.Entropy{}) },
		func() error { return c.Start(ctx) },
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("boot step %d: %v", i, err)
		}
	}
	agent := agentclient.New(filepath.Join(spec.Root(), "v.sock"))
	t.Cleanup(agent.Close)
	return jail, c, agent
}

func run(ctx context.Context, t *testing.T, agent *agentclient.Client, argv ...string) (string, *agentproto.Frame) {
	t.Helper()
	var out bytes.Buffer
	exit, err := agent.Exec(ctx, agentproto.ExecRequest{Argv: argv, TimeoutMS: 10_000}, func(f *agentproto.Frame) error {
		out.Write(f.Data)
		return nil
	})
	if err != nil {
		t.Fatalf("exec %v: %v", argv, err)
	}
	return out.String(), exit
}

func TestAgentInGuest(t *testing.T) {
	requireGuestImage(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	spec := guestSpec(ctx, t, "agent")
	start := time.Now()
	_, _, agent := bootGuest(ctx, t, spec)
	if err := agent.WaitReady(ctx); err != nil {
		console, _ := os.ReadFile(filepath.Join(spec.Root(), "console.log"))
		t.Fatalf("agent never answered: %v\nconsole:\n%s", err, console)
	}
	t.Logf("cold boot to agent ready: %v", time.Since(start))

	if out, exit := run(ctx, t, agent, "python3", "-c", "print(6*7)"); out != "42\n" || exit.ExitCode != 0 {
		t.Errorf("python3 printed %q with exit %+v, want 42", out, exit)
	}
	// The overlay makes the read-only root writable, and writes land on the
	// sandbox's own scratch disk.
	if out, exit := run(ctx, t, agent, "sh", "-c", "echo hi > /etc/rh-probe && cat /etc/rh-probe && hostname"); out != "hi\nsandbox\n" || exit.ExitCode != 0 {
		t.Errorf("overlay write printed %q, exit %+v", out, exit)
	}
	if err := agent.WriteFile(ctx, "/root/data/in.txt", strings.NewReader("from host")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if out, _ := run(ctx, t, agent, "cat", "/root/data/in.txt"); out != "from host" {
		t.Errorf("guest read %q, want the uploaded content", out)
	}
	var got bytes.Buffer
	if _, err := agent.ReadFile(ctx, "/etc/hostname", &got); err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	start = time.Now()
	for range 20 {
		run(ctx, t, agent, "true")
	}
	t.Logf("exec round trip (true), mean of 20: %v", time.Since(start)/20)
}

func TestRestoredClonesAreUnique(t *testing.T) {
	requireGuestImage(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Build a snapshot of a guest whose agent is ready.
	tmpl := guestSpec(ctx, t, "tmpl")
	tmplJail, tc, tmplAgent := bootGuest(ctx, t, tmpl)
	if err := tmplAgent.WaitReady(ctx); err != nil {
		t.Fatalf("template agent: %v", err)
	}
	tmplAgent.Close()
	if err := tc.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tc.CreateSnapshot(ctx, vmm.SnapshotCreate{SnapshotType: vmm.SnapshotFull, SnapshotPath: "vm.state", MemFilePath: "vm.mem"}); err != nil {
		t.Fatal(err)
	}
	keep, err := os.MkdirTemp(scratchDir, "it-tmpl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(keep) })
	for _, f := range []string{"vm.state", "vm.mem", "scratch.ext4"} {
		if err := os.Link(filepath.Join(tmpl.Root(), f), filepath.Join(keep, f)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tmplJail.Destroy(ctx); err != nil {
		t.Fatal(err)
	}

	// Restore two clones; each gets its own copy of the template's scratch disk.
	outputs := make([]string, 2)
	for i := range outputs {
		dir, err := os.MkdirTemp(scratchDir, "it-clone-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		scratch := filepath.Join(dir, "scratch.ext4")
		if out, err := exec.CommandContext(ctx, "cp", "--sparse=always", filepath.Join(keep, "scratch.ext4"), scratch).CombinedOutput(); err != nil { //nolint:gosec // G204: fixed program, test-owned paths
			t.Fatalf("copy scratch: %v: %s", err, out)
		}
		spec := jailSpec(t, fmt.Sprintf("clone%d", i),
			vmm.JailFile{Source: filepath.Join(keep, "vm.state"), Name: "vm.state"},
			vmm.JailFile{Source: filepath.Join(keep, "vm.mem"), Name: "vm.mem"},
			vmm.JailFile{Source: scratch, Name: "scratch.ext4", Writable: true},
		)
		spec.Files[1] = vmm.JailFile{Source: filepath.Join(guestImageDir, "rootfs.ext4"), Name: "rootfs.ext4"}

		start := time.Now()
		_, c := launch(ctx, t, spec)
		if err := c.PutSerial(ctx, vmm.Serial{OutputPath: "console.log"}); err != nil {
			t.Fatal(err)
		}
		if err := c.LoadSnapshot(ctx, vmm.SnapshotLoad{
			SnapshotPath: "vm.state", MemBackend: vmm.MemoryBackend{BackendType: vmm.BackendFile, BackendPath: "vm.mem"},
			ResumeVM: true, ClockRealtime: true,
		}); err != nil {
			t.Fatalf("LoadSnapshot: %v", err)
		}
		agent := agentclient.New(filepath.Join(spec.Root(), "v.sock"))
		t.Cleanup(agent.Close)
		entropy := make([]byte, 64)
		_, _ = rand.Read(entropy)
		if err := agent.WaitReady(ctx); err != nil {
			t.Fatalf("clone %d agent: %v", i, err)
		}
		if err := agent.Restore(ctx, agentproto.RestoreRequest{SandboxID: spec.ID, Entropy: entropy}); err != nil {
			t.Fatalf("clone %d restore: %v", i, err)
		}
		t.Logf("clone %d: jail launch to agent restored: %v", i, time.Since(start))
		outputs[i], _ = run(ctx, t, agent, "sh", "-c", "cat /proc/sys/kernel/random/uuid; head -c 16 /dev/urandom | od -An -tx1")
	}
	if outputs[0] == outputs[1] || outputs[0] == "" {
		t.Errorf("two clones produced the same random output:\n%s", outputs[0])
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

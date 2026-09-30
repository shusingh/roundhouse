package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManifestMismatchNamesTheField(t *testing.T) {
	t.Parallel()
	want := Manifest{
		Image: "default", VCPUs: 1, MemoryMiB: 256, FirecrackerVersion: "Firecracker v1.17.0",
		CPUModel: "AMD Ryzen 9", KernelSHA256: "aa", RootfsSHA256: "bb", KernelArgs: "console=ttyS0",
		NetworkBytesPerSecond: 12_500_000,
	}
	same := want
	same.CreatedAt = time.Now()
	if got := same.mismatch(want); got != "" {
		t.Errorf("a manifest differing only in created_at mismatched: %s", got)
	}
	for field, change := range map[string]func(*Manifest){
		"firecracker_version":      func(m *Manifest) { m.FirecrackerVersion = "Firecracker v1.18.0" },
		"cpu_model":                func(m *Manifest) { m.CPUModel = "Intel Xeon" },
		"rootfs_sha256":            func(m *Manifest) { m.RootfsSHA256 = "cc" },
		"kernel_args":              func(m *Manifest) { m.KernelArgs = "console=ttyS0 quiet" },
		"network_bytes_per_second": func(m *Manifest) { m.NetworkBytesPerSecond = 1 },
		"memory_mib":               func(m *Manifest) { m.MemoryMiB = 512 },
		"disk_ops_per_second":      func(m *Manifest) { m.DiskOpsPerSecond = 1 },
	} {
		got := want
		change(&got)
		if reason := got.mismatch(want); !strings.HasPrefix(reason, field+" ") {
			t.Errorf("changing %s gave mismatch %q", field, reason)
		}
	}
}

func TestCPUModel(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "cpuinfo")
	info := "processor\t: 0\nvendor_id\t: AuthenticAMD\nmodel name\t: AMD Ryzen 9 7950X 16-Core Processor\nflags\t\t: fpu\n"
	if err := os.WriteFile(file, []byte(info), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := cpuModel(file); err != nil || got != "AMD Ryzen 9 7950X 16-Core Processor" {
		t.Errorf("cpuModel = %q, %v", got, err)
	}
	if err := os.WriteFile(file, []byte("processor\t: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cpuModel(file); err == nil {
		t.Error("cpuModel accepted cpuinfo without a model name")
	}
}

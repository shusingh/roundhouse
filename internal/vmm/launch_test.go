package vmm

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCgroupPopulated(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		events  *string
		want    bool
		wantErr bool
	}{
		{"populated", ptr("populated 1\nfrozen 0\n"), true, false},
		{"empty", ptr("populated 0\nfrozen 0\n"), false, false},
		{"missing cgroup", nil, false, false},
		{"malformed", ptr("frozen 0\n"), false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.events != nil {
				writeFile(t, filepath.Join(dir, "cgroup.events"), *tc.events)
			} else {
				dir = filepath.Join(dir, "absent")
			}
			got, err := cgroupPopulated(dir)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("cgroupPopulated = %v, %v; want %v, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestKillCgroupMissingIsNoop(t *testing.T) {
	t.Parallel()
	if err := killCgroup(context.Background(), filepath.Join(t.TempDir(), "gone")); err != nil {
		t.Fatalf("killCgroup on a missing cgroup = %v, want nil", err)
	}
}

func TestWaitCgroupEmpty(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		// Windows fails an open that races the rename below; cgroups exist
		// only on Linux anyway.
		t.Skip("cgroup.events replacement is only race-free on Linux")
	}
	dir := t.TempDir()
	events := filepath.Join(dir, "cgroup.events")
	writeFile(t, events, "populated 1\n")
	go func() {
		time.Sleep(20 * time.Millisecond)
		// Replace the file atomically: the kernel's cgroup.events never reads
		// as empty, but a file rewritten in place briefly does.
		tmp := events + ".tmp"
		_ = os.WriteFile(tmp, []byte("populated 0\n"), 0o600)
		_ = os.Rename(tmp, events)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := waitCgroupEmpty(ctx, dir); err != nil {
		t.Fatalf("waitCgroupEmpty = %v, want nil once the group empties", err)
	}
}

func TestKillCgroupWritesKillAndHonorsContext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "cgroup.events"), "populated 1\n")
	writeFile(t, filepath.Join(dir, "cgroup.kill"), "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := killCgroup(ctx, dir); err == nil {
		t.Fatal("killCgroup returned nil for a cgroup that never empties")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "cgroup.kill")); string(got) != "1" {
		t.Errorf("cgroup.kill = %q, want %q", got, "1")
	}
}

func TestReadPIDInvalid(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "firecracker.pid")
	writeFile(t, file, "not-a-pid")
	if _, err := readPID(context.Background(), file); err == nil {
		t.Fatal("readPID accepted a malformed pid file")
	}
}

func TestLaunchRejectsInvalidSpec(t *testing.T) {
	t.Parallel()
	s := validSpec()
	s.UID = 0
	if _, err := Launch(context.Background(), s); err == nil {
		t.Fatal("Launch accepted a spec that runs the VMM as root")
	}
}

func ptr(s string) *string { return &s }

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

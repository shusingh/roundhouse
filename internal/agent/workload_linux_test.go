package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMemTotalMiB(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(file, []byte("MemTotal:         235812 kB\nMemFree:          1 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := memTotalMiB(file); err != nil || got != 230 {
		t.Errorf("memTotalMiB = %d, %v; want 230", got, err)
	}
}

func TestOpenWorkloadCgroupOutsideAGuest(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat(workloadCgroup); err == nil {
		t.Skip("running where a workload cgroup exists")
	}
	if fd := openWorkloadCgroup(); fd != -1 {
		t.Errorf("openWorkloadCgroup = %d outside a guest, want -1 so commands run unconfined in tests", fd)
	}
}

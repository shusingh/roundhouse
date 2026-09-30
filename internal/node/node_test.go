package node

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMemTotalMiB(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	good := filepath.Join(dir, "meminfo")
	if err := os.WriteFile(good, []byte("MemTotal:       32505856 kB\nMemFree:        1000 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := memTotalMiB(good); err != nil || got != 31744 {
		t.Errorf("memTotalMiB = %d, %v; want 31744", got, err)
	}
	bad := filepath.Join(dir, "empty")
	if err := os.WriteFile(bad, []byte("MemFree: 1 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := memTotalMiB(bad); err == nil {
		t.Error("memTotalMiB accepted a file without MemTotal")
	}
}

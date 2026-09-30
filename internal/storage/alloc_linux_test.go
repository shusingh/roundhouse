package storage

import (
	"syscall"
	"testing"
)

// allocatedBytes reports the disk space a file really occupies.
func allocatedBytes(t *testing.T, name string) int64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(name, &st); err != nil {
		t.Fatal(err)
	}
	return st.Blocks * 512
}

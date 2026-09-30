//go:build !linux

package storage

import "testing"

// allocatedBytes is only meaningful on Linux, where sparseness is checked.
func allocatedBytes(t *testing.T, _ string) int64 {
	t.Helper()
	return 0
}

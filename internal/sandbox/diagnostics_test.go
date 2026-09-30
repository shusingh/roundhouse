package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPruneDiagnosticsKeepsTheNewest(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	names := []string{"20260929T100000Z-sb-a", "20260929T110000Z-sb-b", "20260929T120000Z-sb-c", "20260929T130000Z-sb-d"}
	for _, n := range names {
		if err := os.Mkdir(filepath.Join(base, n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := pruneDiagnostics(base, 2); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(base)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if !slices.Equal(left, names[2:]) {
		t.Errorf("kept %v, want the newest two %v", left, names[2:])
	}
}

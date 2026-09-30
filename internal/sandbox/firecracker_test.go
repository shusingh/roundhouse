package sandbox

import "testing"

func TestJailFileSizeLimitCoversDiskAndMemory(t *testing.T) {
	t.Parallel()
	b := NewFirecrackerBooter(FirecrackerConfig{VMMOverheadMiB: 64}, nil)
	limits := Limits{VCPUs: 1, MemoryMiB: 256}
	mem := int64(256+64) << 20
	for _, tc := range []struct {
		name string
		disk int64
		want int64
	}{
		{"disk larger than memory", 2 << 30, 2<<30 + 64<<20},
		{"memory larger than disk", 64 << 20, mem + 64<<20},
	} {
		spec := b.jailSpec("sb-1", 100, limits, tc.disk, nil)
		if spec.FileSizeLimit != tc.want {
			t.Errorf("%s: FileSizeLimit = %d, want %d", tc.name, spec.FileSizeLimit, tc.want)
		}
	}
}

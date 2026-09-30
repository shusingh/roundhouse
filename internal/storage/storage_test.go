package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func writeSparse(t *testing.T, name string, size int64, chunks map[int64][]byte) {
	t.Helper()
	f, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	for off, data := range chunks {
		if _, err := f.WriteAt(data, off); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSparseCopyPreservesContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src.img"), filepath.Join(dir, "dst.img")
	const size = 64 << 20
	chunks := map[int64][]byte{
		0:             []byte("superblock"),
		8 << 20:       bytes.Repeat([]byte{0xab}, 1<<20),
		size - 4096*2: []byte("tail"),
	}
	writeSparse(t, src, size, chunks)

	if err := SparseCopy(dst, src, 0o600); err != nil {
		t.Fatalf("SparseCopy: %v", err)
	}
	want, _ := os.ReadFile(src)
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(want, got) {
		t.Fatal("copy differs from source")
	}
	if runtime.GOOS == "linux" {
		// The copy should be sparse: far fewer blocks than its size.
		if used := allocatedBytes(t, dst); used > 8<<20 {
			t.Errorf("copy allocates %d bytes for 1 MiB of data; holes were not preserved", used)
		}
	}
}

func TestSparseCopyRefusesExistingDestination(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	writeSparse(t, src, 10, nil)
	writeSparse(t, dst, 10, nil)
	if err := SparseCopy(dst, src, 0o600); err == nil {
		t.Fatal("SparseCopy overwrote an existing file")
	}
}

func TestLayoutSandboxLifecycle(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	l := Layout{Root: root}
	imgDir := l.ImageDir("default")
	if err := os.MkdirAll(imgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"vmlinux", "rootfs.ext4"} {
		writeSparse(t, filepath.Join(imgDir, f), 16, nil)
	}
	if _, err := l.OpenImage("default"); err == nil {
		t.Fatal("OpenImage accepted an image without a scratch disk")
	}
	writeSparse(t, filepath.Join(imgDir, "scratch.ext4"), 1<<20, map[int64][]byte{0: []byte("ext4")})
	img, err := l.OpenImage("default")
	if err != nil {
		t.Fatalf("OpenImage: %v", err)
	}

	uid, gid := os.Getuid(), os.Getgid()
	if runtime.GOOS == "windows" {
		t.Skip("chown is not supported on Windows")
	}
	scratch, err := l.CreateSandboxDir("sb-1", img, uid, gid)
	if err != nil {
		t.Fatalf("CreateSandboxDir: %v", err)
	}
	if info, err := os.Stat(scratch); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("scratch disk %s: %v, mode %v; want 0600", scratch, err, info.Mode().Perm())
	}
	if _, err := l.CreateSandboxDir("sb-1", img, uid, gid); err == nil {
		t.Error("CreateSandboxDir reused an existing sandbox directory")
	}
	ids, err := l.SandboxIDs()
	if err != nil || !slices.Equal(ids, []string{"sb-1"}) {
		t.Errorf("SandboxIDs = %v, %v; want [sb-1]", ids, err)
	}
	if err := l.RemoveSandboxDir("sb-1"); err != nil {
		t.Fatal(err)
	}
	if err := l.RemoveSandboxDir("sb-1"); err != nil {
		t.Errorf("second RemoveSandboxDir = %v, want nil", err)
	}
	for _, bad := range []string{"../x", "SB", "", "a/b"} {
		if _, err := l.CreateSandboxDir(bad, img, uid, gid); err == nil {
			t.Errorf("CreateSandboxDir(%q) accepted an invalid id", bad)
		}
	}
}

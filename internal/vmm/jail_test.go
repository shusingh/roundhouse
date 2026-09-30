package vmm

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func validSpec() JailSpec {
	return JailSpec{
		ID:              "rh-test1",
		JailerPath:      "/usr/local/bin/jailer",
		FirecrackerPath: "/usr/local/bin/firecracker",
		ChrootBase:      "/var/lib/roundhouse/jail",
		CgroupRoot:      "/sys/fs/cgroup",
		CgroupParent:    "roundhouse",
		UID:             100001,
		GID:             100001,
		MemoryMax:       384 << 20,
		CPUs:            1.5,
		PidsMax:         64,
		FileSizeLimit:   4 << 30,
		NoFileLimit:     1024,
		NetNS:           "/var/run/netns/rh-test1",
		Files: []JailFile{
			{Source: "/var/lib/roundhouse/images/base/vmlinux", Name: "vmlinux"},
			{Source: "/var/lib/roundhouse/sandboxes/test1/scratch.ext4", Name: "scratch.ext4", Writable: true},
		},
	}
}

func TestJailSpecPaths(t *testing.T) {
	t.Parallel()
	s := validSpec()
	got := map[string]string{
		"dir": s.Dir(), "root": s.Root(), "socket": s.SocketPath(), "cgroup": s.CgroupDir(),
	}
	want := map[string]string{
		"dir":    "/var/lib/roundhouse/jail/firecracker/rh-test1",
		"root":   "/var/lib/roundhouse/jail/firecracker/rh-test1/root",
		"socket": "/var/lib/roundhouse/jail/firecracker/rh-test1/root/run/firecracker.socket",
		"cgroup": "/sys/fs/cgroup/roundhouse/rh-test1",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("paths (-want +got):\n%s", diff)
	}
}

func TestJailerArgs(t *testing.T) {
	t.Parallel()
	want := []string{
		"--id", "rh-test1",
		"--exec-file", "/usr/local/bin/firecracker",
		"--uid", "100001", "--gid", "100001",
		"--chroot-base-dir", "/var/lib/roundhouse/jail",
		"--cgroup-version", "2",
		"--parent-cgroup", "roundhouse",
		"--cgroup", "memory.max=402653184",
		"--cgroup", "cpu.max=150000 100000",
		"--cgroup", "pids.max=64",
		"--resource-limit", "fsize=4294967296",
		"--resource-limit", "no-file=1024",
		"--netns", "/var/run/netns/rh-test1",
		"--new-pid-ns", "--daemonize",
		"--", "--api-sock", "/run/firecracker.socket", "--level", "Warning",
	}
	if diff := cmp.Diff(want, validSpec().jailerArgs()); diff != "" {
		t.Errorf("jailerArgs (-want +got):\n%s", diff)
	}
}

func TestJailerArgsWithoutNetNS(t *testing.T) {
	t.Parallel()
	s := validSpec()
	s.NetNS = ""
	for _, arg := range s.jailerArgs() {
		if arg == "--netns" {
			t.Fatal("jailerArgs includes --netns for a spec without a namespace")
		}
	}
}

func TestJailSpecValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*JailSpec)
		wantErr string
	}{
		{"valid", func(*JailSpec) {}, ""},
		{"id with slash", func(s *JailSpec) { s.ID = "rh/../x" }, "jail id"},
		{"id too long", func(s *JailSpec) { s.ID = strings.Repeat("a", 65) }, "jail id"},
		{"relative jailer", func(s *JailSpec) { s.JailerPath = "jailer" }, "jailer path"},
		{"absolute cgroup parent", func(s *JailSpec) { s.CgroupParent = "/roundhouse" }, "cgroup parent"},
		{"root uid", func(s *JailSpec) { s.UID = 0 }, "unprivileged"},
		{"no memory limit", func(s *JailSpec) { s.MemoryMax = 0 }, "must all be positive"},
		{"file escapes jail", func(s *JailSpec) { s.Files[0].Name = "../../etc/passwd" }, "inside the jail"},
		{"relative file source", func(s *JailSpec) { s.Files[1].Source = "scratch.ext4" }, "must be absolute"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := validSpec()
			s.Files = append([]JailFile(nil), s.Files...)
			tc.mutate(&s)
			err := s.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("Validate() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

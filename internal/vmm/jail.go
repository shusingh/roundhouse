package vmm

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// cpuPeriod is the cgroup v2 CPU accounting period used for cpu.max.
const cpuPeriod = 100 * time.Millisecond

// apiSocketInJail is where Firecracker creates its API socket, relative to the
// jail root.
const apiSocketInJail = "run/firecracker.socket"

// validJailID matches the jailer's rule for --id: alphanumerics and hyphens,
// at most 64 characters.
var validJailID = regexp.MustCompile(`^[a-zA-Z0-9-]{1,64}$`)

// Jail paths are Linux paths on every platform and are handled with package
// path, never filepath.

// JailFile is a host file made available inside the jail root. Files are hard
// linked, never copied, so the source and the jail must be on one file system
// (see docs/adr/0006-storage.md).
type JailFile struct {
	// Source is the absolute host path.
	Source string
	// Name is the file's path relative to the jail root; it is also the path
	// Firecracker is given, because Firecracker runs chrooted there.
	Name string
	// Writable files are chowned to the jail's uid and gid so the unprivileged
	// VMM can write them. Read-only files keep their owner and must be world
	// readable.
	Writable bool
}

// JailSpec describes one jailed Firecracker process.
type JailSpec struct {
	// ID names the jail, its cgroup, and its directory. It must satisfy the
	// jailer's rules and, by convention, carry the "rh-" prefix.
	ID string
	// JailerPath and FirecrackerPath are absolute paths to the binaries, which
	// must be the same release.
	JailerPath      string
	FirecrackerPath string
	// ChrootBase is the directory under which the jailer builds
	// <ChrootBase>/<firecracker binary name>/<ID>/root.
	ChrootBase string
	// CgroupRoot is the cgroup v2 mount point, normally /sys/fs/cgroup.
	CgroupRoot string
	// CgroupParent is the cgroup under CgroupRoot that holds every jail.
	CgroupParent string
	// UID and GID are the unprivileged identity the VMM runs as; one pair per
	// sandbox, so sandboxes cannot signal or read each other.
	UID, GID int
	// MemoryMax caps the VMM's memory (guest memory plus VMM overhead), in
	// bytes.
	MemoryMax int64
	// CPUs caps CPU time as a number of CPUs (cpu.max quota over period).
	CPUs float64
	// PidsMax caps the number of processes and threads in the jail's cgroup.
	PidsMax int
	// FileSizeLimit caps any file the VMM writes (snapshots included), in bytes.
	FileSizeLimit int64
	// NoFileLimit caps the VMM's open file descriptors.
	NoFileLimit int
	// NetNS is the path of a network namespace to join, or empty for the
	// host's namespace (only for tests).
	NetNS string
	// Files are linked into the jail root before launch.
	Files []JailFile
}

// Validate reports the first problem with the spec.
func (s JailSpec) Validate() error {
	var errs []error
	if !validJailID.MatchString(s.ID) {
		errs = append(errs, fmt.Errorf("jail id %q: want 1 to 64 letters, digits, or hyphens", s.ID))
	}
	for name, p := range map[string]string{
		"jailer path": s.JailerPath, "firecracker path": s.FirecrackerPath,
		"chroot base": s.ChrootBase, "cgroup root": s.CgroupRoot,
	} {
		if !path.IsAbs(p) {
			errs = append(errs, fmt.Errorf("%s %q: must be absolute", name, p))
		}
	}
	if s.CgroupParent == "" || path.IsAbs(s.CgroupParent) {
		errs = append(errs, fmt.Errorf("cgroup parent %q: must be a relative cgroup name", s.CgroupParent))
	}
	if s.UID <= 0 || s.GID <= 0 {
		errs = append(errs, fmt.Errorf("uid %d, gid %d: must be unprivileged (greater than 0)", s.UID, s.GID))
	}
	if s.MemoryMax <= 0 || s.CPUs <= 0 || s.PidsMax <= 0 || s.FileSizeLimit <= 0 || s.NoFileLimit <= 0 {
		errs = append(errs, errors.New("memory, CPU, pids, file size, and open file limits must all be positive"))
	}
	for _, f := range s.Files {
		if !path.IsAbs(f.Source) {
			errs = append(errs, fmt.Errorf("jail file source %q: must be absolute", f.Source))
		}
		if !isLocal(f.Name) {
			errs = append(errs, fmt.Errorf("jail file name %q: must be a relative path inside the jail", f.Name))
		}
	}
	return errors.Join(errs...)
}

// isLocal reports whether p is a relative slash path that stays inside its
// base directory. Jail paths are Linux paths on every platform, so this uses
// package path rather than filepath.
func isLocal(p string) bool {
	if p == "" || path.IsAbs(p) {
		return false
	}
	c := path.Clean(p)
	return c != "." && c != ".." && !strings.HasPrefix(c, "../")
}

// Dir is the jailer's per-VM directory, <ChrootBase>/<binary>/<ID>. Removing
// it removes the jail.
func (s JailSpec) Dir() string {
	return path.Join(s.ChrootBase, path.Base(s.FirecrackerPath), s.ID)
}

// Root is the jail's chroot: the directory Firecracker sees as /.
func (s JailSpec) Root() string {
	return path.Join(s.Dir(), "root")
}

// SocketPath is the host path of Firecracker's API socket.
func (s JailSpec) SocketPath() string {
	return path.Join(s.Root(), apiSocketInJail)
}

// CgroupDir is the jail's cgroup directory.
func (s JailSpec) CgroupDir() string {
	return path.Join(s.CgroupRoot, s.CgroupParent, s.ID)
}

// jailerArgs returns the jailer's command line (without argv[0]). The VMM
// runs in a new PID namespace and daemonizes; its console goes through the
// serial device API, not stdout.
func (s JailSpec) jailerArgs() []string {
	quota := int64(s.CPUs * float64(cpuPeriod.Microseconds()))
	args := []string{
		"--id", s.ID,
		"--exec-file", s.FirecrackerPath,
		"--uid", strconv.Itoa(s.UID),
		"--gid", strconv.Itoa(s.GID),
		"--chroot-base-dir", s.ChrootBase,
		"--cgroup-version", "2",
		"--parent-cgroup", s.CgroupParent,
		"--cgroup", "memory.max=" + strconv.FormatInt(s.MemoryMax, 10),
		"--cgroup", fmt.Sprintf("cpu.max=%d %d", quota, cpuPeriod.Microseconds()),
		"--cgroup", "pids.max=" + strconv.Itoa(s.PidsMax),
		"--resource-limit", "fsize=" + strconv.FormatInt(s.FileSizeLimit, 10),
		"--resource-limit", "no-file=" + strconv.Itoa(s.NoFileLimit),
	}
	if s.NetNS != "" {
		args = append(args, "--netns", s.NetNS)
	}
	return append(args,
		"--new-pid-ns",
		"--daemonize",
		"--",
		"--api-sock", "/"+apiSocketInJail,
		"--level", "Warning",
	)
}

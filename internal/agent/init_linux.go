package agent

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/shusingh/roundhouse/internal/agentproto"
)

// Guest layout. The root file system is the template's read-only image
// (/dev/vda); the sandbox's writable scratch disk (/dev/vdb) is overlaid on it.
// See docs/adr/0006-storage.md.
const (
	scratchDevice = "/dev/vdb"
	scratchMount  = "/mnt"
	overlayRoot   = "/mnt/root"
	oldRootName   = ".oldroot"
	hostname      = "sandbox"
)

// ServeArg is the argument that starts the agent in server mode. As PID 1 the
// agent re-executes itself with it.
const ServeArg = "serve"

// RunInit runs the agent as the guest's init process: it prepares the file
// systems, starts the protocol server as a child, and then reaps orphaned
// processes for the life of the VM. It returns only on a setup failure, which
// ends the VM (the kernel panics when init exits).
//
// The server runs in a child rather than in PID 1 itself because init must
// reap every orphan with wait4(-1), which would steal exit statuses from the
// commands the server runs with os/exec.
func RunInit(logger *slog.Logger) error {
	if err := mountEssentials(); err != nil {
		return err
	}
	switch err := overlayScratch(); {
	case errors.Is(err, os.ErrNotExist):
		logger.Warn("no scratch disk; root file system stays read-only")
	case err != nil:
		return err
	}
	if err := mountAfterPivot(); err != nil {
		return err
	}
	if err := unix.Sethostname([]byte(hostname)); err != nil {
		return fmt.Errorf("set hostname: %w", err)
	}
	if err := loopbackUp(); err != nil {
		return err
	}
	if err := writeResolvConf(); err != nil {
		logger.Warn("write resolv.conf", slog.String("error", err.Error()))
	}
	superviseServer(logger)
	return nil
}

func mountEssentials() error {
	mounts := []struct{ source, target, fstype, data string }{
		{"proc", "/proc", "proc", ""},
		{"sysfs", "/sys", "sysfs", ""},
		// The kernel mounts devtmpfs on /dev itself (CONFIG_DEVTMPFS_MOUNT).
		{"tmpfs", "/run", "tmpfs", "mode=0755,size=16m"},
	}
	for _, m := range mounts {
		if err := unix.Mount(m.source, m.target, m.fstype, unix.MS_NOSUID|unix.MS_NODEV, m.data); err != nil &&
			!errors.Is(err, unix.EBUSY) {
			return fmt.Errorf("mount %s on %s: %w", m.fstype, m.target, err)
		}
	}
	return nil
}

// overlayScratch mounts the scratch disk, builds an overlay of it over the
// read-only root, and pivots into the overlay. It returns os.ErrNotExist when
// the VM has no scratch disk.
func overlayScratch() error {
	if _, err := os.Stat(scratchDevice); err != nil {
		return fmt.Errorf("scratch disk %s: %w", scratchDevice, os.ErrNotExist)
	}
	if err := unix.Mount(scratchDevice, scratchMount, "ext4", unix.MS_NOATIME, ""); err != nil {
		return fmt.Errorf("mount scratch disk: %w", err)
	}
	for _, dir := range []string{"/mnt/upper", "/mnt/work", overlayRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: guest system directories
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	opts := "lowerdir=/,upperdir=/mnt/upper,workdir=/mnt/work"
	if err := unix.Mount("overlay", overlayRoot, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount overlay root: %w", err)
	}
	// Carry the pseudo file systems into the new root.
	for _, m := range []string{"/proc", "/sys", "/dev", "/run"} {
		if err := unix.Mount(m, overlayRoot+m, "", unix.MS_MOVE, ""); err != nil {
			return fmt.Errorf("move %s into the overlay: %w", m, err)
		}
	}
	oldRoot := overlayRoot + "/" + oldRootName
	if err := os.MkdirAll(oldRoot, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", oldRoot, err)
	}
	if err := unix.Chdir(overlayRoot); err != nil {
		return fmt.Errorf("enter overlay root: %w", err)
	}
	if err := unix.PivotRoot(".", oldRootName); err != nil {
		return fmt.Errorf("pivot into overlay root: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("enter new root: %w", err)
	}
	// The old root stays alive underneath the overlay as its lower layer; only
	// its mount point in the new tree goes away.
	if err := unix.Unmount("/"+oldRootName, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detach old root: %w", err)
	}
	return os.Remove("/" + oldRootName)
}

// mountAfterPivot mounts what belongs under /dev, which moved with the pivot.
func mountAfterPivot() error {
	for _, dir := range []string{"/dev/pts", "/dev/shm"} {
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: guest system directories
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	if err := unix.Mount("devpts", "/dev/pts", "devpts", unix.MS_NOSUID|unix.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620"); err != nil &&
		!errors.Is(err, unix.EBUSY) {
		return fmt.Errorf("mount devpts: %w", err)
	}
	if err := unix.Mount("tmpfs", "/dev/shm", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777,size=64m"); err != nil &&
		!errors.Is(err, unix.EBUSY) {
		return fmt.Errorf("mount /dev/shm: %w", err)
	}
	return nil
}

// writeResolvConf points the guest at the public resolvers. The image's
// /etc/resolv.conf is a symlink to systemd-resolved's stub, which does not run
// in a sandbox, so it is replaced with a plain file.
func writeResolvConf() error {
	var b strings.Builder
	for _, r := range agentproto.DNSResolvers {
		b.WriteString("nameserver " + r + "\n")
	}
	_ = os.Remove("/etc/resolv.conf")
	return os.WriteFile("/etc/resolv.conf", []byte(b.String()), 0o644) //nolint:gosec // G306: resolv.conf is world-readable by design
}

// loopbackUp brings up lo; many programs expect localhost to work.
func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("socket for lo: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return fmt.Errorf("lo: %w", err)
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("read lo flags: %w", err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("bring lo up: %w", err)
	}
	return nil
}

// superviseServer starts the protocol server and reaps every child that exits,
// restarting the server if it is the one that died. It never returns.
func superviseServer(logger *slog.Logger) {
	sigchld := make(chan os.Signal, 1)
	signal.Notify(sigchld, unix.SIGCHLD)

	serverPID := startServer(logger)
	for range sigchld {
		for {
			var status unix.WaitStatus
			pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
			if pid == serverPID {
				logger.Error("agent server exited; restarting", slog.Int("status", status.ExitStatus()))
				time.Sleep(100 * time.Millisecond)
				serverPID = startServer(logger)
			}
		}
	}
}

// startServer starts the protocol server as a child and returns its PID, or 0
// if it could not start (the next SIGCHLD pass retries nothing; the host will
// see the sandbox as unhealthy and destroy it).
func startServer(logger *slog.Logger) int {
	proc, err := os.StartProcess("/proc/self/exe", []string{"roundhouse-agent", ServeArg}, &os.ProcAttr{
		Env:   baseEnv,
		Files: []*os.File{nil, os.Stdout, os.Stderr},
		Sys:   &unix.SysProcAttr{Setsid: true},
	})
	if err != nil {
		logger.Error("start agent server", slog.String("error", err.Error()))
		return 0
	}
	return proc.Pid
}

package vmm

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"

	"golang.org/x/sys/unix"
)

// mountLogs mounts a tmpfs of spec.LogBytes at the jail's log directory,
// owned by the jail's uid. A full tmpfs fails further writes rather than
// growing, which caps the console and the VMM's log for the jail's whole
// life; the pages are charged to the memory cgroup of the VMM that writes
// them, so they count against the jail's own memory limit.
func mountLogs(spec JailSpec) error {
	dir := path.Join(spec.Root(), LogDir)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create log directory: %w", err)
	}
	opts := "size=" + strconv.FormatInt(spec.LogBytes, 10) + ",mode=0700,uid=" + strconv.Itoa(spec.UID) + ",gid=" + strconv.Itoa(spec.GID)
	if err := unix.Mount("tmpfs", dir, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, opts); err != nil {
		return fmt.Errorf("mount log tmpfs: %w", err)
	}
	return nil
}

// unmountLogs detaches the log tmpfs, if mounted, so the jail directory can
// be removed.
func unmountLogs(spec JailSpec) error {
	err := unix.Unmount(path.Join(spec.Root(), LogDir), unix.MNT_DETACH)
	if err == nil || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
		return nil // unmounted now, or never mounted
	}
	return fmt.Errorf("unmount log tmpfs: %w", err)
}

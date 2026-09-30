package agent

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// configureCommand starts the command inside the workload cgroup (unless
// cgroupFD is -1) and in its own process group, and makes cancellation (time
// limit or host hang-up) kill the whole group, so children the command
// started do not outlive it.
func configureCommand(cmd *exec.Cmd, cgroupFD int) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cgroupFD >= 0 {
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = cgroupFD
	}
	cmd.Cancel = func() error {
		// With Setpgid the group ID equals the leader's PID.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// setWallClock sets the guest's CLOCK_REALTIME.
func setWallClock(t time.Time) error {
	ts := unix.NsecToTimespec(t.UnixNano())
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
		return fmt.Errorf("clock_settime: %w", err)
	}
	return nil
}

// rndReseedCRNG is the RNDRESEEDCRNG ioctl, _IO('R', 0x07): it makes the
// kernel reseed its random number generator from the input pool immediately.
const rndReseedCRNG = 0x5207

// reseedKernel mixes host-supplied entropy into the guest kernel's input pool
// and forces an immediate reseed, so clones restored from one snapshot stop
// sharing random output right away (docs/engineering/design.md section 7.3).
func reseedKernel(entropy []byte) error {
	f, err := os.OpenFile("/dev/urandom", os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open /dev/urandom: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(entropy); err != nil {
		return fmt.Errorf("mix entropy: %w", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), rndReseedCRNG, 0); errno != 0 {
		return fmt.Errorf("force reseed: %w", errno)
	}
	return nil
}

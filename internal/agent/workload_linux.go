package agent

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Every command runs in the workload cgroup, whose memory and process limits
// sit below the guest's own. A command that exhausts either is then stopped
// by the guest kernel inside the workload, while the agent, outside it,
// keeps answering the host. The host's limits on the VM are the security
// boundary; these keep a sandbox usable after its workload misbehaves
// (docs/engineering/design.md section 11).
const (
	cgroupMount    = "/sys/fs/cgroup"
	workloadCgroup = cgroupMount + "/workload"
	// workloadPidsMax bounds processes and threads in the workload.
	workloadPidsMax = 1024
	// agentReserveMiB is guest memory kept out of the workload's reach for
	// the kernel, init, and the agent server.
	agentReserveMiB = 32
)

// setupWorkloadCgroup mounts cgroup v2 and creates the workload cgroup with
// its limits. The init process calls it once, before the server starts.
func setupWorkloadCgroup() error {
	if err := unix.Mount("cgroup2", cgroupMount, "cgroup2", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil &&
		!errors.Is(err, unix.EBUSY) {
		return fmt.Errorf("mount cgroup2: %w", err)
	}
	if err := os.WriteFile(filepath.Join(cgroupMount, "cgroup.subtree_control"), []byte("+memory +pids +cpu"), 0); err != nil {
		return fmt.Errorf("enable cgroup controllers: %w", err)
	}
	if err := os.Mkdir(workloadCgroup, 0o755); err != nil && !errors.Is(err, os.ErrExist) { //nolint:gosec // G301: a cgroup directory
		return fmt.Errorf("create workload cgroup: %w", err)
	}
	total, err := memTotalMiB("/proc/meminfo")
	if err != nil {
		return err
	}
	limit := max(total-agentReserveMiB, total/2)
	settings := map[string]string{
		"memory.max": strconv.Itoa(limit) + "M",
		"pids.max":   strconv.Itoa(workloadPidsMax),
	}
	for file, value := range settings {
		if err := os.WriteFile(filepath.Join(workloadCgroup, file), []byte(value), 0); err != nil {
			return fmt.Errorf("set workload %s: %w", file, err)
		}
	}
	return nil
}

// openWorkloadCgroup returns a descriptor for the workload cgroup, or -1 when
// there is none (the server runs outside a guest in tests). Commands are
// started directly inside it with clone3's CLONE_INTO_CGROUP, so no command
// ever runs outside the limits, not even briefly.
func openWorkloadCgroup() int {
	fd, err := unix.Open(workloadCgroup, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1
	}
	return fd
}

// memTotalMiB reads MemTotal from a meminfo file.
func memTotalMiB(meminfo string) (int, error) {
	f, err := os.Open(meminfo) //nolint:gosec // G304: a fixed /proc path, or a test file
	if err != nil {
		return 0, fmt.Errorf("read guest memory: %w", err)
	}
	defer func() { _ = f.Close() }()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) == 3 && fields[0] == "MemTotal:" && fields[2] == "kB" {
			kib, err := strconv.Atoi(fields[1])
			if err != nil {
				return 0, fmt.Errorf("read guest memory: %w", err)
			}
			return kib / 1024, nil
		}
	}
	return 0, fmt.Errorf("read guest memory: no MemTotal in %s", meminfo)
}

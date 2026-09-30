package vmm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"
)

// Jail is a running, jailed Firecracker process. Its lifetime is bounded by
// Destroy, which is idempotent.
type Jail struct {
	spec JailSpec
	pid  int
}

// Spec returns the spec the jail was launched with.
func (j *Jail) Spec() JailSpec { return j.spec }

// PID returns the VMM's process ID in the host's PID namespace.
func (j *Jail) PID() int { return j.pid }

// Launch prepares the jail root, links the spec's files into it, and starts
// Firecracker through the jailer. It returns once the jailer has daemonized
// the VMM; callers then wait for the API with
// NewClient(spec.SocketPath()).WaitReady. On failure, everything Launch
// created is removed.
func Launch(ctx context.Context, spec JailSpec) (_ *Jail, err error) {
	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("invalid jail spec: %w", err)
	}
	defer func() {
		if err != nil {
			// Clean up with a fresh deadline: ctx may be why we are failing.
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if cleanupErr := DestroyJail(cleanupCtx, spec); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("clean up after failed launch: %w", cleanupErr))
			}
		}
	}()

	if err := prepareRoot(spec); err != nil {
		return nil, err
	}

	var output bytes.Buffer
	cmd := exec.CommandContext(ctx, spec.JailerPath, spec.jailerArgs()...) //nolint:gosec // G204: fixed binary, validated arguments
	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.Env = []string{}
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("run jailer for %s: %w: %s", spec.ID, err, strings.TrimSpace(tail(output.String(), 2048)))
	}

	pid, err := readPID(ctx, path.Join(spec.Root(), path.Base(spec.FirecrackerPath)+".pid"))
	if err != nil {
		return nil, fmt.Errorf("find VMM process for %s: %w", spec.ID, err)
	}
	return &Jail{spec: spec, pid: pid}, nil
}

// prepareRoot creates the jail root and hard links the spec's files into it.
// The jailer accepts an existing root and chowns it to the jail's uid.
func prepareRoot(spec JailSpec) error {
	root := spec.Root()
	if err := os.MkdirAll(root, 0o750); err != nil {
		return fmt.Errorf("create jail root: %w", err)
	}
	if spec.LogBytes > 0 {
		if err := mountLogs(spec); err != nil {
			return err
		}
	}
	for _, f := range spec.Files {
		dst := path.Join(root, f.Name)
		if err := os.MkdirAll(path.Dir(dst), 0o750); err != nil {
			return fmt.Errorf("create directory for %s: %w", f.Name, err)
		}
		if err := os.Link(f.Source, dst); err != nil {
			return fmt.Errorf("link %s into jail (source and jail must share a file system): %w", f.Source, err)
		}
		if f.Writable {
			if err := os.Chown(dst, spec.UID, spec.GID); err != nil {
				return fmt.Errorf("chown %s: %w", f.Name, err)
			}
		}
	}
	return nil
}

// readPID waits briefly for the jailer's PID file, which it writes when it
// starts the VMM in a new PID namespace.
func readPID(ctx context.Context, file string) (int, error) {
	for {
		data, err := os.ReadFile(file) //nolint:gosec // G304: path derived from a validated spec
		// The jailer creates the file before writing it, so an empty file means
		// "not yet". It writes the PID in one small write, which a reader sees
		// whole or not at all.
		if err == nil && len(data) > 0 {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr != nil || pid <= 0 {
				return 0, fmt.Errorf("pid file %s: invalid contents %q", file, data)
			}
			return pid, nil
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return 0, fmt.Errorf("read pid file: %w", err)
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("pid file %s never appeared: %w", file, ctx.Err())
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// Exited reports whether every process in the jail's cgroup has exited. A
// missing cgroup counts as exited.
func (j *Jail) Exited() (bool, error) {
	populated, err := cgroupPopulated(j.spec.CgroupDir())
	return !populated, err
}

// Destroy kills the VMM and removes the jail. It is idempotent.
func (j *Jail) Destroy(ctx context.Context) error {
	return DestroyJail(ctx, j.spec)
}

// DestroyJail removes everything a jail with this spec may have left behind:
// its processes, its cgroup, and its directory. It works on jails that were
// never fully launched, and on orphans found at startup. Every step runs even
// if an earlier one failed.
func DestroyJail(ctx context.Context, spec JailSpec) error {
	var errs []error
	if err := killCgroup(ctx, spec.CgroupDir()); err != nil {
		errs = append(errs, err)
	}
	// Unmounting is attempted whatever the spec says: a jail found by
	// reconciliation has no record of its log size.
	if err := unmountLogs(spec); err != nil {
		errs = append(errs, err)
	}
	if err := os.RemoveAll(spec.Dir()); err != nil {
		errs = append(errs, fmt.Errorf("remove jail directory: %w", err))
	}
	return errors.Join(errs...)
}

// killCgroup kills every process in a cgroup v2 group and removes the group.
//
// Signals are the wrong tool here: with --new-pid-ns the VMM is the init
// process of its PID namespace, and the kernel discards signals to a
// namespace's init that it has no handler for, so SIGTERM from the host is
// silently ignored. cgroup.kill sends SIGKILL to every member at once.
func killCgroup(ctx context.Context, dir string) error {
	err := os.WriteFile(path.Join(dir, "cgroup.kill"), []byte("1"), 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // already gone
	}
	if err != nil {
		return fmt.Errorf("kill cgroup %s: %w", dir, err)
	}
	if err := waitCgroupEmpty(ctx, dir); err != nil {
		return err
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove cgroup %s: %w", dir, err)
	}
	return nil
}

// waitCgroupEmpty polls until no process remains in the cgroup.
func waitCgroupEmpty(ctx context.Context, dir string) error {
	for {
		populated, err := cgroupPopulated(dir)
		if err != nil || !populated {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("cgroup %s still has processes: %w", dir, ctx.Err())
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// cgroupPopulated reads the "populated" key of a cgroup's cgroup.events,
// which is 1 while any process lives in the group or its descendants.
func cgroupPopulated(dir string) (bool, error) {
	data, err := os.ReadFile(path.Join(dir, "cgroup.events")) //nolint:gosec // G304: path derived from a validated spec
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read cgroup events for %s: %w", dir, err)
	}
	for line := range strings.Lines(string(data)) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "populated "); ok {
			return value == "1", nil
		}
	}
	return false, fmt.Errorf("cgroup events for %s: no populated key", dir)
}

// tail returns at most the last n bytes of s.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

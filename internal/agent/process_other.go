//go:build !linux

package agent

import (
	"errors"
	"os/exec"
	"time"
)

// configureCommand is a no-op off Linux: the agent only runs inside Linux
// guests, and other platforms build it only for handler tests.
func configureCommand(*exec.Cmd, int) {}

func openWorkloadCgroup() int { return -1 }

func reseedKernel([]byte) error {
	return errors.ErrUnsupported
}

func setWallClock(time.Time) error {
	return errors.ErrUnsupported
}

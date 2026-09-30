//go:build !linux

package vmm

import "errors"

// Jails exist only on Linux; other platforms build this package for its
// unit tests.

func mountLogs(JailSpec) error { return errors.ErrUnsupported }

func unmountLogs(JailSpec) error { return nil }

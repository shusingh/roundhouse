//go:build !linux

package storage

import (
	"fmt"
	"io"
	"os"
)

// SparseCopy copies src to a new file dst. Off Linux it copies every byte;
// the control plane only runs on Linux, and other platforms build this
// package for tests.
func SparseCopy(dst, src string, perm os.FileMode) (err error) {
	in, err := os.Open(src) //nolint:gosec // G304: paths come from the image layout, not from requests
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //nolint:gosec // G304: see above
	if err != nil {
		return fmt.Errorf("create destination: %w", err)
	}
	defer func() {
		if closeErr := out.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close destination: %w", closeErr)
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return nil
}

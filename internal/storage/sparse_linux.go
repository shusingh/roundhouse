package storage

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// SparseCopy copies src to a new file dst, preserving holes: only the data
// regions of src (found with SEEK_DATA and SEEK_HOLE) are read and written, so
// copying a mostly-empty disk image costs time proportional to its data, not
// its size. dst must not exist.
func SparseCopy(dst, src string, perm os.FileMode) (err error) {
	in, err := os.Open(src) //nolint:gosec // G304: paths come from the image layout, not from requests
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}
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
	size := info.Size()
	// Set the full size first: trailing holes then need no writes.
	if err := out.Truncate(size); err != nil {
		return fmt.Errorf("size destination: %w", err)
	}
	inFD := int(in.Fd())
	for offset := int64(0); offset < size; {
		dataStart, err := unix.Seek(inFD, offset, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			return nil // only a hole remains
		}
		if err != nil {
			return fmt.Errorf("seek data: %w", err)
		}
		holeStart, err := unix.Seek(inFD, dataStart, unix.SEEK_HOLE)
		if err != nil {
			return fmt.Errorf("seek hole: %w", err)
		}
		if _, err := io.Copy(io.NewOffsetWriter(out, dataStart), io.NewSectionReader(in, dataStart, holeStart-dataStart)); err != nil {
			return fmt.Errorf("copy data at %d: %w", dataStart, err)
		}
		offset = holeStart
	}
	return nil
}

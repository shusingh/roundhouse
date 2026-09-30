package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/shusingh/roundhouse/internal/vmm"
)

// keepDiagnostics bounds how many failed sandboxes' logs are kept.
const keepDiagnostics = 20

// SaveDiagnostics copies the jail's console and VMM log into the data
// directory's diagnostics area, with a note of the reason, and prunes it to
// the newest keepDiagnostics entries. The copies are files for an operator
// to read, never log lines: the console carries whatever the guest wrote.
// Their size is bounded by the jail's log tmpfs.
func (v *firecrackerVM) SaveDiagnostics(reason string) (string, error) {
	if v.jailSpec == nil {
		return "", errors.New("no jail to diagnose")
	}
	base := v.layout.DiagnosticsDir()
	dir := filepath.Join(base, time.Now().UTC().Format("20060102T150405Z")+"-"+v.id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create diagnostics directory: %w", err)
	}
	var errs []error
	logs := filepath.Join(v.jailSpec.Root(), vmm.LogDir)
	for _, name := range []string{filepath.Base(jailConsole), filepath.Base(jailVMMLog)} {
		if err := copyFile(filepath.Join(dir, name), filepath.Join(logs, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	note := fmt.Sprintf("sandbox: %s\nreason: %s\nstart path: %s\n", v.id, reason, v.path)
	if err := os.WriteFile(filepath.Join(dir, "reason.txt"), []byte(note), 0o600); err != nil {
		errs = append(errs, err)
	}
	if err := pruneDiagnostics(base, keepDiagnostics); err != nil {
		errs = append(errs, err)
	}
	return dir, errors.Join(errs...)
}

func copyFile(dst, src string) error {
	in, err := os.Open(src) //nolint:gosec // G304: a log file in a jail this process created
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: under the data directory
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s: %w", filepath.Base(src), err)
	}
	return out.Close()
}

// pruneDiagnostics removes all but the newest keep entries. Entry names
// start with a UTC timestamp, so name order is age order.
func pruneDiagnostics(base string, keep int) error {
	entries, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	var errs []error
	for _, name := range names[:max(0, len(names)-keep)] {
		if err := os.RemoveAll(filepath.Join(base, name)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

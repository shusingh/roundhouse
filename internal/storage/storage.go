// Package storage lays out Roundhouse's data directory and prepares
// per-sandbox disks.
//
// Everything lives under one data directory on one file system, so jails can
// hard link images instead of copying them (docs/adr/0006-storage.md):
//
//	<root>/images/<name>/{vmlinux,rootfs.ext4,scratch.ext4}
//	<root>/images/<name>/templates/<vcpus>cpu-<mib>mib/{manifest.json,vm.state,vm.mem,scratch.ext4}
//	<root>/sandboxes/<id>/scratch.ext4
//	<root>/jail/firecracker/rh-<id>/root/...
package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// validName matches image names and sandbox IDs.
var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Layout resolves paths under a data directory.
type Layout struct {
	Root string
}

// JailBase is the jailer's chroot base directory.
func (l Layout) JailBase() string { return filepath.Join(l.Root, "jail") }

// SandboxesDir holds one directory per live sandbox.
func (l Layout) SandboxesDir() string { return filepath.Join(l.Root, "sandboxes") }

// SandboxDir is one sandbox's directory.
func (l Layout) SandboxDir(id string) string { return filepath.Join(l.SandboxesDir(), id) }

// ImageDir is one image's directory.
func (l Layout) ImageDir(name string) string { return filepath.Join(l.Root, "images", name) }

// Image is a guest image: a kernel, a read-only root file system, and a
// pristine scratch disk that each sandbox copies.
type Image struct {
	Name    string
	Kernel  string
	Rootfs  string
	Scratch string
}

// OpenImage checks that an image's files exist and returns their paths.
func (l Layout) OpenImage(name string) (Image, error) {
	if !validName.MatchString(name) {
		return Image{}, fmt.Errorf("image name %q: want lowercase letters, digits, and hyphens", name)
	}
	dir := l.ImageDir(name)
	img := Image{
		Name:    name,
		Kernel:  filepath.Join(dir, "vmlinux"),
		Rootfs:  filepath.Join(dir, "rootfs.ext4"),
		Scratch: filepath.Join(dir, "scratch.ext4"),
	}
	for _, p := range []string{img.Kernel, img.Rootfs, img.Scratch} {
		info, err := os.Stat(p)
		if err != nil {
			return Image{}, fmt.Errorf("image %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return Image{}, fmt.Errorf("image %s: %s is not a regular file", name, p)
		}
	}
	return img, nil
}

// Template is a snapshot of a booted guest, taken from one image at one VM
// shape. The scratch disk is the booted guest's disk at snapshot time: the
// snapshot's page cache describes that disk, not the image's pristine one.
type Template struct {
	Dir      string
	Manifest string
	State    string
	Memory   string
	Scratch  string
}

// TemplateDir is where the template of image at a VM shape lives.
func (l Layout) TemplateDir(image string, vcpus, memoryMiB int) string {
	return filepath.Join(l.ImageDir(image), "templates", fmt.Sprintf("%dcpu-%dmib", vcpus, memoryMiB))
}

// TemplateAt returns the paths of a template's files in dir.
func TemplateAt(dir string) Template {
	return Template{
		Dir:      dir,
		Manifest: filepath.Join(dir, "manifest.json"),
		State:    filepath.Join(dir, "vm.state"),
		Memory:   filepath.Join(dir, "vm.mem"),
		Scratch:  filepath.Join(dir, "scratch.ext4"),
	}
}

// TemplateDirs lists every template directory holding a manifest, across
// images.
func (l Layout) TemplateDirs() ([]string, error) {
	manifests, err := filepath.Glob(filepath.Join(l.Root, "images", "*", "templates", "*", "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	dirs := make([]string, len(manifests))
	for i, m := range manifests {
		dirs[i] = filepath.Dir(m)
	}
	return dirs, nil
}

// CreateSandboxDir creates a sandbox's directory and a private copy of the
// scratch disk at source (an image's or a template's) in it, owned by uid
// and gid and writable only by them. It returns the copy's path.
func (l Layout) CreateSandboxDir(id, source string, uid, gid int) (string, error) {
	if !validName.MatchString(id) {
		return "", fmt.Errorf("sandbox id %q: want lowercase letters, digits, and hyphens", id)
	}
	dir := l.SandboxDir(id)
	if err := os.MkdirAll(l.SandboxesDir(), 0o750); err != nil {
		return "", fmt.Errorf("create sandboxes directory: %w", err)
	}
	if err := os.Mkdir(dir, 0o750); err != nil {
		return "", fmt.Errorf("create sandbox directory: %w", err)
	}
	scratch := filepath.Join(dir, "scratch.ext4")
	if err := SparseCopy(scratch, source, 0o600); err != nil {
		return "", errors.Join(fmt.Errorf("copy scratch disk: %w", err), os.RemoveAll(dir))
	}
	if err := os.Chown(scratch, uid, gid); err != nil {
		return "", errors.Join(fmt.Errorf("chown scratch disk: %w", err), os.RemoveAll(dir))
	}
	return scratch, nil
}

// RemoveSandboxDir deletes a sandbox's directory. A missing directory is not
// an error.
func (l Layout) RemoveSandboxDir(id string) error {
	if !validName.MatchString(id) {
		return fmt.Errorf("sandbox id %q: invalid", id)
	}
	if err := os.RemoveAll(l.SandboxDir(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove sandbox directory: %w", err)
	}
	return nil
}

// SandboxIDs lists the sandbox directories present, for reconciliation.
func (l Layout) SandboxIDs() ([]string, error) {
	entries, err := os.ReadDir(l.SandboxesDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && validName.MatchString(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

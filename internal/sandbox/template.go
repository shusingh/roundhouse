package sandbox

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/shusingh/roundhouse/internal/netns"
	"github.com/shusingh/roundhouse/internal/storage"
	"github.com/shusingh/roundhouse/internal/vmm"
)

// Manifest records what a template was built from. A snapshot restores
// correctly only into the same Firecracker, on the same CPU model, with the
// same kernel, root file system, and device configuration, so a template
// whose manifest differs from the host's in any field is not used
// (docs/engineering/design.md section 7.2).
type Manifest struct {
	Image                 string `json:"image"`
	VCPUs                 int    `json:"vcpus"`
	MemoryMiB             int    `json:"memory_mib"`
	FirecrackerVersion    string `json:"firecracker_version"`
	CPUModel              string `json:"cpu_model"`
	KernelSHA256          string `json:"kernel_sha256"`
	RootfsSHA256          string `json:"rootfs_sha256"`
	KernelArgs            string `json:"kernel_args"`
	NetworkBytesPerSecond int64  `json:"network_bytes_per_second"`
	DiskBytesPerSecond    int64  `json:"disk_bytes_per_second"`
	DiskOpsPerSecond      int64  `json:"disk_ops_per_second"`
	// CreatedAt is informational and not compared.
	CreatedAt time.Time `json:"created_at"`
}

// mismatch describes the first field in which m differs from want, or
// returns "" when m can be restored on the host want describes.
func (m Manifest) mismatch(want Manifest) string {
	m.CreatedAt, want.CreatedAt = time.Time{}, time.Time{}
	if m == want {
		return ""
	}
	fields := []struct {
		name      string
		got, need any
	}{
		{"image", m.Image, want.Image},
		{"vcpus", m.VCPUs, want.VCPUs},
		{"memory_mib", m.MemoryMiB, want.MemoryMiB},
		{"firecracker_version", m.FirecrackerVersion, want.FirecrackerVersion},
		{"cpu_model", m.CPUModel, want.CPUModel},
		{"kernel_sha256", m.KernelSHA256, want.KernelSHA256},
		{"rootfs_sha256", m.RootfsSHA256, want.RootfsSHA256},
		{"kernel_args", m.KernelArgs, want.KernelArgs},
		{"network_bytes_per_second", m.NetworkBytesPerSecond, want.NetworkBytesPerSecond},
		{"disk_bytes_per_second", m.DiskBytesPerSecond, want.DiskBytesPerSecond},
		{"disk_ops_per_second", m.DiskOpsPerSecond, want.DiskOpsPerSecond},
	}
	for _, f := range fields {
		if f.got != f.need {
			return fmt.Sprintf("%s is %v, host needs %v", f.name, f.got, f.need)
		}
	}
	return "manifest differs"
}

// hostFacts computes the manifest a template must carry to restore on this
// host. It caches file digests, which are the expensive part, across images.
type hostFacts struct {
	booter   *FirecrackerBooter
	version  string
	cpuModel string
	digests  map[string]string
}

func (b *FirecrackerBooter) newHostFacts(ctx context.Context) (*hostFacts, error) {
	out, err := exec.CommandContext(ctx, b.cfg.FirecrackerPath, "--version").Output() //nolint:gosec // G204: configured binary, fixed argument
	if err != nil {
		return nil, fmt.Errorf("read Firecracker version: %w", err)
	}
	version, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	cpu, err := cpuModel("/proc/cpuinfo")
	if err != nil {
		return nil, err
	}
	return &hostFacts{booter: b, version: version, cpuModel: cpu, digests: map[string]string{}}, nil
}

func (h *hostFacts) manifest(img storage.Image, limits Limits) (Manifest, error) {
	kernel, err := h.digest(img.Kernel)
	if err != nil {
		return Manifest{}, err
	}
	rootfs, err := h.digest(img.Rootfs)
	if err != nil {
		return Manifest{}, err
	}
	return Manifest{
		Image: img.Name, VCPUs: limits.VCPUs, MemoryMiB: limits.MemoryMiB,
		FirecrackerVersion: h.version, CPUModel: h.cpuModel,
		KernelSHA256: kernel, RootfsSHA256: rootfs,
		KernelArgs:            h.booter.kernelArgs(),
		NetworkBytesPerSecond: h.booter.cfg.NetworkBytesPerSecond,
		DiskBytesPerSecond:    h.booter.cfg.DiskBytesPerSecond,
		DiskOpsPerSecond:      h.booter.cfg.DiskOpsPerSecond,
	}, nil
}

func (h *hostFacts) digest(file string) (string, error) {
	if d, ok := h.digests[file]; ok {
		return d, nil
	}
	f, err := os.Open(file) //nolint:gosec // G304: an image file under the data directory
	if err != nil {
		return "", fmt.Errorf("digest image file: %w", err)
	}
	defer func() { _ = f.Close() }()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", fmt.Errorf("digest %s: %w", file, err)
	}
	d := hex.EncodeToString(sum.Sum(nil))
	h.digests[file] = d
	return d, nil
}

// cpuModel returns the first "model name" in a cpuinfo file.
func cpuModel(cpuinfo string) (string, error) {
	f, err := os.Open(cpuinfo) //nolint:gosec // G304: a fixed /proc path, or a test file
	if err != nil {
		return "", fmt.Errorf("read CPU model: %w", err)
	}
	defer func() { _ = f.Close() }()
	s := bufio.NewScanner(f)
	for s.Scan() {
		key, value, ok := strings.Cut(s.Text(), ":")
		if ok && strings.TrimSpace(key) == "model name" {
			return strings.TrimSpace(value), nil
		}
	}
	if err := s.Err(); err != nil {
		return "", fmt.Errorf("read CPU model: %w", err)
	}
	return "", errors.New("read CPU model: no model name in " + cpuinfo)
}

// template returns the loaded template for a VM of image and limits.
func (b *FirecrackerBooter) template(image string, limits Limits) (storage.Template, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	tpl, ok := b.templates[shape{image, limits.VCPUs, limits.MemoryMiB}]
	return tpl, ok
}

// loadTemplates keeps every template whose manifest matches this host.
// Others are logged and skipped, so a VM of their shape cold-boots instead
// of restoring into a host it was not built for.
func (b *FirecrackerBooter) loadTemplates(ctx context.Context) error {
	dirs, err := b.cfg.Layout.TemplateDirs()
	if err != nil {
		return err
	}
	loaded := map[shape]storage.Template{}
	if len(dirs) > 0 {
		facts, err := b.newHostFacts(ctx)
		if err != nil {
			return err
		}
		for _, dir := range dirs {
			tpl := storage.TemplateAt(dir)
			m, err := b.checkTemplate(facts, tpl)
			if err != nil {
				b.logger.Warn("template rejected", slog.String("template", dir), slog.String("reason", err.Error()))
				continue
			}
			loaded[shape{m.Image, m.VCPUs, m.MemoryMiB}] = tpl
			b.logger.Info("template loaded", slog.String("image", m.Image),
				slog.Int("vcpus", m.VCPUs), slog.Int("memory_mib", m.MemoryMiB))
		}
	}
	b.mu.Lock()
	b.templates = loaded
	b.mu.Unlock()
	return nil
}

// checkTemplate reads a template's manifest and compares it with the host.
func (b *FirecrackerBooter) checkTemplate(facts *hostFacts, tpl storage.Template) (Manifest, error) {
	data, err := os.ReadFile(tpl.Manifest)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if b.cfg.Layout.TemplateDir(m.Image, m.VCPUs, m.MemoryMiB) != tpl.Dir {
		return Manifest{}, errors.New("manifest does not match its directory")
	}
	for _, p := range []string{tpl.State, tpl.Memory, tpl.Scratch} {
		if _, err := os.Stat(p); err != nil {
			return Manifest{}, err
		}
	}
	img, err := b.cfg.Layout.OpenImage(m.Image)
	if err != nil {
		return Manifest{}, err
	}
	want, err := facts.manifest(img, Limits{VCPUs: m.VCPUs, MemoryMiB: m.MemoryMiB})
	if err != nil {
		return Manifest{}, err
	}
	if reason := m.mismatch(want); reason != "" {
		return Manifest{}, errors.New(reason)
	}
	return m, nil
}

// BuildTemplate cold-boots a guest from image at the shape in limits,
// snapshots it as soon as its agent listens, and installs the snapshot as
// that image and shape's template, replacing any earlier one. No user
// process has run and the guest has no identity yet, so nothing in the
// snapshot is specific to one sandbox (design.md section 7.3). uid must not
// belong to any live sandbox. The control plane loads new templates when it
// next starts.
func (b *FirecrackerBooter) BuildTemplate(ctx context.Context, image string, limits Limits, uid int) (Manifest, error) {
	if err := limits.validateShape(); err != nil {
		return Manifest{}, err
	}
	img, err := b.cfg.Layout.OpenImage(image)
	if err != nil {
		return Manifest{}, err
	}
	facts, err := b.newHostFacts(ctx)
	if err != nil {
		return Manifest{}, err
	}
	manifest, err := facts.manifest(img, limits)
	if err != nil {
		return Manifest{}, err
	}

	var raw [6]byte
	_, _ = rand.Read(raw[:])
	spec := BootSpec{ID: "tpl-" + hex.EncodeToString(raw[:]), Image: image, Limits: limits, Egress: netns.Egress{Mode: netns.ModeNone}, UID: uid}
	bootCtx, cancel := context.WithTimeout(ctx, b.cfg.BootTimeout)
	defer cancel()
	vm, err := b.launch(bootCtx, spec, StartCold, img.Scratch, coldFiles(img), b.coldStart(limits))
	if err != nil {
		return Manifest{}, err
	}
	defer func() { _ = vm.destroyDetached(ctx) }()

	// A host connection open across the snapshot would be dead in every clone.
	vm.agent.Close()
	api := vmm.NewClient(vm.jailSpec.SocketPath())
	defer api.Close()
	if err := api.Pause(ctx); err != nil {
		return Manifest{}, err
	}
	if err := api.CreateSnapshot(ctx, vmm.SnapshotCreate{SnapshotType: vmm.SnapshotFull, SnapshotPath: jailState, MemFilePath: jailMemory}); err != nil {
		return Manifest{}, fmt.Errorf("snapshot template: %w", err)
	}

	final := b.cfg.Layout.TemplateDir(image, limits.VCPUs, limits.MemoryMiB)
	staging := final + ".building-" + spec.ID
	if err := os.MkdirAll(staging, 0o750); err != nil {
		return Manifest{}, fmt.Errorf("create template directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	stagedTpl := storage.TemplateAt(staging)
	root := vm.jailSpec.Root()
	for src, dst := range map[string]string{filepath.Join(root, jailState): stagedTpl.State, filepath.Join(root, jailMemory): stagedTpl.Memory} {
		if err := takeFile(src, dst); err != nil {
			return Manifest{}, err
		}
	}
	// The paused guest's disk, which its snapshotted page cache describes.
	if err := storage.SparseCopy(stagedTpl.Scratch, vm.scratch, 0o644); err != nil {
		return Manifest{}, fmt.Errorf("copy template scratch disk: %w", err)
	}
	manifest.CreatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	if err := os.WriteFile(stagedTpl.Manifest, append(data, '\n'), 0o644); err != nil { //nolint:gosec // G306: read by jailed VMMs through hard links; the data directory is not world-traversable
		return Manifest{}, fmt.Errorf("write manifest: %w", err)
	}
	return manifest, replaceDir(staging, final)
}

// takeFile moves a snapshot file out of the jail and makes it a root-owned,
// read-only file that every sandbox's VMM can open through a hard link.
func takeFile(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("move snapshot file: %w", err)
	}
	if err := os.Chown(dst, 0, 0); err != nil {
		return fmt.Errorf("chown snapshot file: %w", err)
	}
	if err := os.Chmod(dst, 0o644); err != nil { //nolint:gosec // G302: see WriteFile of the manifest
		return fmt.Errorf("chmod snapshot file: %w", err)
	}
	return nil
}

// replaceDir swaps staging in at final. A reader sees the old directory, then
// briefly none, then the new one; never a mix. Jails still restoring from the
// old template keep their hard links to its files.
func replaceDir(staging, final string) error {
	old := staging + ".old"
	if err := os.Rename(final, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("retire old template: %w", err)
	}
	if err := os.Rename(staging, final); err != nil {
		return fmt.Errorf("install template: %w", err)
	}
	if err := os.RemoveAll(old); err != nil {
		return fmt.Errorf("remove old template: %w", err)
	}
	return nil
}

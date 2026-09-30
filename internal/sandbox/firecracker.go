package sandbox

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"
	"time"

	"github.com/shusingh/roundhouse/internal/agentclient"
	"github.com/shusingh/roundhouse/internal/agentproto"
	"github.com/shusingh/roundhouse/internal/storage"
	"github.com/shusingh/roundhouse/internal/vmm"
)

// Names inside every jail. Snapshots record jail-relative paths, so these are
// the same for every sandbox.
const (
	jailKernel  = "vmlinux"
	jailRootfs  = "rootfs.ext4"
	jailScratch = "scratch.ext4"
	jailVsock   = "/v.sock"
	jailConsole = "console.log"
	guestCID    = 3
	// jailPrefix marks every host resource Roundhouse creates for a sandbox,
	// so reconciliation finds orphans and touches nothing else.
	jailPrefix = "rh-"
)

// FirecrackerConfig configures FirecrackerBooter.
type FirecrackerConfig struct {
	Layout          storage.Layout
	JailerPath      string
	FirecrackerPath string
	CgroupRoot      string
	CgroupParent    string
	// BootArgs is the guest kernel command line.
	BootArgs string
	// VMMOverheadMiB is added to guest memory for the jail's memory.max: the
	// VMM process itself needs memory beyond what it maps for the guest.
	VMMOverheadMiB int
	// PidsMax caps processes and threads in the jail's cgroup.
	PidsMax int
	// BootTimeout bounds the whole boot, from jail launch to agent ready.
	BootTimeout time.Duration
	// ConsoleBytesPerSecond limits how fast a guest can write its console.
	ConsoleBytesPerSecond int64
}

// DefaultBootArgs start the guest agent as init. The kernel adds the root
// device (read-only) from the drive configuration.
const DefaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off quiet init=/sbin/roundhouse-agent"

// FirecrackerBooter boots sandboxes as jailed Firecracker microVMs.
type FirecrackerBooter struct {
	cfg    FirecrackerConfig
	logger *slog.Logger
}

// NewFirecrackerBooter returns a booter with cfg.
func NewFirecrackerBooter(cfg FirecrackerConfig, logger *slog.Logger) *FirecrackerBooter {
	return &FirecrackerBooter{cfg: cfg, logger: logger}
}

func (b *FirecrackerBooter) jailSpec(id string, uid int, limits Limits, files []vmm.JailFile) vmm.JailSpec {
	mem := int64(limits.MemoryMiB+b.cfg.VMMOverheadMiB) << 20
	return vmm.JailSpec{
		ID:              jailPrefix + id,
		JailerPath:      b.cfg.JailerPath,
		FirecrackerPath: b.cfg.FirecrackerPath,
		ChrootBase:      b.cfg.Layout.JailBase(),
		CgroupRoot:      b.cfg.CgroupRoot,
		CgroupParent:    b.cfg.CgroupParent,
		UID:             uid,
		GID:             uid,
		MemoryMax:       mem,
		CPUs:            float64(limits.VCPUs),
		PidsMax:         b.cfg.PidsMax,
		// Room for guest memory (a snapshot's memory file) plus the console.
		FileSizeLimit: mem + 64<<20,
		NoFileLimit:   1024,
		Files:         files,
	}
}

// Boot cold-boots a sandbox: it copies the image's scratch disk, launches the
// jailed VMM, configures and starts the VM, and waits for the guest agent.
// On failure every resource created is released.
func (b *FirecrackerBooter) Boot(ctx context.Context, spec BootSpec) (_ VM, err error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.BootTimeout)
	defer cancel()

	img, err := b.cfg.Layout.OpenImage(spec.Image)
	if err != nil {
		return nil, err
	}
	scratch, err := b.cfg.Layout.CreateSandboxDir(spec.ID, img, spec.UID, spec.UID)
	if err != nil {
		return nil, err
	}
	vm := &firecrackerVM{layout: b.cfg.Layout, id: spec.ID}
	defer func() {
		if err != nil {
			teardownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			err = errors.Join(err, vm.Destroy(teardownCtx))
		}
	}()

	jailSpec := b.jailSpec(spec.ID, spec.UID, spec.Limits, []vmm.JailFile{
		{Source: img.Kernel, Name: jailKernel},
		{Source: img.Rootfs, Name: jailRootfs},
		{Source: scratch, Name: jailScratch, Writable: true},
	})
	vm.jailSpec = &jailSpec
	jail, err := vmm.Launch(ctx, jailSpec)
	if err != nil {
		return nil, err
	}
	vm.jail = jail
	api := vmm.NewClient(jailSpec.SocketPath())
	defer api.Close()
	if err := api.WaitReady(ctx); err != nil {
		return nil, err
	}
	if err := b.configure(ctx, api, spec.Limits); err != nil {
		return nil, err
	}
	if err := api.Start(ctx); err != nil {
		return nil, err
	}
	vm.agent = agentclient.New(path.Join(jailSpec.Root(), strings.TrimPrefix(jailVsock, "/")))
	if err := vm.agent.WaitReady(ctx); err != nil {
		return nil, fmt.Errorf("guest agent did not come up: %w", err)
	}
	if err := assignIdentity(ctx, vm.agent, spec.ID); err != nil {
		return nil, err
	}
	return vm, nil
}

func (b *FirecrackerBooter) configure(ctx context.Context, api *vmm.Client, limits Limits) error {
	console := b.cfg.ConsoleBytesPerSecond
	steps := []func() error{
		func() error {
			return api.PutMachineConfig(ctx, vmm.MachineConfig{VCPUCount: limits.VCPUs, MemSizeMiB: limits.MemoryMiB})
		},
		func() error {
			return api.PutBootSource(ctx, vmm.BootSource{KernelImagePath: jailKernel, BootArgs: b.cfg.BootArgs})
		},
		func() error {
			return api.PutDrive(ctx, vmm.Drive{DriveID: "rootfs", PathOnHost: jailRootfs, IsRootDevice: true, IsReadOnly: true})
		},
		func() error { return api.PutDrive(ctx, vmm.Drive{DriveID: "scratch", PathOnHost: jailScratch}) },
		func() error { return api.PutVsock(ctx, vmm.Vsock{GuestCID: guestCID, UDSPath: jailVsock}) },
		func() error {
			return api.PutSerial(ctx, vmm.Serial{
				OutputPath:  jailConsole,
				RateLimiter: &vmm.TokenBucket{Size: console, RefillTimeMS: 1000},
			})
		},
		func() error {
			// Plenty for reseeding; bounded so a guest cannot drain host entropy.
			return api.PutEntropy(ctx, vmm.Entropy{RateLimiter: &vmm.RateLimiter{
				Bandwidth: &vmm.TokenBucket{Size: 4096, RefillTimeMS: 1000},
			}})
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return fmt.Errorf("configure VM: %w", err)
		}
	}
	return nil
}

// assignIdentity gives the guest its sandbox ID and fresh host entropy. A
// cold-booted guest gets the same call as a restored one, so every sandbox
// starts the same way (docs/engineering/design.md section 7.3).
func assignIdentity(ctx context.Context, agent *agentclient.Client, id string) error {
	entropy := make([]byte, 64)
	if _, err := rand.Read(entropy); err != nil {
		return fmt.Errorf("generate entropy: %w", err)
	}
	if err := agent.Restore(ctx, agentproto.RestoreRequest{SandboxID: id, Entropy: entropy}); err != nil {
		return fmt.Errorf("assign sandbox identity: %w", err)
	}
	return nil
}

// Reconcile removes jails, cgroups, and sandbox directories left by a
// previous run of the control plane. Only names with the Roundhouse prefix
// are considered.
func (b *FirecrackerBooter) Reconcile(ctx context.Context) error {
	var errs []error
	seen := map[string]bool{}
	for _, dir := range []string{
		path.Join(b.cfg.Layout.JailBase(), path.Base(b.cfg.FirecrackerPath)),
		path.Join(b.cfg.CgroupRoot, b.cfg.CgroupParent),
	} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("list %s: %w", dir, err))
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !e.IsDir() || !strings.HasPrefix(name, jailPrefix) || seen[name] {
				continue
			}
			seen[name] = true
			spec := b.jailSpec(strings.TrimPrefix(name, jailPrefix), 1, Limits{VCPUs: 1, MemoryMiB: 128}, nil)
			if err := vmm.DestroyJail(ctx, spec); err != nil {
				errs = append(errs, err)
				continue
			}
			b.logger.Warn("removed orphaned jail", slog.String("jail", name))
		}
	}
	ids, err := b.cfg.Layout.SandboxIDs()
	if err != nil {
		errs = append(errs, err)
	}
	for _, id := range ids {
		if err := b.cfg.Layout.RemoveSandboxDir(id); err != nil {
			errs = append(errs, err)
			continue
		}
		b.logger.Warn("removed orphaned sandbox directory", slog.String("sandbox_id", id))
	}
	return errors.Join(errs...)
}

// firecrackerVM is a booted sandbox. Its fields are set during Boot and not
// changed afterwards.
type firecrackerVM struct {
	layout   storage.Layout
	id       string
	jailSpec *vmm.JailSpec
	jail     *vmm.Jail
	agent    *agentclient.Client
}

func (v *firecrackerVM) Exec(ctx context.Context, req agentproto.ExecRequest, onOutput func(*agentproto.Frame) error) (*agentproto.Frame, error) {
	return v.agent.Exec(ctx, req, onOutput)
}

func (v *firecrackerVM) WriteFile(ctx context.Context, p string, content io.Reader) error {
	return v.agent.WriteFile(ctx, p, content)
}

func (v *firecrackerVM) ReadFile(ctx context.Context, p string, w io.Writer) (int64, error) {
	return v.agent.ReadFile(ctx, p, w)
}

func (v *firecrackerVM) Exited() (bool, error) {
	return v.jail.Exited()
}

// Destroy kills the VM and removes its jail and sandbox directory. Every step
// runs even if an earlier one fails, and each is idempotent.
func (v *firecrackerVM) Destroy(ctx context.Context) error {
	var errs []error
	if v.agent != nil {
		v.agent.Close()
	}
	if v.jailSpec != nil {
		if err := vmm.DestroyJail(ctx, *v.jailSpec); err != nil {
			errs = append(errs, err)
		}
	}
	if err := v.layout.RemoveSandboxDir(v.id); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

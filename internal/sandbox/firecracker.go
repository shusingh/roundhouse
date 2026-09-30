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
	"sync"
	"time"

	"github.com/shusingh/roundhouse/internal/agentclient"
	"github.com/shusingh/roundhouse/internal/agentproto"
	"github.com/shusingh/roundhouse/internal/netns"
	"github.com/shusingh/roundhouse/internal/storage"
	"github.com/shusingh/roundhouse/internal/vmm"
)

// Names inside every jail. Snapshots record jail-relative paths, so these are
// the same for every sandbox.
const (
	jailKernel  = "vmlinux"
	jailRootfs  = "rootfs.ext4"
	jailScratch = "scratch.ext4"
	jailState   = "vm.state"
	jailMemory  = "vm.mem"
	jailVsock   = "/v.sock"
	jailConsole = vmm.LogDir + "/console.log"
	jailVMMLog  = vmm.LogDir + "/firecracker.log"
	// jailLogBytes caps the console and the VMM's log together for a
	// sandbox's whole life: at the console's rate limit, over eight minutes
	// of continuous output.
	jailLogBytes = 8 << 20
	guestCID     = 3
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
	// BootTimeout bounds the whole start, from jail launch to agent ready.
	BootTimeout time.Duration
	// ConsoleBytesPerSecond limits how fast a guest can write its console.
	ConsoleBytesPerSecond int64
	// Network creates each sandbox's namespace and egress rules.
	Network *netns.Manager
	// NetworkBytesPerSecond limits each direction of a sandbox's network
	// device (Firecracker's rx and tx rate limiters).
	NetworkBytesPerSecond int64
	// DiskBytesPerSecond and DiskOpsPerSecond limit each of a sandbox's
	// drives (Firecracker's block rate limiter).
	DiskBytesPerSecond int64
	DiskOpsPerSecond   int64
}

// DefaultBootArgs start the guest agent as init. The kernel adds the root
// device (read-only) from the drive configuration.
const DefaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off quiet init=/sbin/roundhouse-agent"

// FirecrackerBooter starts sandboxes as jailed Firecracker microVMs, by
// restoring a template snapshot when one matches and by cold boot otherwise.
type FirecrackerBooter struct {
	cfg    FirecrackerConfig
	logger *slog.Logger

	mu        sync.Mutex
	templates map[shape]storage.Template // guarded by mu; set by Prepare
}

// shape identifies the VMs a template can start: its image and machine size.
type shape struct {
	image     string
	vcpus     int
	memoryMiB int
}

// NewFirecrackerBooter returns a booter with cfg.
func NewFirecrackerBooter(cfg FirecrackerConfig, logger *slog.Logger) *FirecrackerBooter {
	return &FirecrackerBooter{cfg: cfg, logger: logger, templates: map[shape]storage.Template{}}
}

// jailSpec describes the jail for sandbox id. diskBytes is the size of the
// largest disk image the VMM writes to.
func (b *FirecrackerBooter) jailSpec(id string, uid int, limits Limits, diskBytes int64, files []vmm.JailFile) vmm.JailSpec {
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
		// RLIMIT_FSIZE bounds write offsets, not bytes written: the guest writes
		// anywhere on its disk (the ext4 journal sits mid-disk), and a snapshot
		// writes all of guest memory. Crossing the limit kills the VMM with
		// SIGXFSZ, so it must cover the larger of the two, plus the console.
		FileSizeLimit: max(mem, diskBytes) + 64<<20,
		NoFileLimit:   1024,
		NetNS:         netns.NamespacePath(id),
		Files:         files,
		LogBytes:      jailLogBytes,
	}
}

// Boot starts a sandbox's VM, restoring the template for its image and shape
// if one was loaded and cold-booting otherwise, and returns once the guest
// agent answers and holds the sandbox's identity. On failure every resource
// created is released.
func (b *FirecrackerBooter) Boot(ctx context.Context, spec BootSpec) (_ VM, err error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.BootTimeout)
	defer cancel()
	img, err := b.cfg.Layout.OpenImage(spec.Image)
	if err != nil {
		return nil, err
	}
	var vm *firecrackerVM
	if tpl, ok := b.template(spec.Image, spec.Limits); ok {
		vm, err = b.launch(ctx, spec, StartRestore, tpl.Scratch, []vmm.JailFile{
			{Source: img.Rootfs, Name: jailRootfs},
			{Source: tpl.State, Name: jailState},
			{Source: tpl.Memory, Name: jailMemory},
		}, b.restore)
	} else {
		vm, err = b.launch(ctx, spec, StartCold, img.Scratch, coldFiles(img), b.coldStart(spec.Limits))
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, vm.destroyDetached(ctx))
		}
	}()
	if err := assignIdentity(ctx, vm.agent, spec.ID); err != nil {
		return nil, err
	}
	return vm, nil
}

func coldFiles(img storage.Image) []vmm.JailFile {
	return []vmm.JailFile{{Source: img.Kernel, Name: jailKernel}, {Source: img.Rootfs, Name: jailRootfs}}
}

// launch prepares a sandbox's disk and network, launches its jailed VMM,
// runs start against the VMM's API, and waits for the guest agent. files are
// linked into the jail beside a private copy of the scratch disk at
// scratchSource. On failure every resource created is released.
func (b *FirecrackerBooter) launch(ctx context.Context, spec BootSpec, how StartPath, scratchSource string, files []vmm.JailFile,
	start func(context.Context, *vmm.Client) error,
) (_ *firecrackerVM, err error) {
	scratch, err := b.cfg.Layout.CreateSandboxDir(spec.ID, scratchSource, spec.UID, spec.UID)
	if err != nil {
		return nil, err
	}
	vm := &firecrackerVM{layout: b.cfg.Layout, network: b.cfg.Network, id: spec.ID, scratch: scratch, path: how}
	defer func() {
		if err == nil {
			return
		}
		// A start that failed on its own, not because the caller gave up,
		// keeps the VM's logs for diagnosis.
		if vm.jail != nil && ctx.Err() == nil {
			if dir, derr := vm.SaveDiagnostics("start_failed"); derr == nil {
				b.logger.Warn("sandbox start failed; logs kept", slog.String("sandbox_id", spec.ID), slog.String("diagnostics", dir))
			}
		}
		err = errors.Join(err, vm.destroyDetached(ctx))
	}()

	// The namespace exists before the jailer joins it.
	vm.hasNetwork = true
	if err := b.cfg.Network.Create(ctx, spec.ID, spec.UID, spec.Egress); err != nil {
		return nil, fmt.Errorf("create sandbox network: %w", err)
	}
	scratchInfo, err := os.Stat(scratch)
	if err != nil {
		return nil, fmt.Errorf("stat scratch disk: %w", err)
	}
	files = append(files, vmm.JailFile{Source: scratch, Name: jailScratch, Writable: true})
	jailSpec := b.jailSpec(spec.ID, spec.UID, spec.Limits, scratchInfo.Size(), files)
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
	if err := start(ctx, api); err != nil {
		return nil, err
	}
	vm.agent = agentclient.New(path.Join(jailSpec.Root(), strings.TrimPrefix(jailVsock, "/")))
	if err := vm.agent.WaitReady(ctx); err != nil {
		return nil, fmt.Errorf("guest agent did not come up: %w", err)
	}
	return vm, nil
}

// coldStart configures a fresh VM and boots its kernel.
func (b *FirecrackerBooter) coldStart(limits Limits) func(context.Context, *vmm.Client) error {
	return func(ctx context.Context, api *vmm.Client) error {
		if err := b.startLogger(ctx, api); err != nil {
			return err
		}
		if err := b.configure(ctx, api, limits); err != nil {
			return err
		}
		return api.Start(ctx)
	}
}

// restore loads the template snapshot linked into the jail and resumes it.
// The console is set first: its output path is not part of a snapshot.
func (b *FirecrackerBooter) restore(ctx context.Context, api *vmm.Client) error {
	if err := b.startLogger(ctx, api); err != nil {
		return err
	}
	if err := api.PutSerial(ctx, b.serial()); err != nil {
		return fmt.Errorf("configure console: %w", err)
	}
	err := api.LoadSnapshot(ctx, vmm.SnapshotLoad{
		SnapshotPath: jailState,
		MemBackend:   vmm.MemoryBackend{BackendType: vmm.BackendFile, BackendPath: jailMemory},
		ResumeVM:     true,
		// Advances the guest's wall clock past the snapshot's age where KVM
		// supports it (not under nested virtualization); the agent's restore
		// call sets the clock regardless.
		ClockRealtime: true,
	})
	if err != nil {
		return fmt.Errorf("restore snapshot: %w", err)
	}
	return nil
}

// startLogger sends the VMM's own warnings and errors to the jail's log
// directory, where they are kept for diagnosis if the VM fails.
func (b *FirecrackerBooter) startLogger(ctx context.Context, api *vmm.Client) error {
	if err := api.PutLogger(ctx, vmm.Logger{LogPath: jailVMMLog, Level: "Warning", ShowLevel: true}); err != nil {
		return fmt.Errorf("configure VMM log: %w", err)
	}
	return nil
}

func (b *FirecrackerBooter) serial() vmm.Serial {
	return vmm.Serial{
		OutputPath:  jailConsole,
		RateLimiter: &vmm.TokenBucket{Size: b.cfg.ConsoleBytesPerSecond, RefillTimeMS: 1000},
	}
}

func (b *FirecrackerBooter) configure(ctx context.Context, api *vmm.Client, limits Limits) error {
	steps := []func() error{
		func() error {
			return api.PutMachineConfig(ctx, vmm.MachineConfig{VCPUCount: limits.VCPUs, MemSizeMiB: limits.MemoryMiB})
		},
		func() error {
			return api.PutBootSource(ctx, vmm.BootSource{KernelImagePath: jailKernel, BootArgs: b.kernelArgs()})
		},
		func() error {
			limit := &vmm.RateLimiter{Bandwidth: &vmm.TokenBucket{Size: b.cfg.NetworkBytesPerSecond, RefillTimeMS: 1000}}
			return api.PutNetworkInterface(ctx, vmm.NetworkInterface{
				IfaceID: "eth0", HostDevName: netns.TapName, GuestMAC: netns.GuestMAC,
				RxRateLimiter: limit, TxRateLimiter: limit,
			})
		},
		func() error {
			return api.PutDrive(ctx, vmm.Drive{
				DriveID: "rootfs", PathOnHost: jailRootfs, IsRootDevice: true, IsReadOnly: true, RateLimiter: b.diskLimiter(),
			})
		},
		func() error {
			return api.PutDrive(ctx, vmm.Drive{DriveID: "scratch", PathOnHost: jailScratch, RateLimiter: b.diskLimiter()})
		},
		func() error { return api.PutVsock(ctx, vmm.Vsock{GuestCID: guestCID, UDSPath: jailVsock}) },
		func() error { return api.PutSerial(ctx, b.serial()) },
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

// diskLimiter returns a fresh block rate limiter; each drive gets its own.
func (b *FirecrackerBooter) diskLimiter() *vmm.RateLimiter {
	return &vmm.RateLimiter{
		Bandwidth: &vmm.TokenBucket{Size: b.cfg.DiskBytesPerSecond, RefillTimeMS: 1000},
		Ops:       &vmm.TokenBucket{Size: b.cfg.DiskOpsPerSecond, RefillTimeMS: 1000},
	}
}

func (b *FirecrackerBooter) kernelArgs() string {
	return b.cfg.BootArgs + " " + netns.GuestKernelArgs()
}

// assignIdentity gives the guest its sandbox ID, fresh host entropy, and the
// host's wall clock. A cold-booted guest gets the same call as a restored
// one, so every sandbox starts the same way (docs/engineering/design.md
// section 7.3).
func assignIdentity(ctx context.Context, agent *agentclient.Client, id string) error {
	entropy := make([]byte, 64)
	if _, err := rand.Read(entropy); err != nil {
		return fmt.Errorf("generate entropy: %w", err)
	}
	req := agentproto.RestoreRequest{SandboxID: id, Entropy: entropy, UnixNano: time.Now().UnixNano()}
	if err := agent.Restore(ctx, req); err != nil {
		return fmt.Errorf("assign sandbox identity: %w", err)
	}
	return nil
}

// Prepare removes jails, cgroups, namespaces, and sandbox directories left by
// a previous run of the control plane, installs the host firewall, and loads
// the templates that match this host. Only names with the Roundhouse prefix
// are touched.
func (b *FirecrackerBooter) Prepare(ctx context.Context) error {
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
			spec := b.jailSpec(strings.TrimPrefix(name, jailPrefix), 1, Limits{VCPUs: 1, MemoryMiB: 128}, 0, nil)
			if err := vmm.DestroyJail(ctx, spec); err != nil {
				errs = append(errs, err)
				continue
			}
			b.logger.Warn("removed orphaned jail", slog.String("jail", name))
		}
	}
	namespaces, err := netns.Namespaces()
	if err != nil {
		errs = append(errs, err)
	}
	// Nothing is live yet at startup, so every Roundhouse namespace is an orphan.
	if err := b.cfg.Network.Reconcile(ctx, namespaces, nil); err != nil {
		errs = append(errs, err)
	}
	if err := b.cfg.Network.Setup(ctx); err != nil {
		errs = append(errs, err)
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
	if err := b.loadTemplates(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// firecrackerVM is a started sandbox. Its fields are set while it starts and
// not changed afterwards.
type firecrackerVM struct {
	layout     storage.Layout
	network    *netns.Manager
	hasNetwork bool
	id         string
	path       StartPath
	scratch    string
	jailSpec   *vmm.JailSpec
	jail       *vmm.Jail
	agent      *agentclient.Client
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

func (v *firecrackerVM) StartPath() StartPath { return v.path }

// Claim applies the caller's egress policy to a pooled VM, which was started
// with none, and gives the guest fresh entropy. The agent answering that
// call is the claim's health check.
func (v *firecrackerVM) Claim(ctx context.Context, egress netns.Egress) error {
	if err := v.network.ApplyEgress(ctx, v.id, egress); err != nil {
		return fmt.Errorf("apply egress policy: %w", err)
	}
	return assignIdentity(ctx, v.agent, v.id)
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
	// The network goes after the VMM that uses it.
	if v.hasNetwork {
		if err := v.network.Destroy(ctx, v.id); err != nil {
			errs = append(errs, err)
		}
	}
	if err := v.layout.RemoveSandboxDir(v.id); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// destroyDetached destroys the VM on a failure path, where ctx may already
// be done.
func (v *firecrackerVM) destroyDetached(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return v.Destroy(ctx)
}

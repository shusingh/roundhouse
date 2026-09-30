// Package sandbox owns every sandbox on this host: creation, the calls made
// into it, its deadlines, and its teardown. See docs/engineering/design.md
// section 6.
//
// The manager is independent of how VMs are made: it drives a Booter, and
// FirecrackerBooter is the real one. That keeps lifecycle logic testable
// without KVM.
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/shusingh/roundhouse/internal/agentproto"
)

// Errors callers branch on.
var (
	// ErrNotFound means no sandbox with that ID belongs to the caller. Another
	// caller's sandbox is reported the same way, so existence is not revealed.
	ErrNotFound = errors.New("sandbox not found")
	// ErrCapacity means the host is at its sandbox limit.
	ErrCapacity = errors.New("host is at sandbox capacity")
	// ErrNotReady means the sandbox is starting or stopping.
	ErrNotReady = errors.New("sandbox is not ready")
)

// VM is a running sandbox VM as the manager sees it.
type VM interface {
	Exec(ctx context.Context, req agentproto.ExecRequest, onOutput func(*agentproto.Frame) error) (*agentproto.Frame, error)
	WriteFile(ctx context.Context, path string, content io.Reader) error
	ReadFile(ctx context.Context, path string, w io.Writer) (int64, error)
	// Exited reports whether the VM's process has gone away on its own.
	Exited() (bool, error)
	// Destroy releases every host resource the VM holds. It is idempotent.
	Destroy(ctx context.Context) error
}

// BootSpec is what a Booter needs to start one sandbox's VM.
type BootSpec struct {
	ID     string
	Image  string
	Limits Limits
	// UID is the unprivileged host uid (and gid) the VMM runs as; unique among
	// live sandboxes.
	UID int
}

// Booter starts VMs and cleans up after crashed control planes.
type Booter interface {
	Boot(ctx context.Context, spec BootSpec) (VM, error)
	// Reconcile removes host resources left by sandboxes that have no live
	// record, such as after a crash. It runs once, before any Boot.
	Reconcile(ctx context.Context) error
}

// Limits are a sandbox's resource limits.
type Limits struct {
	VCPUs     int
	MemoryMiB int
	// Lifetime is the maximum time the sandbox exists.
	Lifetime time.Duration
	// IdleTimeout ends the sandbox after this long with no call in progress.
	IdleTimeout time.Duration
}

func (l Limits) validate() error {
	switch {
	case l.VCPUs != 1 && (l.VCPUs < 2 || l.VCPUs%2 != 0):
		return fmt.Errorf("vcpus %d: want 1 or an even number", l.VCPUs)
	case l.MemoryMiB < 128:
		return fmt.Errorf("memory %d MiB: want at least 128", l.MemoryMiB)
	case l.Lifetime <= 0 || l.IdleTimeout <= 0:
		return errors.New("lifetime and idle timeout must be positive")
	}
	return nil
}

// CreateRequest asks for a new sandbox.
type CreateRequest struct {
	Owner  string
	Image  string
	Limits Limits
}

// State is a sandbox's lifecycle state.
type State string

// Lifecycle states.
const (
	StateStarting State = "starting"
	StateReady    State = "ready"
	StateStopping State = "stopping"
)

// Info describes a sandbox.
type Info struct {
	ID          string
	Owner       string
	Image       string
	State       State
	Limits      Limits
	CreatedAt   time.Time
	ExpiresAt   time.Time
	LastActive  time.Time
	ActiveCalls int
}

// Config configures a Manager.
type Config struct {
	// MaxSandboxes bounds live sandboxes on this host.
	MaxSandboxes int
	// UIDBase is the first of MaxSandboxes consecutive host uids handed to
	// VMMs; the range must be otherwise unused on the host.
	UIDBase int
	// ReapInterval is how often deadlines and VM exits are checked.
	ReapInterval time.Duration
	// Now returns the current time; tests replace it.
	Now func() time.Time
}

// Manager owns the sandboxes on this host. Its methods are safe for
// concurrent use.
type Manager struct {
	cfg    Config
	booter Booter
	logger *slog.Logger
	newID  func() string

	mu        sync.Mutex
	sandboxes map[string]*sandbox // guarded by mu
	uids      []bool              // guarded by mu; uids[i] is UIDBase+i in use
}

// sandbox is the manager's record. Fields are guarded by Manager.mu.
type sandbox struct {
	info Info
	uid  int
	vm   VM
	// teardownFailed marks a sandbox whose resources could not all be
	// released; the reaper retries, and its uid stays reserved meanwhile.
	teardownFailed bool
}

// NewManager returns a manager that boots VMs with booter.
func NewManager(cfg Config, booter Booter, logger *slog.Logger) *Manager {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ReapInterval <= 0 {
		cfg.ReapInterval = time.Second
	}
	return &Manager{
		cfg:       cfg,
		booter:    booter,
		logger:    logger,
		newID:     randomID,
		sandboxes: make(map[string]*sandbox),
		uids:      make([]bool, cfg.MaxSandboxes),
	}
}

// randomID returns a sandbox ID: "sb-" and 12 hex digits from crypto/rand.
func randomID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "sb-" + hex.EncodeToString(b[:])
}

// Run reconciles leftovers from a previous run, then enforces deadlines until
// ctx ends. Call Shutdown afterwards to destroy what remains.
func (m *Manager) Run(ctx context.Context) error {
	if err := m.booter.Reconcile(ctx); err != nil {
		return fmt.Errorf("reconcile host state: %w", err)
	}
	ticker := time.NewTicker(m.cfg.ReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.Reap(ctx)
		}
	}
}

// Create boots a sandbox and returns it once its agent answers.
func (m *Manager) Create(ctx context.Context, req CreateRequest) (Info, error) {
	if err := req.Limits.validate(); err != nil {
		return Info{}, err
	}
	now := m.cfg.Now()
	sb := &sandbox{info: Info{
		ID: m.newID(), Owner: req.Owner, Image: req.Image, State: StateStarting, Limits: req.Limits,
		CreatedAt: now, ExpiresAt: now.Add(req.Limits.Lifetime), LastActive: now,
	}}

	// Reserve the slot and uid before booting, so capacity holds while VMs
	// are still starting.
	m.mu.Lock()
	slot := slices.Index(m.uids, false)
	if slot < 0 || len(m.sandboxes) >= m.cfg.MaxSandboxes {
		m.mu.Unlock()
		return Info{}, ErrCapacity
	}
	m.uids[slot] = true
	sb.uid = m.cfg.UIDBase + slot
	m.sandboxes[sb.info.ID] = sb
	m.mu.Unlock()

	vm, err := m.booter.Boot(ctx, BootSpec{ID: sb.info.ID, Image: req.Image, Limits: req.Limits, UID: sb.uid})
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		// Boot cleans up after itself; the slot is free again.
		delete(m.sandboxes, sb.info.ID)
		m.uids[slot] = false
		return Info{}, fmt.Errorf("boot sandbox: %w", err)
	}
	sb.vm = vm
	sb.info.State = StateReady
	now = m.cfg.Now()
	sb.info.LastActive = now
	// The lifetime starts when the sandbox is usable.
	sb.info.ExpiresAt = now.Add(req.Limits.Lifetime)
	return sb.info, nil
}

// Get returns one of the owner's sandboxes.
func (m *Manager) Get(owner, id string) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sb, ok := m.sandboxes[id]
	if !ok || sb.info.Owner != owner {
		return Info{}, ErrNotFound
	}
	return sb.info, nil
}

// List returns the owner's sandboxes, oldest first.
func (m *Manager) List(owner string) []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Info
	for _, sb := range m.sandboxes {
		if sb.info.Owner == owner {
			out = append(out, sb.info)
		}
	}
	slices.SortFunc(out, func(a, b Info) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

// Exec runs a command in one of the owner's sandboxes. See VM.Exec.
func (m *Manager) Exec(ctx context.Context, owner, id string, req agentproto.ExecRequest, onOutput func(*agentproto.Frame) error) (*agentproto.Frame, error) {
	vm, release, err := m.acquire(owner, id)
	if err != nil {
		return nil, err
	}
	defer release()
	return vm.Exec(ctx, req, onOutput)
}

// WriteFile writes a file in one of the owner's sandboxes.
func (m *Manager) WriteFile(ctx context.Context, owner, id, path string, content io.Reader) error {
	vm, release, err := m.acquire(owner, id)
	if err != nil {
		return err
	}
	defer release()
	return vm.WriteFile(ctx, path, content)
}

// ReadFile reads a file from one of the owner's sandboxes into w.
func (m *Manager) ReadFile(ctx context.Context, owner, id, path string, w io.Writer) (int64, error) {
	vm, release, err := m.acquire(owner, id)
	if err != nil {
		return 0, err
	}
	defer release()
	return vm.ReadFile(ctx, path, w)
}

// acquire marks a call in progress, which pauses the idle timeout, and
// returns the VM with a release function that ends the call.
func (m *Manager) acquire(owner, id string) (VM, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sb, ok := m.sandboxes[id]
	if !ok || sb.info.Owner != owner {
		return nil, nil, ErrNotFound
	}
	if sb.info.State != StateReady {
		return nil, nil, ErrNotReady
	}
	sb.info.ActiveCalls++
	sb.info.LastActive = m.cfg.Now()
	return sb.vm, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		sb.info.ActiveCalls--
		sb.info.LastActive = m.cfg.Now()
	}, nil
}

// Delete destroys one of the owner's sandboxes. Deleting a sandbox that is
// already stopping succeeds.
func (m *Manager) Delete(ctx context.Context, owner, id string) error {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok || sb.info.Owner != owner {
		m.mu.Unlock()
		return ErrNotFound
	}
	state := sb.info.State
	m.mu.Unlock()
	if state == StateStarting {
		return ErrNotReady
	}
	if !m.claimForTeardown(sb) {
		return nil // already stopping
	}
	return m.destroy(ctx, sb, "deleted")
}

// Reap destroys sandboxes past their lifetime or idle timeout, sandboxes
// whose VM exited on its own, and retries failed teardowns. Run calls it
// periodically.
func (m *Manager) Reap(ctx context.Context) {
	now := m.cfg.Now()
	type candidate struct {
		sb     *sandbox
		reason string // empty: check whether the VM exited
	}
	var candidates []candidate
	m.mu.Lock()
	for _, sb := range m.sandboxes {
		switch {
		case sb.teardownFailed:
			candidates = append(candidates, candidate{sb, "teardown retry"})
		case sb.info.State != StateReady:
		case !now.Before(sb.info.ExpiresAt):
			candidates = append(candidates, candidate{sb, "lifetime expired"})
		case sb.info.ActiveCalls == 0 && now.Sub(sb.info.LastActive) >= sb.info.Limits.IdleTimeout:
			candidates = append(candidates, candidate{sb, "idle timeout"})
		default:
			candidates = append(candidates, candidate{sb, ""})
		}
	}
	m.mu.Unlock()

	for _, c := range candidates {
		if c.reason == "" {
			// Reading the VM's cgroup state is I/O, so it happens unlocked.
			if exited, err := c.sb.vm.Exited(); err != nil || !exited {
				continue
			}
			c.reason = "vm exited"
		}
		if m.claimForTeardown(c.sb) {
			_ = m.destroy(ctx, c.sb, c.reason)
		}
	}
}

// claimForTeardown marks a sandbox stopping and reports whether the caller
// should destroy it: true for a ready sandbox or a failed teardown to retry,
// false if another caller already claimed it.
func (m *Manager) claimForTeardown(sb *sandbox) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sb.teardownFailed {
		sb.teardownFailed = false
		return true
	}
	if sb.info.State != StateReady {
		return false
	}
	sb.info.State = StateStopping
	return true
}

// Shutdown destroys every sandbox. The control plane calls it on exit;
// sandboxes do not outlive it in v1.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	all := make([]*sandbox, 0, len(m.sandboxes))
	for _, sb := range m.sandboxes {
		all = append(all, sb)
	}
	m.mu.Unlock()
	var errs []error
	for _, sb := range all {
		if !m.claimForTeardown(sb) {
			continue
		}
		if err := m.destroy(ctx, sb, "shutdown"); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// destroy tears down a sandbox already marked stopping. On success the record
// and uid are freed; on failure the record stays, flagged for the reaper to
// retry, so a uid that may still own files is never handed out again early.
func (m *Manager) destroy(ctx context.Context, sb *sandbox, reason string) error {
	err := sb.vm.Destroy(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		sb.teardownFailed = true
		m.logger.Error("sandbox teardown failed; will retry",
			slog.String("sandbox_id", sb.info.ID), slog.String("reason", reason), slog.String("error", err.Error()))
		return fmt.Errorf("destroy sandbox %s: %w", sb.info.ID, err)
	}
	delete(m.sandboxes, sb.info.ID)
	m.uids[sb.uid-m.cfg.UIDBase] = false
	m.logger.Info("sandbox destroyed", slog.String("sandbox_id", sb.info.ID), slog.String("reason", reason))
	return nil
}

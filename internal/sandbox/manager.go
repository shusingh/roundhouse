// Package sandbox owns every sandbox on this host: creation, the calls made
// into it, its deadlines, and its teardown. See docs/engineering/design.md
// sections 6 and 7.
//
// The manager is independent of how VMs are made: it drives a Booter, and
// FirecrackerBooter is the real one. That keeps lifecycle logic testable
// without KVM.
package sandbox

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shusingh/roundhouse/internal/admission"
	"github.com/shusingh/roundhouse/internal/agentproto"
	"github.com/shusingh/roundhouse/internal/netns"
	"github.com/shusingh/roundhouse/internal/telemetry"
)

// Errors callers branch on.
var (
	// ErrNotFound means no sandbox with that ID belongs to the caller. Another
	// caller's sandbox is reported the same way, so existence is not revealed.
	ErrNotFound = errors.New("sandbox not found")
	// ErrCapacity means the host has no room for another sandbox.
	ErrCapacity = errors.New("host is at sandbox capacity")
	// ErrNotReady means the sandbox is starting or stopping.
	ErrNotReady = errors.New("sandbox is not ready")
	// ErrQuota means the owner already holds as many sandboxes as it may.
	ErrQuota = errors.New("owner is at its sandbox quota")
	// ErrUnresponsive means the guest did not finish a command well after its
	// time limit, which the guest itself should have enforced.
	ErrUnresponsive = errors.New("sandbox stopped responding")
	// ErrInvalid means the request itself is invalid.
	ErrInvalid = errors.New("invalid request")
)

// Reasons a sandbox is destroyed, for logs and metrics.
const (
	reasonDeleted     = "deleted"
	reasonLifetime    = "lifetime_expired"
	reasonIdle        = "idle_timeout"
	reasonVMExited    = "vm_exited"
	reasonRetry       = "teardown_retry"
	reasonPoolRecycle = "pool_recycle"
	reasonEvicted     = "evicted"
	reasonFailedClaim = "failed_claim"
	reasonShutdown    = "shutdown"
)

// StartPath is how a sandbox's VM was started (design.md section 7.1).
type StartPath string

// Start paths.
const (
	StartCold    StartPath = "cold"
	StartRestore StartPath = "restore"
	StartPool    StartPath = "pool"
)

// VM is a running sandbox VM as the manager sees it.
type VM interface {
	Exec(ctx context.Context, req agentproto.ExecRequest, onOutput func(*agentproto.Frame) error) (*agentproto.Frame, error)
	WriteFile(ctx context.Context, path string, content io.Reader) error
	ReadFile(ctx context.Context, path string, w io.Writer) (int64, error)
	// Exited reports whether the VM's process has gone away on its own.
	Exited() (bool, error)
	// StartPath reports whether the VM was cold-booted or restored.
	StartPath() StartPath
	// Claim readies a pooled VM, started with no egress, for a caller: it
	// applies the caller's egress policy and checks the guest answers.
	Claim(ctx context.Context, egress netns.Egress) error
	// Destroy releases every host resource the VM holds. It is idempotent.
	Destroy(ctx context.Context) error
}

// BootSpec is what a Booter needs to start one sandbox's VM.
type BootSpec struct {
	ID     string
	Image  string
	Limits Limits
	Egress netns.Egress
	// UID is the unprivileged host uid (and gid) the VMM runs as; unique among
	// live sandboxes.
	UID int
}

// Booter starts VMs.
type Booter interface {
	Boot(ctx context.Context, spec BootSpec) (VM, error)
	// Prepare readies the host before the first Boot: it removes resources
	// left by a previous run, such as after a crash, and loads anything Boot
	// needs.
	Prepare(ctx context.Context) error
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

// validateShape checks the VM size alone.
func (l Limits) validateShape() error {
	switch {
	case l.VCPUs != 1 && (l.VCPUs < 2 || l.VCPUs%2 != 0):
		return fmt.Errorf("vcpus %d: want 1 or an even number", l.VCPUs)
	case l.MemoryMiB < 128:
		return fmt.Errorf("memory %d MiB: want at least 128", l.MemoryMiB)
	}
	return nil
}

func (l Limits) validate() error {
	if err := l.validateShape(); err != nil {
		return err
	}
	if l.Lifetime <= 0 || l.IdleTimeout <= 0 {
		return errors.New("lifetime and idle timeout must be positive")
	}
	return nil
}

// CreateRequest asks for a new sandbox.
type CreateRequest struct {
	Owner  string
	Image  string
	Limits Limits
	Egress netns.Egress
	// Quota caps how many sandboxes Owner may hold, this one included. Zero
	// means no per-owner cap.
	Quota int
}

// State is a sandbox's lifecycle state.
type State string

// Lifecycle states. StatePooled is internal: a pooled sandbox has no owner,
// and no caller ever sees one.
const (
	StateStarting State = "starting"
	StatePooled   State = "pooled"
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
	Egress      netns.Egress
	StartPath   StartPath
	CreatedAt   time.Time
	ExpiresAt   time.Time
	LastActive  time.Time
	ActiveCalls int
}

// PoolConfig asks for a warm pool of sandboxes of one image and shape.
type PoolConfig struct {
	Image     string
	VCPUs     int
	MemoryMiB int
	// Size is how many idle sandboxes the pool keeps ready.
	Size int
}

// Config configures a Manager.
type Config struct {
	// MaxSandboxes bounds live sandboxes on this host, pooled ones included.
	MaxSandboxes int
	// UIDBase is the first of MaxSandboxes consecutive host uids handed to
	// VMMs; the range must be otherwise unused on the host.
	UIDBase int
	// ReapInterval is how often deadlines, VM exits, and pools are checked.
	ReapInterval time.Duration
	// Admission is the host budget every sandbox reserves from. Nil means
	// only MaxSandboxes bounds the host.
	Admission *admission.Controller
	// VMMOverheadMiB is charged to the budget on top of each guest's memory.
	VMMOverheadMiB int
	// Pools are the warm pools to keep filled.
	Pools []PoolConfig
	// PoolMaxAge recycles pooled sandboxes older than this, so a pool does not
	// age. Zero means never.
	PoolMaxAge time.Duration
	// ExecGrace is how long past a command's time limit the host waits for
	// the guest's answer before giving up on it. Zero means ten seconds.
	ExecGrace time.Duration
	// Metrics receives lifecycle events. Nil means a private set.
	Metrics *telemetry.Metrics
	// Now returns the current time; tests replace it.
	Now func() time.Time
}

// Pool tuning.
const (
	// maxPoolStarts bounds concurrent starts per pool, so refilling a drained
	// pool does not crowd out callers' own creates.
	maxPoolStarts = 2
	// poolRetryDelay pauses a pool's refill after a failed start.
	poolRetryDelay = 5 * time.Second
)

// Manager owns the sandboxes on this host. Its methods are safe for
// concurrent use.
type Manager struct {
	cfg    Config
	booter Booter
	logger *slog.Logger
	newID  func() string
	// refill wakes the pool filler; it holds at most one pending signal.
	refill chan struct{}
	// starts tracks pool starts in flight, which Shutdown waits for.
	starts sync.WaitGroup
	// ready is set once Prepare has succeeded.
	ready atomic.Bool

	mu        sync.Mutex
	sandboxes map[string]*sandbox // guarded by mu; pooled sandboxes included
	uids      []bool              // guarded by mu; uids[i] is UIDBase+i in use
	pools     map[shape]*pool     // guarded by mu
}

// sandbox is the manager's record. Fields are guarded by Manager.mu.
type sandbox struct {
	info   Info
	uid    int
	vm     VM
	charge admission.Request
	// pool is set while the sandbox belongs to a pool: starting for it or
	// waiting in it. A claim clears it.
	pool *pool
	// teardownFailed marks a sandbox whose resources could not all be
	// released; the reaper retries, and its uid and budget stay reserved
	// meanwhile.
	teardownFailed bool
}

// pool is one warm pool's state. Fields are guarded by Manager.mu.
type pool struct {
	cfg PoolConfig
	// idle holds started sandboxes waiting for a claim, oldest first.
	idle     []*sandbox
	starting int
	// retryAt delays refills after a failed start.
	retryAt time.Time
}

// NewManager returns a manager that boots VMs with booter.
func NewManager(cfg Config, booter Booter, logger *slog.Logger) *Manager {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ReapInterval <= 0 {
		cfg.ReapInterval = time.Second
	}
	if cfg.ExecGrace <= 0 {
		cfg.ExecGrace = 10 * time.Second
	}
	if cfg.Metrics == nil {
		cfg.Metrics = telemetry.NewMetrics()
	}
	if cfg.Admission == nil {
		cfg.Admission, _ = admission.New(admission.Budget{
			MemoryMiB: math.MaxInt32, VCPUs: math.MaxInt32, Sandboxes: max(cfg.MaxSandboxes, 1),
		})
	}
	pools := map[shape]*pool{}
	for _, pc := range cfg.Pools {
		pools[shape{pc.Image, pc.VCPUs, pc.MemoryMiB}] = &pool{cfg: pc}
	}
	return &Manager{
		cfg:       cfg,
		booter:    booter,
		logger:    logger,
		newID:     randomID,
		refill:    make(chan struct{}, 1),
		sandboxes: make(map[string]*sandbox),
		uids:      make([]bool, cfg.MaxSandboxes),
		pools:     pools,
	}
}

// randomID returns a sandbox ID: "sb-" and 12 hex digits from crypto/rand.
func randomID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "sb-" + hex.EncodeToString(b[:])
}

// Prepare readies the host: it removes resources left by a previous run of
// the control plane and loads templates. It must finish before the first
// Create: it treats every Roundhouse resource on the host as an orphan,
// including one a concurrent Create had just made.
func (m *Manager) Prepare(ctx context.Context) error {
	if err := m.booter.Prepare(ctx); err != nil {
		return fmt.Errorf("prepare host: %w", err)
	}
	m.ready.Store(true)
	return nil
}

// Ready reports whether Prepare has finished, so creates can be served.
func (m *Manager) Ready() bool { return m.ready.Load() }

// Run keeps the pools filled and enforces deadlines until ctx ends. Call
// Prepare before it, and Shutdown after it to destroy what remains.
func (m *Manager) Run(ctx context.Context) error {
	ticker := time.NewTicker(m.cfg.ReapInterval)
	defer ticker.Stop()
	m.fillPools(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.Reap(ctx)
			m.fillPools(ctx)
		case <-m.refill:
			m.fillPools(ctx)
		}
	}
}

// Create returns a new sandbox once its agent answers: a pooled one if a
// pool of the requested image and shape has one idle, and a freshly started
// one otherwise.
func (m *Manager) Create(ctx context.Context, req CreateRequest) (info Info, err error) {
	start := time.Now()
	defer func() {
		path := string(info.StartPath)
		if err != nil {
			path = "none"
		}
		m.cfg.Metrics.ObserveCreate(path, createOutcome(err), time.Since(start))
	}()
	if err := req.Limits.validate(); err != nil {
		return Info{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := req.Egress.Validate(); err != nil {
		return Info{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if info, ok, err := m.claim(ctx, req); ok || err != nil {
		return info, err
	}

	now := m.cfg.Now()
	sb := &sandbox{
		info: Info{
			ID: m.newID(), Owner: req.Owner, Image: req.Image, State: StateStarting, Limits: req.Limits, Egress: req.Egress,
			CreatedAt: now, ExpiresAt: now.Add(req.Limits.Lifetime), LastActive: now,
		},
		charge: m.charge(req.Limits),
	}
	// Reserve the uid and budget before booting, so capacity holds while VMs
	// are still starting.
	if err := m.reserve(ctx, sb, req.Quota); err != nil {
		return Info{}, err
	}
	vm, err := m.booter.Boot(ctx, BootSpec{ID: sb.info.ID, Image: req.Image, Limits: req.Limits, Egress: req.Egress, UID: sb.uid})
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		// Boot cleans up after itself; the reservation is free again.
		m.releaseLocked(sb)
		return Info{}, fmt.Errorf("boot sandbox: %w", err)
	}
	sb.vm = vm
	m.readyLocked(sb, req.Limits, vm.StartPath())
	return sb.info, nil
}

func createOutcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrInvalid):
		return "invalid"
	case errors.Is(err, ErrCapacity):
		return "capacity"
	case errors.Is(err, ErrQuota):
		return "quota"
	default:
		return "failed"
	}
}

// charge is what a sandbox of limits takes from the host budget.
func (m *Manager) charge(limits Limits) admission.Request {
	return admission.Request{MemoryMiB: limits.MemoryMiB + m.cfg.VMMOverheadMiB, VCPUs: limits.VCPUs}
}

// readyLocked makes a started sandbox usable. Its lifetime starts now.
func (m *Manager) readyLocked(sb *sandbox, limits Limits, path StartPath) {
	now := m.cfg.Now()
	sb.info.State = StateReady
	sb.info.Limits = limits
	sb.info.StartPath = path
	sb.info.LastActive = now
	sb.info.ExpiresAt = now.Add(limits.Lifetime)
}

// claim hands the caller an idle pooled sandbox of the requested image and
// shape, if there is one. A pooled sandbox that fails its claim is destroyed
// and the caller falls back to a fresh start.
func (m *Manager) claim(ctx context.Context, req CreateRequest) (Info, bool, error) {
	m.mu.Lock()
	p := m.pools[shape{req.Image, req.Limits.VCPUs, req.Limits.MemoryMiB}]
	if p == nil || len(p.idle) == 0 {
		m.mu.Unlock()
		return Info{}, false, nil
	}
	if m.overQuotaLocked(req.Owner, req.Quota) {
		m.mu.Unlock()
		return Info{}, false, ErrQuota
	}
	// The newest sandbox has spent the least time in the pool.
	sb := p.idle[len(p.idle)-1]
	p.idle = p.idle[:len(p.idle)-1]
	sb.pool = nil
	sb.info.State = StateStarting
	sb.info.Owner = req.Owner
	sb.info.Egress = req.Egress
	sb.info.CreatedAt = m.cfg.Now()
	m.mu.Unlock()
	m.wakeFiller()

	if err := sb.vm.Claim(ctx, req.Egress); err != nil {
		m.logger.Warn("pooled sandbox failed its claim; starting a fresh one",
			slog.String("sandbox_id", sb.info.ID), slog.String("error", err.Error()))
		m.mu.Lock()
		sb.info.State = StateStopping
		sb.info.Owner = ""
		m.mu.Unlock()
		_ = m.destroy(context.WithoutCancel(ctx), sb, reasonFailedClaim)
		return Info{}, false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readyLocked(sb, req.Limits, StartPool)
	return sb.info, true, nil
}

// overQuotaLocked reports whether owner already holds quota sandboxes,
// counting those still starting.
func (m *Manager) overQuotaLocked(owner string, quota int) bool {
	if quota <= 0 {
		return false
	}
	held := 0
	for _, sb := range m.sandboxes {
		if sb.ownedBy(owner) && sb.info.State != StateStopping {
			held++
		}
	}
	return held >= quota
}

// reserve takes a uid and budget for sb and records it, unless its owner is
// at quota. When the host is full, idle pooled sandboxes are evicted, oldest
// first, to make room: callers' sandboxes come before the pool.
func (m *Manager) reserve(ctx context.Context, sb *sandbox, quota int) error {
	for {
		m.mu.Lock()
		if m.overQuotaLocked(sb.info.Owner, quota) {
			m.mu.Unlock()
			return ErrQuota
		}
		err := m.reserveLocked(sb)
		var victim *sandbox
		if err != nil {
			victim = m.evictLocked()
		}
		m.mu.Unlock()
		if err == nil {
			return nil
		}
		if victim == nil {
			return err
		}
		if m.destroy(context.WithoutCancel(ctx), victim, reasonEvicted) != nil {
			return err // its resources stay held until the reaper's retry
		}
	}
}

// reserveLocked takes a free uid and sb's charge from the budget, and records
// sb, or takes nothing and returns an error wrapping ErrCapacity.
func (m *Manager) reserveLocked(sb *sandbox) error {
	slot := slices.Index(m.uids, false)
	if slot < 0 {
		return ErrCapacity
	}
	if err := m.cfg.Admission.Reserve(sb.charge); err != nil {
		return fmt.Errorf("%w: %w", ErrCapacity, err)
	}
	m.uids[slot] = true
	sb.uid = m.cfg.UIDBase + slot
	m.sandboxes[sb.info.ID] = sb
	return nil
}

// releaseLocked frees what reserveLocked took.
func (m *Manager) releaseLocked(sb *sandbox) {
	delete(m.sandboxes, sb.info.ID)
	m.uids[sb.uid-m.cfg.UIDBase] = false
	m.cfg.Admission.Release(sb.charge)
}

// evictLocked takes the oldest idle pooled sandbox out of its pool and marks
// it stopping, or returns nil if every pool is empty.
func (m *Manager) evictLocked() *sandbox {
	var victim *sandbox
	for _, p := range m.pools {
		if len(p.idle) > 0 && (victim == nil || p.idle[0].info.CreatedAt.Before(victim.info.CreatedAt)) {
			victim = p.idle[0]
		}
	}
	if victim != nil {
		m.leavePoolLocked(victim)
	}
	return victim
}

// leavePoolLocked removes an idle pooled sandbox from its pool and marks it
// stopping.
func (m *Manager) leavePoolLocked(sb *sandbox) {
	p := sb.pool
	p.idle = slices.DeleteFunc(p.idle, func(x *sandbox) bool { return x == sb })
	sb.info.State = StateStopping
}

// wakeFiller asks Run to refill the pools soon.
func (m *Manager) wakeFiller() {
	select {
	case m.refill <- struct{}{}:
	default:
	}
}

// fillPools starts sandboxes for every pool below its size. It never evicts
// anything and stops quietly when the host is full, so pools only use
// capacity callers are not using.
func (m *Manager) fillPools(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.cfg.Now()
	for _, p := range m.pools {
		for len(p.idle)+p.starting < p.cfg.Size && p.starting < maxPoolStarts && !now.Before(p.retryAt) {
			limits := Limits{VCPUs: p.cfg.VCPUs, MemoryMiB: p.cfg.MemoryMiB}
			sb := &sandbox{
				info: Info{
					ID: m.newID(), Image: p.cfg.Image, State: StateStarting, Limits: limits,
					Egress: netns.Egress{Mode: netns.ModeNone}, CreatedAt: now,
				},
				charge: m.charge(limits),
				pool:   p,
			}
			if m.reserveLocked(sb) != nil {
				return
			}
			p.starting++
			m.starts.Go(func() { m.startPooled(ctx, p, sb) })
		}
	}
}

// startPooled starts one sandbox for pool p.
func (m *Manager) startPooled(ctx context.Context, p *pool, sb *sandbox) {
	vm, err := m.booter.Boot(ctx, BootSpec{ID: sb.info.ID, Image: sb.info.Image, Limits: sb.info.Limits, Egress: sb.info.Egress, UID: sb.uid})
	if err == nil {
		if err = warm(ctx, vm); err != nil {
			err = errors.Join(err, vm.Destroy(context.WithoutCancel(ctx)))
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p.starting--
	if err != nil {
		m.cfg.Metrics.PoolStarts.WithLabelValues("failed").Inc()
		m.releaseLocked(sb)
		if ctx.Err() == nil {
			p.retryAt = m.cfg.Now().Add(poolRetryDelay)
			m.logger.Warn("pool start failed", slog.String("image", p.cfg.Image), slog.String("error", err.Error()))
		}
		return
	}
	m.cfg.Metrics.PoolStarts.WithLabelValues("ok").Inc()
	sb.vm = vm
	sb.info.State = StatePooled
	sb.info.StartPath = vm.StartPath()
	sb.info.CreatedAt = m.cfg.Now()
	p.idle = append(p.idle, sb)
}

// warm runs one trivial command in a pooled sandbox before it is offered.
// The first command in a restored guest faults in the guest memory that the
// agent's exec path touches, which costs about 25 ms on the development host
// (nested virtualization makes each fault expensive); paying it here keeps it
// off the claim. It runs after the clone's identity and entropy are set, so
// it does not weaken clone safety.
func warm(ctx context.Context, vm VM) error {
	exit, err := vm.Exec(ctx, agentproto.ExecRequest{Argv: []string{"true"}, TimeoutMS: 10_000},
		func(*agentproto.Frame) error { return nil })
	if err != nil {
		return fmt.Errorf("warm pooled sandbox: %w", err)
	}
	if exit.Type != agentproto.FrameExit || exit.ExitCode != 0 {
		return fmt.Errorf("warm pooled sandbox: command ended %+v", exit)
	}
	return nil
}

// Get returns one of the owner's sandboxes.
func (m *Manager) Get(owner, id string) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sb, ok := m.sandboxes[id]
	if !ok || !sb.ownedBy(owner) {
		return Info{}, ErrNotFound
	}
	return sb.info, nil
}

// ownedBy reports whether owner may see the sandbox. Pooled sandboxes belong
// to nobody.
func (sb *sandbox) ownedBy(owner string) bool {
	return sb.pool == nil && sb.info.Owner != "" && sb.info.Owner == owner
}

// List returns the owner's sandboxes, oldest first.
func (m *Manager) List(owner string) []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Info
	for _, sb := range m.sandboxes {
		if sb.ownedBy(owner) {
			out = append(out, sb.info)
		}
	}
	slices.SortFunc(out, func(a, b Info) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

// Exec runs a command in one of the owner's sandboxes. See VM.Exec. The
// guest enforces the command's time limit; the host enforces it too, a grace
// period later, so a guest that stops answering cannot hold a call open.
func (m *Manager) Exec(ctx context.Context, owner, id string, req agentproto.ExecRequest, onOutput func(*agentproto.Frame) error) (*agentproto.Frame, error) {
	vm, release, err := m.acquire(owner, id)
	if err != nil {
		return nil, err
	}
	defer release()
	backstop, cancel := context.WithTimeout(ctx, req.Timeout()+m.cfg.ExecGrace)
	defer cancel()
	start := time.Now()
	exit, err := vm.Exec(backstop, req, onOutput)
	if err != nil && ctx.Err() == nil && errors.Is(backstop.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("%w: no answer %v after the command's time limit", ErrUnresponsive, m.cfg.ExecGrace)
		exit = nil
	}
	m.cfg.Metrics.ObserveExec(execOutcome(exit, err), time.Since(start))
	return exit, err
}

func execOutcome(exit *agentproto.Frame, err error) string {
	switch {
	case errors.Is(err, ErrUnresponsive):
		return "unresponsive"
	case err != nil || exit == nil:
		return "error"
	case exit.Type == agentproto.FrameError:
		return "start_failed"
	case exit.TimedOut:
		return "timed_out"
	case exit.ExitCode != 0:
		return "nonzero"
	default:
		return "ok"
	}
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
	if !ok || !sb.ownedBy(owner) {
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
	if !ok || !sb.ownedBy(owner) {
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
	return m.destroy(ctx, sb, reasonDeleted)
}

// Reap destroys sandboxes past their lifetime or idle timeout, pooled
// sandboxes past the pool age limit, and sandboxes whose VM exited on its
// own, and retries failed teardowns. Run calls it periodically.
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
			candidates = append(candidates, candidate{sb, reasonRetry})
		case sb.info.State == StatePooled:
			if m.cfg.PoolMaxAge > 0 && now.Sub(sb.info.CreatedAt) >= m.cfg.PoolMaxAge {
				candidates = append(candidates, candidate{sb, reasonPoolRecycle})
			} else {
				candidates = append(candidates, candidate{sb, ""})
			}
		case sb.info.State != StateReady:
		case !now.Before(sb.info.ExpiresAt):
			candidates = append(candidates, candidate{sb, reasonLifetime})
		case sb.info.ActiveCalls == 0 && now.Sub(sb.info.LastActive) >= sb.info.Limits.IdleTimeout:
			candidates = append(candidates, candidate{sb, reasonIdle})
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
			c.reason = reasonVMExited
		}
		if m.claimForTeardown(c.sb) {
			_ = m.destroy(ctx, c.sb, c.reason)
		}
	}
}

// claimForTeardown marks a sandbox stopping and reports whether the caller
// should destroy it: true for a ready or pooled sandbox or a failed teardown
// to retry, false if another caller already claimed it or it is starting.
func (m *Manager) claimForTeardown(sb *sandbox) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sb.teardownFailed {
		sb.teardownFailed = false
		return true
	}
	switch sb.info.State {
	case StateReady:
		sb.info.State = StateStopping
	case StatePooled:
		m.leavePoolLocked(sb)
	default:
		return false
	}
	return true
}

// Shutdown waits for pool starts in flight, then destroys every sandbox. The
// control plane calls it after Run returns; sandboxes do not outlive it in v1.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.starts.Wait()
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
		if err := m.destroy(ctx, sb, reasonShutdown); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// destroy tears down a sandbox already marked stopping. On success its
// record, uid, and budget are freed; on failure the record stays, flagged for
// the reaper to retry, so a uid that may still own files is never handed out
// again early.
func (m *Manager) destroy(ctx context.Context, sb *sandbox, reason string) error {
	if d, ok := sb.vm.(diagnoser); ok && (reason == reasonVMExited || reason == reasonFailedClaim) {
		if dir, err := d.SaveDiagnostics(reason); err != nil {
			m.logger.Warn("save sandbox diagnostics", slog.String("sandbox_id", sb.info.ID), slog.String("error", err.Error()))
		} else {
			m.logger.Warn("sandbox failed; logs kept", slog.String("sandbox_id", sb.info.ID),
				slog.String("reason", reason), slog.String("diagnostics", dir))
		}
	}
	err := sb.vm.Destroy(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		sb.teardownFailed = true
		m.cfg.Metrics.TeardownFailures.Inc()
		m.logger.Error("sandbox teardown failed; will retry",
			slog.String("sandbox_id", sb.info.ID), slog.String("reason", reason), slog.String("error", err.Error()))
		return fmt.Errorf("destroy sandbox %s: %w", sb.info.ID, err)
	}
	m.releaseLocked(sb)
	m.cfg.Metrics.Destroys.WithLabelValues(reason).Inc()
	m.logger.Info("sandbox destroyed", slog.String("sandbox_id", sb.info.ID), slog.String("reason", reason))
	if sb.pool != nil {
		m.wakeFiller()
	}
	return nil
}

// diagnoser is a VM that can keep its logs after an unexpected failure.
type diagnoser interface {
	SaveDiagnostics(reason string) (dir string, err error)
}

// Stats is a snapshot of the manager's state for monitoring.
type Stats struct {
	// ByState counts sandboxes by state, pooled ones included.
	ByState     map[State]int
	Pools       []PoolStatus
	BudgetUsed  admission.Budget
	BudgetLimit admission.Budget
}

// Stats returns a snapshot of the manager's state.
func (m *Manager) Stats() Stats {
	st := Stats{ByState: map[State]int{}, Pools: m.Pools(),
		BudgetUsed: m.cfg.Admission.InUse(), BudgetLimit: m.cfg.Admission.Limit()}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sb := range m.sandboxes {
		st.ByState[sb.info.State]++
	}
	return st
}

// PoolStatus is one warm pool's occupancy.
type PoolStatus struct {
	PoolConfig
	Idle     int
	Starting int
}

// Pools reports every pool's occupancy, ordered by image and shape.
func (m *Manager) Pools() []PoolStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]PoolStatus, 0, len(m.pools))
	for _, p := range m.pools {
		out = append(out, PoolStatus{PoolConfig: p.cfg, Idle: len(p.idle), Starting: p.starting})
	}
	slices.SortFunc(out, func(a, b PoolStatus) int {
		return cmp.Or(strings.Compare(a.Image, b.Image), cmp.Compare(a.VCPUs, b.VCPUs), cmp.Compare(a.MemoryMiB, b.MemoryMiB))
	})
	return out
}

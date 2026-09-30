package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shusingh/roundhouse/internal/agentproto"
	"github.com/shusingh/roundhouse/internal/netns"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fakeVM struct {
	spec          BootSpec
	exited        atomic.Bool
	destroyed     atomic.Int32
	failDestroyN  atomic.Int32 // Destroy fails this many more times
	execStarted   chan struct{}
	execRelease   chan struct{}
	lastExecInput agentproto.ExecRequest
}

func (v *fakeVM) Exec(ctx context.Context, req agentproto.ExecRequest, _ func(*agentproto.Frame) error) (*agentproto.Frame, error) {
	v.lastExecInput = req
	if v.execStarted != nil {
		v.execStarted <- struct{}{}
		select {
		case <-v.execRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &agentproto.Frame{Type: agentproto.FrameExit}, nil
}

func (v *fakeVM) WriteFile(context.Context, string, io.Reader) error         { return nil }
func (v *fakeVM) ReadFile(context.Context, string, io.Writer) (int64, error) { return 0, nil }
func (v *fakeVM) Exited() (bool, error)                                      { return v.exited.Load(), nil }

func (v *fakeVM) Destroy(context.Context) error {
	if v.failDestroyN.Load() > 0 {
		v.failDestroyN.Add(-1)
		return errors.New("simulated teardown failure")
	}
	v.destroyed.Add(1)
	return nil
}

type fakeBooter struct {
	mu        sync.Mutex
	vms       map[string]*fakeVM
	failBoot  bool
	reconcile error
}

func (b *fakeBooter) Boot(_ context.Context, spec BootSpec) (VM, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failBoot {
		return nil, errors.New("simulated boot failure")
	}
	vm := &fakeVM{spec: spec}
	b.vms[spec.ID] = vm
	return vm, nil
}

func (b *fakeBooter) Reconcile(context.Context) error { return b.reconcile }

func (b *fakeBooter) vm(id string) *fakeVM {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.vms[id]
}

func newTestManager(t *testing.T, max int) (*Manager, *fakeBooter, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
	booter := &fakeBooter{vms: map[string]*fakeVM{}}
	m := NewManager(Config{MaxSandboxes: max, UIDBase: 200_000, Now: clock.Now}, booter, slog.New(slog.DiscardHandler))
	var n atomic.Int32
	m.newID = func() string { return fmt.Sprintf("sb-%d", n.Add(1)) }
	return m, booter, clock
}

var none = netns.Egress{Mode: netns.ModeNone}

var testLimits = Limits{VCPUs: 1, MemoryMiB: 256, Lifetime: time.Hour, IdleTimeout: 5 * time.Minute}

func create(t *testing.T, m *Manager, owner string) Info {
	t.Helper()
	info, err := m.Create(context.Background(), CreateRequest{Owner: owner, Image: "default", Limits: testLimits, Egress: netns.Egress{Mode: netns.ModeNone}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return info
}

func TestCreateAssignsUniqueUIDsAndEnforcesCapacity(t *testing.T) {
	t.Parallel()
	m, booter, _ := newTestManager(t, 2)
	a, b := create(t, m, "alice"), create(t, m, "alice")
	if a.State != StateReady || a.ExpiresAt.Sub(a.CreatedAt) != time.Hour {
		t.Errorf("created sandbox = %+v, want ready with a one-hour lifetime", a)
	}
	if ua, ub := booter.vm(a.ID).spec.UID, booter.vm(b.ID).spec.UID; ua == ub || ua < 200_000 || ub < 200_000 {
		t.Errorf("uids %d and %d, want distinct uids from the configured range", ua, ub)
	}
	if _, err := m.Create(context.Background(), CreateRequest{Owner: "alice", Limits: testLimits, Egress: none}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("third Create error = %v, want ErrCapacity", err)
	}
	if err := m.Delete(context.Background(), "alice", a.ID); err != nil {
		t.Fatal(err)
	}
	create(t, m, "alice") // the freed slot is usable again
}

func TestCreateRejectsInvalidLimits(t *testing.T) {
	t.Parallel()
	m, _, _ := newTestManager(t, 4)
	for name, l := range map[string]Limits{
		"three vcpus":   {VCPUs: 3, MemoryMiB: 256, Lifetime: time.Hour, IdleTimeout: time.Minute},
		"tiny memory":   {VCPUs: 1, MemoryMiB: 64, Lifetime: time.Hour, IdleTimeout: time.Minute},
		"zero lifetime": {VCPUs: 1, MemoryMiB: 256, IdleTimeout: time.Minute},
	} {
		if _, err := m.Create(context.Background(), CreateRequest{Owner: "a", Limits: l, Egress: none}); err == nil {
			t.Errorf("%s: Create accepted invalid limits", name)
		}
	}
}

func TestFailedBootFreesTheSlot(t *testing.T) {
	t.Parallel()
	m, booter, _ := newTestManager(t, 1)
	booter.failBoot = true
	if _, err := m.Create(context.Background(), CreateRequest{Owner: "a", Limits: testLimits, Egress: none}); err == nil {
		t.Fatal("Create succeeded although boot failed")
	}
	booter.failBoot = false
	create(t, m, "a")
}

func TestOwnersCannotSeeEachOther(t *testing.T) {
	t.Parallel()
	m, _, _ := newTestManager(t, 4)
	ctx := context.Background()
	mine := create(t, m, "alice")
	create(t, m, "bob")

	if _, err := m.Get("bob", mine.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob's Get of alice's sandbox = %v, want ErrNotFound", err)
	}
	if _, err := m.Exec(ctx, "bob", mine.ID, agentproto.ExecRequest{}, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob's Exec in alice's sandbox = %v, want ErrNotFound", err)
	}
	if err := m.Delete(ctx, "bob", mine.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob's Delete of alice's sandbox = %v, want ErrNotFound", err)
	}
	if list := m.List("alice"); len(list) != 1 || list[0].ID != mine.ID {
		t.Errorf("alice's List = %+v, want only her sandbox", list)
	}
}

func TestIdleTimeoutPausesDuringCalls(t *testing.T) {
	t.Parallel()
	m, booter, clock := newTestManager(t, 4)
	ctx := context.Background()
	info := create(t, m, "a")
	vm := booter.vm(info.ID)
	vm.execStarted = make(chan struct{})
	vm.execRelease = make(chan struct{})

	done := make(chan error, 1)
	go func() {
		_, err := m.Exec(ctx, "a", info.ID, agentproto.ExecRequest{}, nil)
		done <- err
	}()
	<-vm.execStarted

	// A long-running call keeps the sandbox alive past its idle timeout.
	clock.Advance(10 * time.Minute)
	m.Reap(ctx)
	if vm.destroyed.Load() != 0 {
		t.Fatal("reaper destroyed a sandbox with a call in progress")
	}
	close(vm.execRelease)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// Once the call ends, the idle clock restarts from then.
	clock.Advance(4 * time.Minute)
	m.Reap(ctx)
	if vm.destroyed.Load() != 0 {
		t.Fatal("reaper destroyed a sandbox before its idle timeout")
	}
	clock.Advance(2 * time.Minute)
	m.Reap(ctx)
	if vm.destroyed.Load() != 1 {
		t.Fatal("reaper did not destroy an idle sandbox")
	}
	if _, err := m.Get("a", info.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after reap = %v, want ErrNotFound", err)
	}
}

func TestLifetimeAndVMExitAreReaped(t *testing.T) {
	t.Parallel()
	m, booter, clock := newTestManager(t, 4)
	ctx := context.Background()
	old := create(t, m, "a")
	crashed := create(t, m, "a")
	booter.vm(crashed.ID).exited.Store(true)
	m.Reap(ctx)
	if booter.vm(crashed.ID).destroyed.Load() != 1 || booter.vm(old.ID).destroyed.Load() != 0 {
		t.Fatal("reaper should destroy exactly the sandbox whose VM exited")
	}
	for range 12 { // keep it busy-looking so only the lifetime can end it
		clock.Advance(5 * time.Minute)
		_, _ = m.Exec(ctx, "a", old.ID, agentproto.ExecRequest{}, nil)
	}
	m.Reap(ctx)
	if booter.vm(old.ID).destroyed.Load() != 1 {
		t.Fatal("reaper did not destroy a sandbox past its lifetime")
	}
}

func TestFailedTeardownKeepsUIDReservedAndRetries(t *testing.T) {
	t.Parallel()
	m, booter, _ := newTestManager(t, 1)
	ctx := context.Background()
	info := create(t, m, "a")
	vm := booter.vm(info.ID)
	vm.failDestroyN.Store(1)

	if err := m.Delete(ctx, "a", info.ID); err == nil {
		t.Fatal("Delete reported success although teardown failed")
	}
	if _, err := m.Create(ctx, CreateRequest{Owner: "a", Limits: testLimits, Egress: none}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("Create after a failed teardown = %v, want ErrCapacity: the uid must stay reserved", err)
	}
	m.Reap(ctx) // retries the teardown, which now succeeds
	if vm.destroyed.Load() != 1 {
		t.Fatal("reaper did not retry the failed teardown")
	}
	create(t, m, "a")
}

func TestDeleteIsIdempotentAndShutdownDestroysAll(t *testing.T) {
	t.Parallel()
	m, booter, _ := newTestManager(t, 4)
	ctx := context.Background()
	a, b := create(t, m, "a"), create(t, m, "b")
	if err := m.Delete(ctx, "a", a.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, "a", a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete = %v, want ErrNotFound", err)
	}
	if err := m.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if booter.vm(b.ID).destroyed.Load() != 1 || len(m.List("b")) != 0 {
		t.Error("Shutdown left a sandbox behind")
	}
}

func TestConcurrentCreatesRespectCapacity(t *testing.T) {
	t.Parallel()
	m, _, _ := newTestManager(t, 5)
	var ok, full atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_, err := m.Create(context.Background(), CreateRequest{Owner: "a", Limits: testLimits, Egress: none})
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrCapacity):
				full.Add(1)
			}
		})
	}
	wg.Wait()
	if ok.Load() != 5 || full.Load() != 15 {
		t.Errorf("%d creates succeeded and %d hit capacity, want 5 and 15", ok.Load(), full.Load())
	}
}

func TestReconcileReportsFailure(t *testing.T) {
	t.Parallel()
	m, booter, _ := newTestManager(t, 1)
	booter.reconcile = errors.New("cannot list jails")
	if err := m.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile hid a booter failure")
	}
}

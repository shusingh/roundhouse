// Package admission decides whether the host can take another sandbox. It
// keeps a budget of guest memory, vCPUs, and sandbox count, and refuses a
// reservation that would exceed any of them rather than overcommit the host
// (docs/engineering/design.md section 11).
package admission

import (
	"errors"
	"fmt"
	"sync"
)

// ErrOverBudget means a reservation would exceed the host's budget.
var ErrOverBudget = errors.New("host budget exhausted")

// Budget is an amount of host capacity.
type Budget struct {
	// MemoryMiB covers guest memory plus each VMM's own overhead.
	MemoryMiB int
	VCPUs     int
	Sandboxes int
}

// Request is one sandbox's share of the budget.
type Request struct {
	MemoryMiB int
	VCPUs     int
}

// Controller tracks reservations against a fixed budget. It is safe for
// concurrent use.
type Controller struct {
	budget Budget

	mu   sync.Mutex
	used Budget // guarded by mu
}

// New returns a controller for budget. Every dimension must be positive.
func New(budget Budget) (*Controller, error) {
	if budget.MemoryMiB <= 0 || budget.VCPUs <= 0 || budget.Sandboxes <= 0 {
		return nil, fmt.Errorf("admission budget %+v: every dimension must be positive", budget)
	}
	return &Controller{budget: budget}, nil
}

// Reserve takes r from the budget, or returns an error wrapping
// ErrOverBudget that names the exhausted dimension and takes nothing.
func (c *Controller) Reserve(r Request) error {
	if r.MemoryMiB <= 0 || r.VCPUs <= 0 {
		return fmt.Errorf("admission request %+v: memory and vCPUs must be positive", r)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.used.Sandboxes+1 > c.budget.Sandboxes:
		return fmt.Errorf("%w: %d of %d sandboxes in use", ErrOverBudget, c.used.Sandboxes, c.budget.Sandboxes)
	case c.used.MemoryMiB+r.MemoryMiB > c.budget.MemoryMiB:
		return fmt.Errorf("%w: %d MiB requested, %d of %d MiB in use", ErrOverBudget, r.MemoryMiB, c.used.MemoryMiB, c.budget.MemoryMiB)
	case c.used.VCPUs+r.VCPUs > c.budget.VCPUs:
		return fmt.Errorf("%w: %d vCPUs requested, %d of %d in use", ErrOverBudget, r.VCPUs, c.used.VCPUs, c.budget.VCPUs)
	}
	c.used.MemoryMiB += r.MemoryMiB
	c.used.VCPUs += r.VCPUs
	c.used.Sandboxes++
	return nil
}

// Release returns a reservation made with Reserve. Releasing more than was
// reserved is a programming error and panics, because the accounting would
// otherwise drift silently.
func (c *Controller) Release(r Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.used.MemoryMiB -= r.MemoryMiB
	c.used.VCPUs -= r.VCPUs
	c.used.Sandboxes--
	if c.used.MemoryMiB < 0 || c.used.VCPUs < 0 || c.used.Sandboxes < 0 {
		panic(fmt.Sprintf("admission: released more than reserved (in use %+v)", c.used))
	}
}

// InUse returns the capacity currently reserved.
func (c *Controller) InUse() Budget {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// Limit returns the budget.
func (c *Controller) Limit() Budget { return c.budget }

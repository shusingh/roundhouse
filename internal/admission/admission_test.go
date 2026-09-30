package admission

import (
	"errors"
	"sync"
	"testing"
)

func TestReserveEnforcesEveryDimension(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		budget  Budget
		request Request
		held    int // reservations of request made first
	}{
		{"memory", Budget{MemoryMiB: 1000, VCPUs: 100, Sandboxes: 100}, Request{MemoryMiB: 400, VCPUs: 1}, 2},
		{"vcpus", Budget{MemoryMiB: 100_000, VCPUs: 4, Sandboxes: 100}, Request{MemoryMiB: 256, VCPUs: 2}, 2},
		{"count", Budget{MemoryMiB: 100_000, VCPUs: 100, Sandboxes: 3}, Request{MemoryMiB: 256, VCPUs: 1}, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, err := New(tc.budget)
			if err != nil {
				t.Fatal(err)
			}
			for i := range tc.held {
				if err := c.Reserve(tc.request); err != nil {
					t.Fatalf("reservation %d: %v", i, err)
				}
			}
			before := c.InUse()
			if err := c.Reserve(tc.request); !errors.Is(err, ErrOverBudget) {
				t.Fatalf("Reserve past the budget = %v, want ErrOverBudget", err)
			}
			if c.InUse() != before {
				t.Errorf("a refused reservation changed usage: %+v, then %+v", before, c.InUse())
			}
			c.Release(tc.request)
			if err := c.Reserve(tc.request); err != nil {
				t.Errorf("Reserve after Release = %v, want room", err)
			}
		})
	}
}

func TestNewAndReserveRejectInvalidInput(t *testing.T) {
	t.Parallel()
	if _, err := New(Budget{MemoryMiB: 1, VCPUs: 0, Sandboxes: 1}); err == nil {
		t.Error("New accepted a zero vCPU budget")
	}
	c, _ := New(Budget{MemoryMiB: 1024, VCPUs: 4, Sandboxes: 4})
	if err := c.Reserve(Request{MemoryMiB: 0, VCPUs: 1}); err == nil || errors.Is(err, ErrOverBudget) {
		t.Errorf("Reserve of zero memory = %v, want a validation error", err)
	}
}

func TestReleaseBeyondReservedPanics(t *testing.T) {
	t.Parallel()
	c, _ := New(Budget{MemoryMiB: 1024, VCPUs: 4, Sandboxes: 4})
	defer func() {
		if recover() == nil {
			t.Error("releasing an unreserved request did not panic")
		}
	}()
	c.Release(Request{MemoryMiB: 256, VCPUs: 1})
}

func TestConcurrentReservationsNeverExceedBudget(t *testing.T) {
	t.Parallel()
	c, _ := New(Budget{MemoryMiB: 10 * 256, VCPUs: 1000, Sandboxes: 1000})
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for range 50 {
		wg.Go(func() {
			if c.Reserve(Request{MemoryMiB: 256, VCPUs: 1}) == nil {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if granted != 10 {
		t.Errorf("granted %d reservations, want exactly 10", granted)
	}
}

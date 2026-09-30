package node

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/shusingh/roundhouse/internal/admission"
	"github.com/shusingh/roundhouse/internal/sandbox"
)

// stateCollector exports the manager's state at scrape time: sandboxes by
// state, warm pool occupancy, and the host budget.
type stateCollector struct {
	manager     *sandbox.Manager
	sandboxes   *prometheus.Desc
	pool        *prometheus.Desc
	budgetUsed  *prometheus.Desc
	budgetLimit *prometheus.Desc
}

func newStateCollector(m *sandbox.Manager) *stateCollector {
	return &stateCollector{
		manager: m,
		sandboxes: prometheus.NewDesc("roundhouse_sandboxes",
			"Sandboxes on this host by state; pooled sandboxes are waiting in a warm pool.", []string{"state"}, nil),
		pool: prometheus.NewDesc("roundhouse_pool_sandboxes",
			"Warm pool sandboxes by pool and state (idle or starting), with each pool's target as state target.",
			[]string{"image", "vcpus", "memory_mib", "state"}, nil),
		budgetUsed: prometheus.NewDesc("roundhouse_budget_used",
			"Host budget reserved by sandboxes, by resource (memory_mib, vcpus, sandboxes).", []string{"resource"}, nil),
		budgetLimit: prometheus.NewDesc("roundhouse_budget_limit",
			"Host budget available to sandboxes, by resource.", []string{"resource"}, nil),
	}
}

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.sandboxes
	ch <- c.pool
	ch <- c.budgetUsed
	ch <- c.budgetLimit
}

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	st := c.manager.Stats()
	for _, state := range []sandbox.State{sandbox.StateStarting, sandbox.StatePooled, sandbox.StateReady, sandbox.StateStopping} {
		ch <- prometheus.MustNewConstMetric(c.sandboxes, prometheus.GaugeValue, float64(st.ByState[state]), string(state))
	}
	for _, p := range st.Pools {
		labels := []string{p.Image, strconv.Itoa(p.VCPUs), strconv.Itoa(p.MemoryMiB)}
		for state, n := range map[string]int{"idle": p.Idle, "starting": p.Starting, "target": p.Size} {
			ch <- prometheus.MustNewConstMetric(c.pool, prometheus.GaugeValue, float64(n), append(labels, state)...)
		}
	}
	emitBudget(ch, c.budgetUsed, st.BudgetUsed)
	emitBudget(ch, c.budgetLimit, st.BudgetLimit)
}

func emitBudget(ch chan<- prometheus.Metric, desc *prometheus.Desc, b admission.Budget) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(b.MemoryMiB), "memory_mib")
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(b.VCPUs), "vcpus")
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(b.Sandboxes), "sandboxes")
}

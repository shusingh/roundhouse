package main

import (
	"context"
	"sync"
	"time"

	"github.com/shusingh/roundhouse/internal/apiclient"
)

// Workloads a noisy neighbor runs; the same attacks as the containment suite.
var neighborWorkloads = []struct {
	name, script string
}{
	{"cpu_spin", "for i in 1 2 3 4; do (while :; do :; done) & done; wait"},
	{"fork_bomb", "f(){ f | f & }; f; sleep 600"},
	{"memory_hog", "python3 -c 'a = []\nwhile True: a.append(bytearray(8 << 20))'; sleep 600"},
	{"disk_fill", "dd if=/dev/zero of=/root/fill bs=4M 2>/dev/null; sleep 600"},
	{"console_flood", "yes flood-flood-flood-flood > /dev/console"},
}

// runNeighbor measures exec round trip in sandbox A while a fresh sandbox B
// runs each hostile workload, against A with an idle neighbor.
func runNeighbor(ctx context.Context, args []string) error {
	fs := newFlags("neighbor")
	seconds := fs.Int("seconds", 10, "measurement window per workload")
	_ = fs.Parse(args)
	cl, _, err := client()
	if err != nil {
		return err
	}
	window := time.Duration(*seconds) * time.Second
	a, err := cl.Create(ctx, apiclient.CreateOptions{MemoryMiB: 256})
	if err != nil {
		return err
	}
	defer func() { _ = cl.Delete(context.WithoutCancel(ctx), a.ID) }()
	echo := apiclient.ExecOptions{Argv: []string{"echo", "hi"}}
	call := func() error {
		_, err := cl.Exec(ctx, a.ID, echo, nil)
		return err
	}
	timeCalls(ctx, 50, 0, call) // warm up

	idle, idleFailures := timeCalls(ctx, 0, window, call)
	idleSummary := summarize(idle)
	results := map[string]any{"idle": map[string]any{"latency": idleSummary, "failures": idleFailures}}
	for _, w := range neighborWorkloads {
		b, err := cl.Create(ctx, apiclient.CreateOptions{VCPUs: 2, MemoryMiB: 256})
		if err != nil {
			return err
		}
		var wg sync.WaitGroup
		wg.Go(func() {
			opts := apiclient.ExecOptions{Argv: []string{"bash", "-c", w.script}, TimeoutMS: (window + 2*time.Second).Milliseconds()}
			_, _ = cl.Exec(ctx, b.ID, opts, nil)
		})
		time.Sleep(time.Second) // let the workload get going
		busy, failures := timeCalls(ctx, 0, window, call)
		wg.Wait()
		_ = cl.Delete(context.WithoutCancel(ctx), b.ID)
		s := summarize(busy)
		inflation := 0.0
		if idleSummary.P99 > 0 {
			inflation = s.P99 / idleSummary.P99
		}
		results[w.name] = map[string]any{"latency": s, "failures": failures, "p99_inflation": inflation}
	}
	return emit("neighbor", map[string]any{"params": map[string]any{"seconds": *seconds}, "results": results})
}

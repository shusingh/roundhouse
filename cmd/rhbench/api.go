package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shusingh/roundhouse/internal/apiclient"
)

// runStart measures start latency: from sending the create request to the
// sandbox answering its first exec of true. The sandbox is then deleted,
// outside the measurement.
func runStart(ctx context.Context, args []string) error {
	fs := newFlags("start")
	n := fs.Int("n", 1000, "creates to measure")
	conc := fs.Int("c", 1, "concurrent creates")
	image := fs.String("image", "default", "image")
	vcpus := fs.Int("vcpus", 1, "vCPUs")
	memory := fs.Int("memory-mib", 256, "guest memory in MiB")
	waitPool := fs.Bool("wait-pool", false, "before each create, wait until the pool of this shape has an idle sandbox, so each create measures a claim rather than the refill rate")
	_ = fs.Parse(args)
	cl, base, err := client()
	if err != nil {
		return err
	}

	var (
		mu       sync.Mutex
		samples  []time.Duration
		paths    = map[string]int{}
		failures = map[string]int{}
	)
	jobs := make(chan struct{})
	var wg sync.WaitGroup
	start := time.Now()
	for range *conc {
		wg.Go(func() {
			for range jobs {
				if *waitPool {
					if err := waitForPool(ctx, base, *image, *vcpus, *memory); err != nil {
						mu.Lock()
						failures["pool_wait"]++
						mu.Unlock()
						continue
					}
				}
				t0 := time.Now()
				sb, err := cl.Create(ctx, apiclient.CreateOptions{Image: *image, VCPUs: *vcpus, MemoryMiB: *memory})
				if err == nil {
					_, err = cl.Exec(ctx, sb.ID, apiclient.ExecOptions{Argv: []string{"true"}}, nil)
				}
				d := time.Since(t0)
				mu.Lock()
				if err != nil {
					failures[errorKind(err)]++
				} else {
					samples = append(samples, d)
					paths[sb.StartPath]++
				}
				mu.Unlock()
				if sb.ID != "" {
					_ = cl.Delete(context.WithoutCancel(ctx), sb.ID)
				}
			}
		})
	}
feed:
	for range *n {
		select {
		case jobs <- struct{}{}:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	return emit("start", map[string]any{
		"params":       map[string]any{"n": *n, "concurrency": *conc, "image": *image, "vcpus": *vcpus, "memory_mib": *memory, "wait_pool": *waitPool},
		"latency":      summarize(samples),
		"start_paths":  paths,
		"failures":     failures,
		"wall_seconds": time.Since(start).Seconds(),
	})
}

// runExec measures the round trip of echo in one ready sandbox.
func runExec(ctx context.Context, args []string) error {
	fs := newFlags("exec")
	n := fs.Int("n", 10000, "calls to measure")
	memory := fs.Int("memory-mib", 256, "guest memory in MiB")
	_ = fs.Parse(args)
	cl, _, err := client()
	if err != nil {
		return err
	}
	sb, err := cl.Create(ctx, apiclient.CreateOptions{MemoryMiB: *memory})
	if err != nil {
		return err
	}
	defer func() { _ = cl.Delete(context.WithoutCancel(ctx), sb.ID) }()
	echo := apiclient.ExecOptions{Argv: []string{"echo", "hi"}}
	for range 20 { // warm the connection and the guest's page cache
		if _, err := cl.Exec(ctx, sb.ID, echo, nil); err != nil {
			return err
		}
	}
	samples, failures := timeCalls(ctx, *n, 0, func() error {
		_, err := cl.Exec(ctx, sb.ID, echo, nil)
		return err
	})
	return emit("exec", map[string]any{
		"params":   map[string]any{"n": *n, "memory_mib": *memory, "start_path": sb.StartPath},
		"latency":  summarize(samples),
		"failures": failures,
	})
}

// timeCalls runs call n times, or until the duration elapses when n is 0,
// and returns the successful calls' durations and the failure count.
func timeCalls(ctx context.Context, n int, duration time.Duration, call func() error) ([]time.Duration, int) {
	var samples []time.Duration
	failures := 0
	deadline := time.Now().Add(duration)
	for i := 0; ctx.Err() == nil; i++ {
		if (n > 0 && i >= n) || (n == 0 && time.Now().After(deadline)) {
			break
		}
		t0 := time.Now()
		if err := call(); err != nil {
			failures++
			continue
		}
		samples = append(samples, time.Since(t0))
	}
	return samples, failures
}

// errorKind reduces an error to a stable key: the API's error code if there
// is one.
func errorKind(err error) string {
	var apiErr *apiclient.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "transport"
}

// waitForPool waits until the pool of a shape reports an idle sandbox.
func waitForPool(ctx context.Context, base, image string, vcpus, memoryMiB int) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		body, err := fetch(ctx, base+"/metrics")
		if err != nil {
			return err
		}
		if poolIdle(body, image, vcpus, memoryMiB) > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	return errors.New("pool never had an idle sandbox")
}

func fetch(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

// poolIdle reads a pool's idle count from the metrics exposition.
func poolIdle(metrics, image string, vcpus, memoryMiB int) int {
	labels := fmt.Sprintf(`{image=%q,memory_mib="%d",state="idle",vcpus="%d"}`, image, memoryMiB, vcpus)
	for _, line := range strings.Split(metrics, "\n") {
		if rest, ok := strings.CutPrefix(line, "roundhouse_pool_sandboxes"+labels+" "); ok {
			v, _ := strconv.ParseFloat(strings.TrimSpace(rest), 64)
			return int(v)
		}
	}
	return 0
}

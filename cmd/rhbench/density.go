package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shusingh/roundhouse/internal/apiclient"
)

// runDensity creates idle sandboxes until the server refuses or the host's
// available memory falls to the reserve, then reports what each costs. Run it
// as root on the server's host, against a server whose admission budget is
// set above what the host can hold, so memory is the limit being found.
func runDensity(ctx context.Context, args []string) error {
	fs := newFlags("density")
	memory := fs.Int("memory-mib", 256, "guest memory in MiB")
	reserve := fs.Int("reserve-mib", 4096, "stop when the host's available memory falls to this")
	limit := fs.Int("max", 2000, "stop after this many sandboxes")
	settle := fs.Duration("settle", 5*time.Second, "wait before measuring, so starts finish and caches settle")
	cgroups := fs.String("cgroup-root", "/sys/fs/cgroup/roundhouse", "parent cgroup of the jails")
	_ = fs.Parse(args)
	cl, _, err := client()
	if err != nil {
		return err
	}
	availBefore, err := memAvailableMiB()
	if err != nil {
		return err
	}

	var ids []string
	defer func() { deleteAll(context.WithoutCancel(ctx), cl, ids) }()
	stop := "max reached"
	start := time.Now()
	for len(ids) < *limit && ctx.Err() == nil {
		if avail, err := memAvailableMiB(); err != nil || avail <= *reserve {
			stop = fmt.Sprintf("host available memory at %d MiB", avail)
			break
		}
		sb, err := cl.Create(ctx, apiclient.CreateOptions{MemoryMiB: *memory})
		if err != nil {
			stop = "create failed: " + errorKind(err)
			break
		}
		ids = append(ids, sb.ID)
	}
	elapsed := time.Since(start)
	time.Sleep(*settle)

	usage, err := jailUsage(*cgroups)
	if err != nil {
		return err
	}
	availAfter, err := memAvailableMiB()
	if err != nil {
		return err
	}
	n := float64(max(len(ids), 1))
	return emit("density", map[string]any{
		"params":                     map[string]any{"memory_mib": *memory, "reserve_mib": *reserve, "max": *limit},
		"sandboxes":                  len(ids),
		"stop_reason":                stop,
		"create_seconds":             elapsed.Seconds(),
		"jails_measured":             usage.jails,
		"pss_mib_per_sandbox":        usage.pssKiB / 1024 / n,
		"rss_mib_per_sandbox":        usage.rssKiB / 1024 / n,
		"cgroup_mib_per_sandbox":     usage.cgroupBytes / (1 << 20) / n,
		"host_available_drop_mib":    availBefore - availAfter,
		"host_available_mib_per_box": float64(availBefore-availAfter) / n,
	})
}

type jailTotals struct {
	jails                       int
	pssKiB, rssKiB, cgroupBytes float64
}

// jailUsage sums the proportional and resident set sizes of every process in
// the jails' cgroups, and the cgroups' own memory accounting.
func jailUsage(root string) (jailTotals, error) {
	var u jailTotals
	jails, err := filepath.Glob(filepath.Join(root, "rh-*"))
	if err != nil {
		return u, err
	}
	for _, jail := range jails {
		u.jails++
		if current, err := os.ReadFile(filepath.Join(jail, "memory.current")); err == nil { //nolint:gosec // G304: a cgroup file
			v, _ := strconv.ParseFloat(string(trimSpace(current)), 64)
			u.cgroupBytes += v
		}
		procs, err := os.ReadFile(filepath.Join(jail, "cgroup.procs")) //nolint:gosec // G304: a cgroup file
		if err != nil {
			continue // the jail went away
		}
		for _, pid := range strings.Fields(string(procs)) {
			rollup, err := os.ReadFile("/proc/" + pid + "/smaps_rollup") //nolint:gosec // G304: a numeric pid from the cgroup
			if err != nil {
				continue
			}
			u.pssKiB += float64(fieldKiB(string(rollup), "Pss:"))
			u.rssKiB += float64(fieldKiB(string(rollup), "Rss:"))
		}
	}
	return u, nil
}

// fieldKiB reads a "Name: N kB" line.
func fieldKiB(text, name string) int64 {
	for _, line := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(line, name); ok {
			fields := strings.Fields(rest)
			if len(fields) >= 1 {
				v, _ := strconv.ParseInt(fields[0], 10, 64)
				return v
			}
		}
	}
	return 0
}

func memAvailableMiB() (int, error) {
	v, err := readField("/proc/meminfo", "MemAvailable")
	if err != nil {
		return 0, err
	}
	kib, err := strconv.Atoi(strings.Fields(v)[0])
	if err != nil {
		return 0, err
	}
	return kib / 1024, nil
}

// readField returns the value of the first "name: value" line in a file.
func readField(file, name string) (string, error) {
	f, err := os.Open(file) //nolint:gosec // G304: fixed /proc paths
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	s := bufio.NewScanner(f)
	for s.Scan() {
		key, value, ok := strings.Cut(s.Text(), ":")
		if ok && strings.TrimSpace(key) == name {
			return strings.TrimSpace(value), nil
		}
	}
	return "", fmt.Errorf("%s: no %s", file, name)
}

func trimSpace(b []byte) []byte { return bytes.TrimSpace(b) }

// deleteAll deletes sandboxes, eight at a time.
func deleteAll(ctx context.Context, cl *apiclient.Client, ids []string) {
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, id := range ids {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			_ = cl.Delete(ctx, id)
		})
	}
	wg.Wait()
}

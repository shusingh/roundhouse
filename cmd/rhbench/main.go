// Command rhbench measures a running roundhoused as design.md section 15
// describes: start latency per path, exec round trip, density, and a noisy
// neighbor's effect, plus container baselines with runc or gVisor.
//
// It is a client of the public API, so it measures the service as callers
// see it. Density needs root on the same host, to read each VMM's
// proportional set size from /proc.
//
// Usage:
//
//	rhbench start    -n 1000 -c 1 -memory-mib 256 [-wait-pool]
//	rhbench exec     -n 10000
//	rhbench density  -memory-mib 256 -reserve-mib 4096
//	rhbench neighbor -seconds 10
//	rhbench baseline -runtime runc -bundle DIR -n 1000
//
// The server and key come from ROUNDHOUSE_URL and ROUNDHOUSE_API_KEY.
// Results print as one JSON object per line, for bench/ scripts to collect.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/shusingh/roundhouse/internal/apiclient"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "rhbench:", err)
		os.Exit(1)
	}
}

func run(cmd string, args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	var err error
	switch cmd {
	case "start":
		err = runStart(ctx, args)
	case "exec":
		err = runExec(ctx, args)
	case "density":
		err = runDensity(ctx, args)
	case "neighbor":
		err = runNeighbor(ctx, args)
	case "baseline":
		err = runBaseline(ctx, args)
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
	return err
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: rhbench start|exec|density|neighbor|baseline [flags]; see the package documentation")
	os.Exit(2)
}

// client returns an API client from the environment.
func client() (*apiclient.Client, string, error) {
	base, key := os.Getenv("ROUNDHOUSE_URL"), os.Getenv("ROUNDHOUSE_API_KEY")
	if base == "" {
		base = "http://127.0.0.1:8480"
	}
	if key == "" {
		return nil, "", errors.New("set ROUNDHOUSE_API_KEY")
	}
	return apiclient.New(base, key), base, nil
}

// emit prints one result as a JSON line, stamped with the host it ran on.
func emit(kind string, result any) error {
	line, err := json.Marshal(map[string]any{
		"kind":   kind,
		"host":   hostInfo(),
		"at":     time.Now().UTC().Format(time.RFC3339),
		"result": result,
	})
	if err != nil {
		return err
	}
	_, err = fmt.Println(string(line))
	return err
}

// hostInfo describes the machine, as every published number must.
func hostInfo() map[string]any {
	info := map[string]any{"cpus": runtime.NumCPU()}
	if model, err := readField("/proc/cpuinfo", "model name"); err == nil {
		info["cpu_model"] = model
	}
	if total, err := readField("/proc/meminfo", "MemTotal"); err == nil {
		info["mem_total"] = total
	}
	if release, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		info["kernel"] = string(trimSpace(release))
	}
	return info
}

func newFlags(name string) *flag.FlagSet {
	return flag.NewFlagSet("rhbench "+name, flag.ExitOnError)
}

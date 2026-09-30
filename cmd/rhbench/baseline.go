package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runBaseline measures container start and exec with an OCI runtime (runc,
// or gVisor's runsc) on the same host and root file system, as context for
// the cost of VM isolation. The bundle directory holds config.json and
// rootfs/; rhbench derives its own configs from it.
func runBaseline(ctx context.Context, args []string) error {
	fs := newFlags("baseline")
	runtimeBin := fs.String("runtime", "runc", "OCI runtime binary")
	runtimeArgs := fs.String("runtime-args", "", "global flags for the runtime, space separated (for example --network=none for runsc)")
	bundle := fs.String("bundle", "", "OCI bundle directory with config.json and rootfs/")
	n := fs.Int("n", 1000, "container starts to measure")
	execN := fs.Int("exec-n", 1000, "execs to measure")
	_ = fs.Parse(args)
	if *bundle == "" {
		return errors.New("-bundle is required")
	}
	global := strings.Fields(*runtimeArgs)
	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, *runtimeBin, append(global, args...)...) //nolint:gosec // G204: the operator's runtime and fixed arguments
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %w: %s", *runtimeBin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}

	work, err := os.MkdirTemp("", "rhbench-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()
	startBundle, err := deriveBundle(*bundle, filepath.Join(work, "start"), []string{"true"})
	if err != nil {
		return err
	}
	execBundle, err := deriveBundle(*bundle, filepath.Join(work, "exec"), []string{"sleep", "infinity"})
	if err != nil {
		return err
	}

	// Start: create, start, and wait for a container that runs true.
	id := fmt.Sprintf("rhbench-%d", os.Getpid())
	var i int
	starts, startFailures := timeCalls(ctx, *n, 0, func() error {
		i++
		return run("run", "--bundle", startBundle, fmt.Sprintf("%s-%d", id, i))
	})

	// Exec: echo in a running container. The container's process inherits
	// create's stdio, so create must not get pipes: waiting for their EOF
	// would wait for the container to exit.
	long := id + "-exec"
	create := exec.CommandContext(ctx, *runtimeBin, append(global, "create", "--bundle", execBundle, long)...) //nolint:gosec // G204: as above
	if err := create.Run(); err != nil {
		return fmt.Errorf("%s create: %w", *runtimeBin, err)
	}
	defer func() {
		_ = run("kill", long, "KILL")
		time.Sleep(200 * time.Millisecond)
		_ = run("delete", "--force", long)
	}()
	if err := run("start", long); err != nil {
		return err
	}
	execs, execFailures := timeCalls(ctx, *execN, 0, func() error { return run("exec", long, "echo", "hi") })

	return emit("baseline", map[string]any{
		"params":         map[string]any{"runtime": *runtimeBin, "runtime_args": *runtimeArgs, "n": *n, "exec_n": *execN},
		"start":          summarize(starts),
		"start_failures": startFailures,
		"exec":           summarize(execs),
		"exec_failures":  execFailures,
	})
}

// deriveBundle writes a bundle at dir whose config runs args without a
// terminal, over the original bundle's root file system.
func deriveBundle(orig, dir string, args []string) (string, error) {
	data, err := os.ReadFile(filepath.Join(orig, "config.json")) //nolint:gosec // G304: the operator's bundle
	if err != nil {
		return "", err
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("parse bundle config: %w", err)
	}
	process, _ := cfg["process"].(map[string]any)
	if process == nil {
		return "", errors.New("bundle config has no process")
	}
	process["args"] = args
	process["terminal"] = false
	root, _ := cfg["root"].(map[string]any)
	if root == nil {
		return "", errors.New("bundle config has no root")
	}
	rootPath, _ := root["path"].(string)
	if !filepath.IsAbs(rootPath) {
		rootPath = filepath.Join(orig, rootPath)
	}
	abs, err := filepath.Abs(rootPath)
	if err != nil {
		return "", err
	}
	root["path"] = abs
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	return dir, os.WriteFile(filepath.Join(dir, "config.json"), out, 0o600)
}

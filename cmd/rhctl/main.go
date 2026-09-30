// Command rhctl is a command-line client for Roundhouse.
//
// It reads the API address from ROUNDHOUSE_URL (default
// http://127.0.0.1:8480) and the key from ROUNDHOUSE_API_KEY.
//
//	rhctl create [-vcpus N] [-memory MiB] [-lifetime 1h] [-idle 10m]
//	rhctl ls
//	rhctl exec ID [-timeout 1m] -- COMMAND [ARG...]
//	rhctl put ID LOCAL_FILE REMOTE_PATH
//	rhctl get ID REMOTE_PATH [LOCAL_FILE]
//	rhctl rm ID
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"text/tabwriter"
	"time"

	"github.com/shusingh/roundhouse/internal/agentproto"
	"github.com/shusingh/roundhouse/internal/apiclient"
)

const usage = `usage:
  rhctl create [-vcpus N] [-memory MiB] [-lifetime 1h] [-idle 10m]
  rhctl ls
  rhctl exec ID [-timeout 1m] -- COMMAND [ARG...]
  rhctl put ID LOCAL_FILE REMOTE_PATH
  rhctl get ID REMOTE_PATH [LOCAL_FILE]
  rhctl rm ID

Environment: ROUNDHOUSE_URL (default http://127.0.0.1:8480), ROUNDHOUSE_API_KEY.`

// exitError carries a command's exit code out of rhctl exec.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func main() {
	err := run(os.Args[1:])
	var exit exitError
	switch {
	case errors.As(err, &exit):
		os.Exit(exit.code)
	case err != nil:
		fmt.Fprintln(os.Stderr, "rhctl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return exitError{2}
	}
	base := os.Getenv("ROUNDHOUSE_URL")
	if base == "" {
		base = "http://127.0.0.1:8480"
	}
	key := os.Getenv("ROUNDHOUSE_API_KEY")
	if key == "" {
		return errors.New("set ROUNDHOUSE_API_KEY")
	}
	c := apiclient.New(base, key)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch cmd, rest := args[0], args[1:]; cmd {
	case "create":
		return create(ctx, c, rest)
	case "ls":
		return list(ctx, c)
	case "exec":
		return execCmd(ctx, c, rest)
	case "put":
		return put(ctx, c, rest)
	case "get":
		return get(ctx, c, rest)
	case "rm":
		if len(rest) != 1 {
			return errors.New("usage: rhctl rm ID")
		}
		return c.Delete(ctx, rest[0])
	default:
		fmt.Fprintln(os.Stderr, usage)
		return exitError{2}
	}
}

func create(ctx context.Context, c *apiclient.Client, args []string) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	vcpus := fs.Int("vcpus", 0, "vCPUs (default: server default)")
	memory := fs.Int("memory", 0, "memory in MiB (default: server default)")
	lifetime := fs.Duration("lifetime", 0, "maximum lifetime (default: server default)")
	idle := fs.Duration("idle", 0, "idle timeout (default: server default)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	start := time.Now()
	sb, err := c.Create(ctx, apiclient.CreateOptions{
		VCPUs: *vcpus, MemoryMiB: *memory,
		LifetimeSeconds: int64(lifetime.Seconds()), IdleTimeoutSeconds: int64(idle.Seconds()),
	})
	if err != nil {
		return err
	}
	fmt.Println(sb.ID)
	fmt.Fprintf(os.Stderr, "ready in %v (%d vCPU, %d MiB, expires %s)\n",
		time.Since(start).Round(time.Millisecond), sb.VCPUs, sb.MemoryMiB, sb.ExpiresAt.Local().Format(time.TimeOnly))
	return nil
}

func list(ctx context.Context, c *apiclient.Client) error {
	sandboxes, err := c.List(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tSTATE\tVCPUS\tMEMORY\tCREATED\tEXPIRES\tACTIVE")
	for _, sb := range sandboxes {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%d MiB\t%s\t%s\t%d\n", sb.ID, sb.State, sb.VCPUs, sb.MemoryMiB,
			sb.CreatedAt.Local().Format(time.TimeOnly), sb.ExpiresAt.Local().Format(time.TimeOnly), sb.ActiveCalls)
	}
	return tw.Flush()
}

func execCmd(ctx context.Context, c *apiclient.Client, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: rhctl exec ID [-timeout 1m] -- COMMAND [ARG...]")
	}
	id := args[0]
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 0, "command time limit (default: server default)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	argv := fs.Args()
	if len(argv) == 0 {
		return errors.New("no command given")
	}
	exit, err := c.Exec(ctx, id, apiclient.ExecOptions{Argv: argv, TimeoutMS: timeout.Milliseconds()}, func(f *agentproto.Frame) error {
		out := os.Stdout
		if f.Type == agentproto.FrameStderr {
			out = os.Stderr
		}
		_, err := out.Write(f.Data)
		return err
	})
	if err != nil {
		return err
	}
	switch {
	case exit.Type == agentproto.FrameError:
		return errors.New(exit.Error)
	case exit.TimedOut:
		fmt.Fprintf(os.Stderr, "rhctl: command timed out after %d ms\n", exit.DurationMS)
		return exitError{124}
	case exit.ExitCode != 0:
		return exitError{exit.ExitCode}
	}
	return nil
}

func put(ctx context.Context, c *apiclient.Client, args []string) error {
	if len(args) != 3 {
		return errors.New("usage: rhctl put ID LOCAL_FILE REMOTE_PATH")
	}
	f, err := os.Open(args[1]) //nolint:gosec // G703: the user's own local file, named on the command line
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return c.WriteFile(ctx, args[0], args[2], f)
}

func get(ctx context.Context, c *apiclient.Client, args []string) error {
	if len(args) < 2 || len(args) > 3 {
		return errors.New("usage: rhctl get ID REMOTE_PATH [LOCAL_FILE]")
	}
	var w io.Writer = os.Stdout
	if len(args) == 3 {
		f, err := os.Create(args[2]) //nolint:gosec // G703: the user's own local file, named on the command line
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		w = f
	}
	_, err := c.ReadFile(ctx, args[0], args[1], w)
	return err
}

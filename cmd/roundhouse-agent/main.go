//go:build linux

// Command roundhouse-agent runs inside every Roundhouse microVM.
//
// As the guest's init (PID 1) it prepares the file systems and supervises the
// protocol server; with the "serve" argument it is that server, answering the
// host over vsock. It is built static (CGO_ENABLED=0) and installed in the
// guest image as its init.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/shusingh/roundhouse/internal/agent"
	"github.com/shusingh/roundhouse/internal/agentproto"
)

func main() {
	// Logs go to the serial console, which the host rate-limits and keeps in
	// the jail for diagnosis. They never include commands or output.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if os.Getpid() == 1 {
		if err := agent.RunInit(logger); err != nil {
			logger.Error("guest init failed", slog.String("error", err.Error()))
		}
		os.Exit(1) // init exiting ends the VM
	}
	if len(os.Args) == 2 && os.Args[1] == agent.ServeArg {
		if err := serve(logger); err != nil {
			logger.Error("agent server stopped", slog.String("error", err.Error()))
			os.Exit(1)
		}
		return
	}
	fmt.Fprintf(os.Stderr, "usage: roundhouse-agent runs as the guest's init, or as `roundhouse-agent %s`\n", agent.ServeArg)
	os.Exit(2)
}

func serve(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	ln, err := agent.ListenVsock(agentproto.Port)
	if err != nil {
		return err
	}
	logger.Info("agent listening", slog.Int("vsock_port", agentproto.Port))
	return agent.NewServer(logger).Serve(ctx, ln)
}

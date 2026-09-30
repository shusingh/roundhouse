// Command roundhoused is Roundhouse's control plane: it serves the API and
// runs every sandbox on this host.
//
// Usage:
//
//	roundhoused -config /etc/roundhouse/roundhoused.yaml
//	roundhoused hash-key            generate an API key and its config digest
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/shusingh/roundhouse/internal/api"
	"github.com/shusingh/roundhouse/internal/config"
	"github.com/shusingh/roundhouse/internal/sandbox"
	"github.com/shusingh/roundhouse/internal/storage"
	"github.com/shusingh/roundhouse/internal/telemetry"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "hash-key" {
		hashKey()
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "roundhoused:", err)
		os.Exit(1)
	}
}

// hashKey prints a new random API key and the digest to put in the config.
func hashKey() {
	var raw [24]byte
	_, _ = rand.Read(raw[:])
	key := "rh_" + base64.RawURLEncoding.EncodeToString(raw[:])
	d := config.Digest(key)
	fmt.Printf("key:    %s\nsha256: %s\n", key, hex.EncodeToString(d[:]))
}

func run() error {
	configPath := flag.String("config", "/etc/roundhouse/roundhoused.yaml", "configuration file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	logger, err := telemetry.NewLogger(os.Stdout, cfg.LogLevel)
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("must run as root: it launches jailed VMMs and manages cgroups and namespaces")
	}

	booter := sandbox.NewFirecrackerBooter(sandbox.FirecrackerConfig{
		Layout:                storage.Layout{Root: cfg.Host.DataDir},
		JailerPath:            cfg.Host.JailerPath,
		FirecrackerPath:       cfg.Host.FirecrackerPath,
		CgroupRoot:            cfg.Host.CgroupRoot,
		CgroupParent:          cfg.Host.CgroupParent,
		BootArgs:              sandbox.DefaultBootArgs,
		VMMOverheadMiB:        cfg.Host.VMMOverheadMiB,
		PidsMax:               cfg.Sandbox.PidsMax,
		BootTimeout:           cfg.Sandbox.BootTimeout,
		ConsoleBytesPerSecond: 16 << 10,
	}, logger)
	manager := sandbox.NewManager(sandbox.Config{
		MaxSandboxes: cfg.Sandbox.MaxSandboxes,
		UIDBase:      cfg.Host.UIDBase,
		ReapInterval: cfg.Sandbox.ReapInterval,
	}, booter, logger)
	apiServer, err := api.New(cfg, manager, logger)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	// Reconcile before listening, so no request races a cleanup of leftovers.
	managerErr := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { managerErr <- manager.Run(ctx) })

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Server.Listen)
	if err != nil {
		stop()
		wg.Wait()
		return fmt.Errorf("listen: %w", err)
	}
	httpServer := &http.Server{
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Exec responses stream for as long as a command runs; command time
		// limits bound them, so there is no write timeout.
		IdleTimeout: 2 * time.Minute,
		ErrorLog:    slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(ln) }()
	logger.Info("roundhoused started", slog.String("listen", ln.Addr().String()),
		slog.Int("max_sandboxes", cfg.Sandbox.MaxSandboxes), slog.String("data_dir", cfg.Host.DataDir))

	var runErr error
	select {
	case err := <-serveErr:
		runErr = fmt.Errorf("serve: %w", err)
	case err := <-managerErr:
		runErr = err
	case <-ctx.Done():
	}
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		_ = httpServer.Close()
	}
	wg.Wait()
	// Sandboxes do not outlive the control plane in v1.
	if err := manager.Shutdown(shutdownCtx); err != nil {
		runErr = errors.Join(runErr, err)
	}
	logger.Info("roundhoused stopped")
	return runErr
}

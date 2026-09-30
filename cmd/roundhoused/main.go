// Command roundhoused is Roundhouse's control plane: it serves the API and
// runs every sandbox on this host.
//
// Usage:
//
//	roundhoused -config /etc/roundhouse/roundhoused.yaml
//	roundhoused hash-key            generate an API key and its config digest
//	roundhoused template build -config FILE [-image NAME] [-vcpus N] [-memory-mib N]
//	                                snapshot a booted guest as a template
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
	"github.com/shusingh/roundhouse/internal/node"
	"github.com/shusingh/roundhouse/internal/sandbox"
	"github.com/shusingh/roundhouse/internal/telemetry"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "hash-key" {
		hashKey()
		return
	}
	var err error
	if len(os.Args) >= 3 && os.Args[1] == "template" && os.Args[2] == "build" {
		err = buildTemplate(os.Args[3:])
	} else {
		err = run()
	}
	if err != nil {
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

	n, err := node.New(cfg, logger)
	if err != nil {
		return err
	}
	manager := n.Manager
	apiServer, err := api.New(cfg, manager, logger)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	// Prepare before listening: it removes every Roundhouse resource on the
	// host, so it must not overlap any request.
	if err := manager.Prepare(ctx); err != nil {
		return err
	}
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

// buildTemplate boots a guest and saves it as the template for its image and
// shape. Run it while roundhoused is stopped, or restart roundhoused after:
// templates are loaded at startup.
func buildTemplate(args []string) error {
	fs := flag.NewFlagSet("template build", flag.ContinueOnError)
	configPath := fs.String("config", "/etc/roundhouse/roundhoused.yaml", "configuration file")
	image := fs.String("image", "", "image name (default: sandbox.image)")
	vcpus := fs.Int("vcpus", 0, "vCPUs (default: sandbox.defaults.vcpus)")
	memory := fs.Int("memory-mib", 0, "guest memory in MiB (default: sandbox.defaults.memory_mib)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("must run as root: it launches a jailed VMM")
	}
	logger, err := telemetry.NewLogger(os.Stderr, cfg.LogLevel)
	if err != nil {
		return err
	}
	if *image == "" {
		*image = cfg.Sandbox.Image
	}
	if *vcpus == 0 {
		*vcpus = cfg.Sandbox.Defaults.VCPUs
	}
	if *memory == 0 {
		*memory = cfg.Sandbox.Defaults.MemoryMiB
	}
	n, err := node.New(cfg, logger)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	start := time.Now()
	m, err := n.Booter.BuildTemplate(ctx, *image, sandbox.Limits{VCPUs: *vcpus, MemoryMiB: *memory}, n.TemplateUID)
	if err != nil {
		return fmt.Errorf("build template: %w", err)
	}
	fmt.Printf("template %s %dcpu-%dmib built in %v (Firecracker %s, kernel %.12s, rootfs %.12s)\n",
		m.Image, m.VCPUs, m.MemoryMiB, time.Since(start).Round(time.Millisecond), m.FirecrackerVersion, m.KernelSHA256, m.RootfsSHA256)
	return nil
}

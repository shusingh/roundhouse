// Package node assembles one host's sandbox machinery from configuration:
// networking, the Firecracker booter, and the sandbox manager. roundhoused
// and the integration tests build the node the same way through it.
package node

import (
	"fmt"
	"log/slog"

	"github.com/shusingh/roundhouse/internal/config"
	"github.com/shusingh/roundhouse/internal/netns"
	"github.com/shusingh/roundhouse/internal/sandbox"
	"github.com/shusingh/roundhouse/internal/storage"
)

// New builds the sandbox manager for this host. The manager's Run reconciles
// leftovers and installs the host firewall before serving.
func New(cfg config.Config, logger *slog.Logger) (*sandbox.Manager, error) {
	uplink := cfg.Network.Uplink
	if uplink == "" {
		var err error
		if uplink, err = netns.DefaultUplink(); err != nil {
			return nil, err
		}
	}
	network, err := netns.NewManager(netns.Config{Uplink: uplink, Pool: cfg.Network.PoolPrefix()})
	if err != nil {
		return nil, fmt.Errorf("network: %w", err)
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
		Network:               network,
		NetworkBytesPerSecond: int64(cfg.Network.BandwidthMbps) * 1_000_000 / 8,
	}, logger)
	return sandbox.NewManager(sandbox.Config{
		MaxSandboxes: cfg.Sandbox.MaxSandboxes,
		UIDBase:      cfg.Host.UIDBase,
		ReapInterval: cfg.Sandbox.ReapInterval,
	}, booter, logger), nil
}

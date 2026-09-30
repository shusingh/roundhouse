// Package node assembles one host's sandbox machinery from configuration:
// networking, the Firecracker booter, admission, and the sandbox manager.
// roundhoused and the integration tests build the node the same way through
// it.
package node

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/shusingh/roundhouse/internal/admission"
	"github.com/shusingh/roundhouse/internal/config"
	"github.com/shusingh/roundhouse/internal/netns"
	"github.com/shusingh/roundhouse/internal/sandbox"
	"github.com/shusingh/roundhouse/internal/storage"
)

// Node is one host's sandbox machinery.
type Node struct {
	Manager *sandbox.Manager
	// Booter also builds templates.
	Booter *sandbox.FirecrackerBooter
	// Budget is the host capacity sandboxes reserve from.
	Budget *admission.Controller
	// TemplateUID is the host uid template builds run as: the one after the
	// sandboxes' range, so a build never shares a uid with a live sandbox.
	TemplateUID int
}

// New builds the node. Call Manager.Prepare before serving.
func New(cfg config.Config, logger *slog.Logger) (*Node, error) {
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
	budget, err := hostBudget(cfg)
	if err != nil {
		return nil, err
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
	pools := make([]sandbox.PoolConfig, len(cfg.Sandbox.Pools))
	for i, p := range cfg.Sandbox.Pools {
		pools[i] = sandbox.PoolConfig{Image: p.Image, VCPUs: p.VCPUs, MemoryMiB: p.MemoryMiB, Size: p.Size}
	}
	manager := sandbox.NewManager(sandbox.Config{
		MaxSandboxes:   cfg.Sandbox.MaxSandboxes,
		UIDBase:        cfg.Host.UIDBase,
		ReapInterval:   cfg.Sandbox.ReapInterval,
		Admission:      budget,
		VMMOverheadMiB: cfg.Host.VMMOverheadMiB,
		Pools:          pools,
		PoolMaxAge:     cfg.Sandbox.PoolMaxAge,
	}, booter, logger)
	return &Node{
		Manager:     manager,
		Booter:      booter,
		Budget:      budget,
		TemplateUID: cfg.Host.UIDBase + cfg.Sandbox.MaxSandboxes,
	}, nil
}

// hostBudget returns the admission budget, deriving what the configuration
// leaves at zero from the host.
func hostBudget(cfg config.Config) (*admission.Controller, error) {
	memory := cfg.Host.MemoryBudgetMiB
	if memory == 0 {
		total, err := memTotalMiB("/proc/meminfo")
		if err != nil {
			return nil, err
		}
		memory = total * 3 / 4
	}
	vcpus := cfg.Host.VCPUBudget
	if vcpus == 0 {
		vcpus = 2 * runtime.NumCPU()
	}
	return admission.New(admission.Budget{MemoryMiB: memory, VCPUs: vcpus, Sandboxes: cfg.Sandbox.MaxSandboxes})
}

// memTotalMiB reads MemTotal from a meminfo file.
func memTotalMiB(meminfo string) (int, error) {
	f, err := os.Open(meminfo) //nolint:gosec // G304: a fixed /proc path, or a test file
	if err != nil {
		return 0, fmt.Errorf("read host memory: %w", err)
	}
	defer func() { _ = f.Close() }()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) == 3 && fields[0] == "MemTotal:" && fields[2] == "kB" {
			kib, err := strconv.Atoi(fields[1])
			if err != nil {
				return 0, fmt.Errorf("read host memory: %w", err)
			}
			return kib / 1024, nil
		}
	}
	return 0, fmt.Errorf("read host memory: no MemTotal in %s", meminfo)
}

// Package config loads and validates roundhoused's configuration: YAML with
// unknown fields rejected, defaults applied, then one validation pass whose
// errors name the field and the fix.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config is roundhoused's configuration.
type Config struct {
	Server   Server   `yaml:"server"`
	Host     Host     `yaml:"host"`
	Sandbox  Sandbox  `yaml:"sandbox"`
	Network  Network  `yaml:"network"`
	APIKeys  []APIKey `yaml:"api_keys"`
	LogLevel string   `yaml:"log_level"`
}

// Server is the API listener.
type Server struct {
	Listen          string        `yaml:"listen"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// Host locates binaries and state on this machine.
type Host struct {
	DataDir         string `yaml:"data_dir"`
	FirecrackerPath string `yaml:"firecracker_path"`
	JailerPath      string `yaml:"jailer_path"`
	CgroupRoot      string `yaml:"cgroup_root"`
	CgroupParent    string `yaml:"cgroup_parent"`
	// UIDBase is the first of max_sandboxes host uids given to VMMs; the
	// range must not be used by anything else on the host.
	UIDBase int `yaml:"uid_base"`
	// VMMOverheadMiB is added to guest memory for each jail's memory limit.
	VMMOverheadMiB int `yaml:"vmm_overhead_mib"`
}

// Sandbox sets sandbox capacity, defaults, and maxima.
type Sandbox struct {
	Image        string        `yaml:"image"`
	MaxSandboxes int           `yaml:"max_sandboxes"`
	BootTimeout  time.Duration `yaml:"boot_timeout"`
	ReapInterval time.Duration `yaml:"reap_interval"`
	PidsMax      int           `yaml:"pids_max"`
	Defaults     Limits        `yaml:"defaults"`
	Max          Limits        `yaml:"max"`
}

// Limits are per-sandbox resource settings.
type Limits struct {
	VCPUs       int           `yaml:"vcpus"`
	MemoryMiB   int           `yaml:"memory_mib"`
	Lifetime    time.Duration `yaml:"lifetime"`
	IdleTimeout time.Duration `yaml:"idle_timeout"`
	ExecTimeout time.Duration `yaml:"exec_timeout"`
}

// Network configures sandbox networking (docs/engineering/design.md section 8).
type Network struct {
	// Uplink is the host interface egress leaves by; empty means the
	// interface of the default route.
	Uplink string `yaml:"uplink"`
	// Pool is the host-side range split into one /30 per sandbox with egress.
	Pool string `yaml:"pool"`
	// BandwidthMbps limits each direction of every sandbox's network device.
	BandwidthMbps int `yaml:"bandwidth_mbps"`
}

// PoolPrefix returns the parsed address pool.
func (n Network) PoolPrefix() netip.Prefix {
	p, _ := netip.ParsePrefix(n.Pool)
	return p
}

// APIKey authorizes one caller. Only the key's SHA-256 digest is stored, so a
// leaked configuration file does not leak usable keys.
type APIKey struct {
	// Name identifies the caller; it becomes the owner of its sandboxes.
	Name string `yaml:"name"`
	// SHA256 is the hex SHA-256 digest of the key.
	SHA256 string `yaml:"sha256"`
}

// Digest returns the SHA-256 digest of a presented key, for comparison with
// APIKey.SHA256.
func Digest(key string) [sha256.Size]byte {
	return sha256.Sum256([]byte(key))
}

// Load reads, defaults, and validates a configuration file.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the operator's own config path
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return Parse(data)
}

// Parse decodes, defaults, and validates configuration YAML.
func Parse(data []byte) (Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c *Config) applyDefaults() {
	setDefault(&c.Server.Listen, "127.0.0.1:8480")
	setDefault(&c.Server.ShutdownTimeout, 30*time.Second)
	setDefault(&c.Host.DataDir, "/var/lib/roundhouse")
	setDefault(&c.Host.FirecrackerPath, "/usr/local/bin/firecracker")
	setDefault(&c.Host.JailerPath, "/usr/local/bin/jailer")
	setDefault(&c.Host.CgroupRoot, "/sys/fs/cgroup")
	setDefault(&c.Host.CgroupParent, "roundhouse")
	setDefault(&c.Host.UIDBase, 200000)
	setDefault(&c.Host.VMMOverheadMiB, 64)
	setDefault(&c.Sandbox.Image, "default")
	setDefault(&c.Sandbox.MaxSandboxes, 64)
	setDefault(&c.Sandbox.BootTimeout, 10*time.Second)
	setDefault(&c.Sandbox.ReapInterval, time.Second)
	setDefault(&c.Sandbox.PidsMax, 64)
	setDefault(&c.Sandbox.Defaults.VCPUs, 1)
	setDefault(&c.Sandbox.Defaults.MemoryMiB, 512)
	setDefault(&c.Sandbox.Defaults.Lifetime, time.Hour)
	setDefault(&c.Sandbox.Defaults.IdleTimeout, 10*time.Minute)
	setDefault(&c.Sandbox.Defaults.ExecTimeout, time.Minute)
	setDefault(&c.Sandbox.Max.VCPUs, 4)
	setDefault(&c.Sandbox.Max.MemoryMiB, 4096)
	setDefault(&c.Sandbox.Max.Lifetime, 24*time.Hour)
	setDefault(&c.Sandbox.Max.IdleTimeout, 2*time.Hour)
	setDefault(&c.Sandbox.Max.ExecTimeout, time.Hour)
	setDefault(&c.Network.Pool, "10.200.0.0/16")
	setDefault(&c.Network.BandwidthMbps, 100)
	setDefault(&c.LogLevel, "info")
}

func setDefault[T comparable](field *T, value T) {
	var zero T
	if *field == zero {
		*field = value
	}
}

// Validate reports every problem with the configuration.
func (c *Config) Validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}
	for field, p := range map[string]string{
		"host.data_dir": c.Host.DataDir, "host.firecracker_path": c.Host.FirecrackerPath,
		"host.jailer_path": c.Host.JailerPath, "host.cgroup_root": c.Host.CgroupRoot,
	} {
		check(path.IsAbs(p), "%s %q: must be an absolute path", field, p)
	}
	check(c.Host.UIDBase >= 1000, "host.uid_base %d: must be 1000 or more (an unused uid range)", c.Host.UIDBase)
	check(c.Host.VMMOverheadMiB >= 16, "host.vmm_overhead_mib %d: must be at least 16", c.Host.VMMOverheadMiB)
	check(c.Sandbox.MaxSandboxes > 0, "sandbox.max_sandboxes must be positive")
	check(c.Sandbox.PidsMax >= 16, "sandbox.pids_max %d: the VMM needs at least 16", c.Sandbox.PidsMax)
	d, m := c.Sandbox.Defaults, c.Sandbox.Max
	check(d.VCPUs <= m.VCPUs && d.MemoryMiB <= m.MemoryMiB && d.Lifetime <= m.Lifetime &&
		d.IdleTimeout <= m.IdleTimeout && d.ExecTimeout <= m.ExecTimeout,
		"sandbox.defaults must not exceed sandbox.max")
	pool, err := netip.ParsePrefix(c.Network.Pool)
	check(err == nil && pool.Addr().Is4() && pool.Bits() <= 28 && pool == pool.Masked(),
		"network.pool %q: want a masked IPv4 prefix of /28 or larger, such as 10.200.0.0/16", c.Network.Pool)
	check(c.Network.BandwidthMbps > 0, "network.bandwidth_mbps must be positive")
	check(len(c.APIKeys) > 0, "api_keys: at least one key is required")
	names := map[string]bool{}
	for i, k := range c.APIKeys {
		check(k.Name != "" && !names[k.Name], "api_keys[%d]: name must be present and unique", i)
		names[k.Name] = true
		raw, err := hex.DecodeString(k.SHA256)
		check(err == nil && len(raw) == sha256.Size,
			"api_keys[%d].sha256: want 64 hex digits (echo -n KEY | sha256sum)", i)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		check(false, "log_level %q: want debug, info, warn, or error", c.LogLevel)
	}
	return errors.Join(errs...)
}

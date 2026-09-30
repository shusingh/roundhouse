// Package netns gives each sandbox its own network namespace and enforces its
// egress policy. See docs/engineering/design.md section 8 and
// docs/adr/0005-network-namespace-per-sandbox.md.
//
// Every sandbox namespace holds a tap device, tap0, with the same addresses,
// so VMs restored from one snapshot need no reconfiguration. A sandbox that
// may reach the internet also gets a veth pair to the host with its own /30;
// traffic is masqueraded twice (in the namespace, then on the host uplink),
// and host nftables rules drop anything bound for the host, other sandboxes,
// private or link-local ranges, cloud metadata, or IPv6.
//
// Host changes go through the ip and nft commands, built from fixed argument
// lists with validated values and never through a shell. Together with
// package vmm this is the only privileged code in Roundhouse.
package netns

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Fixed addressing inside every sandbox namespace.
const (
	TapName    = "tap0"
	TapAddr    = "172.16.0.1"
	GuestAddr  = "172.16.0.2"
	GuestMAC   = "06:00:ac:10:00:02"
	nsVethName = "veth0"
	tableName  = "roundhouse"
	// NamePrefix marks every namespace and host interface Roundhouse creates.
	NamePrefix = "rh-"
)

// GuestKernelArgs configures the guest's eth0 at boot through the kernel's IP
// autoconfiguration (CONFIG_IP_PNP), and turns off IPv6, which sandboxes never
// get.
func GuestKernelArgs() string {
	return "ip=" + GuestAddr + "::" + TapAddr + ":255.255.255.252::eth0:off ipv6.disable=1"
}

// Mode is an egress policy.
type Mode string

// Egress modes.
const (
	// ModeNone gives the sandbox no route out: no veth pair exists.
	ModeNone Mode = "none"
	// ModeInternet allows public destinations.
	ModeInternet Mode = "internet"
	// ModeAllowlist allows only listed public destinations.
	ModeAllowlist Mode = "allowlist"
)

// Egress is a sandbox's egress policy.
type Egress struct {
	Mode Mode
	// Allow lists destinations for ModeAllowlist.
	Allow []Destination
}

// Destination is an allowed prefix and, optionally, one TCP or UDP port.
type Destination struct {
	Prefix netip.Prefix
	Port   uint16 // 0 means any port
}

// ParseDestination parses "203.0.113.0/24", "198.51.100.7", or
// "198.51.100.7:443".
func ParseDestination(s string) (Destination, error) {
	host, portStr, hasPort := strings.Cut(s, ":")
	var d Destination
	if hasPort {
		port, err := strconv.ParseUint(portStr, 10, 16)
		if err != nil || port == 0 {
			return d, fmt.Errorf("destination %q: invalid port", s)
		}
		d.Port = uint16(port)
	}
	if strings.Contains(host, "/") {
		p, err := netip.ParsePrefix(host)
		if err != nil {
			return d, fmt.Errorf("destination %q: %w", s, err)
		}
		d.Prefix = p.Masked()
	} else {
		a, err := netip.ParseAddr(host)
		if err != nil {
			return d, fmt.Errorf("destination %q: %w", s, err)
		}
		d.Prefix = netip.PrefixFrom(a, a.BitLen())
	}
	if !d.Prefix.Addr().Is4() {
		return d, fmt.Errorf("destination %q: only IPv4 is supported", s)
	}
	for _, b := range blocked {
		if b.Overlaps(d.Prefix) {
			return d, fmt.Errorf("destination %q: overlaps blocked range %s", s, b)
		}
	}
	return d, nil
}

// blocked are destinations no sandbox may reach, whatever its policy.
var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), // link-local, including cloud metadata
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// Validate checks a policy.
func (e Egress) Validate() error {
	switch e.Mode {
	case ModeNone, ModeInternet:
		if len(e.Allow) > 0 {
			return fmt.Errorf("egress %s takes no allow list", e.Mode)
		}
	case ModeAllowlist:
		if len(e.Allow) == 0 || len(e.Allow) > 64 {
			return errors.New("egress allowlist needs 1 to 64 destinations")
		}
	default:
		return fmt.Errorf("egress mode %q: want none, internet, or allowlist", e.Mode)
	}
	return nil
}

// Runner runs a host command, optionally feeding it stdin. Tests replace it.
type Runner func(ctx context.Context, stdin string, name string, args ...string) error

// execRunner runs commands for real.
func execRunner(ctx context.Context, stdin, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: fixed programs, validated arguments
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Config configures the network manager.
type Config struct {
	// Uplink is the host interface sandbox traffic leaves by.
	Uplink string
	// Pool is the host-side range carved into one /30 per sandbox with egress.
	Pool netip.Prefix
}

// Manager creates and removes sandbox networks. It is safe for concurrent use.
type Manager struct {
	cfg Config
	run Runner

	mu    sync.Mutex
	slots map[string]int // guarded by mu; sandbox ID to /30 index
	used  []bool         // guarded by mu
}

var validID = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// NewManager returns a manager. Call Setup before creating networks.
func NewManager(cfg Config) (*Manager, error) {
	if !cfg.Pool.Addr().Is4() || cfg.Pool.Bits() > 28 || cfg.Pool != cfg.Pool.Masked() {
		return nil, fmt.Errorf("address pool %s: want an IPv4 prefix of /28 or larger", cfg.Pool)
	}
	if !validInterface.MatchString(cfg.Uplink) {
		return nil, fmt.Errorf("uplink %q: invalid interface name", cfg.Uplink)
	}
	size := 1 << (32 - cfg.Pool.Bits() - 2)
	return &Manager{cfg: cfg, run: execRunner, slots: map[string]int{}, used: make([]bool, size)}, nil
}

var validInterface = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,15}$`)

// NamespaceName is the network namespace for a sandbox.
func NamespaceName(sandboxID string) string { return NamePrefix + sandboxID }

// NamespacePath is where the namespace is mounted, for the jailer's --netns.
func NamespacePath(sandboxID string) string { return "/var/run/netns/" + NamespaceName(sandboxID) }

// hostVethName is the host end of a sandbox's veth pair. Interface names are
// at most 15 bytes; "rh-" plus the ID's final 12 characters fits, and the
// sandbox ID's random suffix makes it unique.
func hostVethName(sandboxID string) string {
	return NamePrefix + sandboxID[max(0, len(sandboxID)-12):]
}

// slotAddrs returns the host and namespace addresses of /30 number slot.
func (m *Manager) slotAddrs(slot int) (host, ns netip.Addr) {
	base := m.cfg.Pool.Addr().As4()
	n := binary.BigEndian.Uint32(base[:])
	n += uint32(slot) * 4 //nolint:gosec // G115: slot is bounded by the pool size
	return addrFrom(n + 1), addrFrom(n + 2)
}

func addrFrom(n uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], n)
	return netip.AddrFrom4(b)
}

// Setup installs the host's nftables table, replacing any previous one. It is
// safe to call on every start.
func (m *Manager) Setup(ctx context.Context) error {
	_ = m.run(ctx, "", "nft", "delete", "table", "inet", tableName) // absent on a fresh host
	if err := m.run(ctx, hostRuleset(m.cfg), "nft", "-f", "-"); err != nil {
		return fmt.Errorf("install host firewall: %w", err)
	}
	return nil
}

// Create builds a sandbox's namespace and, for egress, its veth pair and
// rules. uid owns the tap device, so the jailed VMM can attach to it. On
// failure everything created is removed.
func (m *Manager) Create(ctx context.Context, sandboxID string, uid int, egress Egress) (err error) {
	if !validID.MatchString(sandboxID) {
		return fmt.Errorf("sandbox id %q: invalid", sandboxID)
	}
	if err := egress.Validate(); err != nil {
		return err
	}
	ns := NamespaceName(sandboxID)
	defer func() {
		if err != nil {
			err = errors.Join(err, m.Destroy(context.WithoutCancel(ctx), sandboxID))
		}
	}()
	if err := m.run(ctx, "", "ip", "netns", "add", ns); err != nil {
		return err
	}
	uidStr := strconv.Itoa(uid)
	if err := m.run(ctx, batch(
		"link set lo up",
		"tuntap add dev "+TapName+" mode tap user "+uidStr+" group "+uidStr,
		"addr add "+TapAddr+"/30 dev "+TapName,
		"link set "+TapName+" up",
	), "ip", "-n", ns, "-batch", "-"); err != nil {
		return err
	}
	return m.applyEgress(ctx, sandboxID, egress)
}

// ApplyEgress gives a sandbox whose network was created with ModeNone the
// policy egress. The warm pool uses it: pooled sandboxes start with no
// egress, and a claim applies the policy the caller asked for. On failure
// the sandbox's network is in an unknown state and must be destroyed.
func (m *Manager) ApplyEgress(ctx context.Context, sandboxID string, egress Egress) error {
	if !validID.MatchString(sandboxID) {
		return fmt.Errorf("sandbox id %q: invalid", sandboxID)
	}
	if err := egress.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	_, attached := m.slots[sandboxID]
	m.mu.Unlock()
	if attached {
		return fmt.Errorf("sandbox %s already has egress", sandboxID)
	}
	return m.applyEgress(ctx, sandboxID, egress)
}

// applyEgress adds the veth pair, routes, and rules for a policy other than
// ModeNone to an existing namespace.
func (m *Manager) applyEgress(ctx context.Context, sandboxID string, egress Egress) error {
	if egress.Mode == ModeNone {
		return nil
	}
	ns := NamespaceName(sandboxID)
	slot, err := m.allocate(sandboxID)
	if err != nil {
		return err
	}
	hostAddr, nsAddr := m.slotAddrs(slot)
	veth := hostVethName(sandboxID)
	if err := m.run(ctx, batch(
		"link add "+veth+" type veth peer name "+nsVethName+" netns "+ns,
		"addr add "+hostAddr.String()+"/30 dev "+veth,
		"link set "+veth+" up",
	), "ip", "-batch", "-"); err != nil {
		return err
	}
	if err := m.run(ctx, batch(
		"addr add "+nsAddr.String()+"/30 dev "+nsVethName,
		"link set "+nsVethName+" up",
		"route add default via "+hostAddr.String(),
	), "ip", "-n", ns, "-batch", "-"); err != nil {
		return err
	}
	if err := m.run(ctx, "", "ip", "netns", "exec", ns, "sysctl", "-qw", "net.ipv4.ip_forward=1"); err != nil {
		return err
	}
	if err := m.run(ctx, namespaceRuleset(), "ip", "netns", "exec", ns, "nft", "-f", "-"); err != nil {
		return fmt.Errorf("install namespace NAT: %w", err)
	}
	if egress.Mode == ModeAllowlist {
		if err := m.run(ctx, allowlistRules(veth, egress.Allow), "nft", "-f", "-"); err != nil {
			return fmt.Errorf("install allowlist: %w", err)
		}
	}
	return nil
}

// Destroy removes a sandbox's namespace, veth pair, and rules. It is
// idempotent, and every step runs even if an earlier one fails.
func (m *Manager) Destroy(ctx context.Context, sandboxID string) error {
	var errs []error
	veth := hostVethName(sandboxID)
	// Remove the allowlist chain first: while it exists the forward chain
	// jumps to it, so it cannot be deleted before the jump rule.
	if err := m.run(ctx, removeAllowlist(veth), "nft", "-f", "-"); err != nil && !isMissing(err) {
		errs = append(errs, err)
	}
	// Deleting the namespace destroys tap0 and the namespace end of the veth
	// pair, which takes the host end with it.
	if err := m.run(ctx, "", "ip", "netns", "delete", NamespaceName(sandboxID)); err != nil && !isMissing(err) {
		errs = append(errs, err)
	}
	m.release(sandboxID)
	return errors.Join(errs...)
}

// Reconcile removes namespaces with the Roundhouse prefix whose sandbox is not
// in live. Run at startup, before Setup rebuilds the host table.
func (m *Manager) Reconcile(ctx context.Context, namespaces []string, live map[string]bool) error {
	var errs []error
	for _, ns := range namespaces {
		id, ok := strings.CutPrefix(ns, NamePrefix)
		if !ok || live[id] || !validID.MatchString(id) {
			continue
		}
		if err := m.Destroy(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) allocate(sandboxID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, used := range m.used {
		if !used {
			m.used[i] = true
			m.slots[sandboxID] = i
			return i, nil
		}
	}
	return 0, errors.New("sandbox address pool exhausted")
}

func (m *Manager) release(sandboxID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if slot, ok := m.slots[sandboxID]; ok {
		m.used[slot] = false
		delete(m.slots, sandboxID)
	}
}

// isMissing reports whether a command failed only because the object did not
// exist, which makes teardown idempotent.
func isMissing(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "No such file or directory") || strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "Cannot find device")
}

func batch(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

package netns

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
)

type recordedCmd struct {
	stdin string
	argv  string
}

// fakeRunner records commands and fails those whose argv contains failOn.
type fakeRunner struct {
	mu     sync.Mutex
	cmds   []recordedCmd
	failOn string
}

func (f *fakeRunner) run(_ context.Context, stdin, name string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	argv := name + " " + strings.Join(args, " ")
	f.cmds = append(f.cmds, recordedCmd{stdin: stdin, argv: argv})
	if f.failOn != "" && (strings.Contains(argv, f.failOn) || strings.Contains(stdin, f.failOn)) {
		return errors.New("simulated failure")
	}
	return nil
}

func (f *fakeRunner) all() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, c := range f.cmds {
		b.WriteString(c.argv + "\n" + c.stdin)
	}
	return b.String()
}

func newTestManager(t *testing.T) (*Manager, *fakeRunner) {
	t.Helper()
	m, err := NewManager(Config{Uplink: "eth0", Pool: netip.MustParsePrefix("10.200.0.0/24")})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{}
	m.run = f.run
	return m, f
}

func TestParseDestination(t *testing.T) {
	t.Parallel()
	ok := map[string]Destination{
		"203.0.113.0/24":   {Prefix: netip.MustParsePrefix("203.0.113.0/24")},
		"203.0.113.9/24":   {Prefix: netip.MustParsePrefix("203.0.113.0/24")}, // masked
		"198.51.100.7":     {Prefix: netip.MustParsePrefix("198.51.100.7/32")},
		"198.51.100.7:443": {Prefix: netip.MustParsePrefix("198.51.100.7/32"), Port: 443},
	}
	for in, want := range ok {
		got, err := ParseDestination(in)
		if err != nil || got != want {
			t.Errorf("ParseDestination(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"10.1.2.3",          // private
		"169.254.169.254",   // cloud metadata
		"192.168.0.0/16",    // private
		"0.0.0.0/0",         // overlaps everything blocked
		"2001:db8::1",       // IPv6
		"198.51.100.7:0",    // port 0
		"198.51.100.7:http", // named port
		"not-an-address",
	} {
		if _, err := ParseDestination(bad); err == nil {
			t.Errorf("ParseDestination(%q) accepted a destination it must refuse", bad)
		}
	}
}

func TestSlotAddresses(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)
	for slot, want := range map[int][2]string{0: {"10.200.0.1", "10.200.0.2"}, 1: {"10.200.0.5", "10.200.0.6"}, 63: {"10.200.0.253", "10.200.0.254"}} {
		host, ns := m.slotAddrs(slot)
		if host.String() != want[0] || ns.String() != want[1] {
			t.Errorf("slot %d = %s, %s; want %s, %s", slot, host, ns, want[0], want[1])
		}
	}
	if len(m.used) != 64 {
		t.Errorf("a /24 pool has %d slots, want 64", len(m.used))
	}
}

func TestCreateNoneHasNoVeth(t *testing.T) {
	t.Parallel()
	m, f := newTestManager(t)
	if err := m.Create(context.Background(), "sb-000000000001", 200001, Egress{Mode: ModeNone}); err != nil {
		t.Fatal(err)
	}
	out := f.all()
	for _, want := range []string{"ip netns add rh-sb-000000000001", "tuntap add dev tap0 mode tap user 200001 group 200001", "addr add 172.16.0.1/30 dev tap0"} {
		if !strings.Contains(out, want) {
			t.Errorf("commands lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "veth") || strings.Contains(out, "masquerade") {
		t.Errorf("egress none created a route out:\n%s", out)
	}
}

func TestCreateInternetAndTeardown(t *testing.T) {
	t.Parallel()
	m, f := newTestManager(t)
	ctx := context.Background()
	if err := m.Create(ctx, "sb-000000000002", 200002, Egress{Mode: ModeInternet}); err != nil {
		t.Fatal(err)
	}
	out := f.all()
	for _, want := range []string{
		"link add rh-000000000002 type veth peer name veth0 netns rh-sb-000000000002",
		"addr add 10.200.0.1/30 dev rh-000000000002",
		"route add default via 10.200.0.1",
		"net.ipv4.ip_forward=1",
		`oifname "veth0" masquerade`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("commands lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "allowlists {") {
		t.Error("internet mode installed an allowlist")
	}

	// The slot is reused after teardown.
	if err := m.Destroy(ctx, "sb-000000000002"); err != nil {
		t.Fatal(err)
	}
	if err := m.Create(ctx, "sb-000000000003", 200003, Egress{Mode: ModeInternet}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.all(), "addr add 10.200.0.1/30 dev rh-000000000003") {
		t.Error("a destroyed sandbox's /30 was not reused")
	}
}

func TestCreateAllowlist(t *testing.T) {
	t.Parallel()
	m, f := newTestManager(t)
	dest, _ := ParseDestination("198.51.100.7:443")
	if err := m.Create(context.Background(), "sb-000000000004", 200004, Egress{Mode: ModeAllowlist, Allow: []Destination{dest}}); err != nil {
		t.Fatal(err)
	}
	out := f.all()
	for _, want := range []string{
		"add chain inet roundhouse allow-rh-000000000004",
		"ip daddr { 1.1.1.1, 8.8.8.8 } th dport 53 accept",
		"ip daddr 198.51.100.7/32 th dport 443 accept",
		"add rule inet roundhouse allow-rh-000000000004 drop",
		`add element inet roundhouse allowlists { "rh-000000000004" : jump allow-rh-000000000004 }`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("allowlist lacks %q:\n%s", want, out)
		}
	}
}

func TestCreateFailureCleansUp(t *testing.T) {
	t.Parallel()
	m, f := newTestManager(t)
	f.failOn = "route add default"
	err := m.Create(context.Background(), "sb-000000000005", 200005, Egress{Mode: ModeInternet})
	if err == nil {
		t.Fatal("Create succeeded although a step failed")
	}
	if !strings.Contains(f.all(), "ip netns delete rh-sb-000000000005") {
		t.Error("a failed Create did not remove its namespace")
	}
	f.failOn = ""
	// The slot was released, so the next sandbox gets the first /30 again.
	if err := m.Create(context.Background(), "sb-000000000006", 200006, Egress{Mode: ModeInternet}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.all(), "addr add 10.200.0.1/30 dev rh-000000000006") {
		t.Error("a failed Create leaked its /30")
	}
}

func TestHostRuleset(t *testing.T) {
	t.Parallel()
	rules := hostRuleset(Config{Uplink: "eth0", Pool: netip.MustParsePrefix("10.200.0.0/16")})
	// Drops come before the allowlist map and the final accept.
	order := []string{
		`iifname "rh-*" meta nfproto ipv6 drop`,
		`iifname "rh-*" oifname != "eth0" drop`,
		`iifname "rh-*" ip daddr @blocked drop`,
		`iifname vmap @allowlists`,
		`iifname "rh-*" accept`,
	}
	last := -1
	for _, rule := range order {
		i := strings.Index(rules, rule)
		if i < 0 || i < last {
			t.Fatalf("forward rule %q missing or out of order:\n%s", rule, rules)
		}
		last = i
	}
	for _, want := range []string{"169.254.0.0/16", "10.0.0.0/8", `iifname "rh-*" drop`, `ip saddr 10.200.0.0/16 oifname "eth0" masquerade`} {
		if !strings.Contains(rules, want) {
			t.Errorf("host ruleset lacks %q", want)
		}
	}
}

func TestValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewManager(Config{Uplink: "eth0; rm -rf", Pool: netip.MustParsePrefix("10.200.0.0/16")}); err == nil {
		t.Error("NewManager accepted a malicious uplink name")
	}
	if _, err := NewManager(Config{Uplink: "eth0", Pool: netip.MustParsePrefix("10.200.0.0/30")}); err == nil {
		t.Error("NewManager accepted a pool too small to split")
	}
	m, _ := newTestManager(t)
	if err := m.Create(context.Background(), "../etc", 1, Egress{Mode: ModeNone}); err == nil {
		t.Error("Create accepted an invalid sandbox id")
	}
	if err := m.Create(context.Background(), "sb-1", 1, Egress{Mode: "bridge"}); err == nil {
		t.Error("Create accepted an unknown egress mode")
	}
}

func TestApplyEgressUpgradesAPooledNamespaceOnce(t *testing.T) {
	t.Parallel()
	m, f := newTestManager(t)
	ctx := context.Background()
	if err := m.Create(ctx, "sb-pooled", 100_000, Egress{Mode: ModeNone}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.all(), "veth") {
		t.Fatal("a pooled namespace got a veth pair before any claim")
	}
	if err := m.ApplyEgress(ctx, "sb-pooled", Egress{Mode: ModeInternet}); err != nil {
		t.Fatalf("ApplyEgress: %v", err)
	}
	if got := f.all(); !strings.Contains(got, "type veth peer name veth0 netns rh-sb-pooled") || !strings.Contains(got, "masquerade") {
		t.Errorf("ApplyEgress did not attach the namespace:\n%s", got)
	}
	if err := m.ApplyEgress(ctx, "sb-pooled", Egress{Mode: ModeInternet}); err == nil {
		t.Error("a second ApplyEgress succeeded; it would allocate another /30")
	}
	if err := m.ApplyEgress(ctx, "sb-other", Egress{Mode: "bogus"}); err == nil {
		t.Error("ApplyEgress accepted an invalid policy")
	}
}

//go:build integration

package integration

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shusingh/roundhouse/internal/apiclient"
	"github.com/shusingh/roundhouse/internal/netns"
)

// probeScript tries a TCP connection from inside the guest and prints ok or
// fail; with a host name it also exercises DNS.
const probeScript = `import socket, sys
s = socket.socket()
s.settimeout(2)
try:
    s.connect((sys.argv[1], int(sys.argv[2])))
    print("ok")
except Exception:
    print("fail")
`

func probe(t *testing.T, ctx context.Context, c *apiclient.Client, id, host string, port int) string {
	t.Helper()
	out, exit := execOut(t, ctx, c, id, "python3", "-c", probeScript, host, strconv.Itoa(port))
	if exit.ExitCode != 0 {
		t.Fatalf("probe exited %d", exit.ExitCode)
	}
	return strings.TrimSpace(out)
}

// requireInternet skips when the host itself cannot reach the internet.
func requireInternet(t *testing.T) {
	t.Helper()
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(context.Background(), "tcp", "1.1.1.1:443")
	if err != nil {
		t.Skip("host has no internet access")
	}
	_ = conn.Close()
}

// hostListener accepts on every host address and counts connections, so a
// test can prove the guest never reached the host.
func hostListener(t *testing.T) (port int, accepted *atomic.Int32) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted = &atomic.Int32{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = conn.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, accepted
}

func uplinkAddr(t *testing.T) string {
	t.Helper()
	name, err := netns.DefaultUplink()
	if err != nil {
		t.Fatal(err)
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		t.Fatal(err)
	}
	addrs, _ := iface.Addrs()
	for _, a := range addrs {
		if ip, ok := a.(*net.IPNet); ok && ip.IP.To4() != nil {
			return ip.IP.String()
		}
	}
	t.Fatalf("uplink %s has no IPv4 address", name)
	return ""
}

func TestEgressNone(t *testing.T) {
	s := startStack(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sb, err := s.Client.Create(ctx, apiclient.CreateOptions{MemoryMiB: 256})
	if err != nil {
		t.Fatal(err)
	}
	if sb.Egress != "none" {
		t.Errorf("default egress = %q, want none", sb.Egress)
	}
	for _, dst := range []struct {
		host string
		port int
	}{{"1.1.1.1", 443}, {"example.com", 443}, {uplinkAddr(t), 22}} {
		if got := probe(t, ctx, s.Client, sb.ID, dst.host, dst.port); got != "fail" {
			t.Errorf("egress none reached %s:%d", dst.host, dst.port)
		}
	}
}

func TestEgressInternetBlocksHostAndPrivateRanges(t *testing.T) {
	requireInternet(t)
	s := startStack(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sb, err := s.Client.Create(ctx, apiclient.CreateOptions{MemoryMiB: 256, Egress: &apiclient.Egress{Mode: "internet"}})
	if err != nil {
		t.Fatal(err)
	}
	neighbor, err := s.Client.Create(ctx, apiclient.CreateOptions{MemoryMiB: 256, Egress: &apiclient.Egress{Mode: "internet"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = neighbor

	if got := probe(t, ctx, s.Client, sb.ID, "1.1.1.1", 443); got != "ok" {
		t.Errorf("internet egress could not reach 1.1.1.1:443")
	}
	if got := probe(t, ctx, s.Client, sb.ID, "example.com", 443); got != "ok" {
		t.Errorf("internet egress could not resolve and reach example.com:443")
	}

	port, accepted := hostListener(t)
	for _, dst := range []struct {
		name, host string
		port       int
	}{
		{"host uplink address", uplinkAddr(t), port},
		{"host side of the sandbox link", "10.200.0.1", port},
		{"neighbor sandbox link", "10.200.0.6", port},
		{"cloud metadata", "169.254.169.254", 80},
		{"private range", "192.168.1.1", 80},
	} {
		if got := probe(t, ctx, s.Client, sb.ID, dst.host, dst.port); got != "fail" {
			t.Errorf("internet egress reached the %s (%s:%d)", dst.name, dst.host, dst.port)
		}
	}
	if n := accepted.Load(); n != 0 {
		t.Errorf("the host listener accepted %d connections from the sandbox", n)
	}
}

func TestEgressAllowlist(t *testing.T) {
	requireInternet(t)
	s := startStack(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sb, err := s.Client.Create(ctx, apiclient.CreateOptions{
		MemoryMiB: 256,
		Egress:    &apiclient.Egress{Mode: "allowlist", Allow: []string{"1.1.1.1:443"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := probe(t, ctx, s.Client, sb.ID, "1.1.1.1", 443); got != "ok" {
		t.Error("allowlisted 1.1.1.1:443 was unreachable")
	}
	for _, dst := range []struct {
		host string
		port int
	}{{"1.0.0.1", 443}, {"1.1.1.1", 80}, {"8.8.8.8", 443}} {
		if got := probe(t, ctx, s.Client, sb.ID, dst.host, dst.port); got != "fail" {
			t.Errorf("allowlist let through %s:%d", dst.host, dst.port)
		}
	}
}

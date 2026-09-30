package netns

import (
	"fmt"
	"strings"

	"github.com/shusingh/roundhouse/internal/agentproto"
)

// hostRuleset is the host's nftables table. Sandbox interfaces all carry the
// "rh-" prefix, so one set of wildcard rules covers every sandbox; per-sandbox
// allowlists hang off a verdict map keyed by interface name.
//
// Order matters in the forward chain: IPv6, anything not leaving by the
// uplink (other sandboxes, the host's other networks), and blocked ranges are
// dropped before an allowlist is consulted.
func hostRuleset(cfg Config) string {
	var blockedList []string
	for _, p := range blocked {
		blockedList = append(blockedList, p.String())
	}
	return fmt.Sprintf(`table inet %[1]s {
	set blocked {
		type ipv4_addr
		flags interval
		elements = { %[2]s }
	}
	map allowlists {
		type ifname : verdict
	}
	chain input {
		type filter hook input priority filter; policy accept;
		iifname "%[3]s*" ct state established,related accept
		iifname "%[3]s*" drop
	}
	chain forward {
		type filter hook forward priority filter; policy accept;
		iifname "%[3]s*" meta nfproto ipv6 drop
		iifname "%[3]s*" oifname != "%[4]s" drop
		iifname "%[3]s*" ip daddr @blocked drop
		iifname vmap @allowlists
		iifname "%[3]s*" accept
		oifname "%[3]s*" ct state established,related accept
		oifname "%[3]s*" drop
	}
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr %[5]s oifname "%[4]s" masquerade
	}
}
`, tableName, strings.Join(blockedList, ", "), NamePrefix, cfg.Uplink, cfg.Pool)
}

// namespaceRuleset masquerades the guest's traffic behind the namespace's
// veth address, so every sandbox's identical guest address leaves the
// namespace as a unique one.
func namespaceRuleset() string {
	return `table ip roundhouse_ns {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		oifname "` + nsVethName + `" masquerade
	}
}
`
}

func allowlistChain(veth string) string { return "allow-" + veth }

// allowlistRules installs a sandbox's allowlist chain and its map entry in one
// atomic nft transaction.
func allowlistRules(veth string, allow []Destination) string {
	chain := allowlistChain(veth)
	var b strings.Builder
	fmt.Fprintf(&b, "add chain inet %s %s\n", tableName, chain)
	fmt.Fprintf(&b, "add rule inet %s %s ip daddr { %s } th dport 53 accept\n", tableName, chain, strings.Join(agentproto.DNSResolvers, ", "))
	for _, d := range allow {
		if d.Port != 0 {
			fmt.Fprintf(&b, "add rule inet %s %s ip daddr %s th dport %d accept\n", tableName, chain, d.Prefix, d.Port)
		} else {
			fmt.Fprintf(&b, "add rule inet %s %s ip daddr %s accept\n", tableName, chain, d.Prefix)
		}
	}
	fmt.Fprintf(&b, "add rule inet %s %s drop\n", tableName, chain)
	fmt.Fprintf(&b, "add element inet %s allowlists { \"%s\" : jump %s }\n", tableName, veth, chain)
	return b.String()
}

// removeAllowlist deletes a sandbox's map entry, then its chain.
func removeAllowlist(veth string) string {
	return fmt.Sprintf("delete element inet %[1]s allowlists { \"%[2]s\" }\ndelete chain inet %[1]s %[3]s\n",
		tableName, veth, allowlistChain(veth))
}

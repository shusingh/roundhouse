package netns

import (
	"strconv"
	"strings"
	"testing"
)

// FuzzParseDestination checks that every accepted destination is a masked
// IPv4 prefix clear of the blocked ranges, and renders as exactly one rule
// of the expected form.
func FuzzParseDestination(f *testing.F) {
	for _, s := range []string{"203.0.113.0/24", "198.51.100.7:443", "10.0.0.1", "0.0.0.0/0", "1.1.1.1:0", "::1", "1.1.1.1/33", "1.2.3.4:65535"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := ParseDestination(s)
		if err != nil {
			return
		}
		if !d.Prefix.Addr().Is4() || d.Prefix != d.Prefix.Masked() {
			t.Fatalf("ParseDestination(%q) = %v, want a masked IPv4 prefix", s, d.Prefix)
		}
		for _, b := range blocked {
			if b.Overlaps(d.Prefix) {
				t.Fatalf("ParseDestination(%q) = %v overlaps blocked %v", s, d.Prefix, b)
			}
		}
		want := "ip daddr " + d.Prefix.String()
		if d.Port != 0 {
			want += " th dport " + strconv.Itoa(int(d.Port))
		}
		want += " accept"
		rules := allowlistRules("rh-000000000001", []Destination{d})
		if n := strings.Count(rules, "\n"); n != 5 || !strings.Contains(rules, want+"\n") {
			t.Fatalf("rules for %q (%d lines) lack exactly %q:\n%s", s, n, want, rules)
		}
	})
}

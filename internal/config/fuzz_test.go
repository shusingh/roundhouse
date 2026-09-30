package config

import "testing"

// FuzzParse checks that no input panics the parser and that everything it
// accepts passes validation again.
func FuzzParse(f *testing.F) {
	d := Digest("k")
	key := "api_keys:\n  - name: a\n    sha256: " + hexString(d[:]) + "\n"
	f.Add([]byte(key))
	f.Add([]byte(key + "sandbox:\n  pools:\n    - size: 2\n      memory_mib: 256\n"))
	f.Add([]byte(key + "network:\n  pool: 10.9.0.0/24\n  bandwidth_mbps: 8\n"))
	f.Add([]byte("api_keys: [\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := Parse(data)
		if err != nil {
			return
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Parse accepted a config that fails Validate: %v", err)
		}
	})
}

func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

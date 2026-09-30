package config

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func keyYAML(name, key string) string {
	d := Digest(key)
	return "  - name: " + name + "\n    sha256: " + hex.EncodeToString(d[:]) + "\n"
}

func TestParseAppliesDefaults(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte("api_keys:\n" + keyYAML("alice", "secret")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Server.Listen != "127.0.0.1:8480" || c.Sandbox.Defaults.MemoryMiB != 512 || c.Sandbox.Max.Lifetime != 24*time.Hour {
		t.Errorf("defaults not applied: %+v", c)
	}
	d := Digest("secret")
	if c.APIKeys[0].SHA256 != hex.EncodeToString(d[:]) {
		t.Error("key digest changed")
	}
}

func TestParseRejects(t *testing.T) {
	t.Parallel()
	valid := "api_keys:\n" + keyYAML("alice", "secret")
	tests := map[string]struct {
		yaml, wantErr string
	}{
		"unknown field":          {valid + "surprise: 1\n", "surprise"},
		"no keys":                {"log_level: info\n", "at least one key"},
		"bad digest":             {"api_keys:\n  - name: a\n    sha256: plaintext-key\n", "64 hex digits"},
		"duplicate names":        {"api_keys:\n" + keyYAML("a", "x") + keyYAML("a", "y"), "unique"},
		"relative data dir":      {valid + "host:\n  data_dir: var/lib\n", "absolute"},
		"low uid base":           {valid + "host:\n  uid_base: 10\n", "uid_base"},
		"defaults above max":     {valid + "sandbox:\n  defaults:\n    memory_mib: 8192\n", "must not exceed"},
		"unknown log level":      {valid + "log_level: loud\n", "log_level"},
		"malformed yaml":         {"api_keys: [\n", "decode config"},
		"tiny pids":              {valid + "sandbox:\n  pids_max: 2\n", "pids_max"},
		"negative max sandboxes": {valid + "sandbox:\n  max_sandboxes: -1\n", "max_sandboxes"},
		"empty pool":             {valid + "sandbox:\n  pools:\n    - size: 0\n", "size must be positive"},
		"oversized pools":        {valid + "sandbox:\n  max_sandboxes: 2\n  pools:\n    - size: 3\n", "more than max_sandboxes"},
		"duplicate pool shapes":  {valid + "sandbox:\n  pools:\n    - size: 1\n    - size: 2\n", "same image and shape"},
		"pool above max":         {valid + "sandbox:\n  pools:\n    - size: 1\n      vcpus: 8\n", "within sandbox.max"},
		"negative budget":        {valid + "host:\n  memory_budget_mib: -1\n", "must not be negative"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Parse error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestPoolsInheritSandboxDefaults(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte("api_keys:\n" + keyYAML("alice", "secret") + "sandbox:\n  pools:\n    - size: 4\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := Pool{Image: "default", VCPUs: 1, MemoryMiB: 512, Size: 4}
	if len(c.Sandbox.Pools) != 1 || c.Sandbox.Pools[0] != want {
		t.Errorf("pools = %+v, want [%+v]", c.Sandbox.Pools, want)
	}
}

func TestNegativeKeyQuotaIsRejected(t *testing.T) {
	t.Parallel()
	d := Digest("x")
	_, err := Parse([]byte("api_keys:\n  - name: a\n    max_sandboxes: -1\n    sha256: " + hex.EncodeToString(d[:]) + "\n"))
	if err == nil || !strings.Contains(err.Error(), "max_sandboxes must not be negative") {
		t.Errorf("Parse = %v, want a negative quota rejected", err)
	}
}

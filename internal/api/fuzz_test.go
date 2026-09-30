package api

import (
	"bytes"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shusingh/roundhouse/internal/config"
	"github.com/shusingh/roundhouse/internal/telemetry"
)

// FuzzCreateBody checks that no create body makes the API fail with a server
// error or reach the manager with limits beyond the configured maxima.
func FuzzCreateBody(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"vcpus":2,"memory_mib":1024,"lifetime_seconds":60,"idle_timeout_seconds":30}`))
	f.Add([]byte(`{"egress":{"mode":"allowlist","allow":["203.0.113.0/24","198.51.100.7:443"]}}`))
	f.Add([]byte(`{"egress":{"mode":"internet","allow":["1.1.1.1"]}}`))
	f.Add([]byte(`{"memory_mib":-5}`))
	f.Add([]byte(`[1,2,3]`))
	cfg, err := config.Parse([]byte("api_keys:\n  - name: alice\n    sha256: " + digestHex(aliceKey) + "\n"))
	if err != nil {
		f.Fatal(err)
	}
	fake := &fakeSandboxes{files: map[string][]byte{}}
	s, err := New(cfg, fake, telemetry.NewMetrics(), slog.New(slog.DiscardHandler))
	if err != nil {
		f.Fatal(err)
	}
	h := s.Handler()
	f.Fuzz(func(t *testing.T, body []byte) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/sandboxes", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+aliceKey)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		switch rec.Code {
		case http.StatusCreated, http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		default:
			t.Fatalf("create with %q: status %d", body, rec.Code)
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, c := range fake.created {
			if c.Limits.VCPUs > cfg.Sandbox.Max.VCPUs || c.Limits.MemoryMiB > cfg.Sandbox.Max.MemoryMiB ||
				c.Limits.Lifetime > cfg.Sandbox.Max.Lifetime || c.Limits.IdleTimeout > cfg.Sandbox.Max.IdleTimeout {
				t.Fatalf("create with %q reached the manager with limits %+v beyond the maxima", body, c.Limits)
			}
		}
		fake.created = fake.created[:0]
	})
}

func digestHex(key string) string {
	d := config.Digest(key)
	return hex.EncodeToString(d[:])
}

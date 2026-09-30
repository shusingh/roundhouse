package agent

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"log/slog"

	"github.com/shusingh/roundhouse/internal/agentproto"
)

// FuzzRestoreBody drives the agent's request decoding through the restore
// handler, with the kernel and clock calls stubbed: no body may cause a
// server error, and only valid requests reach the kernel. The exec and file
// handlers share the decoder but are not fuzzed here, because a valid body
// would run commands or write files on the test host.
func FuzzRestoreBody(f *testing.F) {
	f.Add([]byte(`{"sandbox_id":"sb-1","entropy":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","unix_nano":1}`))
	f.Add([]byte(`{"sandbox_id":"sb-1","entropy":"AA=="}`))
	f.Add([]byte(`{"sandbox_id":"sb-1","extra":1}`))
	f.Add([]byte(`{`))
	s := NewServer(slog.New(slog.DiscardHandler))
	var reseeded int
	s.reseed = func(b []byte) error {
		if len(b) < agentproto.MinEntropyBytes || len(b) > agentproto.MaxEntropyBytes {
			panic("reseed with invalid entropy")
		}
		reseeded++
		return nil
	}
	s.setClock = func(time.Time) error { return nil }
	h := s.Handler()
	f.Fuzz(func(t *testing.T, body []byte) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, agentproto.PathRestore, bytes.NewReader(body)))
		if rec.Code != http.StatusNoContent && rec.Code != http.StatusBadRequest {
			t.Fatalf("restore with %q: status %d", body, rec.Code)
		}
	})
}

package agentproto

import (
	"encoding/json"
	"path"
	"strings"
	"testing"
)

// FuzzExecRequest checks that every exec request that validates is one the
// agent can run safely: a program, no NUL bytes, bounded sizes, and a clean
// absolute working directory.
func FuzzExecRequest(f *testing.F) {
	f.Add([]byte(`{"argv":["sh","-c","echo hi"],"timeout_ms":1000}`))
	f.Add([]byte(`{"argv":["ls"],"env":{"A":"b"},"dir":"/root","timeout_ms":5}`))
	f.Add([]byte(`{"argv":[""],"timeout_ms":1}`))
	f.Add([]byte(`{"argv":["x"],"dir":"/a/../b","timeout_ms":1}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var r ExecRequest
		if json.Unmarshal(data, &r) != nil || r.Validate() != nil {
			return
		}
		if len(r.Argv) == 0 || r.Argv[0] == "" || len(r.Argv) > MaxArgs {
			t.Fatalf("validated argv %q", r.Argv)
		}
		for _, a := range r.Argv {
			if strings.IndexByte(a, 0) >= 0 {
				t.Fatal("validated argv with a NUL byte")
			}
		}
		for k, v := range r.Env {
			if k == "" || strings.ContainsAny(k, "=\x00") || strings.IndexByte(v, 0) >= 0 {
				t.Fatalf("validated env %q=%q", k, v)
			}
		}
		if r.Dir != "" && (!path.IsAbs(r.Dir) || path.Clean(r.Dir) != r.Dir) {
			t.Fatalf("validated dir %q", r.Dir)
		}
		if r.Timeout() <= 0 || r.Timeout() > MaxTimeout {
			t.Fatalf("validated timeout %v", r.Timeout())
		}
	})
}

// FuzzRestoreRequest checks the bounds of validated restore requests.
func FuzzRestoreRequest(f *testing.F) {
	f.Add([]byte(`{"sandbox_id":"sb-1","entropy":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","unix_nano":1}`))
	f.Add([]byte(`{"sandbox_id":"","entropy":""}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var r RestoreRequest
		if json.Unmarshal(data, &r) != nil || r.Validate() != nil {
			return
		}
		if r.SandboxID == "" || len(r.SandboxID) > 128 || len(r.Entropy) < MinEntropyBytes ||
			len(r.Entropy) > MaxEntropyBytes || r.UnixNano < 0 {
			t.Fatalf("validated restore request %+v", r)
		}
	})
}

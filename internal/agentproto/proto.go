// Package agentproto defines the protocol between the host and the guest
// agent: HTTP/1.1 over vsock (docs/adr/0004-http-over-vsock.md), with JSON
// request bodies and newline-delimited JSON frames for streamed command
// output.
//
// Both sides import this package, and both treat what they receive as
// untrusted: every decoder bounds its input and validates what it decodes.
package agentproto

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// Port is the vsock port the agent listens on inside the guest.
const Port = 1024

// Endpoint paths, all served by the agent.
const (
	PathExec    = "/agent/v1/exec"
	PathFiles   = "/agent/v1/files"
	PathHealth  = "/agent/v1/health"
	PathRestore = "/agent/v1/restore"
)

// Limits shared by host and guest.
const (
	// MaxArgs bounds the number of argv entries in one command.
	MaxArgs = 4096
	// MaxArgvBytes bounds argv plus environment, in bytes.
	MaxArgvBytes = 256 << 10
	// MaxStdinBytes bounds stdin passed with a command.
	MaxStdinBytes = 16 << 20
	// MaxRequestBytes bounds any JSON request body; it covers MaxStdinBytes
	// after base64 encoding.
	MaxRequestBytes = 24 << 20
	// MaxChunkBytes bounds the output carried by one frame.
	MaxChunkBytes = 32 << 10
	// MaxFrameBytes bounds one encoded frame line, which carries at most
	// MaxChunkBytes of base64 data plus a few fields.
	MaxFrameBytes = 64 << 10
	// MaxFileBytes bounds a file read or written through the agent.
	MaxFileBytes = 256 << 20
	// MaxTimeout bounds a command's time limit.
	MaxTimeout = 24 * time.Hour
	// MinEntropyBytes and MaxEntropyBytes bound the entropy sent on restore.
	MinEntropyBytes = 32
	MaxEntropyBytes = 4096
)

// DNSResolvers are the public resolvers a sandbox with egress uses. The host
// firewall always allows DNS to them, and the agent writes them to the
// guest's /etc/resolv.conf.
var DNSResolvers = []string{"1.1.1.1", "8.8.8.8"}

// ExecRequest runs one command in the guest.
type ExecRequest struct {
	// Argv is the program and its arguments; Argv[0] is resolved through PATH.
	Argv []string `json:"argv"`
	// Env adds to or overrides the agent's base environment.
	Env map[string]string `json:"env,omitempty"`
	// Dir is the absolute working directory; empty means /root.
	Dir string `json:"dir,omitempty"`
	// Stdin is written to the command's standard input, which is then closed.
	Stdin []byte `json:"stdin,omitempty"`
	// TimeoutMS is the command's time limit in milliseconds. When it expires,
	// the command's whole process group is killed.
	TimeoutMS int64 `json:"timeout_ms"`
}

// Timeout returns the time limit as a duration.
func (r *ExecRequest) Timeout() time.Duration {
	return time.Duration(r.TimeoutMS) * time.Millisecond
}

// Validate checks the request against the protocol's limits.
func (r *ExecRequest) Validate() error {
	if len(r.Argv) == 0 || r.Argv[0] == "" {
		return errors.New("argv: need a program")
	}
	if len(r.Argv) > MaxArgs {
		return fmt.Errorf("argv: %d entries, limit %d", len(r.Argv), MaxArgs)
	}
	size := 0
	for _, a := range r.Argv {
		if strings.IndexByte(a, 0) >= 0 {
			return errors.New("argv: contains a NUL byte")
		}
		size += len(a) + 1
	}
	for k, v := range r.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.IndexByte(v, 0) >= 0 {
			return fmt.Errorf("env: invalid variable %q", k)
		}
		size += len(k) + len(v) + 2
	}
	if size > MaxArgvBytes {
		return fmt.Errorf("argv and env: %d bytes, limit %d", size, MaxArgvBytes)
	}
	if r.Dir != "" {
		if err := ValidatePath(r.Dir); err != nil {
			return fmt.Errorf("dir: %w", err)
		}
	}
	if len(r.Stdin) > MaxStdinBytes {
		return fmt.Errorf("stdin: %d bytes, limit %d", len(r.Stdin), MaxStdinBytes)
	}
	if r.TimeoutMS <= 0 || r.Timeout() > MaxTimeout {
		return fmt.Errorf("timeout_ms: %d, want 1 to %d", r.TimeoutMS, MaxTimeout.Milliseconds())
	}
	return nil
}

// ValidatePath checks a guest path taken from a request: absolute, already
// clean, and free of NUL bytes. The path is interpreted inside the guest only;
// the host never joins it into a host path.
func ValidatePath(p string) error {
	switch {
	case p == "" || !path.IsAbs(p):
		return fmt.Errorf("path %q: must be absolute", p)
	case strings.IndexByte(p, 0) >= 0:
		return errors.New("path contains a NUL byte")
	case path.Clean(p) != p:
		return fmt.Errorf("path %q: must be clean (want %q)", p, path.Clean(p))
	case len(p) > 4096:
		return errors.New("path longer than 4096 bytes")
	}
	return nil
}

// RestoreRequest is sent once after a sandbox is restored from a snapshot.
// See docs/engineering/design.md section 7.3.
type RestoreRequest struct {
	// SandboxID is the identity assigned to this clone.
	SandboxID string `json:"sandbox_id"`
	// Entropy is fresh randomness from the host, mixed into the guest kernel's
	// pool before a forced reseed.
	Entropy []byte `json:"entropy"`
}

// Validate checks the request's bounds.
func (r *RestoreRequest) Validate() error {
	if r.SandboxID == "" || len(r.SandboxID) > 128 {
		return errors.New("sandbox_id: need 1 to 128 bytes")
	}
	if len(r.Entropy) < MinEntropyBytes || len(r.Entropy) > MaxEntropyBytes {
		return fmt.Errorf("entropy: %d bytes, want %d to %d", len(r.Entropy), MinEntropyBytes, MaxEntropyBytes)
	}
	return nil
}

// Health describes the agent's state.
type Health struct {
	// SandboxID is empty until a restore request assigns one; a cold-booted
	// sandbox is assigned one the same way.
	SandboxID string `json:"sandbox_id,omitempty"`
	// Restores counts restore requests handled since the agent started.
	Restores int `json:"restores"`
	// UptimeMS is time since the agent started, in milliseconds, as measured
	// by the guest's monotonic clock.
	UptimeMS int64 `json:"uptime_ms"`
}

// ErrorResponse is the body of any non-2xx agent response.
type ErrorResponse struct {
	Error string `json:"error"`
}

package agentproto

import (
	"strings"
	"testing"
	"time"
)

func validExec() ExecRequest {
	return ExecRequest{
		Argv:      []string{"python3", "-c", "print(1)"},
		Env:       map[string]string{"PYTHONUNBUFFERED": "1"},
		Dir:       "/root/work",
		Stdin:     []byte("input"),
		TimeoutMS: 30_000,
	}
}

func TestExecRequestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*ExecRequest)
		wantErr string
	}{
		{"valid", func(*ExecRequest) {}, ""},
		{"no argv", func(r *ExecRequest) { r.Argv = nil }, "need a program"},
		{"empty program", func(r *ExecRequest) { r.Argv = []string{""} }, "need a program"},
		{"NUL in argv", func(r *ExecRequest) { r.Argv = []string{"sh", "a\x00b"} }, "NUL"},
		{"too many args", func(r *ExecRequest) { r.Argv = make([]string, MaxArgs+1); r.Argv[0] = "x" }, "entries"},
		{"argv too large", func(r *ExecRequest) { r.Argv = []string{"x", strings.Repeat("a", MaxArgvBytes)} }, "bytes"},
		{"env key with =", func(r *ExecRequest) { r.Env = map[string]string{"A=B": "1"} }, "invalid variable"},
		{"empty env key", func(r *ExecRequest) { r.Env = map[string]string{"": "1"} }, "invalid variable"},
		{"relative dir", func(r *ExecRequest) { r.Dir = "work" }, "absolute"},
		{"unclean dir", func(r *ExecRequest) { r.Dir = "/root/../etc" }, "clean"},
		{"stdin too large", func(r *ExecRequest) { r.Stdin = make([]byte, MaxStdinBytes+1) }, "stdin"},
		{"zero timeout", func(r *ExecRequest) { r.TimeoutMS = 0 }, "timeout_ms"},
		{"timeout too long", func(r *ExecRequest) { r.TimeoutMS = (MaxTimeout + time.Millisecond).Milliseconds() }, "timeout_ms"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := validExec()
			tc.mutate(&r)
			err := r.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("Validate() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidatePath(t *testing.T) {
	t.Parallel()
	for _, p := range []string{"/", "/root", "/root/data.csv", "/tmp/a b"} {
		if err := ValidatePath(p); err != nil {
			t.Errorf("ValidatePath(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range []string{"", "root", "./x", "/root/../etc/passwd", "/root//x", "/root/", "/a\x00b", "/" + strings.Repeat("a", 4096)} {
		if err := ValidatePath(p); err == nil {
			t.Errorf("ValidatePath(%q) = nil, want an error", p)
		}
	}
}

func TestRestoreRequestValidate(t *testing.T) {
	t.Parallel()
	ok := RestoreRequest{SandboxID: "sb-1", Entropy: make([]byte, MinEntropyBytes)}
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
	for name, r := range map[string]RestoreRequest{
		"no id":            {Entropy: make([]byte, 64)},
		"too little":       {SandboxID: "sb-1", Entropy: make([]byte, MinEntropyBytes-1)},
		"too much entropy": {SandboxID: "sb-1", Entropy: make([]byte, MaxEntropyBytes+1)},
	} {
		if err := r.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want an error", name)
		}
	}
}

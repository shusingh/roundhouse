//go:build linux

// The agent runs only inside Linux guests; its tests rely on Linux paths and
// process groups, and run in Linux through `make test-linux`.

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shusingh/roundhouse/internal/agentproto"
)

// The test binary doubles as the command under test: when RH_AGENT_HELPER is
// set it runs helperMain instead of the tests, so exec tests need no shell or
// coreutils and run on every platform.
func TestMain(m *testing.M) {
	if mode := os.Getenv("RH_AGENT_HELPER"); mode != "" {
		os.Exit(helperMain(mode))
	}
	os.Exit(m.Run())
}

func helperMain(mode string) int {
	switch mode {
	case "echo":
		_, _ = fmt.Fprint(os.Stdout, "out:"+os.Getenv("GREETING"))
		_, _ = fmt.Fprint(os.Stderr, "err")
		return 3
	case "cat":
		_, _ = io.Copy(os.Stdout, os.Stdin)
		return 0
	case "pwd":
		dir, _ := os.Getwd()
		_, _ = fmt.Fprint(os.Stdout, dir)
		return 0
	case "sleep":
		time.Sleep(time.Minute)
		return 0
	case "flood":
		n, _ := strconv.Atoi(os.Getenv("BYTES"))
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("y"), n))
		return 0
	}
	return 99
}

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := NewServer(slog.New(slog.DiscardHandler))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv
}

// helperExec builds a request that runs this test binary in the given mode.
func helperExec(t *testing.T, mode string, env map[string]string) agentproto.ExecRequest {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The race detector sleeps a second before a clean exit by default;
	// helpers skip that so test durations reflect the agent.
	merged := map[string]string{"RH_AGENT_HELPER": mode, "GORACE": "atexit_sleep_ms=0"}
	for k, v := range env {
		merged[k] = v
	}
	return agentproto.ExecRequest{Argv: []string{exe}, Env: merged, Dir: os.TempDir(), TimeoutMS: 10_000}
}

type execResult struct {
	stdout, stderr []byte
	chunks         int
	terminal       *agentproto.Frame
}

func runExec(t *testing.T, srv *httptest.Server, req agentproto.ExecRequest) execResult {
	t.Helper()
	body, _ := json.Marshal(req)
	resp, err := http.Post(srv.URL+agentproto.PathExec, "application/json", bytes.NewReader(body)) //nolint:noctx // test helper
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("exec status %d: %s", resp.StatusCode, msg)
	}
	var res execResult
	fr := agentproto.NewFrameReader(resp.Body)
	for {
		f, err := fr.Next()
		if errors.Is(err, io.EOF) {
			return res
		}
		if err != nil {
			t.Fatalf("read frames: %v", err)
		}
		switch f.Type {
		case agentproto.FrameStdout:
			res.stdout = append(res.stdout, f.Data...)
			res.chunks++
		case agentproto.FrameStderr:
			res.stderr = append(res.stderr, f.Data...)
		default:
			res.terminal = f
		}
	}
}

func TestExecOutputAndExitCode(t *testing.T) {
	t.Parallel()
	_, srv := newTestServer(t)
	res := runExec(t, srv, helperExec(t, "echo", map[string]string{"GREETING": "hi"}))
	if string(res.stdout) != "out:hi" || string(res.stderr) != "err" {
		t.Errorf("stdout %q, stderr %q; want %q, %q", res.stdout, res.stderr, "out:hi", "err")
	}
	if res.terminal.Type != agentproto.FrameExit || res.terminal.ExitCode != 3 || res.terminal.TimedOut {
		t.Errorf("terminal frame = %+v, want exit code 3, not timed out", res.terminal)
	}
}

func TestExecStdinAndDir(t *testing.T) {
	t.Parallel()
	_, srv := newTestServer(t)
	req := helperExec(t, "cat", nil)
	req.Stdin = []byte("piped input")
	if res := runExec(t, srv, req); string(res.stdout) != "piped input" {
		t.Errorf("stdout = %q, want the stdin echoed back", res.stdout)
	}

	req = helperExec(t, "pwd", nil)
	dir := t.TempDir()
	req.Dir = dir
	if res := runExec(t, srv, req); string(res.stdout) != dir {
		t.Errorf("working directory = %q, want %q", res.stdout, dir)
	}
}

func TestExecTimeoutKills(t *testing.T) {
	t.Parallel()
	_, srv := newTestServer(t)
	req := helperExec(t, "sleep", nil)
	req.TimeoutMS = 200
	start := time.Now()
	res := runExec(t, srv, req)
	if !res.terminal.TimedOut || res.terminal.ExitCode != -1 {
		t.Errorf("terminal frame = %+v, want timed out with exit code -1", res.terminal)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timed-out command took %v to return", elapsed)
	}
}

func TestExecSplitsLargeOutput(t *testing.T) {
	t.Parallel()
	_, srv := newTestServer(t)
	const n = 1 << 20
	res := runExec(t, srv, helperExec(t, "flood", map[string]string{"BYTES": strconv.Itoa(n)}))
	if len(res.stdout) != n {
		t.Fatalf("received %d bytes of stdout, want %d", len(res.stdout), n)
	}
	if res.chunks < n/agentproto.MaxChunkBytes {
		t.Errorf("output arrived in %d frames, want at least %d", res.chunks, n/agentproto.MaxChunkBytes)
	}
}

func TestExecProgramNotFound(t *testing.T) {
	t.Parallel()
	_, srv := newTestServer(t)
	req := agentproto.ExecRequest{Argv: []string{"rh-definitely-not-a-program"}, TimeoutMS: 1000}
	res := runExec(t, srv, req)
	if res.terminal.Type != agentproto.FrameError || !strings.Contains(res.terminal.Error, "not found") {
		t.Errorf("terminal frame = %+v, want an error frame saying not found", res.terminal)
	}
}

func TestExecRejectsInvalidRequests(t *testing.T) {
	t.Parallel()
	_, srv := newTestServer(t)
	for name, body := range map[string]string{
		"no argv":       `{"timeout_ms": 1000}`,
		"no timeout":    `{"argv": ["true"]}`,
		"unknown field": `{"argv": ["true"], "timeout_ms": 1000, "user": "root"}`,
		"not json":      `argv=true`,
	} {
		resp, err := http.Post(srv.URL+agentproto.PathExec, "application/json", strings.NewReader(body)) //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
}

func TestExecClientHangupKillsCommand(t *testing.T) {
	t.Parallel()
	_, srv := newTestServer(t)
	req := helperExec(t, "sleep", nil)
	body, _ := json.Marshal(req)
	ctx, cancel := context.WithCancel(context.Background())
	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+agentproto.PathExec, bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()
	// The server must finish the request promptly once the client is gone;
	// httptest.Server.Close blocks until every handler returns.
	done := make(chan struct{})
	go func() { srv.Close(); close(done) }()
	// The command's own time limit is 10 s; returning well before it proves
	// the hang-up, not the limit, ended the command.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("exec handler kept running after the client hung up")
	}
}

func putFile(t *testing.T, srv *httptest.Server, p string, content []byte) int {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut,
		srv.URL+agentproto.PathFiles+"?path="+url.QueryEscape(p), bytes.NewReader(content))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func getFile(t *testing.T, srv *httptest.Server, p string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(srv.URL + agentproto.PathFiles + "?path=" + url.QueryEscape(p)) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func TestFilesRoundTrip(t *testing.T) {
	t.Parallel()
	_, srv := newTestServer(t)
	dir := t.TempDir()
	p := dir + "/nested/data.bin"
	content := []byte{0, 1, 2, 250, 255}
	if code := putFile(t, srv, p, content); code != http.StatusNoContent {
		t.Fatalf("PUT status %d, want 204", code)
	}
	code, got := getFile(t, srv, p)
	if code != http.StatusOK || !bytes.Equal(got, content) {
		t.Errorf("GET = %d, %v; want 200, %v", code, got, content)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "nested"))
	if len(entries) != 1 {
		t.Errorf("upload left %d entries in the directory, want only the file", len(entries))
	}
}

func TestFilesErrors(t *testing.T) {
	t.Parallel()
	_, srv := newTestServer(t)
	if code := putFile(t, srv, "relative/path", []byte("x")); code != http.StatusBadRequest {
		t.Errorf("PUT relative path: status %d, want 400", code)
	}
	if code, _ := getFile(t, srv, "/root/../etc/shadow"); code != http.StatusBadRequest {
		t.Errorf("GET unclean path: status %d, want 400", code)
	}
	dir := t.TempDir()
	if code, _ := getFile(t, srv, dir+"/missing"); code != http.StatusNotFound {
		t.Errorf("GET missing file: status %d, want 404", code)
	}
	if code, _ := getFile(t, srv, dir); code != http.StatusBadRequest {
		t.Errorf("GET directory: status %d, want 400", code)
	}
}

func TestRestoreAndHealth(t *testing.T) {
	t.Parallel()
	s, srv := newTestServer(t)
	var reseeded []byte
	s.reseed = func(b []byte) error { reseeded = b; return nil }

	entropy := bytes.Repeat([]byte{7}, 64)
	body, _ := json.Marshal(agentproto.RestoreRequest{SandboxID: "sb-42", Entropy: entropy})
	resp, err := http.Post(srv.URL+agentproto.PathRestore, "application/json", bytes.NewReader(body)) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || !bytes.Equal(reseeded, entropy) {
		t.Fatalf("restore status %d, reseeded %d bytes; want 204 and the entropy passed through", resp.StatusCode, len(reseeded))
	}

	resp, err = http.Get(srv.URL + agentproto.PathHealth) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var h agentproto.Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if h.SandboxID != "sb-42" || h.Restores != 1 {
		t.Errorf("health = %+v, want sandbox sb-42 after one restore", h)
	}
}

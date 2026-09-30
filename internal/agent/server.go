// Package agent is the guest side of Roundhouse: the program that runs inside
// every microVM, serves the agent protocol (package agentproto) over vsock,
// and, as the guest's init process, prepares the root file system and reaps
// orphaned processes.
//
// The agent is the only guest component the host talks to. It trusts nothing
// it receives from processes in the guest, and it never logs command lines,
// output, or file contents.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shusingh/roundhouse/internal/agentproto"
)

// baseEnv is the environment every command starts from.
var baseEnv = []string{
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	"HOME=/root",
	"LANG=C.UTF-8",
}

// waitDelay bounds how long a command's output pipes are drained after the
// command exits. A background process started by the command can hold them
// open indefinitely; after this delay the agent stops waiting and reports the
// exit. See exec.Cmd.WaitDelay.
const waitDelay = 250 * time.Millisecond

// Server serves the agent protocol. Its zero value is not usable; use
// NewServer.
type Server struct {
	logger *slog.Logger
	start  time.Time
	// reseed mixes host entropy into the guest kernel and forces a reseed.
	reseed func([]byte) error

	mu        sync.Mutex
	sandboxID string // guarded by mu
	restores  int    // guarded by mu
}

// NewServer returns a server that logs with logger.
func NewServer(logger *slog.Logger) *Server {
	return &Server{logger: logger, start: time.Now(), reseed: reseedKernel}
}

// Handler returns the HTTP handler for the agent protocol.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+agentproto.PathExec, s.handleExec)
	mux.HandleFunc("PUT "+agentproto.PathFiles, s.handlePutFile)
	mux.HandleFunc("GET "+agentproto.PathFiles, s.handleGetFile)
	mux.HandleFunc("GET "+agentproto.PathHealth, s.handleHealth)
	mux.HandleFunc("POST "+agentproto.PathRestore, s.handleRestore)
	return mux
}

// Serve serves the protocol on ln until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Exec responses stream for as long as a command runs, so there is no
		// write timeout; command time limits bound them instead.
		IdleTimeout: 5 * time.Minute,
		ErrorLog:    slog.NewLogLogger(s.logger.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return fmt.Errorf("serve agent protocol: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	var req agentproto.ExecRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// The time limit starts now; the request context also ends if the host
	// hangs up, which kills the command.
	ctx, cancel := context.WithTimeout(r.Context(), req.Timeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...) //nolint:gosec // G204: running the caller's command is the agent's purpose
	cmd.Env = commandEnv(req.Env)
	cmd.Dir = req.Dir
	if cmd.Dir == "" {
		cmd.Dir = "/root"
	}
	cmd.Stdin = bytes.NewReader(req.Stdin)
	cmd.WaitDelay = waitDelay
	configureProcessGroup(cmd)

	frames := make(chan *agentproto.Frame)
	cmd.Stdout = &chunkWriter{kind: agentproto.FrameStdout, out: frames}
	cmd.Stderr = &chunkWriter{kind: agentproto.FrameStderr, out: frames}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	fw := agentproto.NewFrameWriter(w)

	started := time.Now()
	// Send the headers now: a command may print nothing for a long time, and
	// the host needs to know the stream has started.
	if err := rc.Flush(); err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		_ = fw.Write(&agentproto.Frame{Type: agentproto.FrameError, Error: startError(err)})
		_ = rc.Flush()
		return
	}
	waitErr := make(chan error, 1)
	go func() {
		// Wait returns once the process has exited and its output has been
		// copied (or waitDelay has passed); only then can frames be closed.
		err := cmd.Wait()
		close(frames)
		waitErr <- err
	}()

	// One goroutine writes frames, in order; the copy goroutines block on the
	// unbuffered channel, so a slow reader slows the command's output rather
	// than growing a buffer.
	clientGone := false
	for f := range frames {
		if clientGone {
			continue // drain so the copiers can finish
		}
		if err := fw.Write(f); err != nil || rc.Flush() != nil {
			clientGone = true
			cancel()
		}
	}
	err := <-waitErr
	if clientGone {
		return
	}
	exit := &agentproto.Frame{
		Type:       agentproto.FrameExit,
		ExitCode:   cmd.ProcessState.ExitCode(),
		TimedOut:   errors.Is(ctx.Err(), context.DeadlineExceeded),
		DurationMS: time.Since(started).Milliseconds(),
	}
	if err != nil && !isExitError(err) && !exit.TimedOut {
		s.logger.Warn("wait for command", slog.String("error", err.Error()))
	}
	_ = fw.Write(exit)
	_ = rc.Flush()
}

// chunkWriter turns a command's output into frames of at most MaxChunkBytes.
// os/exec copies the process's pipe into it from its own goroutine.
type chunkWriter struct {
	kind agentproto.FrameType
	out  chan<- *agentproto.Frame
}

func (c *chunkWriter) Write(p []byte) (int, error) {
	for off := 0; off < len(p); off += agentproto.MaxChunkBytes {
		end := min(off+agentproto.MaxChunkBytes, len(p))
		// The frame owns its data: os/exec reuses p after Write returns.
		c.out <- &agentproto.Frame{Type: c.kind, Data: bytes.Clone(p[off:end])}
	}
	return len(p), nil
}

func (s *Server) handlePutFile(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if err := agentproto.ValidatePath(p); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil { //nolint:gosec // G301: an ordinary directory for the sandbox's user
		writeError(w, http.StatusBadRequest, "create parent directory: "+err.Error())
		return
	}
	// Write to a temporary file and rename, so a failed or oversized upload
	// never leaves a partial file at the destination.
	tmp, err := os.CreateTemp(filepath.Dir(p), ".rh-upload-*")
	if err != nil {
		writeError(w, http.StatusBadRequest, "create file: "+err.Error())
		return
	}
	defer func() { _ = os.Remove(tmp.Name()) }() //nolint:gosec // G703: the caller's own guest path, validated by ValidatePath
	n, copyErr := io.Copy(tmp, io.LimitReader(r.Body, agentproto.MaxFileBytes+1))
	closeErr := tmp.Close()
	switch {
	case n > agentproto.MaxFileBytes:
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file larger than %d bytes", agentproto.MaxFileBytes))
		return
	case copyErr != nil || closeErr != nil:
		writeError(w, http.StatusBadRequest, "write file: "+errors.Join(copyErr, closeErr).Error())
		return
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil { //nolint:gosec // G302: uploaded files are ordinary readable files
		writeError(w, http.StatusInternalServerError, "set permissions: "+err.Error())
		return
	}
	if err := os.Rename(tmp.Name(), p); err != nil { //nolint:gosec // G703: the caller's own guest path, validated by ValidatePath
		writeError(w, http.StatusBadRequest, "place file: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if err := agentproto.ValidatePath(p); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	f, err := os.Open(p) //nolint:gosec // G304: reading the caller's file inside its own sandbox
	if errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, "no such file")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "open file: "+err.Error())
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	switch {
	case err != nil:
		writeError(w, http.StatusInternalServerError, "stat file: "+err.Error())
		return
	case !info.Mode().IsRegular():
		writeError(w, http.StatusBadRequest, "not a regular file")
		return
	case info.Size() > agentproto.MaxFileBytes:
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file larger than %d bytes", agentproto.MaxFileBytes))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	// The limit guards against a file that grows while it is being read.
	_, _ = io.Copy(w, io.LimitReader(f, agentproto.MaxFileBytes))
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	h := agentproto.Health{SandboxID: s.sandboxID, Restores: s.restores, UptimeMS: time.Since(s.start).Milliseconds()}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	var req agentproto.RestoreRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.reseed(req.Entropy); err != nil {
		writeError(w, http.StatusInternalServerError, "reseed: "+err.Error())
		return
	}
	s.mu.Lock()
	s.sandboxID = req.SandboxID
	s.restores++
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// commandEnv merges the request's variables over the base environment.
func commandEnv(extra map[string]string) []string {
	env := make([]string, 0, len(baseEnv)+len(extra))
	for _, kv := range baseEnv {
		if k, _, _ := strings.Cut(kv, "="); extra[k] == "" {
			env = append(env, kv)
		}
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// startError describes why a command could not start, without echoing the
// command line.
func startError(err error) string {
	var pathErr *fs.PathError
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return "program not found in PATH"
	case errors.As(err, &pathErr):
		return pathErr.Op + ": " + pathErr.Err.Error()
	default:
		return err.Error()
	}
}

func isExitError(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body := http.MaxBytesReader(w, r.Body, agentproto.MaxRequestBytes)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "decode request: "+err.Error())
		return false
	}
	// Read the body to EOF. net/http notices a client hanging up only once
	// the body is consumed, and exec relies on that to kill a command whose
	// caller has gone away.
	if _, err := io.Copy(io.Discard, body); err != nil {
		writeError(w, http.StatusBadRequest, "read request: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, agentproto.ErrorResponse{Error: msg})
}

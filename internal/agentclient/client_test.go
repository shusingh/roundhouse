package agentclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shusingh/roundhouse/internal/agentproto"
)

// fakeVsock stands in for Firecracker's vsock Unix socket: it answers the
// CONNECT handshake and then serves HTTP on the connection.
type fakeVsock struct {
	socket string
	reply  string // handshake reply line
}

func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rhac")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "v.sock")
}

func startFake(t *testing.T, reply string, handler http.Handler) *fakeVsock {
	t.Helper()
	f := &fakeVsock{socket: socketPath(t), reply: reply}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go srv.Serve(&handshakeListener{Listener: ln, reply: reply}) //nolint:errcheck // returns ErrServerClosed
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

type handshakeListener struct {
	net.Listener
	reply string
}

func (l *handshakeListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		br := bufio.NewReader(conn)
		line, err := br.ReadString('\n')
		if err != nil || line != "CONNECT 1024\n" {
			_ = conn.Close()
			continue
		}
		if _, err := io.WriteString(conn, l.reply+"\n"); err != nil || !strings.HasPrefix(l.reply, "OK ") {
			_ = conn.Close()
			continue
		}
		return conn, nil
	}
}

func agentHandler(t *testing.T) http.Handler {
	t.Helper()
	files := &sync.Map{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+agentproto.PathHealth, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(agentproto.Health{SandboxID: "sb-1", Restores: 2})
	})
	mux.HandleFunc("POST "+agentproto.PathExec, func(w http.ResponseWriter, r *http.Request) {
		var req agentproto.ExecRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		fw := agentproto.NewFrameWriter(w)
		switch req.Argv[0] {
		case "echo":
			_ = fw.Write(&agentproto.Frame{Type: agentproto.FrameStdout, Data: []byte("a")})
			_ = fw.Write(&agentproto.Frame{Type: agentproto.FrameStderr, Data: []byte("b")})
			_ = fw.Write(&agentproto.Frame{Type: agentproto.FrameExit, ExitCode: 0})
		case "die":
			_ = fw.Write(&agentproto.Frame{Type: agentproto.FrameStdout, Data: []byte("partial")})
		case "missing":
			_ = fw.Write(&agentproto.Frame{Type: agentproto.FrameError, Error: "program not found in PATH"})
		}
	})
	mux.HandleFunc("PUT "+agentproto.PathFiles, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		files.Store(r.URL.Query().Get("path"), data)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET "+agentproto.PathFiles, func(w http.ResponseWriter, r *http.Request) {
		data, ok := files.Load(r.URL.Query().Get("path"))
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(agentproto.ErrorResponse{Error: "no such file"})
			return
		}
		_, _ = w.Write(data.([]byte)) //nolint:gosec // G705: test double echoing stored bytes
	})
	return mux
}

func TestHealthThroughHandshake(t *testing.T) {
	t.Parallel()
	h := agentHandler(t)
	f := startFake(t, "OK 1073741824", h)
	c := New(f.socket)
	defer c.Close()
	got, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if got.SandboxID != "sb-1" || got.Restores != 2 {
		t.Errorf("Health = %+v, want sandbox sb-1 with 2 restores", got)
	}
}

func TestHandshakeRefused(t *testing.T) {
	t.Parallel()
	// A raw listener that refuses, as Firecracker does when no guest process
	// listens on the port.
	socket := socketPath(t)
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = bufio.NewReader(conn).ReadString('\n')
		_, _ = io.WriteString(conn, "FAILED\n")
	}()
	c := New(socket)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Health(ctx); !errors.Is(err, ErrHandshake) {
		t.Fatalf("Health error = %v, want ErrHandshake", err)
	}
}

func TestExec(t *testing.T) {
	t.Parallel()
	h := agentHandler(t)
	f := startFake(t, "OK 1", h)
	c := New(f.socket)
	defer c.Close()
	ctx := context.Background()

	var out bytes.Buffer
	exit, err := c.Exec(ctx, agentproto.ExecRequest{Argv: []string{"echo"}, TimeoutMS: 1000}, func(fr *agentproto.Frame) error {
		out.WriteString(string(fr.Type) + ":" + string(fr.Data) + ";")
		return nil
	})
	if err != nil || exit.Type != agentproto.FrameExit || out.String() != "stdout:a;stderr:b;" {
		t.Errorf("Exec = %+v, %v, output %q; want an exit frame after stdout a and stderr b", exit, err, out.String())
	}

	exit, err = c.Exec(ctx, agentproto.ExecRequest{Argv: []string{"missing"}, TimeoutMS: 1000}, func(*agentproto.Frame) error { return nil })
	if err != nil || exit.Type != agentproto.FrameError {
		t.Errorf("Exec(missing) = %+v, %v; want an error frame", exit, err)
	}

	_, err = c.Exec(ctx, agentproto.ExecRequest{Argv: []string{"die"}, TimeoutMS: 1000}, func(*agentproto.Frame) error { return nil })
	if !errors.Is(err, agentproto.ErrStreamTruncated) {
		t.Errorf("Exec(die) error = %v, want ErrStreamTruncated", err)
	}

	if _, err := c.Exec(ctx, agentproto.ExecRequest{TimeoutMS: 1000}, nil); err == nil {
		t.Error("Exec accepted a request without argv")
	}
}

func TestFilesAndAgentErrors(t *testing.T) {
	t.Parallel()
	h := agentHandler(t)
	f := startFake(t, "OK 1", h)
	c := New(f.socket)
	defer c.Close()
	ctx := context.Background()

	if err := c.WriteFile(ctx, "/root/a.txt", strings.NewReader("hello")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	var buf bytes.Buffer
	if n, err := c.ReadFile(ctx, "/root/a.txt", &buf); err != nil || n != 5 || buf.String() != "hello" {
		t.Errorf("ReadFile = %d, %v, %q; want 5, nil, hello", n, err, buf.String())
	}
	_, err := c.ReadFile(ctx, "/root/missing", io.Discard)
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.StatusCode != http.StatusNotFound || agentErr.Message != "no such file" {
		t.Errorf("ReadFile(missing) error = %v, want a 404 AgentError", err)
	}
	if err := c.WriteFile(ctx, "relative", strings.NewReader("x")); err == nil {
		t.Error("WriteFile accepted a relative path")
	}
}

func TestWaitReady(t *testing.T) {
	t.Parallel()
	h := agentHandler(t)
	socket := socketPath(t)
	c := New(socket)
	defer c.Close()

	go func() {
		time.Sleep(30 * time.Millisecond)
		ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", socket)
		if err != nil {
			return
		}
		srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
		t.Cleanup(func() { _ = srv.Close() })
		_ = srv.Serve(&handshakeListener{Listener: ln, reply: "OK 1"})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}

// Package agentclient is the host side of the agent protocol: it reaches the
// guest agent through Firecracker's vsock Unix socket and speaks HTTP/1.1 over
// the connection (docs/adr/0004-http-over-vsock.md).
//
// Everything the guest returns is treated as untrusted: bodies and frames are
// size-limited and validated, and every call is bounded by its context.
package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shusingh/roundhouse/internal/agentproto"
)

// maxErrorBytes bounds how much of an error response is read.
const maxErrorBytes = 16 << 10

// ErrHandshake means Firecracker did not accept the vsock CONNECT request,
// typically because nothing listens on the port in the guest yet.
var ErrHandshake = errors.New("vsock handshake refused")

// Client talks to one guest's agent. It is safe for concurrent use; each
// request may use its own connection.
type Client struct {
	socket string
	http   *http.Client
}

// New returns a client for the guest behind Firecracker's vsock Unix socket
// at socketPath.
func New(socketPath string) *Client {
	c := &Client{socket: socketPath}
	transport := &http.Transport{
		DialContext:           func(ctx context.Context, _, _ string) (net.Conn, error) { return c.dial(ctx) },
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   4,
		// Firecracker resets vsock connections across a snapshot; short idle
		// reuse keeps a stale connection from being picked up after restore.
		IdleConnTimeout:    10 * time.Second,
		DisableCompression: true,
	}
	c.http = &http.Client{Transport: transport}
	return c
}

// Close drops idle connections. Call it after the guest is restored or
// destroyed.
func (c *Client) Close() {
	c.http.CloseIdleConnections()
}

// dial connects to the vsock socket and asks Firecracker to forward the
// connection to the agent's port: the host writes "CONNECT <port>\n" and
// Firecracker answers "OK <host port>\n".
func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return nil, fmt.Errorf("dial vsock socket: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", agentproto.Port); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("vsock handshake: %w", err)
	}
	// Read the reply byte by byte so nothing past the newline is consumed;
	// the rest of the stream belongs to HTTP.
	reply, err := readLine(conn, 64)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("vsock handshake: %w", err)
	}
	if !strings.HasPrefix(reply, "OK ") {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: %q", ErrHandshake, reply)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func readLine(r io.Reader, limit int) (string, error) {
	var buf [1]byte
	var line []byte
	for len(line) < limit {
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return "", err
		}
		if buf[0] == '\n' {
			return string(line), nil
		}
		line = append(line, buf[0])
	}
	return "", fmt.Errorf("reply longer than %d bytes", limit)
}

// Health returns the agent's state.
func (c *Client) Health(ctx context.Context) (agentproto.Health, error) {
	var h agentproto.Health
	resp, err := c.do(ctx, http.MethodGet, agentproto.PathHealth, nil, "")
	if err != nil {
		return h, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&h); err != nil {
		return h, fmt.Errorf("decode health: %w", err)
	}
	return h, nil
}

// WaitReady polls the agent's health until it answers or ctx ends. After a
// boot or restore the agent may not be listening yet.
func (c *Client) WaitReady(ctx context.Context) error {
	delay := time.Millisecond
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := c.Health(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("wait for guest agent: %w", errors.Join(ctx.Err(), err))
		case <-timer.C:
		}
		delay = min(delay*2, 50*time.Millisecond)
	}
}

// Restore tells a freshly restored agent its identity and gives it entropy.
func (c *Client) Restore(ctx context.Context, req agentproto.RestoreRequest) error {
	if err := req.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode restore request: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, agentproto.PathRestore, bytes.NewReader(body), "application/json")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Exec runs a command and calls onOutput for each stdout or stderr frame, in
// order, as it arrives. It returns the terminal frame: an exit frame, or an
// error frame if the command could not start. An error return means the
// stream itself failed (for example, the guest died mid-command).
func (c *Client) Exec(ctx context.Context, req agentproto.ExecRequest, onOutput func(*agentproto.Frame) error) (*agentproto.Frame, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode exec request: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, agentproto.PathExec, bytes.NewReader(body), "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	frames := agentproto.NewFrameReader(resp.Body)
	for {
		f, err := frames.Next()
		if err != nil {
			return nil, fmt.Errorf("read exec output: %w", err)
		}
		if f.Terminal() {
			return f, nil
		}
		if err := onOutput(f); err != nil {
			return nil, err
		}
	}
}

// WriteFile writes content to path in the guest, replacing any existing file.
func (c *Client) WriteFile(ctx context.Context, path string, content io.Reader) error {
	if err := agentproto.ValidatePath(path); err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPut, agentproto.PathFiles+"?path="+url.QueryEscape(path), content, "application/octet-stream")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ReadFile streams the file at path in the guest to w, copying at most
// agentproto.MaxFileBytes.
func (c *Client) ReadFile(ctx context.Context, path string, w io.Writer) (int64, error) {
	if err := agentproto.ValidatePath(path); err != nil {
		return 0, err
	}
	resp, err := c.do(ctx, http.MethodGet, agentproto.PathFiles+"?path="+url.QueryEscape(path), nil, "")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	n, err := io.Copy(w, io.LimitReader(resp.Body, agentproto.MaxFileBytes))
	if err != nil {
		return n, fmt.Errorf("read file from guest: %w", err)
	}
	return n, nil
}

// AgentError is a non-2xx response from the agent.
type AgentError struct {
	StatusCode int
	Message    string
}

func (e *AgentError) Error() string {
	return fmt.Sprintf("guest agent: status %d: %s", e.StatusCode, e.Message)
}

// do sends one request and returns the response if its status is 2xx. The
// URL's host is a placeholder; the transport always dials the vsock socket.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, body)
	if err != nil {
		return nil, fmt.Errorf("build %s %s: %w", method, path, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("guest agent %s %s: %w", method, strings.SplitN(path, "?", 2)[0], err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	var er agentproto.ErrorResponse
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	if json.Unmarshal(raw, &er) != nil || er.Error == "" {
		er.Error = strings.TrimSpace(string(raw))
	}
	return nil, &AgentError{StatusCode: resp.StatusCode, Message: er.Error}
}

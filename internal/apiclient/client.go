// Package apiclient is a Go client for Roundhouse's HTTP API, used by rhctl
// and the benchmark driver.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shusingh/roundhouse/internal/agentproto"
)

// Client calls one Roundhouse control plane.
type Client struct {
	base string
	key  string
	http *http.Client
}

// New returns a client for the API at baseURL (for example
// "http://127.0.0.1:8480"), authenticating with key.
func New(baseURL, key string) *Client {
	return &Client{
		base: strings.TrimRight(baseURL, "/"),
		key:  key,
		http: &http.Client{Transport: &http.Transport{
			ResponseHeaderTimeout: 2 * time.Minute, // creates wait for a boot
			MaxIdleConnsPerHost:   64,
		}},
	}
}

// Sandbox is a sandbox as the API reports it.
type Sandbox struct {
	ID                 string    `json:"id"`
	Image              string    `json:"image"`
	State              string    `json:"state"`
	VCPUs              int       `json:"vcpus"`
	MemoryMiB          int       `json:"memory_mib"`
	CreatedAt          time.Time `json:"created_at"`
	ExpiresAt          time.Time `json:"expires_at"`
	IdleTimeoutSeconds int64     `json:"idle_timeout_seconds"`
	Egress             string    `json:"egress"`
	StartPath          string    `json:"start_path"`
	ActiveCalls        int       `json:"active_calls"`
}

// CreateOptions requests limits; zero fields take the server's defaults.
type CreateOptions struct {
	Image              string `json:"image,omitempty"`
	VCPUs              int    `json:"vcpus,omitempty"`
	MemoryMiB          int    `json:"memory_mib,omitempty"`
	LifetimeSeconds    int64  `json:"lifetime_seconds,omitempty"`
	IdleTimeoutSeconds int64  `json:"idle_timeout_seconds,omitempty"`
	// Egress is nil for no network access.
	Egress *Egress `json:"egress,omitempty"`
}

// Egress is an egress policy: Mode "none", "internet", or "allowlist" with
// Allow entries such as "203.0.113.0/24" or "198.51.100.7:443".
type Egress struct {
	Mode  string   `json:"mode"`
	Allow []string `json:"allow,omitempty"`
}

// ExecOptions describes one command.
type ExecOptions struct {
	Argv      []string          `json:"argv"`
	Env       map[string]string `json:"env,omitempty"`
	Dir       string            `json:"dir,omitempty"`
	Stdin     []byte            `json:"stdin,omitempty"`
	TimeoutMS int64             `json:"timeout_ms,omitempty"`
}

// Error is an API error response.
type Error struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("roundhouse: %d %s: %s", e.StatusCode, e.Code, e.Message)
}

// Create starts a sandbox and returns it once it is ready.
func (c *Client) Create(ctx context.Context, opts CreateOptions) (Sandbox, error) {
	var sb Sandbox
	err := c.doJSON(ctx, http.MethodPost, "/v1/sandboxes", opts, &sb)
	return sb, err
}

// Get returns one sandbox.
func (c *Client) Get(ctx context.Context, id string) (Sandbox, error) {
	var sb Sandbox
	err := c.doJSON(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id), nil, &sb)
	return sb, err
}

// List returns the caller's sandboxes.
func (c *Client) List(ctx context.Context) ([]Sandbox, error) {
	var out struct {
		Sandboxes []Sandbox `json:"sandboxes"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/v1/sandboxes", nil, &out)
	return out.Sandboxes, err
}

// Delete destroys a sandbox.
func (c *Client) Delete(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(id), nil, nil)
}

// Exec runs a command, calling onOutput for each stdout or stderr frame, and
// returns the terminal frame (exit, or error if the command could not start
// or the sandbox failed).
func (c *Client) Exec(ctx context.Context, id string, opts ExecOptions, onOutput func(*agentproto.Frame) error) (*agentproto.Frame, error) {
	body, err := json.Marshal(opts)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/exec", bytes.NewReader(body), "application/json")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	frames := agentproto.NewFrameReader(resp.Body)
	for {
		f, err := frames.Next()
		if err != nil {
			return nil, fmt.Errorf("read exec stream: %w", err)
		}
		if f.Terminal() {
			return f, nil
		}
		if onOutput != nil {
			if err := onOutput(f); err != nil {
				return nil, err
			}
		}
	}
}

// WriteFile uploads content to path inside the sandbox.
func (c *Client) WriteFile(ctx context.Context, id, path string, content io.Reader) error {
	resp, err := c.do(ctx, http.MethodPut, c.filesPath(id, path), content, "application/octet-stream")
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// ReadFile downloads path from inside the sandbox into w.
func (c *Client) ReadFile(ctx context.Context, id, path string, w io.Writer) (int64, error) {
	resp, err := c.do(ctx, http.MethodGet, c.filesPath(id, path), nil, "")
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.Copy(w, io.LimitReader(resp.Body, agentproto.MaxFileBytes))
}

func (c *Client) filesPath(id, path string) string {
	return "/v1/sandboxes/" + url.PathEscape(id) + "/files?path=" + url.QueryEscape(path)
}

func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
	}
	resp, err := c.do(ctx, method, path, body, "application/json")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil && contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	apiErr := &Error{StatusCode: resp.StatusCode}
	var eb struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if json.Unmarshal(raw, &eb) == nil && eb.Error.Code != "" {
		apiErr.Code, apiErr.Message = eb.Error.Code, eb.Error.Message
	} else {
		apiErr.Code, apiErr.Message = "unknown", strings.TrimSpace(string(raw))
	}
	return nil, apiErr
}

// IsNotFound reports whether err is a 404 from the API.
func IsNotFound(err error) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

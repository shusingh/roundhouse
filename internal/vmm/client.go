package vmm

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
	"time"
)

// maxFaultBytes bounds how much of an error response is read. Firecracker's
// fault messages are short; anything longer is truncated.
const maxFaultBytes = 64 << 10

// APIError is a non-success response from Firecracker's API.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	// Fault is Firecracker's fault_message, or the raw body if it was not JSON.
	Fault string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("firecracker %s %s: status %d: %s", e.Method, e.Path, e.StatusCode, e.Fault)
}

// Client calls one Firecracker process's API over its Unix socket. It is safe
// for concurrent use, although Firecracker serializes requests itself.
type Client struct {
	socket string
	http   *http.Client
}

// NewClient returns a client for the API socket at socketPath. Every call
// takes a context; callers bound each call with a deadline.
func NewClient(socketPath string) *Client {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
		// Snapshot creation writes all of guest memory before responding.
		ResponseHeaderTimeout: 2 * time.Minute,
		MaxIdleConnsPerHost:   1,
		IdleConnTimeout:       30 * time.Second,
		DisableCompression:    true,
	}
	return &Client{socket: socketPath, http: &http.Client{Transport: transport}}
}

// Close releases idle connections to the VMM.
func (c *Client) Close() {
	c.http.CloseIdleConnections()
}

// WaitReady polls until the VMM's API answers, or ctx ends. A VMM creates its
// socket shortly after start, and the socket accepts connections only once
// the API thread is serving.
func (c *Client) WaitReady(ctx context.Context) error {
	delay := time.Millisecond
	for {
		err := c.do(ctx, http.MethodGet, "/", nil)
		if err == nil {
			return nil
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return err // the API is up and refused the request
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("wait for firecracker API at %s: %w", c.socket, errors.Join(ctx.Err(), err))
		case <-timer.C:
		}
		delay = min(delay*2, 20*time.Millisecond)
	}
}

// PutMachineConfig sets vCPUs and memory. Pre-boot only.
func (c *Client) PutMachineConfig(ctx context.Context, cfg MachineConfig) error {
	return c.do(ctx, http.MethodPut, "/machine-config", cfg)
}

// PutBootSource sets the kernel and its command line. Pre-boot only.
func (c *Client) PutBootSource(ctx context.Context, src BootSource) error {
	return c.do(ctx, http.MethodPut, "/boot-source", src)
}

// PutDrive attaches a block device. Pre-boot only.
func (c *Client) PutDrive(ctx context.Context, d Drive) error {
	return c.do(ctx, http.MethodPut, "/drives/"+url.PathEscape(d.DriveID), d)
}

// PutNetworkInterface attaches a network device. Pre-boot only.
func (c *Client) PutNetworkInterface(ctx context.Context, n NetworkInterface) error {
	return c.do(ctx, http.MethodPut, "/network-interfaces/"+url.PathEscape(n.IfaceID), n)
}

// PutVsock attaches the vsock device. Pre-boot only.
func (c *Client) PutVsock(ctx context.Context, v Vsock) error {
	return c.do(ctx, http.MethodPut, "/vsock", v)
}

// PutSerial routes the serial console to a file. Call it before Start, and
// also before LoadSnapshot: the serial output path is not part of a snapshot,
// so a restored VM's console goes nowhere unless it is set again.
func (c *Client) PutSerial(ctx context.Context, s Serial) error {
	return c.do(ctx, http.MethodPut, "/serial", s)
}

// PutEntropy attaches a virtio-rng device. Pre-boot only.
func (c *Client) PutEntropy(ctx context.Context, e Entropy) error {
	return c.do(ctx, http.MethodPut, "/entropy", e)
}

// PutLogger configures the VMM's own log. It must precede boot or snapshot
// load.
func (c *Client) PutLogger(ctx context.Context, l Logger) error {
	return c.do(ctx, http.MethodPut, "/logger", l)
}

// Start boots a configured VM.
func (c *Client) Start(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/actions", instanceAction{ActionType: "InstanceStart"})
}

// Pause stops the VM's vCPUs; required before creating a snapshot.
func (c *Client) Pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", vmState{State: "Paused"})
}

// Resume restarts a paused VM's vCPUs.
func (c *Client) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", vmState{State: "Resumed"})
}

// CreateSnapshot writes a paused VM's state and memory.
func (c *Client) CreateSnapshot(ctx context.Context, s SnapshotCreate) error {
	return c.do(ctx, http.MethodPut, "/snapshot/create", s)
}

// LoadSnapshot restores a VM into this VMM, which must not have booted.
func (c *Client) LoadSnapshot(ctx context.Context, s SnapshotLoad) error {
	return c.do(ctx, http.MethodPut, "/snapshot/load", s)
}

// do sends one request. Firecracker answers 204 (or 200 for GET) on success
// and 4xx or 5xx with a JSON fault message on failure.
func (c *Client) do(ctx context.Context, method, path string, body any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(buf)
	}
	// The host part is ignored: the transport always dials the Unix socket.
	req, err := http.NewRequestWithContext(ctx, method, "http://firecracker"+path, reader)
	if err != nil {
		return fmt.Errorf("build %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, maxFaultBytes))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	apiErr := &APIError{Method: method, Path: path, StatusCode: resp.StatusCode}
	var fault apiFault
	switch {
	case readErr != nil:
		apiErr.Fault = "unreadable body: " + readErr.Error()
	case json.Unmarshal(payload, &fault) == nil && fault.FaultMessage != "":
		apiErr.Fault = fault.FaultMessage
	default:
		apiErr.Fault = string(payload)
	}
	return apiErr
}

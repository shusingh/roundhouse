package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shusingh/roundhouse/internal/agentclient"
	"github.com/shusingh/roundhouse/internal/agentproto"
	"github.com/shusingh/roundhouse/internal/config"
	"github.com/shusingh/roundhouse/internal/sandbox"
)

// fakeSandboxes records what the API asked for and replays scripted answers.
type fakeSandboxes struct {
	mu        sync.Mutex
	created   []sandbox.CreateRequest
	createErr error
	execReq   agentproto.ExecRequest
	execFn    func(onOutput func(*agentproto.Frame) error) (*agentproto.Frame, error)
	files     map[string][]byte
	readErr   error
}

var created = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

func (f *fakeSandboxes) Create(_ context.Context, req sandbox.CreateRequest) (sandbox.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return sandbox.Info{}, f.createErr
	}
	f.created = append(f.created, req)
	return sandbox.Info{ID: "sb-1", Owner: req.Owner, Image: req.Image, State: sandbox.StateReady, Limits: req.Limits,
		CreatedAt: created, ExpiresAt: created.Add(req.Limits.Lifetime)}, nil
}

func (f *fakeSandboxes) lookup(owner, id string) error {
	if id != "sb-1" || owner != "alice" {
		return sandbox.ErrNotFound
	}
	return nil
}

func (f *fakeSandboxes) Get(owner, id string) (sandbox.Info, error) {
	if err := f.lookup(owner, id); err != nil {
		return sandbox.Info{}, err
	}
	return sandbox.Info{ID: id, State: sandbox.StateReady}, nil
}

func (f *fakeSandboxes) List(owner string) []sandbox.Info {
	if owner != "alice" {
		return nil
	}
	return []sandbox.Info{{ID: "sb-1", State: sandbox.StateReady}}
}

func (f *fakeSandboxes) Delete(_ context.Context, owner, id string) error { return f.lookup(owner, id) }

func (f *fakeSandboxes) Exec(_ context.Context, owner, id string, req agentproto.ExecRequest, onOutput func(*agentproto.Frame) error) (*agentproto.Frame, error) {
	if err := f.lookup(owner, id); err != nil {
		return nil, err
	}
	f.execReq = req
	return f.execFn(onOutput)
}

func (f *fakeSandboxes) WriteFile(_ context.Context, owner, id, path string, content io.Reader) error {
	if err := f.lookup(owner, id); err != nil {
		return err
	}
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	f.files[path] = data
	return nil
}

func (f *fakeSandboxes) ReadFile(_ context.Context, owner, id, path string, w io.Writer) (int64, error) {
	if err := f.lookup(owner, id); err != nil {
		return 0, err
	}
	if f.readErr != nil {
		return 0, f.readErr
	}
	n, err := w.Write(f.files[path])
	return int64(n), err
}

const aliceKey = "alice-secret-key"

func newAPI(t *testing.T) (*fakeSandboxes, *httptest.Server) {
	t.Helper()
	d := config.Digest(aliceKey)
	yaml := "api_keys:\n  - name: alice\n    sha256: " + hex.EncodeToString(d[:]) + "\n"
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeSandboxes{files: map[string][]byte{}}
	s, err := New(cfg, fake, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return fake, srv
}

// result is a response read in full, so tests never manage bodies.
type result struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// call sends a request and returns the whole response.
func call(t *testing.T, srv *httptest.Server, method, path, key string, body io.Reader) result {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return result{StatusCode: resp.StatusCode, Header: resp.Header, Body: data}
}

func errorCode(t *testing.T, res result) string {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(res.Body, &body); err != nil {
		t.Fatalf("decode error body %q: %v", res.Body, err)
	}
	return body.Error.Code
}

func TestAuthentication(t *testing.T) {
	t.Parallel()
	_, srv := newAPI(t)
	for name, key := range map[string]string{"missing": "", "wrong": "not-the-key"} {
		resp := call(t, srv, http.MethodGet, "/v1/sandboxes", key, nil)
		if resp.StatusCode != http.StatusUnauthorized || errorCode(t, resp) != "unauthorized" {
			t.Errorf("%s key: status %d, want 401 unauthorized", name, resp.StatusCode)
		}
	}
	resp := call(t, srv, http.MethodGet, "/v1/sandboxes", aliceKey, nil)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Request-Id") == "" {
		t.Errorf("valid key: status %d, request id %q; want 200 with an id", resp.StatusCode, resp.Header.Get("X-Request-Id"))
	}
	if resp := call(t, srv, http.MethodGet, "/healthz", "", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz needs no key; got status %d", resp.StatusCode)
	}
}

func TestCreateLimits(t *testing.T) {
	t.Parallel()
	fake, srv := newAPI(t)
	resp := call(t, srv, http.MethodPost, "/v1/sandboxes", aliceKey, strings.NewReader(`{"memory_mib": 1024}`))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d", resp.StatusCode)
	}
	var v sandboxView
	_ = json.Unmarshal(resp.Body, &v)
	got := fake.created[0]
	if got.Egress.Mode != "none" {
		t.Errorf("default egress = %q, want none", got.Egress.Mode)
	}
	if got.Owner != "alice" || got.Image != "default" || got.Limits.MemoryMiB != 1024 || got.Limits.VCPUs != 1 ||
		got.Limits.Lifetime != time.Hour || v.ID != "sb-1" || v.MemoryMiB != 1024 {
		t.Errorf("create request %+v, view %+v; want defaults except the requested memory", got, v)
	}

	for body, want := range map[string]string{
		`{"memory_mib": 999999}`:                                          "invalid_limits",
		`{"vcpus": -1}`:                                                   "invalid_limits",
		`{"lifetime_seconds": 1e9}`:                                       "invalid_request",
		`{"surprise": true}`:                                              "invalid_request",
		`{"egress": {"mode": "bridge"}}`:                                  "invalid_egress",
		`{"egress": {"mode": "allowlist"}}`:                               "invalid_egress",
		`{"egress": {"mode": "allowlist", "allow": ["10.0.0.1"]}}`:        "invalid_egress",
		`{"egress": {"mode": "internet", "allow": ["203.0.113.1"]}}`:      "invalid_egress",
		`{"egress": {"mode": "allowlist", "allow": ["169.254.169.254"]}}`: "invalid_egress",
	} {
		resp := call(t, srv, http.MethodPost, "/v1/sandboxes", aliceKey, strings.NewReader(body))
		if resp.StatusCode != http.StatusBadRequest || errorCode(t, resp) != want {
			t.Errorf("create %s: status %d, want 400 %s", body, resp.StatusCode, want)
		}
	}
}

func TestCreateCapacityAndBootFailure(t *testing.T) {
	t.Parallel()
	fake, srv := newAPI(t)
	fake.createErr = sandbox.ErrCapacity
	resp := call(t, srv, http.MethodPost, "/v1/sandboxes", aliceKey, strings.NewReader(`{}`))
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Errorf("at capacity: status %d, Retry-After %q; want 429 with Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	fake.createErr = errors.New("jailer exploded")
	resp = call(t, srv, http.MethodPost, "/v1/sandboxes", aliceKey, strings.NewReader(`{}`))
	if resp.StatusCode != http.StatusServiceUnavailable || errorCode(t, resp) != "boot_failed" {
		t.Errorf("boot failure: status %d, want 503 boot_failed", resp.StatusCode)
	}
}

func TestSandboxNotFound(t *testing.T) {
	t.Parallel()
	_, srv := newAPI(t)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/sandboxes/sb-2", ""},
		{http.MethodDelete, "/v1/sandboxes/sb-2", ""},
		{http.MethodPost, "/v1/sandboxes/sb-2/exec", `{"argv":["true"]}`},
		{http.MethodGet, "/v1/sandboxes/sb-2/files?path=/root/a", ""},
	} {
		resp := call(t, srv, tc.method, tc.path, aliceKey, strings.NewReader(tc.body))
		if resp.StatusCode != http.StatusNotFound || errorCode(t, resp) != "not_found" {
			t.Errorf("%s %s: status %d, want 404 not_found", tc.method, tc.path, resp.StatusCode)
		}
	}
}

func TestExecStreamsFrames(t *testing.T) {
	t.Parallel()
	fake, srv := newAPI(t)
	fake.execFn = func(onOutput func(*agentproto.Frame) error) (*agentproto.Frame, error) {
		_ = onOutput(&agentproto.Frame{Type: agentproto.FrameStdout, Data: []byte("hi\n")})
		return &agentproto.Frame{Type: agentproto.FrameExit, ExitCode: 7}, nil
	}
	resp := call(t, srv, http.MethodPost, "/v1/sandboxes/sb-1/exec", aliceKey, strings.NewReader(`{"argv":["echo","hi"]}`))
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("exec: status %d, content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	fr := agentproto.NewFrameReader(bytes.NewReader(resp.Body))
	out, _ := fr.Next()
	exit, _ := fr.Next()
	if string(out.Data) != "hi\n" || exit.Type != agentproto.FrameExit || exit.ExitCode != 7 {
		t.Errorf("frames %+v then %+v; want stdout hi then exit 7", out, exit)
	}
	if fake.execReq.TimeoutMS != time.Minute.Milliseconds() {
		t.Errorf("exec timeout = %d ms, want the configured default of one minute", fake.execReq.TimeoutMS)
	}
}

func TestExecFailureMidStream(t *testing.T) {
	t.Parallel()
	fake, srv := newAPI(t)
	fake.execFn = func(onOutput func(*agentproto.Frame) error) (*agentproto.Frame, error) {
		_ = onOutput(&agentproto.Frame{Type: agentproto.FrameStdout, Data: []byte("partial")})
		return nil, agentproto.ErrStreamTruncated
	}
	resp := call(t, srv, http.MethodPost, "/v1/sandboxes/sb-1/exec", aliceKey, strings.NewReader(`{"argv":["crash"]}`))
	fr := agentproto.NewFrameReader(bytes.NewReader(resp.Body))
	_, _ = fr.Next()
	last, err := fr.Next()
	if err != nil || last.Type != agentproto.FrameError {
		t.Errorf("stream ended with %+v, %v; want an error frame", last, err)
	}
}

func TestExecRejectsBadRequests(t *testing.T) {
	t.Parallel()
	_, srv := newAPI(t)
	for _, body := range []string{`{"argv":[]}`, `{"argv":["x"],"timeout_ms":999999999999}`, `not json`} {
		resp := call(t, srv, http.MethodPost, "/v1/sandboxes/sb-1/exec", aliceKey, strings.NewReader(body))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("exec %s: status %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestFiles(t *testing.T) {
	t.Parallel()
	fake, srv := newAPI(t)
	resp := call(t, srv, http.MethodPut, "/v1/sandboxes/sb-1/files?path=/root/in.csv", aliceKey, strings.NewReader("a,b\n"))
	if resp.StatusCode != http.StatusNoContent || string(fake.files["/root/in.csv"]) != "a,b\n" {
		t.Fatalf("put: status %d, stored %q", resp.StatusCode, fake.files["/root/in.csv"])
	}
	resp = call(t, srv, http.MethodGet, "/v1/sandboxes/sb-1/files?path=/root/in.csv", aliceKey, nil)
	got := resp.Body
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, []byte("a,b\n")) {
		t.Errorf("get: status %d, body %q", resp.StatusCode, got)
	}
	if resp := call(t, srv, http.MethodGet, "/v1/sandboxes/sb-1/files?path=relative", aliceKey, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("relative path: status %d, want 400", resp.StatusCode)
	}

	// The guest agent's own client errors pass through with their status.
	fake.readErr = &agentclient.AgentError{StatusCode: http.StatusNotFound, Message: "no such file"}
	resp = call(t, srv, http.MethodGet, "/v1/sandboxes/sb-1/files?path=/root/missing", aliceKey, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing file: status %d, want 404", resp.StatusCode)
	}
	fake.readErr = errors.New("vsock reset")
	resp = call(t, srv, http.MethodGet, "/v1/sandboxes/sb-1/files?path=/root/x", aliceKey, nil)
	if resp.StatusCode != http.StatusBadGateway || errorCode(t, resp) != "sandbox_error" {
		t.Errorf("sandbox failure: status %d, want 502 sandbox_error", resp.StatusCode)
	}
}

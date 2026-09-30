package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/shusingh/roundhouse/internal/agentproto"
	"github.com/shusingh/roundhouse/internal/netns"
	"github.com/shusingh/roundhouse/internal/sandbox"
)

// maxJSONBytes bounds JSON request bodies; exec's stdin is the largest field.
const maxJSONBytes = agentproto.MaxRequestBytes

// createRequest is the body of POST /v1/sandboxes. Zero fields take the
// configured defaults.
type createRequest struct {
	Image              string `json:"image,omitempty"`
	VCPUs              int    `json:"vcpus,omitempty"`
	MemoryMiB          int    `json:"memory_mib,omitempty"`
	LifetimeSeconds    int64  `json:"lifetime_seconds,omitempty"`
	IdleTimeoutSeconds int64  `json:"idle_timeout_seconds,omitempty"`
	// Egress defaults to mode "none": no network access.
	Egress *egressRequest `json:"egress,omitempty"`
}

// egressRequest is a sandbox's egress policy as the API accepts it.
type egressRequest struct {
	Mode  string   `json:"mode"`
	Allow []string `json:"allow,omitempty"`
}

func parseEgress(req *egressRequest) (netns.Egress, error) {
	if req == nil {
		return netns.Egress{Mode: netns.ModeNone}, nil
	}
	e := netns.Egress{Mode: netns.Mode(req.Mode)}
	for _, a := range req.Allow {
		d, err := netns.ParseDestination(a)
		if err != nil {
			return e, err
		}
		e.Allow = append(e.Allow, d)
	}
	return e, e.Validate()
}

// sandboxView is a sandbox as the API returns it.
type sandboxView struct {
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

func view(i sandbox.Info) sandboxView {
	return sandboxView{
		ID: i.ID, Image: i.Image, State: string(i.State), VCPUs: i.Limits.VCPUs, MemoryMiB: i.Limits.MemoryMiB,
		CreatedAt: i.CreatedAt.UTC(), ExpiresAt: i.ExpiresAt.UTC(),
		IdleTimeoutSeconds: int64(i.Limits.IdleTimeout / time.Second), Egress: string(i.Egress.Mode),
		StartPath: string(i.StartPath), ActiveCalls: i.ActiveCalls,
	}
}

// execRequest is the body of POST /v1/sandboxes/{id}/exec.
type execRequest struct {
	Argv      []string          `json:"argv"`
	Env       map[string]string `json:"env,omitempty"`
	Dir       string            `json:"dir,omitempty"`
	Stdin     []byte            `json:"stdin,omitempty"`
	TimeoutMS int64             `json:"timeout_ms,omitempty"`
}

func (s *Server) limits(req createRequest) (sandbox.Limits, error) {
	d, m := s.cfg.Defaults, s.cfg.Max
	l := sandbox.Limits{
		VCPUs: pick(req.VCPUs, d.VCPUs), MemoryMiB: pick(req.MemoryMiB, d.MemoryMiB),
		Lifetime:    pickDuration(req.LifetimeSeconds, d.Lifetime),
		IdleTimeout: pickDuration(req.IdleTimeoutSeconds, d.IdleTimeout),
	}
	switch {
	case req.VCPUs < 0 || req.MemoryMiB < 0 || req.LifetimeSeconds < 0 || req.IdleTimeoutSeconds < 0:
		return l, errors.New("limits must not be negative")
	case l.VCPUs > m.VCPUs:
		return l, fmt.Errorf("vcpus %d exceeds the maximum %d", l.VCPUs, m.VCPUs)
	case l.MemoryMiB > m.MemoryMiB:
		return l, fmt.Errorf("memory_mib %d exceeds the maximum %d", l.MemoryMiB, m.MemoryMiB)
	case l.Lifetime > m.Lifetime:
		return l, fmt.Errorf("lifetime %v exceeds the maximum %v", l.Lifetime, m.Lifetime)
	case l.IdleTimeout > m.IdleTimeout:
		return l, fmt.Errorf("idle timeout %v exceeds the maximum %v", l.IdleTimeout, m.IdleTimeout)
	}
	return l, nil
}

func pick(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func pickDuration(seconds int64, def time.Duration) time.Duration {
	if seconds == 0 {
		return def
	}
	return time.Duration(seconds) * time.Second
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !decode(w, r, &req) {
		return
	}
	limits, err := s.limits(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_limits", err.Error())
		return
	}
	egress, err := parseEgress(req.Egress)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_egress", err.Error())
		return
	}
	image := req.Image
	if image == "" {
		image = s.cfg.Image
	}
	info, err := s.sandboxes.Create(r.Context(), sandbox.CreateRequest{
		Owner: owner(r), Quota: quota(r), Image: image, Limits: limits, Egress: egress,
	})
	switch {
	case errors.Is(err, sandbox.ErrCapacity), errors.Is(err, sandbox.ErrQuota):
		s.writeSandboxError(w, r, err)
	case err != nil:
		s.logger.Error("create sandbox", slog.String("error", err.Error()))
		writeError(w, http.StatusServiceUnavailable, "boot_failed", "the sandbox failed to start")
	default:
		writeJSON(w, http.StatusCreated, view(info))
	}
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	list := s.sandboxes.List(owner(r))
	views := make([]sandboxView, 0, len(list))
	for _, i := range list {
		views = append(views, view(i))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": views})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	info, err := s.sandboxes.Get(owner(r), r.PathValue("id"))
	if err != nil {
		s.writeSandboxError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view(info))
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.sandboxes.Delete(r.Context(), owner(r), r.PathValue("id")); err != nil {
		s.writeSandboxError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	var body execRequest
	if !decode(w, r, &body) {
		return
	}
	req := agentproto.ExecRequest{
		Argv: body.Argv, Env: body.Env, Dir: body.Dir, Stdin: body.Stdin,
		TimeoutMS: body.TimeoutMS,
	}
	if req.TimeoutMS == 0 {
		req.TimeoutMS = s.cfg.Defaults.ExecTimeout.Milliseconds()
	}
	if max := s.cfg.Max.ExecTimeout.Milliseconds(); req.TimeoutMS > max {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("timeout_ms exceeds the maximum %d", max))
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// Frames stream as newline-delimited JSON. The status line is sent with the
	// first frame, so a failure before any output still gets a JSON error.
	stream := &frameStream{w: w, rc: http.NewResponseController(w)}
	exit, err := s.sandboxes.Exec(r.Context(), owner(r), r.PathValue("id"), req, stream.write)
	switch {
	case err == nil:
		_ = stream.write(exit)
	case !stream.started:
		s.writeSandboxError(w, r, err)
	default:
		// The stream began and then failed (for example, the VM died): end it
		// with an error frame so the client knows no exit status is coming.
		s.logger.Warn("exec stream failed", slog.String("sandbox_id", r.PathValue("id")), slog.String("error", err.Error()))
		_ = stream.write(&agentproto.Frame{Type: agentproto.FrameError, Error: "the sandbox failed during the command"})
	}
}

type frameStream struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	fw      *agentproto.FrameWriter
	started bool
}

func (fs *frameStream) write(f *agentproto.Frame) error {
	if !fs.started {
		fs.started = true
		fs.w.Header().Set("Content-Type", "application/x-ndjson")
		fs.w.WriteHeader(http.StatusOK)
		fs.fw = agentproto.NewFrameWriter(fs.w)
	}
	if err := fs.fw.Write(f); err != nil {
		return err
	}
	return fs.rc.Flush()
}

func (s *Server) handlePutFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if err := agentproto.ValidatePath(path); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_path", err.Error())
		return
	}
	body := http.MaxBytesReader(w, r.Body, agentproto.MaxFileBytes)
	if err := s.sandboxes.WriteFile(r.Context(), owner(r), r.PathValue("id"), path, body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("file larger than %d bytes", agentproto.MaxFileBytes))
			return
		}
		s.writeSandboxError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if err := agentproto.ValidatePath(path); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_path", err.Error())
		return
	}
	lw := &lazyWriter{w: w}
	if _, err := s.sandboxes.ReadFile(r.Context(), owner(r), r.PathValue("id"), path, lw); err != nil && !lw.started {
		s.writeSandboxError(w, r, err)
		return
	}
	if !lw.started {
		lw.start() // an empty file
	}
}

// lazyWriter sends the success status only when the first byte arrives, so an
// error before then can still be reported properly.
type lazyWriter struct {
	w       http.ResponseWriter
	started bool
}

func (l *lazyWriter) start() {
	l.started = true
	l.w.Header().Set("Content-Type", "application/octet-stream")
	l.w.WriteHeader(http.StatusOK)
}

func (l *lazyWriter) Write(p []byte) (int, error) {
	if !l.started {
		l.start()
	}
	return l.w.Write(p)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body := http.MaxBytesReader(w, r.Body, maxJSONBytes)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "decode request: "+err.Error())
		return false
	}
	// Consume the body: net/http notices a client hanging up only after that,
	// and a long exec relies on it to stop the command.
	if _, err := io.Copy(io.Discard, body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "read request: "+err.Error())
		return false
	}
	return true
}

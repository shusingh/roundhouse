// Package api is Roundhouse's public HTTP API: JSON over HTTP, bearer API
// keys, versioned under /v1. See docs/engineering/design.md section 13.
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/shusingh/roundhouse/internal/agentclient"
	"github.com/shusingh/roundhouse/internal/agentproto"
	"github.com/shusingh/roundhouse/internal/config"
	"github.com/shusingh/roundhouse/internal/sandbox"
)

// Sandboxes is the manager as the API uses it.
type Sandboxes interface {
	Create(ctx context.Context, req sandbox.CreateRequest) (sandbox.Info, error)
	Get(owner, id string) (sandbox.Info, error)
	List(owner string) []sandbox.Info
	Delete(ctx context.Context, owner, id string) error
	Exec(ctx context.Context, owner, id string, req agentproto.ExecRequest, onOutput func(*agentproto.Frame) error) (*agentproto.Frame, error)
	WriteFile(ctx context.Context, owner, id, path string, content io.Reader) error
	ReadFile(ctx context.Context, owner, id, path string, w io.Writer) (int64, error)
}

// Server serves the API.
type Server struct {
	sandboxes Sandboxes
	cfg       config.Sandbox
	keys      []apiKey
	logger    *slog.Logger
	now       func() time.Time
}

type apiKey struct {
	owner  string
	digest [32]byte
}

// New returns an API server over sandboxes, with limits and keys from cfg.
func New(cfg config.Config, sandboxes Sandboxes, logger *slog.Logger) (*Server, error) {
	s := &Server{sandboxes: sandboxes, cfg: cfg.Sandbox, logger: logger, now: time.Now}
	for _, k := range cfg.APIKeys {
		raw, err := hex.DecodeString(k.SHA256)
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("api key %s: invalid digest", k.Name)
		}
		var d [32]byte
		copy(d[:], raw)
		s.keys = append(s.keys, apiKey{owner: k.Name, digest: d})
	}
	return s, nil
}

type ctxKey int

const ownerKey ctxKey = iota

// Handler returns the API's HTTP handler with authentication, request IDs,
// access logs, and panic recovery.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	v1 := http.NewServeMux()
	v1.HandleFunc("POST /v1/sandboxes", s.handleCreate)
	v1.HandleFunc("GET /v1/sandboxes", s.handleList)
	v1.HandleFunc("GET /v1/sandboxes/{id}", s.handleGet)
	v1.HandleFunc("DELETE /v1/sandboxes/{id}", s.handleDelete)
	v1.HandleFunc("POST /v1/sandboxes/{id}/exec", s.handleExec)
	v1.HandleFunc("PUT /v1/sandboxes/{id}/files", s.handlePutFile)
	v1.HandleFunc("GET /v1/sandboxes/{id}/files", s.handleGetFile)
	mux.Handle("/v1/", s.authenticate(v1))
	return s.observe(mux)
}

// authenticate resolves the bearer key to its owner. Every configured key is
// compared, in constant time, so timing reveals neither which key matched nor
// how many are configured beyond their count.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer API key")
			return
		}
		presented := config.Digest(token)
		owner := ""
		for _, k := range s.keys {
			if subtle.ConstantTimeCompare(presented[:], k.digest[:]) == 1 {
				owner = k.owner
			}
		}
		if owner == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "unknown API key")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ownerKey, owner)))
	})
}

func owner(r *http.Request) string {
	o, _ := r.Context().Value(ownerKey).(string)
	return o
}

// observe assigns a request ID, logs one access line per request (method,
// path pattern, status, duration; never bodies), and turns panics into 500s.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		var idBytes [8]byte
		_, _ = rand.Read(idBytes[:])
		requestID := hex.EncodeToString(idBytes[:])
		w.Header().Set("X-Request-Id", requestID)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if p := recover(); p != nil {
				s.logger.Error("panic serving request", slog.String("request_id", requestID), slog.Any("panic", p))
				if !rec.wroteHeader {
					writeError(rec, http.StatusInternalServerError, "internal", "internal error")
				}
			}
			s.logger.Info("request",
				slog.String("request_id", requestID), slog.String("method", r.Method),
				slog.String("path", r.Pattern), slog.String("sandbox_id", r.PathValue("id")),
				slog.Int("status", rec.status), slog.Duration("duration", s.now().Sub(start)))
		}()
		next.ServeHTTP(rec, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	r.wroteHeader = true
	return r.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the underlying writer's Flush.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// errorBody is the one shape every API error takes.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorBody
	body.Error.Code, body.Error.Message = code, message
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeSandboxError maps manager and guest errors to API responses. The
// guest agent's own client errors (bad path, missing file, too large) pass
// through with their status; anything else from the sandbox is a 502.
func (s *Server) writeSandboxError(w http.ResponseWriter, r *http.Request, err error) {
	var agentErr *agentclient.AgentError
	switch {
	case errors.Is(err, sandbox.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such sandbox")
	case errors.Is(err, sandbox.ErrNotReady):
		writeError(w, http.StatusConflict, "not_ready", "sandbox is starting or stopping")
	case errors.Is(err, sandbox.ErrCapacity):
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, "capacity", "host is at sandbox capacity")
	case errors.Is(err, context.Canceled):
		// The client went away; nobody reads the response.
	case errors.As(err, &agentErr) && agentErr.StatusCode >= 400 && agentErr.StatusCode < 500:
		writeError(w, agentErr.StatusCode, codeForStatus(agentErr.StatusCode), agentErr.Message)
	default:
		s.logger.Warn("sandbox call failed", slog.String("sandbox_id", r.PathValue("id")), slog.String("error", err.Error()))
		writeError(w, http.StatusBadGateway, "sandbox_error", "the sandbox failed to handle the request")
	}
}

func codeForStatus(status int) string {
	switch status {
	case http.StatusNotFound:
		return "not_found"
	case http.StatusRequestEntityTooLarge:
		return "too_large"
	default:
		return "bad_request"
	}
}

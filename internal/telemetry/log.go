// Package telemetry builds Roundhouse's logger. Logs are JSON, one event per
// line, and never contain commands, output, environment values, or file
// contents (docs/engineering/coding-standards.md section 2).
package telemetry

import (
	"fmt"
	"io"
	"log/slog"
)

// NewLogger returns a JSON logger at the named level: debug, info, warn, or
// error.
func NewLogger(w io.Writer, level string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("log level %q: %w", level, err)
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l})), nil
}

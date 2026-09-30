package agentproto

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// FrameType identifies a frame in an exec response stream.
type FrameType string

// Frame types. A stream is zero or more stdout and stderr frames followed by
// exactly one exit frame, or by one error frame if the command never started.
const (
	FrameStdout FrameType = "stdout"
	FrameStderr FrameType = "stderr"
	FrameExit   FrameType = "exit"
	FrameError  FrameType = "error"
)

// Frame is one line of an exec response stream.
type Frame struct {
	Type FrameType `json:"type"`
	// Data is an output chunk (stdout and stderr frames).
	Data []byte `json:"data,omitempty"`
	// ExitCode is the process's exit status (exit frames). It is -1 when the
	// process was killed by a signal, including by its time limit.
	ExitCode int `json:"exit_code,omitempty"`
	// TimedOut reports that the time limit killed the process (exit frames).
	TimedOut bool `json:"timed_out,omitempty"`
	// DurationMS is the command's wall time in milliseconds (exit frames).
	DurationMS int64 `json:"duration_ms,omitempty"`
	// Error says why the command could not start (error frames).
	Error string `json:"error,omitempty"`
}

// Terminal reports whether f ends a stream.
func (f *Frame) Terminal() bool {
	return f.Type == FrameExit || f.Type == FrameError
}

func (f *Frame) validate() error {
	switch f.Type {
	case FrameStdout, FrameStderr:
		if len(f.Data) > MaxChunkBytes {
			return fmt.Errorf("%s frame carries %d bytes, limit %d", f.Type, len(f.Data), MaxChunkBytes)
		}
	case FrameExit:
		if len(f.Data) > 0 {
			return errors.New("exit frame carries data")
		}
	case FrameError:
		if f.Error == "" {
			return errors.New("error frame without a message")
		}
	default:
		return fmt.Errorf("unknown frame type %q", f.Type)
	}
	return nil
}

// FrameWriter encodes frames, one per line.
type FrameWriter struct {
	w   io.Writer
	buf []byte
}

// NewFrameWriter returns a writer that encodes frames to w. The caller
// flushes w as needed; each frame is written with a single Write call.
func NewFrameWriter(w io.Writer) *FrameWriter {
	return &FrameWriter{w: w}
}

// Write encodes one frame. Output larger than MaxChunkBytes must be split by
// the caller.
func (fw *FrameWriter) Write(f *Frame) error {
	if err := f.validate(); err != nil {
		return fmt.Errorf("write frame: %w", err)
	}
	line, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("encode frame: %w", err)
	}
	fw.buf = append(append(fw.buf[:0], line...), '\n')
	if _, err := fw.w.Write(fw.buf); err != nil {
		return fmt.Errorf("write frame: %w", err)
	}
	return nil
}

// ErrStreamTruncated means an exec stream ended before its terminal frame,
// for example because the guest or the connection died mid-command.
var ErrStreamTruncated = errors.New("exec stream ended without an exit frame")

// FrameReader decodes an exec response stream, enforcing the frame size limit
// and the stream grammar (output frames, then one terminal frame).
type FrameReader struct {
	r    *bufio.Reader
	done bool
}

// NewFrameReader returns a reader that decodes frames from r.
func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{r: bufio.NewReaderSize(r, 4096)}
}

// Next returns the next frame. After the terminal frame it returns io.EOF. A
// stream that ends early returns ErrStreamTruncated; an oversized or invalid
// frame returns an error and the stream must be abandoned.
func (fr *FrameReader) Next() (*Frame, error) {
	if fr.done {
		return nil, io.EOF
	}
	line, err := fr.readLine()
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, ErrStreamTruncated
		}
		return nil, err
	}
	var f Frame
	if err := json.Unmarshal(line, &f); err != nil {
		return nil, fmt.Errorf("decode frame: %w", err)
	}
	if err := f.validate(); err != nil {
		return nil, fmt.Errorf("invalid frame: %w", err)
	}
	if f.Terminal() {
		fr.done = true
	}
	return &f, nil
}

// readLine reads one newline-terminated line of at most MaxFrameBytes,
// without buffering more than that.
func (fr *FrameReader) readLine() ([]byte, error) {
	var line []byte
	for {
		chunk, err := fr.r.ReadSlice('\n')
		if len(line)+len(chunk) > MaxFrameBytes {
			return nil, fmt.Errorf("frame longer than %d bytes", MaxFrameBytes)
		}
		line = append(line, chunk...)
		switch {
		case err == nil:
			return line[:len(line)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

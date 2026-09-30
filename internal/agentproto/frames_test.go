package agentproto

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestFrameRoundTrip(t *testing.T) {
	t.Parallel()
	frames := []*Frame{
		{Type: FrameStdout, Data: []byte("hello\n")},
		{Type: FrameStderr, Data: []byte{0x00, 0xff, '\n'}}, // binary output survives
		{Type: FrameStdout, Data: bytes.Repeat([]byte("x"), MaxChunkBytes)},
		{Type: FrameExit, ExitCode: 3, DurationMS: 42},
	}
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf)
	for _, f := range frames {
		if err := fw.Write(f); err != nil {
			t.Fatalf("Write(%s): %v", f.Type, err)
		}
	}
	if n := strings.Count(buf.String(), "\n"); n != len(frames) {
		t.Fatalf("encoded %d lines, want %d", n, len(frames))
	}

	fr := NewFrameReader(&buf)
	var got []*Frame
	for {
		f, err := fr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, f)
	}
	if diff := cmp.Diff(frames, got); diff != "" {
		t.Errorf("round trip (-want +got):\n%s", diff)
	}
}

func TestFrameWriterRejectsInvalid(t *testing.T) {
	t.Parallel()
	fw := NewFrameWriter(io.Discard)
	for name, f := range map[string]*Frame{
		"oversized chunk":     {Type: FrameStdout, Data: make([]byte, MaxChunkBytes+1)},
		"unknown type":        {Type: "stdin"},
		"exit with data":      {Type: FrameExit, Data: []byte("x")},
		"error without cause": {Type: FrameError},
	} {
		if err := fw.Write(f); err == nil {
			t.Errorf("%s: Write = nil, want an error", name)
		}
	}
}

func TestFrameReaderErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		stream string
		want   error // checked with errors.Is when non-nil
	}{
		{"no terminal frame", `{"type":"stdout","data":"aGk="}` + "\n", ErrStreamTruncated},
		{"empty stream", "", ErrStreamTruncated},
		{"cut mid-line", `{"type":"stdout","da`, ErrStreamTruncated},
		{"not json", "hello\n", nil},
		{"unknown type", `{"type":"stdin"}` + "\n", nil},
		{"oversized line", `{"type":"stdout","data":"` + strings.Repeat("A", MaxFrameBytes) + `"}` + "\n", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fr := NewFrameReader(strings.NewReader(tc.stream))
			var err error
			for err == nil {
				_, err = fr.Next()
			}
			if errors.Is(err, io.EOF) {
				t.Fatalf("stream %q ended cleanly, want an error", tc.name)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestFrameReaderStopsAfterTerminal(t *testing.T) {
	t.Parallel()
	stream := `{"type":"exit","exit_code":0}` + "\n" + `{"type":"stdout","data":"aGk="}` + "\n"
	fr := NewFrameReader(strings.NewReader(stream))
	if f, err := fr.Next(); err != nil || f.Type != FrameExit {
		t.Fatalf("first Next = %v, %v; want the exit frame", f, err)
	}
	if _, err := fr.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("Next after the exit frame = %v, want io.EOF", err)
	}
}

// FuzzFrameReader checks that arbitrary input never panics the decoder and
// never yields a frame that violates the protocol's limits.
func FuzzFrameReader(f *testing.F) {
	f.Add([]byte(`{"type":"stdout","data":"aGk="}` + "\n" + `{"type":"exit","exit_code":1}` + "\n"))
	f.Add([]byte(`{"type":"error","error":"no such file"}` + "\n"))
	f.Add([]byte("\n\n\n"))
	f.Add([]byte(`{"type":"stdout","data":null}` + "\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		fr := NewFrameReader(bytes.NewReader(data))
		for range 1000 {
			frame, err := fr.Next()
			if err != nil {
				return
			}
			if len(frame.Data) > MaxChunkBytes {
				t.Fatalf("decoded a %d-byte chunk, limit %d", len(frame.Data), MaxChunkBytes)
			}
		}
	})
}

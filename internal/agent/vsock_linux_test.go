package agent

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

// net/http aborts its background read with a past deadline and ignores the
// resulting error only if it is a net.Error timeout. A plain *os.File returns
// *fs.PathError instead, which made the agent's request contexts end
// mid-handler over vsock. A pipe stands in for the socket here: both are
// pollable descriptors wrapped in *os.File.
func TestVsockConnDeadlineIsNetTimeout(t *testing.T) {
	t.Parallel()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	c := &vsockConn{File: r}
	defer func() { _ = c.Close() }()

	if err := c.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	_, err = c.Read(make([]byte, 1))
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("Read after a past deadline returned %T %v; want a net.Error with Timeout() true", err, err)
	}
	if _, ok := err.(net.Error); !ok { //nolint:errorlint // net/http uses a direct type assertion, so this must hold
		t.Fatalf("error %T does not satisfy net.Error by type assertion", err)
	}
}

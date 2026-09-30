package agent

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// vsockAddr is a vsock endpoint: a context ID and a port.
type vsockAddr struct {
	cid  uint32
	port uint32
}

func (a vsockAddr) Network() string { return "vsock" }
func (a vsockAddr) String() string  { return fmt.Sprintf("%d:%d", a.cid, a.port) }

// ListenVsock listens for vsock connections on port, from any context ID.
// The standard library has no vsock support, so the socket is created with
// system calls and driven through Go's network poller via os.File, which
// keeps Accept, Read, and Write non-blocking and deadline-aware.
func ListenVsock(port uint32) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("create vsock socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("bind vsock port %d: %w", port, err)
	}
	if err := unix.Listen(fd, 128); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("listen on vsock port %d: %w", port, err)
	}
	f := os.NewFile(uintptr(fd), "vsock-listener")
	rc, err := f.SyscallConn()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("vsock listener: %w", err)
	}
	return &vsockListener{f: f, rc: rc, addr: vsockAddr{cid: unix.VMADDR_CID_ANY, port: port}}, nil
}

type vsockListener struct {
	f    *os.File
	rc   syscall.RawConn
	addr vsockAddr
}

// Accept waits for the next connection. The poller parks the goroutine until
// the socket is readable, so no thread is blocked while idle.
func (l *vsockListener) Accept() (net.Conn, error) {
	var (
		nfd       int
		sa        unix.Sockaddr
		acceptErr error
	)
	err := l.rc.Read(func(fd uintptr) bool {
		nfd, sa, acceptErr = unix.Accept4(int(fd), unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
		return !errors.Is(acceptErr, unix.EAGAIN)
	})
	if err != nil {
		return nil, fmt.Errorf("accept vsock: %w", err)
	}
	if acceptErr != nil {
		return nil, fmt.Errorf("accept vsock: %w", acceptErr)
	}
	remote := vsockAddr{}
	if vm, ok := sa.(*unix.SockaddrVM); ok {
		remote = vsockAddr{cid: vm.CID, port: vm.Port}
	}
	return &vsockConn{File: os.NewFile(uintptr(nfd), "vsock-conn"), local: l.addr, remote: remote}, nil
}

func (l *vsockListener) Close() error   { return l.f.Close() }
func (l *vsockListener) Addr() net.Addr { return l.addr }

// vsockConn adapts a connected vsock socket to net.Conn. os.File supplies
// the I/O and deadlines for pollable descriptors; vsockConn translates its
// errors into *net.OpError, as real network connections return.
//
// That translation matters: net/http interrupts its background read with a
// past deadline and expects a net.Error timeout back. os.File reports the
// deadline as an *fs.PathError, which is not a net.Error, so without it the
// server takes the interruption for a closed connection and cancels the
// request's context mid-handler.
type vsockConn struct {
	*os.File
	local, remote vsockAddr
}

func (c *vsockConn) Read(p []byte) (int, error) {
	n, err := c.File.Read(p)
	return n, c.opError("read", err)
}

func (c *vsockConn) Write(p []byte) (int, error) {
	n, err := c.File.Write(p)
	return n, c.opError("write", err)
}

// opError wraps a non-nil, non-EOF error from os.File in *net.OpError,
// unwrapping the *fs.PathError so Timeout reports correctly.
func (c *vsockConn) opError(op string, err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		err = pathErr.Err
	}
	return &net.OpError{Op: op, Net: "vsock", Source: c.local, Addr: c.remote, Err: err}
}

func (c *vsockConn) LocalAddr() net.Addr  { return c.local }
func (c *vsockConn) RemoteAddr() net.Addr { return c.remote }

func (c *vsockConn) SetDeadline(t time.Time) error      { return c.File.SetDeadline(t) }
func (c *vsockConn) SetReadDeadline(t time.Time) error  { return c.File.SetReadDeadline(t) }
func (c *vsockConn) SetWriteDeadline(t time.Time) error { return c.File.SetWriteDeadline(t) }

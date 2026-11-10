package tcp_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ninepeach/go-netx/tcp"
)

// What io.Copy and proxies look for on a connection.
var (
	_ io.ReaderFrom                   = (*tcp.Conn)(nil)
	_ io.WriterTo                     = (*tcp.Conn)(nil)
	_ interface{ CloseWrite() error } = (*tcp.Conn)(nil)
	_ interface{ CloseRead() error }  = (*tcp.Conn)(nil)
)

// Half-close on an accepted *tcp.Conn: the client sends a request and
// closes its write side; the handler reads to EOF, answers, and closes its
// own write side with CloseWrite, so the client reads the answer to EOF
// while the connection is still open. On another connection, CloseRead
// makes Read return at once, with no data and an error (io.EOF on Unix,
// a shutdown error on Windows), instead of waiting for data.
//
// Checks: TCP-32
func TestConnHalfClose(t *testing.T) {
	done := make(chan error, 1)
	s, _ := start(t, func(_ context.Context, c *tcp.Conn) {
		req, err := io.ReadAll(c) // EOF only arrives through the client's half-close
		if err != nil || string(req) != "ping" {
			done <- errors.New("handler read " + string(req))
			return
		}
		if _, err := c.Write([]byte("pong")); err != nil {
			done <- err
			return
		}
		if err := c.CloseWrite(); err != nil {
			done <- err
			return
		}
		done <- nil
	}, tcp.Options{})
	c := dial(t, s).(*net.TCPConn)
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	resp, err := io.ReadAll(c) // EOF only arrives through the handler's CloseWrite
	if err != nil || string(resp) != "pong" {
		t.Fatalf("client read %q, %v", resp, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	conns, clients := accepted(t, 1)
	if err := conns[0].CloseRead(); err != nil {
		t.Fatal(err)
	}
	conns[0].SetReadDeadline(time.Now().Add(5 * time.Second)) // fail, not hang
	// io.EOF on Unix; Windows reports the shutdown as an error (WSAESHUTDOWN).
	n, err := conns[0].Read(make([]byte, 1))
	var ne net.Error
	if n != 0 || err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("Read after CloseRead = %d, %v; want 0 and an error without waiting", n, err)
	}
	if _, err := clients[0].Write([]byte("x")); err != nil { // the client may still send
		t.Fatal(err)
	}
}

// accepted returns n *tcp.Conns accepted by a server, with the client
// sides; the handlers stay open until the test ends.
func accepted(t *testing.T, n int) ([]*tcp.Conn, []*net.TCPConn) {
	t.Helper()
	got := make(chan *tcp.Conn, n)
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	s, _ := start(t, func(_ context.Context, c *tcp.Conn) {
		got <- c
		<-hold
	}, tcp.Options{})
	var conns []*tcp.Conn
	var clients []*net.TCPConn
	for i := 0; i < n; i++ {
		clients = append(clients, dial(t, s).(*net.TCPConn))
		conns = append(conns, <-got)
	}
	return conns, clients
}

// relay copies both ways between a and b with io.Copy, closing the write
// side of each destination when its source reaches EOF, as a proxy does.
func relay(a, b net.Conn) <-chan [2]error {
	out := make(chan [2]error, 1)
	var wg sync.WaitGroup
	var errs [2]error
	half := func(i int, dst, src net.Conn) {
		defer wg.Done()
		_, errs[i] = io.Copy(dst, src)
		dst.(interface{ CloseWrite() error }).CloseWrite()
	}
	wg.Add(2)
	go half(0, a, b)
	go half(1, b, a)
	go func() { wg.Wait(); out <- errs }()
	return out
}

// exchange sends 1MB each way between the two outer ends of a relay, each
// closing its write side when done, and checks both arrive intact.
func exchange(t *testing.T, x, y *net.TCPConn) {
	t.Helper()
	data := [2][]byte{make([]byte, 1<<20), make([]byte, 1<<20)}
	rand.Read(data[0])
	rand.Read(data[1])
	var recv [2][]byte // recv[i]: what end i received, data[1-i] if intact
	var wg sync.WaitGroup
	for i, c := range []*net.TCPConn{x, y} {
		wg.Add(2)
		go func() { defer wg.Done(); c.Write(data[i]); c.CloseWrite() }()
		go func() { defer wg.Done(); recv[i], _ = io.ReadAll(c) }()
	}
	wg.Wait()
	if !bytes.Equal(recv[0], data[1]) || !bytes.Equal(recv[1], data[0]) {
		t.Fatalf("received %d / %d bytes, want %d each, or contents differ", len(recv[0]), len(recv[1]), 1<<20)
	}
}

// io.Copy through *tcp.Conn in both directions, between two accepted
// connections and between an accepted connection and a dialed
// *net.TCPConn: 1MB each way arrives intact, and each side's half-close
// reaches the far end, so both copies finish without a full Close.
//
// Checks: TCP-33
func TestConnRelayWithIOCopy(t *testing.T) {
	t.Run("tcp.Conn to tcp.Conn", func(t *testing.T) {
		conns, clients := accepted(t, 2)
		done := relay(conns[0], conns[1])
		exchange(t, clients[0], clients[1])
		if errs := <-done; errs[0] != nil || errs[1] != nil {
			t.Fatal(errs)
		}
	})
	t.Run("tcp.Conn to net.TCPConn", func(t *testing.T) {
		conns, clients := accepted(t, 1)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		up, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer up.Close()
		far, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		defer far.Close()
		done := relay(conns[0], up)
		exchange(t, clients[0], far.(*net.TCPConn))
		if errs := <-done; errs[0] != nil || errs[1] != nil {
			t.Fatal(errs)
		}
	})
}

// countingConn is a caller's wrapper with logic in Read and Write.
type countingConn struct {
	net.Conn
	read, written atomic.Int64
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.written.Add(int64(n))
	return n, err
}

// A *tcp.Conn over a connection that is not TCP (here net.Pipe in a
// caller's wrapper, as from tcp.NewServer with a custom listener):
// CloseWrite and CloseRead return errors.ErrUnsupported and close nothing;
// ReadFrom and WriteTo copy through the wrapper's own Read and Write, so
// its logic is not bypassed, and the data is intact.
//
// Checks: TCP-34
func TestConnNonTCPUnderlying(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	wrapped := &countingConn{Conn: a}
	c := &tcp.Conn{Conn: wrapped}

	if err := c.CloseWrite(); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("CloseWrite = %v, want ErrUnsupported", err)
	}
	if err := c.CloseRead(); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("CloseRead = %v, want ErrUnsupported", err)
	}

	data := bytes.Repeat([]byte("0123456789"), 10_000)

	// ReadFrom: everything reaches the peer through the wrapper's Write.
	recv := make(chan []byte, 1)
	go func() {
		got := make([]byte, len(data))
		io.ReadFull(b, got)
		recv <- got
	}()
	if n, err := c.ReadFrom(bytes.NewReader(data)); n != int64(len(data)) || err != nil {
		t.Fatalf("ReadFrom = %d, %v", n, err)
	}
	if got := <-recv; !bytes.Equal(got, data) {
		t.Fatal("ReadFrom delivered different bytes")
	}
	if w := wrapped.written.Load(); w != int64(len(data)) {
		t.Fatalf("wrapper saw %d bytes written, want %d", w, len(data))
	}

	// WriteTo: everything from the peer, to EOF, through the wrapper's Read.
	go func() {
		b.Write(data)
		b.Close()
	}()
	var out bytes.Buffer
	if n, err := c.WriteTo(&out); n != int64(len(data)) || err != nil {
		t.Fatalf("WriteTo = %d, %v", n, err)
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Fatal("WriteTo delivered different bytes")
	}
	if r := wrapped.read.Load(); r != int64(len(data)) {
		t.Fatalf("wrapper saw %d bytes read, want %d", r, len(data))
	}
}

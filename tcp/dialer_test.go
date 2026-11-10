package tcp_test

import (
	"context"
	"errors"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ninepeach/go-netx/tcp"
)

// Checks: DIAL-01
func TestDialer(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); accepted <- c }()
	d := tcp.NewDialer(tcp.DialOptions{Timeout: time.Second})
	conn, err := d.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	serverConn := <-accepted
	if serverConn == nil {
		t.Fatal("server did not accept")
	}
	serverConn.Close()
}

// Checks: DIAL-05
func TestDialEventSuccess(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, _ := ln.Accept()
		if conn != nil {
			_ = conn.Close()
		}
	}()
	events := make(chan tcp.DialEvent, 1)
	dialer := tcp.NewDialer(tcp.DialOptions{OnDial: func(event tcp.DialEvent) { events <- event }})
	conn, err := dialer.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	event := <-events
	if event.Network != "tcp" || event.Address != ln.Addr().String() {
		t.Fatalf("unexpected event target: %+v", event)
	}
	if event.Err != nil || event.LocalAddr == nil || event.RemoteAddr == nil {
		t.Fatalf("incomplete success event: %+v", event)
	}
	if event.StartedAt.IsZero() || event.Duration < 0 {
		t.Fatalf("invalid timing: %+v", event)
	}
}

// Checks: DIAL-02
func TestDialEventFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	events := make(chan tcp.DialEvent, 1)
	dialer := tcp.NewDialer(tcp.DialOptions{OnDial: func(event tcp.DialEvent) { events <- event }})
	conn, err := dialer.DialContext(ctx, "tcp", "127.0.0.1:1")
	if conn != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("conn=%v err=%v", conn, err)
	}
	event := <-events
	if !errors.Is(event.Err, context.Canceled) {
		t.Fatalf("event error = %v", event.Err)
	}
	if event.LocalAddr != nil || event.RemoteAddr != nil {
		t.Fatalf("failed dial reported addresses: %+v", event)
	}
}

// Checks: DIAL-03
func TestDialContextCancelled(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var events int
	d := tcp.NewDialer(tcp.DialOptions{OnDial: func(tcp.DialEvent) { events++ }})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn, err := d.DialContext(ctx, "tcp", ln.Addr().String())
	if err == nil {
		conn.Close()
		t.Fatal("dial succeeded with a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if events != 1 {
		t.Fatalf("OnDial called %d times, want 1", events)
	}
}

// Checks: DIAL-04
func TestDialTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	d := tcp.NewDialer(tcp.DialOptions{Timeout: time.Nanosecond})
	conn, err := d.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err == nil {
		conn.Close()
		if runtime.GOOS == "windows" {
			// Measured in CI: a loopback connect on Windows can complete
			// before the 1ns deadline is noticed. Linux and macOS cover it.
			t.Skip("loopback connect finished before a 1ns deadline")
		}
		t.Fatal("dial beat a 1ns timeout")
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("got %v, want a timeout error", err)
	}
}

// Checks: DIAL-08
func TestConcurrentDials(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	var events atomic.Int32
	d := tcp.NewDialer(tcp.DialOptions{Timeout: 2 * time.Second, OnDial: func(e tcp.DialEvent) {
		if e.Err == nil {
			events.Add(1)
		}
	}})
	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := d.DialContext(context.Background(), "tcp", ln.Addr().String())
			if err != nil {
				errs <- err
				return
			}
			c.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if events.Load() != n {
		t.Fatalf("OnDial success events=%d, want %d", events.Load(), n)
	}
}

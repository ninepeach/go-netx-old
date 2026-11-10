package tcp_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/ninepeach/go-netx/tcp"
)

func newServer(t *testing.T) *tcp.Server {
	t.Helper()
	s, err := tcp.Listen(context.Background(), "tcp", "127.0.0.1:0", func(context.Context, *tcp.Conn) {}, tcp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Shutdown before Serve has run means the service was asked to stop and is
// stopped: the first Serve returns nil without accepting, a second one
// still reports ErrClosed. The order is fixed by the calls, not by
// scheduling.
//
// Checks: TCP-30
func TestServeAfterShutdownBeforeServe(t *testing.T) {
	s := newServer(t)
	addr := s.Addr().String()
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal("Shutdown:", err)
	}
	if err := s.Serve(context.Background()); err != nil {
		t.Fatalf("first Serve after an early Shutdown = %v, want nil", err)
	}
	if err := s.Serve(context.Background()); !errors.Is(err, tcp.ErrClosed) {
		t.Fatalf("second Serve = %v, want ErrClosed", err)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("listener still accepting after Shutdown")
	}
	// Shutdown after that returns at once.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal("second Shutdown:", err)
	}
}

// Only a graceful Shutdown before Serve counts as a requested stop. Close
// (abort) before Serve keeps returning ErrClosed, as do later calls.
//
// Checks: TCP-31
func TestServeAfterCloseBeforeServeIsErrClosed(t *testing.T) {
	s := newServer(t)
	s.Close()
	if err := s.Serve(context.Background()); !errors.Is(err, tcp.ErrClosed) {
		t.Fatalf("Serve after Close = %v, want ErrClosed", err)
	}
	if err := s.Serve(context.Background()); !errors.Is(err, tcp.ErrClosed) {
		t.Fatalf("second Serve = %v, want ErrClosed", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal("Shutdown after Close and Serve:", err)
	}
}

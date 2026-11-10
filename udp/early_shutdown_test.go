package udp_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ninepeach/go-netx/udp"
)

func newConn(t *testing.T) *udp.Conn {
	t.Helper()
	c, err := udp.Listen(context.Background(), "udp", "127.0.0.1:0", func(*udp.Conn, *udp.Packet) {}, udp.Options{Readers: 2})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Checks: UDP-39
func TestServeAfterShutdownBeforeServe(t *testing.T) {
	c := newConn(t)
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal("Shutdown:", err)
	}
	if err := c.Serve(context.Background()); err != nil {
		t.Fatalf("first Serve after an early Shutdown = %v, want nil", err)
	}
	if err := c.Serve(context.Background()); !errors.Is(err, udp.ErrClosed) {
		t.Fatalf("second Serve = %v, want ErrClosed", err)
	}
	if err := c.WriteTo([]byte("x"), c.LocalAddr()); !errors.Is(err, udp.ErrClosed) {
		t.Fatalf("WriteTo after Shutdown = %v, want ErrClosed", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Shutdown(ctx); err != nil {
		t.Fatal("second Shutdown:", err)
	}
}

// Checks: UDP-40
func TestServeAfterCloseBeforeServeIsErrClosed(t *testing.T) {
	c := newConn(t)
	c.Close()
	if err := c.Serve(context.Background()); !errors.Is(err, udp.ErrClosed) {
		t.Fatalf("Serve after Close = %v, want ErrClosed", err)
	}
	if err := c.Serve(context.Background()); !errors.Is(err, udp.ErrClosed) {
		t.Fatalf("second Serve = %v, want ErrClosed", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Shutdown(ctx); err != nil {
		t.Fatal("Shutdown after Close and Serve:", err)
	}
}

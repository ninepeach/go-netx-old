package udp_test

import (
	"context"
	"net"
	"net/netip"
	"runtime"
	"testing"
	"time"

	"github.com/ninepeach/go-netx/socket"
	"github.com/ninepeach/go-netx/udp"
)

// dstRecorder returns a handler that sends each datagram's Dst to a
// channel (the Packet itself is borrowed, the AddrPort is a value).
func dstRecorder() (udp.Handler, chan netip.AddrPort) {
	ch := make(chan netip.AddrPort, 16)
	return func(_ *udp.Conn, p *udp.Packet) { ch <- p.Dst }, ch
}

func listenServe(t *testing.T, network, address string, h udp.Handler, o udp.Options) *udp.Conn {
	t.Helper()
	c, err := udp.Listen(context.Background(), network, address, h, o)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Serve(context.Background()) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := c.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		if err := <-done; err != nil && err != udp.ErrClosed {
			t.Error(err)
		}
	})
	return c
}

// sendTo sends one datagram to dst from a fresh socket.
func sendTo(t *testing.T, dst netip.AddrPort) {
	t.Helper()
	cl, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(dst))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if _, err := cl.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
}

func recvDst(t *testing.T, ch chan netip.AddrPort) netip.AddrPort {
	t.Helper()
	select {
	case d := <-ch:
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("no datagram")
	}
	return netip.AddrPort{}
}

// Destination reports the address each datagram was sent to: on a wildcard
// socket it tells the local addresses apart, IPv4 through a dual-stack
// socket is unmapped, and both read paths (recvmmsg batches and single
// reads) carry it. Without the option Dst stays zero. Other platforms
// refuse the option instead of leaving Dst empty (D-30).
//
// Checks: UDP-41
func TestDestination(t *testing.T) {
	if runtime.GOOS != "linux" {
		h, _ := dstRecorder()
		_, err := udp.Listen(context.Background(), "udp4", "127.0.0.1:0", h, udp.Options{Destination: true})
		if !socket.IsUnsupported(err) {
			t.Fatalf("Destination on %s = %v, want UnsupportedOptionError", runtime.GOOS, err)
		}
		return
	}
	for _, batch := range []int{0, 1} {
		h, ch := dstRecorder()
		c := listenServe(t, "udp4", "0.0.0.0:0", h, udp.Options{Destination: true, Batch: batch})
		port := c.LocalAddr().Port()
		// The whole of 127.0.0.0/8 is local on Linux.
		for _, a := range []string{"127.0.0.1", "127.0.0.2"} {
			want := netip.AddrPortFrom(netip.MustParseAddr(a), port)
			sendTo(t, want)
			if got := recvDst(t, ch); got != want {
				t.Fatalf("Batch %d: Dst %v, want %v", batch, got, want)
			}
		}
	}

	need6(t)
	h, ch := dstRecorder()
	c := listenServe(t, "udp", "[::]:0", h, udp.Options{Destination: true})
	port := c.LocalAddr().Port()
	for _, a := range []string{"127.0.0.1", "::1"} {
		want := netip.AddrPortFrom(netip.MustParseAddr(a), port)
		sendTo(t, want)
		if got := recvDst(t, ch); got != want {
			t.Fatalf("dual-stack: Dst %v, want %v", got, want)
		}
	}

	h, ch = dstRecorder()
	c = listenServe(t, "udp4", "127.0.0.1:0", h, udp.Options{})
	sendTo(t, c.LocalAddr())
	if got := recvDst(t, ch); got.IsValid() {
		t.Fatalf("Dst without Destination = %v, want zero", got)
	}
}

package tcp_test

import (
	"context"
	"net"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/ninepeach/go-netx/socket"
	"github.com/ninepeach/go-netx/tcp"
)

// listen6 starts a server on network/address, skipping when the host has no
// IPv6. CI sets NETX_REQUIRE_IPV6=1 so a missing IPv6 loopback fails there
// instead of silently skipping.
func listen6(t *testing.T, network, address string, h tcp.Handler, o tcp.Options) *tcp.Server {
	t.Helper()
	if l, err := net.Listen("tcp6", "[::1]:0"); err != nil {
		if os.Getenv("NETX_REQUIRE_IPV6") != "" {
			t.Fatal("IPv6 required (NETX_REQUIRE_IPV6) but there is no IPv6 loopback:", err)
		}
		t.Skip("no IPv6 loopback on this host:", err)
	} else {
		l.Close()
	}
	s, err := tcp.Listen(context.Background(), network, address, h, o)
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(context.Background())
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Checks: TCP-28
func TestIPv6Loopback(t *testing.T) {
	s := listen6(t, "tcp6", "[::1]:0", echo, tcp.Options{
		MaxConnsPerIP: 1,
		Socket: socket.Options{
			RecvBuffer: 64 << 10,
			SendBuffer: 64 << 10,
			KeepAlive:  socket.KeepAlive{Idle: 30 * time.Second, Interval: 5 * time.Second, Count: 3},
		},
	})
	if a := s.Addr().(*net.TCPAddr); a.IP.To4() != nil || !a.IP.IsLoopback() {
		t.Fatalf("listener address %v is not IPv6 loopback", a)
	}
	d := tcp.NewDialer(tcp.DialOptions{Timeout: 2 * time.Second})
	c, err := d.DialContext(context.Background(), "tcp6", s.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if a := c.LocalAddr().(*net.TCPAddr); a.IP.To4() != nil {
		t.Fatalf("client address %v is not IPv6", a)
	}
	roundTrip(t, c, "over ::1")

	// The per-IP cap keys ::1 by its /64 and refuses a second connection.
	c2, err := d.DialContext(context.Background(), "tcp6", s.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	c2.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c2.Read(make([]byte, 1)); err == nil {
		t.Fatal("second IPv6 connection from ::1 admitted")
	}
	waitFor(t, func() bool { return s.Stats().RejectedPerIP == 1 })
}

// Checks: TCP-29
func TestDualStackAcceptsIPv4(t *testing.T) {
	if runtime.GOOS == "openbsd" {
		t.Skip("OpenBSD has no dual-stack sockets")
	}
	s := listen6(t, "tcp", "[::]:0", echo, tcp.Options{MaxConnsPerIP: 1})
	port := s.Addr().(*net.TCPAddr).Port
	c, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	roundTrip(t, c, "v4 on a v6 socket")

	// The IPv4-mapped peer is keyed as IPv4, so a second one is refused and a
	// real IPv6 peer is not affected.
	c2, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	c2.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c2.Read(make([]byte, 1)); err == nil {
		t.Fatal("second IPv4 connection admitted")
	}
	c6, err := net.DialTimeout("tcp6", net.JoinHostPort("::1", itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c6.Close()
	c6.SetDeadline(time.Now().Add(2 * time.Second))
	roundTrip(t, c6, "v6 alongside")
}

func itoa(n int) string { return strconv.Itoa(n) }

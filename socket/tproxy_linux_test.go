//go:build linux

package socket_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/ninepeach/go-netx/socket"
)

const ipv6Transparent = 75 // IPV6_TRANSPARENT, <linux/in6.h>

// checkOriginalDst: without NAT, conntrack reports the address the
// connection was sent to. When conntrack is not loaded (ENOPROTOOPT) or not
// tracking this namespace (ENOENT) the lookup fails, and must say so rather
// than return an address.
func checkOriginalDst(t *testing.T, got netip.AddrPort, err error, want netip.AddrPort) {
	t.Helper()
	switch {
	case err == nil:
		if got != want {
			t.Fatalf("OriginalDestination = %v, want %v", got, want)
		}
	case errors.Is(err, syscall.ENOPROTOOPT), errors.Is(err, syscall.ENOENT):
		if got.IsValid() {
			t.Fatalf("OriginalDestination returned %v with error %v", got, err)
		}
		t.Logf("no conntrack entry here: %v (the netfilter test covers NAT)", err)
	default:
		t.Fatalf("OriginalDestination: %v", err)
	}
}

// IPv4 sockets get IP_TRANSPARENT; IPv6 and dual-stack sockets get both
// options, as with DontFragment.
//
// Checks: SOCK-36
func TestTransparentReadBackLinux(t *testing.T) {
	lc := socket.ListenConfig(socket.Options{Transparent: true})
	probe, err := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if errors.Is(err, syscall.EPERM) {
		t.Skip("no CAP_NET_ADMIN (CI runs this as root in the netfilter step)")
	}
	if err != nil {
		t.Fatal(err)
	}
	probe.Close()

	v4 := listenUDP(t, socket.Options{Transparent: true}, "udp4", "127.0.0.1:0")
	if got := socketIntOption(t, v4, syscall.SOL_IP, syscall.IP_TRANSPARENT); got != 1 {
		t.Fatalf("udp4 IP_TRANSPARENT = %d", got)
	}
	for _, n := range []struct{ network, addr string }{{"udp6", "[::1]:0"}, {"udp", ":0"}} {
		c := listenUDP(t, socket.Options{Transparent: true}, n.network, n.addr)
		if got := socketIntOption(t, c, syscall.SOL_IPV6, ipv6Transparent); got != 1 {
			t.Fatalf("%s IPV6_TRANSPARENT = %d", n.network, got)
		}
		if got := socketIntOption(t, c, syscall.SOL_IP, syscall.IP_TRANSPARENT); got != 1 {
			t.Fatalf("%s IP_TRANSPARENT = %d", n.network, got)
		}
	}
	ln, err := lc.Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	raw, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var got int
	var gerr error
	raw.Control(func(fd uintptr) { got, gerr = syscall.GetsockoptInt(int(fd), syscall.SOL_IP, syscall.IP_TRANSPARENT) })
	if gerr != nil || got != 1 {
		t.Fatalf("tcp4 listener IP_TRANSPARENT = %d, %v", got, gerr)
	}
}

// A dual-stack listener sees IPv4 clients as IPv4-mapped; the lookup must
// still go to IPv4 conntrack and return a plain IPv4 address.
//
// Checks: SOCK-37
func TestOriginalDestinationDualStackLinux(t *testing.T) {
	for _, n := range []struct{ listen, dial string }{{"[::]:0", "127.0.0.1"}, {"[::1]:0", "::1"}} {
		ln, err := net.Listen("tcp", n.listen)
		if err != nil {
			if os.Getenv("NETX_REQUIRE_IPV6") != "" {
				t.Fatal(err)
			}
			t.Skipf("no IPv6 here: %v", err)
		}
		port := ln.Addr().(*net.TCPAddr).AddrPort().Port()
		want := netip.AddrPortFrom(netip.MustParseAddr(n.dial), port)
		srv, cl := acceptPair(t, ln, want.String())
		got, err := socket.OriginalDestination(srv)
		checkOriginalDst(t, got, err, want)
		srv.Close()
		cl.Close()
		ln.Close()
	}
}

// acceptPair dials addr and returns the accepted and the dialed side.
func acceptPair(t *testing.T, ln net.Listener, addr string) (*net.TCPConn, net.Conn) {
	t.Helper()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
		close(accepted)
	}()
	cl, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	srv, ok := <-accepted
	if !ok {
		cl.Close()
		t.Fatal("accept failed")
	}
	return srv.(*net.TCPConn), cl
}

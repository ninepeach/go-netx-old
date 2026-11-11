package socket_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"runtime"
	"testing"

	"github.com/ninepeach/go-netx/socket"
)

// Transparent changes behaviour, so it fails where it cannot be set: on
// Linux without CAP_NET_ADMIN with EPERM (never silently ignored), on every
// other platform with UnsupportedOptionError. Linux reads it back in
// control_linux_test.go, and the root-only netfilter tests prove that a
// transparent listener receives TPROXY traffic.
//
// Checks: SOCK-36
func TestTransparent(t *testing.T) {
	lc := socket.ListenConfig(socket.Options{Transparent: true})
	ln, errTCP := lc.Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if errTCP == nil {
		ln.Close()
	}
	pc, errUDP := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if errUDP == nil {
		pc.Close()
	}
	for _, err := range []error{errTCP, errUDP} {
		if runtime.GOOS != "linux" {
			if !socket.IsUnsupported(err) {
				t.Fatalf("Transparent on %s = %v, want UnsupportedOptionError", runtime.GOOS, err)
			}
			continue
		}
		if err != nil && !errors.Is(err, errPermission) {
			t.Fatalf("Transparent on Linux = %v, want success or EPERM", err)
		}
	}
}

// OriginalDestination reads the address a connection was sent to before
// netfilter NAT changed it. Without NAT it is the listener's own address
// where conntrack tracks the connection; checkOriginalDst holds each
// platform's expectation (Linux: that address or a conntrack error; others:
// UnsupportedOptionError). A nil or non-socket connection is an error
// everywhere.
//
// Checks: SOCK-37
func TestOriginalDestination(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
		close(accepted)
	}()
	cl, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	srv, ok := <-accepted
	if !ok {
		t.Fatal("accept failed")
	}
	defer srv.Close()

	got, err := socket.OriginalDestination(srv.(*net.TCPConn))
	checkOriginalDst(t, got, err, ln.Addr().(*net.TCPAddr).AddrPort())

	if _, err := socket.OriginalDestination(nil); err == nil {
		t.Fatal("OriginalDestination(nil) succeeded")
	}
	var nilConn *net.TCPConn
	if _, err := socket.OriginalDestination(nilConn); err == nil {
		t.Fatal("OriginalDestination(typed nil) succeeded")
	}
}

// addrPort parses s or fails the test.
func addrPort(t *testing.T, s string) netip.AddrPort {
	t.Helper()
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		t.Fatal(err)
	}
	return ap
}

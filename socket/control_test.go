package socket_test

import (
	"context"
	"errors"
	"net"
	"runtime"
	"testing"

	"github.com/ninepeach/go-netx/socket"
)

// loopbackName returns the name of the loopback interface.
func loopbackName(t *testing.T) string {
	t.Helper()
	ifs, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range ifs {
		if i.Flags&net.FlagLoopback != 0 && i.Flags&net.FlagUp != 0 {
			return i.Name
		}
	}
	t.Skip("no loopback interface")
	return ""
}

// interfaceSupported: platforms where Options.Interface is implemented.
func interfaceSupported() bool {
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
		return true
	}
	return false
}

// echoServer returns the address of a TCP listener that accepts and holds
// connections until the test ends.
func echoServer(t *testing.T, network, addr string) string {
	t.Helper()
	ln, err := net.Listen(network, addr)
	if err != nil {
		t.Skipf("%s %s: %v", network, addr, err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	return ln.Addr().String()
}

// Interface binds a socket to a named interface. Dialing the loopback
// address through the loopback interface works on Linux, macOS and
// Windows; a name that is not an interface fails the dial or listen
// instead of being ignored; other platforms return UnsupportedOptionError.
// Each platform's test reads the option back (control_*_test.go).
//
// Checks: SOCK-32
func TestInterfaceBinding(t *testing.T) {
	lo := loopbackName(t)
	addr := echoServer(t, "tcp4", "127.0.0.1:0")
	d := socket.Dialer(net.Dialer{}, socket.Options{Interface: lo})
	c, err := d.Dial("tcp4", addr)
	if !interfaceSupported() {
		if !socket.IsUnsupported(err) {
			t.Fatalf("Interface on %s = %v, want UnsupportedOptionError", runtime.GOOS, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("dial through %s: %v", lo, err)
	}
	c.Close()

	missing := socket.Options{Interface: "netx-none0"}
	md := socket.Dialer(net.Dialer{}, missing)
	if _, err := md.Dial("tcp4", addr); err == nil {
		t.Fatal("dial through a missing interface succeeded")
	}
	lc := socket.ListenConfig(missing)
	if pc, err := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0"); err == nil {
		pc.Close()
		t.Fatal("listen on a missing interface succeeded")
	}
}

// RoutingMark sets SO_MARK on Linux, where it needs CAP_NET_ADMIN: without
// it the dial fails with EPERM rather than running unmarked. Other
// platforms return UnsupportedOptionError. Linux reads it back
// (control_linux_test.go).
//
// Checks: SOCK-33
func TestRoutingMark(t *testing.T) {
	addr := echoServer(t, "tcp4", "127.0.0.1:0")
	d := socket.Dialer(net.Dialer{}, socket.Options{RoutingMark: 0x2a})
	c, err := d.Dial("tcp4", addr)
	switch {
	case runtime.GOOS != "linux":
		if !socket.IsUnsupported(err) {
			t.Fatalf("RoutingMark on %s = %v, want UnsupportedOptionError", runtime.GOOS, err)
		}
	case err != nil:
		if !errors.Is(err, errPermission) {
			t.Fatalf("RoutingMark = %v, want success or EPERM", err)
		}
		t.Log("no CAP_NET_ADMIN: SO_MARK refused with EPERM, as documented")
	default:
		c.Close()
	}
}

// DontFragment is for UDP: on TCP it is refused as a configuration error,
// on every platform. Each supported platform's test reads it back on IPv4,
// IPv6 and dual-stack sockets.
//
// Checks: SOCK-34
func TestDontFragmentNetworks(t *testing.T) {
	lc := socket.ListenConfig(socket.Options{DontFragment: true})
	if ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0"); !socket.IsUnsupported(err) {
		if ln != nil {
			ln.Close()
		}
		t.Fatalf("DontFragment on TCP = %v, want UnsupportedOptionError", err)
	}
	pc, err := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" || runtime.GOOS == "freebsd" || runtime.GOOS == "windows" {
		if err != nil {
			t.Fatalf("DontFragment on UDP: %v", err)
		}
		pc.Close()
	} else if !socket.IsUnsupported(err) {
		t.Fatalf("DontFragment on %s = %v, want UnsupportedOptionError", runtime.GOOS, err)
	}
}

// BindAddressNoPort is a dialer performance option: on Linux it sets
// IP_BIND_ADDRESS_NO_PORT (read back in control_linux_test.go), elsewhere
// dials run without it (Supported reports which). It means nothing to a
// listener or to UDP, so both are refused.
//
// Checks: SOCK-35
func TestBindAddressNoPort(t *testing.T) {
	addr := echoServer(t, "tcp4", "127.0.0.1:0")
	opts := socket.Options{BindAddressNoPort: true}
	d := socket.Dialer(net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}}, opts)
	c, err := d.Dial("tcp4", addr)
	if err != nil {
		t.Fatalf("dial with BindAddressNoPort: %v", err)
	}
	c.Close()
	if got, want := socket.Supported().BindAddressNoPort, runtime.GOOS == "linux"; got != want {
		t.Fatalf("Supported().BindAddressNoPort = %v on %s", got, runtime.GOOS)
	}
	lc := socket.ListenConfig(opts)
	if ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0"); !socket.IsUnsupported(err) {
		if ln != nil {
			ln.Close()
		}
		t.Fatalf("BindAddressNoPort on a listener = %v, want UnsupportedOptionError", err)
	}
	ud := socket.Dialer(net.Dialer{}, opts)
	if _, err := ud.Dial("udp4", addr); !socket.IsUnsupported(err) {
		t.Fatalf("BindAddressNoPort on UDP = %v, want UnsupportedOptionError", err)
	}
}

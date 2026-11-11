//go:build windows

package socket_test

import (
	"context"
	"net"
	"syscall"
	"testing"

	"github.com/ninepeach/go-netx/socket"
)

func winIntOption(t *testing.T, c syscall.Conn, level, name int) int {
	t.Helper()
	raw, err := c.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var v int
	var gerr error
	if err := raw.Control(func(fd uintptr) { v, gerr = syscall.GetsockoptInt(syscall.Handle(fd), level, name) }); err != nil {
		t.Fatal(err)
	}
	if gerr != nil {
		t.Fatalf("getsockopt(%d, %d): %v", level, name, gerr)
	}
	return v
}

func listenUDPWindows(t *testing.T, opts socket.Options, network, addr string) *net.UDPConn {
	t.Helper()
	lc := socket.ListenConfig(opts)
	pc, err := lc.ListenPacket(context.Background(), network, addr)
	if err != nil {
		t.Fatalf("%s %s: %v", network, addr, err)
	}
	t.Cleanup(func() { pc.Close() })
	return pc.(*net.UDPConn)
}

// setsockopt(IP_UNICAST_IF) takes the index in network byte order;
// getsockopt returns it in host order (CI, Windows Server 2025), so reading
// back the plain index shows the kernel took the right interface rather
// than index 16777216.
//
// Checks: SOCK-32
func TestInterfaceBindingReadBackWindows(t *testing.T) {
	lo, err := net.InterfaceByName(loopbackName(t))
	if err != nil {
		t.Fatal(err)
	}
	v4 := listenUDPWindows(t, socket.Options{Interface: lo.Name}, "udp4", "127.0.0.1:0")
	if got := winIntOption(t, v4, syscall.IPPROTO_IP, 31); got != lo.Index {
		t.Fatalf("IP_UNICAST_IF = %#x, want %#x", got, lo.Index)
	}
	v6 := listenUDPWindows(t, socket.Options{Interface: lo.Name}, "udp", ":0")
	if got := winIntOption(t, v6, syscall.IPPROTO_IPV6, 31); got != lo.Index {
		t.Fatalf("IPV6_UNICAST_IF = %d, want %d", got, lo.Index)
	}
	if got := winIntOption(t, v6, syscall.IPPROTO_IP, 31); got != lo.Index {
		t.Fatalf("dual-stack IP_UNICAST_IF = %#x, want %#x", got, lo.Index)
	}
}

// Checks: SOCK-34
func TestDontFragmentReadBackWindows(t *testing.T) {
	opts := socket.Options{DontFragment: true}
	v4 := listenUDPWindows(t, opts, "udp4", "127.0.0.1:0")
	if got := winIntOption(t, v4, syscall.IPPROTO_IP, 14); got != 1 {
		t.Fatalf("IP_DONTFRAGMENT = %d", got)
	}
	ds := listenUDPWindows(t, opts, "udp", ":0")
	if got := winIntOption(t, ds, syscall.IPPROTO_IPV6, 14); got != 1 {
		t.Fatalf("IPV6_DONTFRAG = %d", got)
	}
	if got := winIntOption(t, ds, syscall.IPPROTO_IP, 14); got != 1 {
		t.Fatalf("dual-stack IP_DONTFRAGMENT = %d", got)
	}
}

//go:build darwin

package socket_test

import (
	"context"
	"net"
	"syscall"
	"testing"

	"github.com/ninepeach/go-netx/socket"
)

func listenUDPDarwin(t *testing.T, opts socket.Options, network, addr string) *net.UDPConn {
	t.Helper()
	lc := socket.ListenConfig(opts)
	pc, err := lc.ListenPacket(context.Background(), network, addr)
	if err != nil {
		t.Fatalf("%s %s: %v", network, addr, err)
	}
	t.Cleanup(func() { pc.Close() })
	return pc.(*net.UDPConn)
}

// Checks: SOCK-32
func TestInterfaceBindingReadBackDarwin(t *testing.T) {
	lo, err := net.InterfaceByName(loopbackName(t))
	if err != nil {
		t.Fatal(err)
	}
	v4 := listenUDPDarwin(t, socket.Options{Interface: lo.Name}, "udp4", "127.0.0.1:0")
	if got := socketIntOption(t, v4, syscall.IPPROTO_IP, syscall.IP_BOUND_IF); got != lo.Index {
		t.Fatalf("IP_BOUND_IF = %d, want %d", got, lo.Index)
	}
	v6 := listenUDPDarwin(t, socket.Options{Interface: lo.Name}, "udp6", "[::1]:0")
	if got := socketIntOption(t, v6, syscall.IPPROTO_IPV6, syscall.IPV6_BOUND_IF); got != lo.Index {
		t.Fatalf("IPV6_BOUND_IF = %d, want %d", got, lo.Index)
	}
}

// Checks: SOCK-34
func TestDontFragmentReadBackDarwin(t *testing.T) {
	opts := socket.Options{DontFragment: true}
	v4 := listenUDPDarwin(t, opts, "udp4", "127.0.0.1:0")
	if got := socketIntOption(t, v4, syscall.IPPROTO_IP, 28); got != 1 { // IP_DONTFRAG
		t.Fatalf("IP_DONTFRAG = %d", got)
	}
	for _, n := range []struct{ network, addr string }{{"udp6", "[::1]:0"}, {"udp", ":0"}} {
		c := listenUDPDarwin(t, opts, n.network, n.addr)
		if got := socketIntOption(t, c, syscall.IPPROTO_IPV6, 62); got != 1 { // IPV6_DONTFRAG
			t.Fatalf("%s IPV6_DONTFRAG = %d", n.network, got)
		}
	}
}

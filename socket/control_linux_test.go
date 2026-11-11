//go:build linux

package socket_test

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/ninepeach/go-netx/socket"
)

const (
	soBindToIfindex     = 62 // SO_BINDTOIFINDEX, readable since Linux 5.0
	ipBindAddressNoPort = 24
)

func listenUDP(t *testing.T, opts socket.Options, network, addr string) *net.UDPConn {
	t.Helper()
	lc := socket.ListenConfig(opts)
	pc, err := lc.ListenPacket(context.Background(), network, addr)
	if err != nil && network != "udp4" && errors.Is(err, syscall.EAFNOSUPPORT) && os.Getenv("NETX_REQUIRE_IPV6") == "" {
		t.Skipf("%s: no IPv6 here: %v", network, err) // CI sets NETX_REQUIRE_IPV6
	}
	if err != nil {
		t.Fatalf("%s %s: %v", network, addr, err)
	}
	t.Cleanup(func() { pc.Close() })
	return pc.(*net.UDPConn)
}

// Checks: SOCK-32
func TestInterfaceBindingReadBackLinux(t *testing.T) {
	lo, err := net.InterfaceByName(loopbackName(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []struct{ network, addr string }{{"udp4", "127.0.0.1:0"}, {"udp6", "[::1]:0"}, {"udp", ":0"}} {
		c := listenUDP(t, socket.Options{Interface: lo.Name}, n.network, n.addr)
		if got := socketIntOption(t, c, syscall.SOL_SOCKET, soBindToIfindex); got != lo.Index {
			t.Fatalf("%s: bound to interface %d, want %d", n.network, got, lo.Index)
		}
	}
}

// Checks: SOCK-33
func TestRoutingMarkReadBackLinux(t *testing.T) {
	lc := socket.ListenConfig(socket.Options{RoutingMark: 0x2a})
	pc, err := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if errors.Is(err, syscall.EPERM) {
		t.Skip("no CAP_NET_ADMIN")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if got := socketIntOption(t, pc.(*net.UDPConn), syscall.SOL_SOCKET, syscall.SO_MARK); got != 0x2a {
		t.Fatalf("SO_MARK = %#x, want 0x2a", got)
	}
}

// Dual-stack sockets get both the IPv6 and the IPv4 option, so IPv4
// traffic through an IPv6 socket is not fragmented either.
//
// Checks: SOCK-34
func TestDontFragmentReadBackLinux(t *testing.T) {
	opts := socket.Options{DontFragment: true}
	v4 := listenUDP(t, opts, "udp4", "127.0.0.1:0")
	if got := socketIntOption(t, v4, syscall.IPPROTO_IP, syscall.IP_MTU_DISCOVER); got != syscall.IP_PMTUDISC_DO {
		t.Fatalf("udp4 IP_MTU_DISCOVER = %d", got)
	}
	for _, n := range []struct{ network, addr string }{{"udp6", "[::1]:0"}, {"udp", ":0"}} {
		c := listenUDP(t, opts, n.network, n.addr)
		if got := socketIntOption(t, c, syscall.IPPROTO_IPV6, syscall.IPV6_MTU_DISCOVER); got != syscall.IPV6_PMTUDISC_DO {
			t.Fatalf("%s IPV6_MTU_DISCOVER = %d", n.network, got)
		}
		if got := socketIntOption(t, c, syscall.IPPROTO_IP, syscall.IP_MTU_DISCOVER); got != syscall.IP_PMTUDISC_DO {
			t.Fatalf("%s IP_MTU_DISCOVER = %d", n.network, got)
		}
	}
}

// Checks: SOCK-35
func TestBindAddressNoPortReadBackLinux(t *testing.T) {
	addr := echoServer(t, "tcp4", "127.0.0.1:0")
	d := socket.Dialer(net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}}, socket.Options{BindAddressNoPort: true})
	c, err := d.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got := socketIntOption(t, c.(*net.TCPConn), syscall.IPPROTO_IP, ipBindAddressNoPort)
	if got != 1 {
		t.Fatalf("IP_BIND_ADDRESS_NO_PORT = %d", got)
	}
}

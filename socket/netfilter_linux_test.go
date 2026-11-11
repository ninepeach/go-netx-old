//go:build linux

package socket_test

import (
	"context"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ninepeach/go-netx/socket"
)

// These tests change the host's firewall and routing, so they run only
// when NETX_NETFILTER=1 is set, as root. CI runs them in a separate step
// under sudo; elsewhere they skip.

func needNetfilter(t *testing.T) {
	t.Helper()
	if os.Getenv("NETX_NETFILTER") != "1" {
		t.Skip("changes iptables and routing: set NETX_NETFILTER=1 and run as root")
	}
	if os.Geteuid() != 0 {
		t.Fatal("NETX_NETFILTER=1 needs root")
	}
}

// sh runs a command and fails the test with its output on error.
func sh(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// added runs "<name> <add> <args>" and removes it with "<name> <del>
// <args>" when the test ends.
func added(t *testing.T, name, add, del string, args ...string) {
	t.Helper()
	sh(t, name, append(strings.Fields(add), args...)...)
	t.Cleanup(func() {
		if out, err := exec.Command(name, append(strings.Fields(del), args...)...).CombinedOutput(); err != nil {
			t.Errorf("cleanup %s %s: %v\n%s", name, del, err, out)
		}
	})
}

// freePort returns a TCP port nothing listens on, for a REDIRECT target.
func freePort(t *testing.T, network, host string) int {
	t.Helper()
	ln, err := net.Listen(network, net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// A connection redirected by iptables REDIRECT arrives at the proxy's
// listener; OriginalDestination returns the address the client dialed,
// for IPv4, IPv6 and IPv4 clients of a dual-stack listener.
//
// Checks: SOCK-37
func TestRedirectOriginalDestinationNetfilter(t *testing.T) {
	needNetfilter(t)
	for _, n := range []struct{ name, listen, ipt, dst string }{
		{"ipv4", "127.0.0.1", "iptables", "127.0.0.1"},
		{"ipv6", "::1", "ip6tables", "::1"},
		{"dual-stack", "::", "iptables", "127.0.0.1"},
	} {
		t.Run(n.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", net.JoinHostPort(n.listen, "0"))
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			proxyPort := ln.Addr().(*net.TCPAddr).Port
			target := freePort(t, "tcp", n.dst)
			added(t, n.ipt, "-t nat -A", "-t nat -D", "OUTPUT", "-p", "tcp", "-d", n.dst,
				"--dport", strconv.Itoa(target), "-j", "REDIRECT", "--to-ports", strconv.Itoa(proxyPort))

			want := netip.AddrPortFrom(netip.MustParseAddr(n.dst), uint16(target))
			srv, cl := acceptPair(t, ln, want.String())
			defer cl.Close()
			defer srv.Close()
			got, err := socket.OriginalDestination(srv)
			if err != nil || got != want {
				t.Fatalf("OriginalDestination = %v, %v; want %v", got, err, want)
			}
		})
	}
}

// tproxyLocal routes locally generated traffic for vip:vport (proto) back
// in through lo, where TPROXY hands it to the transparent socket on
// 127.0.0.1:onPort. mark and table are unique per test package.
func tproxyLocal(t *testing.T, proto, vip string, vport, onPort int, mark, table string) {
	t.Helper()
	added(t, "ip", "rule add", "rule del", "fwmark", mark, "lookup", table)
	added(t, "ip", "route add", "route del", "local", "0.0.0.0/0", "dev", "lo", "table", table)
	match := []string{"-p", proto, "-d", vip, "--dport", strconv.Itoa(vport)}
	added(t, "iptables", "-t mangle -A", "-t mangle -D", append(append([]string{"OUTPUT"}, match...),
		"-j", "MARK", "--set-mark", mark)...)
	added(t, "iptables", "-t mangle -A", "-t mangle -D", append(append([]string{"PREROUTING"}, match...),
		"-j", "TPROXY", "--on-ip", "127.0.0.1", "--on-port", strconv.Itoa(onPort), "--tproxy-mark", mark)...)
}

// A Transparent listener accepts TPROXY connections to an address that is
// not local, and the accepted connection's local address is the one the
// client dialed.
//
// Checks: SOCK-36
func TestTransparentListenerNetfilter(t *testing.T) {
	needNetfilter(t)
	ln, err := socket.Listen(context.Background(), "tcp4", "127.0.0.1:0", socket.Options{Transparent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	want := addrPort(t, "198.18.0.1:4431")
	tproxyLocal(t, "tcp", want.Addr().String(), int(want.Port()), ln.Addr().(*net.TCPAddr).Port, "0x1e7", "231")

	ln.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
	srv, cl := acceptPair(t, ln, want.String())
	defer cl.Close()
	defer srv.Close()
	if got := srv.LocalAddr().(*net.TCPAddr).AddrPort(); got != want {
		t.Fatalf("accepted local address %v, want %v", got, want)
	}
}

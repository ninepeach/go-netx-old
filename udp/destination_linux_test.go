//go:build linux

package udp_test

import (
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/ninepeach/go-netx/socket"
	"github.com/ninepeach/go-netx/udp"
)

// Reading the destination adds a control-message parse per datagram and
// nothing else: the handler path still allocates nothing, batched or not.
//
// Checks: UDP-42
func TestDestinationDoesNotAllocate(t *testing.T) {
	for _, batch := range []int{0, 1} {
		c := start(t, echo, udp.Options{Destination: true, Batch: batch})
		cl := dial(t, c)
		msg, in := make([]byte, 512), make([]byte, 2048)
		for i := 0; i < 100; i++ { // warm up
			cl.Write(msg)
			if _, err := cl.Read(in); err != nil {
				t.Fatalf("Batch %d: %v", batch, err)
			}
		}
		allocs := testing.AllocsPerRun(1000, func() {
			cl.Write(msg)
			cl.Read(in)
		})
		if allocs > 0 {
			t.Fatalf("Batch %d: %.1f allocs per round trip", batch, allocs)
		}
	}
}

// netfilter: see socket/netfilter_linux_test.go. Runs only with
// NETX_NETFILTER=1, as root.
func needNetfilter(t *testing.T) {
	t.Helper()
	if os.Getenv("NETX_NETFILTER") != "1" {
		t.Skip("changes iptables and routing: set NETX_NETFILTER=1 and run as root")
	}
	if os.Geteuid() != 0 {
		t.Fatal("NETX_NETFILTER=1 needs root")
	}
}

func added(t *testing.T, name, add, del string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, append(strings.Fields(add), args...)...).CombinedOutput(); err != nil {
		t.Fatalf("%s %s %s: %v\n%s", name, add, strings.Join(args, " "), err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command(name, append(strings.Fields(del), args...)...).CombinedOutput(); err != nil {
			t.Errorf("cleanup %s %s: %v\n%s", name, del, err, out)
		}
	})
}

// A transparent UDP socket receives datagrams that TPROXY diverted from a
// non-local address, and Packet.Dst is that original destination, on
// both read paths.
//
// Checks: UDP-43
func TestTProxyDestinationNetfilter(t *testing.T) {
	needNetfilter(t)
	// Locally generated traffic for the test addresses is marked, routed
	// back in through lo and handed to the socket by TPROXY. Mark and table
	// differ from the socket package's so the two packages can run at once.
	const mark, table = "0x1e8", "232"
	added(t, "ip", "rule add", "rule del", "fwmark", mark, "lookup", table)
	added(t, "ip", "route add", "route del", "local", "0.0.0.0/0", "dev", "lo", "table", table)
	for _, batch := range []int{0, 1} {
		h, ch := dstRecorder()
		c := listenServe(t, "udp4", "127.0.0.1:0", h,
			udp.Options{Destination: true, Batch: batch, Socket: socket.Options{Transparent: true}})
		want := netip.MustParseAddrPort("198.18.0.2:5300")
		want = netip.AddrPortFrom(want.Addr(), want.Port()+uint16(batch))
		match := []string{"-p", "udp", "-d", want.Addr().String(), "--dport", strconv.Itoa(int(want.Port()))}
		added(t, "iptables", "-t mangle -A", "-t mangle -D",
			append(append([]string{"OUTPUT"}, match...), "-j", "MARK", "--set-mark", mark)...)
		added(t, "iptables", "-t mangle -A", "-t mangle -D",
			append(append([]string{"PREROUTING"}, match...), "-j", "TPROXY", "--on-ip", "127.0.0.1",
				"--on-port", strconv.Itoa(int(c.LocalAddr().Port())), "--tproxy-mark", mark)...)

		cl, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(want))
		if err != nil {
			t.Fatal(err)
		}
		defer cl.Close()
		if _, err := cl.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if got := recvDst(t, ch); got != want {
			t.Fatalf("Batch %d: Dst %v, want %v", batch, got, want)
		}
	}
}

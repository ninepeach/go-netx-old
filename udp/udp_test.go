package udp_test

import (
	"context"
	"net"
	"net/netip"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/ninepeach/go-netx/socket"
	"github.com/ninepeach/go-netx/udp"
)

func echo(c *udp.Conn, p *udp.Packet) { _ = c.Reply(p, p.Data) }

func start(t testing.TB, h udp.Handler, o udp.Options) *udp.Conn {
	t.Helper()
	c, err := udp.Listen(context.Background(), "udp", "127.0.0.1:0", h, o)
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

func dial(t testing.TB, c *udp.Conn) *net.UDPConn {
	t.Helper()
	cl, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(c.LocalAddr()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	cl.SetDeadline(time.Now().Add(2 * time.Second))
	return cl
}

// Checks: OBS-03, UDP-01, UDP-17
func TestEcho(t *testing.T) {
	c := start(t, echo, udp.Options{})
	cl := dial(t, c)
	cl.Write([]byte("hello"))
	in := make([]byte, 64)
	n, err := cl.Read(in)
	if err != nil || string(in[:n]) != "hello" {
		t.Fatalf("%q %v", in[:n], err)
	}
	// The reply can reach the client before the server counts the send.
	deadline := time.Now().Add(time.Second)
	s := c.Stats()
	for s.SendPackets == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		s = c.Stats()
	}
	if s.RecvPackets != 1 || s.SendPackets != 1 || s.RecvBytes != 5 {
		t.Fatalf("%+v", s)
	}
}

// Checks: UDP-02, UDP-07
func TestReusePortReaders(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("load spreading is asserted on Linux only: macOS binds but does not balance (D-15), and FreeBSD's SO_REUSEPORT_LB is not run here")
	}
	var mu sync.Mutex
	seen := map[int]bool{}
	c := start(t, func(c *udp.Conn, p *udp.Packet) {
		mu.Lock()
		seen[p.Reader] = true
		mu.Unlock()
		echo(c, p)
	}, udp.Options{Readers: 4, Socket: socket.Options{ReusePort: true}})
	in := make([]byte, 64)
	for i := 0; i < 32; i++ { // many source ports so the kernel spreads them
		cl := dial(t, c)
		cl.Write([]byte("x"))
		if _, err := cl.Read(in); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("all datagrams on one reader: %v", seen)
	}
}

// Checks: OBS-04, UDP-06
func TestTruncatedDropped(t *testing.T) {
	got := make(chan int, 2)
	c := start(t, func(_ *udp.Conn, p *udp.Packet) { got <- len(p.Data) }, udp.Options{MaxPacket: 8})
	cl := dial(t, c)
	cl.Write(make([]byte, 100))
	cl.Write(make([]byte, 4))
	if n := <-got; n != 4 {
		t.Fatalf("handler saw %d bytes", n)
	}
	if s := c.Stats(); s.Truncated != 1 {
		t.Fatalf("%+v", s)
	}
}

// Checks: UDP-18
func TestWriteTooLarge(t *testing.T) {
	c := start(t, echo, udp.Options{MaxPacket: 8})
	if err := c.WriteTo(make([]byte, 100), c.LocalAddr()); err != nil {
		t.Fatal("MaxPacket must not limit sends:", err)
	}
	if err := c.WriteTo(make([]byte, udp.MaxDatagram+1), c.LocalAddr()); err != udp.ErrTooLarge {
		t.Fatal(err)
	}
}

// Checks: UDP-12
func TestServeAfterCloseAndContextCancel(t *testing.T) {
	c, err := udp.Listen(context.Background(), "udp", "127.0.0.1:0", echo, udp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- c.Serve(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not stop on cancel")
	}
	if err := c.Serve(context.Background()); err != udp.ErrClosed {
		t.Fatal(err)
	}
}

// Checks: UDP-15, UDP-34
func TestHandlerDoesNotAllocate(t *testing.T) {
	// The default (recvmmsg batches on Linux) and the one-datagram path.
	for _, batch := range []int{0, 1} {
		c := start(t, echo, udp.Options{Batch: batch})
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

// Checks: NF-12
func BenchmarkEcho(b *testing.B) {
	c := start(b, echo, udp.Options{})
	cl := dial(b, c)
	cl.SetDeadline(time.Time{})
	msg, in := make([]byte, 512), make([]byte, 2048)
	b.ReportAllocs()
	b.SetBytes(512)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cl.Write(msg)
		cl.Read(in)
	}
}

// Checks: UDP-16, UDP-21, UDP-22
func TestReplyValidatesPacket(t *testing.T) {
	c := start(t, echo, udp.Options{Readers: 2})
	if err := c.Reply(nil, []byte("x")); err != udp.ErrNilPacket {
		t.Fatal(err)
	}
	cl := dial(t, c)
	to := cl.LocalAddr().(*net.UDPAddr).AddrPort()
	in := make([]byte, 8)
	for _, reader := range []int{-1, 99} { // not assigned by this Conn
		if err := c.Reply(&udp.Packet{From: to, Reader: reader}, []byte("ok")); err != nil {
			t.Fatalf("reader %d: %v", reader, err)
		}
		if n, err := cl.Read(in); err != nil || string(in[:n]) != "ok" {
			t.Fatalf("reader %d: %q %v", reader, in[:n], err)
		}
	}
}

// Checks: UDP-14
func TestMaxPacketBound(t *testing.T) {
	if _, err := udp.Listen(context.Background(), "udp", "127.0.0.1:0", echo, udp.Options{MaxPacket: udp.MaxDatagram + 1}); err == nil {
		t.Fatal("MaxPacket above the limit accepted")
	}
	c, err := udp.Listen(context.Background(), "udp", "127.0.0.1:0", echo, udp.Options{MaxPacket: udp.MaxDatagram})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

// With the default MaxPacket, a datagram at the limit is delivered and one
// byte more is dropped as Truncated.
// Checks: UDP-05, UDP-06, UDP-19
func TestDefaultMaxDatagram(t *testing.T) {
	got := make(chan int, 2)
	c := start(t, func(_ *udp.Conn, p *udp.Packet) { got <- len(p.Data) }, udp.Options{})
	cl := dial(t, c)
	if _, err := cl.Write(make([]byte, udp.MaxDatagram+1)); err != nil {
		t.Skip("loopback cannot send this size:", err)
	}
	cl.Write(make([]byte, udp.MaxDatagram))
	if n := <-got; n != udp.MaxDatagram {
		t.Fatalf("delivered %d bytes", n)
	}
	if s := c.Stats(); s.Truncated != 1 {
		t.Fatalf("%+v", s)
	}
}

// Checks: UDP-26, UDP-27
func TestKernelDropsEnabled(t *testing.T) {
	c := start(t, echo, udp.Options{})
	if want := runtime.GOOS == "linux"; c.Stats().KernelDropsEnabled != want {
		t.Fatalf("KernelDropsEnabled=%v on %s", c.Stats().KernelDropsEnabled, runtime.GOOS)
	}
}

// Checks: UDP-07, UDP-23, UDP-24
func TestConcurrentWritesAndClose(t *testing.T) {
	c, err := udp.Listen(context.Background(), "udp", "127.0.0.1:0", echo, udp.Options{Readers: 2})
	if err != nil {
		t.Fatal(err)
	}
	go c.Serve(context.Background())
	to := c.LocalAddr()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := &udp.Packet{From: to, Reader: i % 2}
			for j := 0; j < 500; j++ {
				var err error
				if j%2 == 0 {
					err = c.WriteTo([]byte("x"), to)
				} else {
					err = c.Reply(p, []byte("y"))
				}
				if err != nil && err != udp.ErrClosed {
					t.Errorf("unexpected error: %v", err)
					return
				}
			}
		}(i)
	}
	time.Sleep(5 * time.Millisecond)
	c.Close()
	wg.Wait()
}

// Checks: UDP-25
func TestConcurrentShutdown(t *testing.T) {
	c, err := udp.Listen(context.Background(), "udp", "127.0.0.1:0", echo, udp.Options{Readers: 2})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Serve(context.Background()) }()
	time.Sleep(5 * time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := c.Shutdown(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Readers and MaxPacket reject negative values and normalize zero values.
// Checks: UDP-13, UDP-14
func TestListenOptionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts udp.Options
	}{
		{name: "negative readers", opts: udp.Options{Readers: -1}},
		{name: "negative max packet", opts: udp.Options{MaxPacket: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if c, err := udp.Listen(context.Background(), "udp", "127.0.0.1:0", echo, tc.opts); err == nil {
				c.Close()
				t.Fatal("negative option accepted")
			}
		})
	}

	c, err := udp.Listen(context.Background(), "udp", "127.0.0.1:0", echo, udp.Options{Readers: 0, MaxPacket: 0})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Stats(); got.KernelDropsEnabled != (runtime.GOOS == "linux") {
		t.Fatalf("unexpected initial stats: %+v", got)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

// Checks: UDP-04
func TestZeroLengthDatagram(t *testing.T) {
	got := make(chan int, 1)
	c := start(t, func(c *udp.Conn, p *udp.Packet) {
		got <- len(p.Data)
		_ = c.Reply(p, p.Data)
	}, udp.Options{})
	// An unconnected client: on Windows Go's connected Write(nil) sends
	// nothing (measured in CI), while WriteTo sends an empty datagram.
	cl, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	cl.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := cl.WriteToUDPAddrPort(nil, c.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-got:
		if n != 0 {
			t.Fatalf("handler saw %d bytes", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("empty datagram not delivered")
	}
	n, _, err := cl.ReadFromUDPAddrPort(make([]byte, 8))
	if err != nil || n != 0 {
		t.Fatalf("empty reply: n=%d err=%v", n, err)
	}
	if s := c.Stats(); s.RecvPackets != 1 || s.RecvBytes != 0 || s.Truncated != 0 {
		t.Fatalf("%+v", s)
	}
}

// need6 skips when the host has no IPv6 loopback, or fails when CI sets
// NETX_REQUIRE_IPV6=1. Without IPv6 Go silently opens "[::]" as an IPv4
// socket, so a dual-stack test would pass without testing anything.
func need6(t *testing.T) {
	t.Helper()
	c, err := net.ListenPacket("udp6", "[::1]:0")
	if err != nil {
		if os.Getenv("NETX_REQUIRE_IPV6") != "" {
			t.Fatal("IPv6 required (NETX_REQUIRE_IPV6) but there is no IPv6 loopback:", err)
		}
		t.Skip("no IPv6 loopback on this host:", err)
	}
	c.Close()
}

// Checks: UDP-03
func TestFromAddressIsSenderAndUnmapped(t *testing.T) {
	need6(t)
	from := make(chan netip.AddrPort, 1)
	c, err := udp.Listen(context.Background(), "udp", "[::]:0", func(c *udp.Conn, p *udp.Packet) {
		from <- p.From
		_ = c.Reply(p, p.Data)
	}, udp.Options{})
	if err != nil {
		t.Skip("no dual-stack socket:", err)
	}
	go c.Serve(context.Background())
	t.Cleanup(func() { c.Close() })
	cl, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), c.LocalAddr().Port())))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	cl.SetDeadline(time.Now().Add(2 * time.Second))
	cl.Write([]byte("x"))
	got := <-from
	want := cl.LocalAddr().(*net.UDPAddr).AddrPort()
	if got != want || !got.Addr().Is4() {
		t.Fatalf("From=%v (Is4=%v), want %v", got, got.Addr().Is4(), want)
	}
	// Replying to the unmapped IPv4 address over the IPv6 socket works.
	if _, err := cl.Read(make([]byte, 8)); err != nil {
		t.Fatal("no reply to IPv4 sender:", err)
	}
	// A real IPv6 sender on the same socket is reported as IPv6 and replied to.
	cl6, err := net.DialUDP("udp6", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(netip.IPv6Loopback(), c.LocalAddr().Port())))
	if err != nil {
		t.Fatal(err)
	}
	defer cl6.Close()
	cl6.SetDeadline(time.Now().Add(2 * time.Second))
	cl6.Write([]byte("y"))
	got = <-from
	if want := cl6.LocalAddr().(*net.UDPAddr).AddrPort(); got != want || !got.Addr().Is6() || got.Addr().Is4In6() {
		t.Fatalf("From=%v, want %v", got, want)
	}
	if _, err := cl6.Read(make([]byte, 8)); err != nil {
		t.Fatal("no reply to IPv6 sender:", err)
	}
}

// Checks: UDP-19
func TestIPv4MaxDatagramRoundTrip(t *testing.T) {
	got := make(chan int, 1)
	c := start(t, func(_ *udp.Conn, p *udp.Packet) { got <- len(p.Data) }, udp.Options{})
	if !c.LocalAddr().Addr().Is4() {
		t.Fatal("expected an IPv4 socket")
	}
	if err := c.WriteTo(make([]byte, udp.MaxDatagram), c.LocalAddr()); err != nil {
		t.Fatalf("IPv4 send of MaxDatagram failed: %v", err)
	}
	if n := <-got; n != udp.MaxDatagram {
		t.Fatalf("received %d bytes", n)
	}
}

// Checks: UDP-20
func TestIPv6MaxDatagramRoundTrip(t *testing.T) {
	need6(t)
	got := make(chan int, 1)
	c, err := udp.Listen(context.Background(), "udp6", "[::1]:0", func(_ *udp.Conn, p *udp.Packet) { got <- len(p.Data) }, udp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	go c.Serve(context.Background())
	t.Cleanup(func() { c.Close() })
	if err := c.WriteTo(make([]byte, udp.MaxDatagram), c.LocalAddr()); err != nil {
		t.Fatalf("IPv6 send of MaxDatagram failed: %v", err)
	}
	select {
	case n := <-got:
		if n != udp.MaxDatagram {
			t.Fatalf("received %d bytes", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("IPv6 datagram not received")
	}
	if err := c.WriteTo(make([]byte, udp.MaxDatagram+1), c.LocalAddr()); err != udp.ErrTooLarge {
		t.Fatalf("over the limit: %v", err)
	}
}

// Checks: UDP-24, OBS-05
func TestWriteAfterCloseIsErrClosedAndCounted(t *testing.T) {
	c, err := udp.Listen(context.Background(), "udp", "127.0.0.1:0", echo, udp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	to := c.LocalAddr()
	c.Close()
	if err := c.WriteTo([]byte("x"), to); err != udp.ErrClosed {
		t.Fatalf("WriteTo after Close: %v", err)
	}
	if err := c.Reply(&udp.Packet{From: to}, []byte("x")); err != udp.ErrClosed {
		t.Fatalf("Reply after Close: %v", err)
	}
	if s := c.Stats(); s.SendErrors != 2 || s.SendPackets != 0 {
		t.Fatalf("%+v", s)
	}
}

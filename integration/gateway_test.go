package integration_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ninepeach/go-netx"
	"github.com/ninepeach/go-netx/buf"
	"github.com/ninepeach/go-netx/flow"
	"github.com/ninepeach/go-netx/limit"
	"github.com/ninepeach/go-netx/queue"
	"github.com/ninepeach/go-netx/tcp"
	"github.com/ninepeach/go-netx/udp"
)

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(time.Millisecond)
	}
}

// A TCP gateway keys flows by connection ID: open on connect, remove on
// disconnect.
//
// Checks: INT-05
func TestTCPFlowLifecycle(t *testing.T) {
	var closed atomic.Int32
	flows := flow.NewTable(flow.TableOptions[uint64, string]{
		OnClose: func(uint64, *flow.Flow[string]) { closed.Add(1) },
	})
	ln, err := tcp.Listen(context.Background(), "tcp", "127.0.0.1:0", func(_ context.Context, c *tcp.Conn) {
		f, _ := flows.GetOrOpen(c.ID(), func() string { return c.RemoteAddr().String() })
		defer flows.Remove(c.ID(), f.ID())
		_, _ = io.Copy(io.Discard, c) // until the client hangs up
	}, tcp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	srv := netx.NewServer(ln)
	done := make(chan error)
	go func() { done <- srv.LoopContext(context.Background()) }()

	var clients []net.Conn
	for i := 0; i < 5; i++ {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
	}
	eventually(t, func() bool { return flows.Len() == 5 })
	for _, c := range clients {
		c.Close()
	}
	eventually(t, func() bool { return flows.Len() == 0 && closed.Load() == 5 })

	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A full DropOldest queue evicts buffers through OnDrop, and every buffer
// is released (counted in netxdebug builds).
//
// Checks: INT-06
func TestFullQueueReleasesEvictedBuffers(t *testing.T) {
	before := buf.Outstanding()
	pool := buf.NewPool()
	var dropped, handled atomic.Int32
	q := queue.New(queue.Options[*buf.Buf]{
		Cap: 2, Policy: queue.DropOldest,
		OnDrop: func(b *buf.Buf) { dropped.Add(1); b.Release() },
	})
	conn, err := udp.Listen(context.Background(), "udp", "127.0.0.1:0", func(_ *udp.Conn, p *udp.Packet) {
		if b := pool.Copy(p.Data); !q.Push(b) {
			b.Release()
		}
		handled.Add(1)
	}, udp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	go conn.Serve(context.Background())
	defer conn.Close()

	// No consumer: the queue fills, then evicts.
	cl, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(conn.LocalAddr()))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	for i := 0; i < 10; i++ {
		cl.Write([]byte{byte(i)})
	}
	// RecvPackets counts a datagram before its handler runs; wait for the
	// handler itself.
	eventually(t, func() bool { return handled.Load() == 10 })
	if dropped.Load() != 8 || q.Len() != 2 {
		t.Fatalf("dropped=%d len=%d", dropped.Load(), q.Len())
	}
	if held := buf.Outstanding() - before; buf.Debug && held != 2 {
		t.Fatalf("%d buffers held, want only the 2 queued", held)
	}
	q.Close()
	q.Drain()
	if held := buf.Outstanding() - before; held != 0 {
		t.Fatalf("%d buffers leaked", held)
	}
}

// The gateway bounds the memory it holds with its own limit.Budget around
// buf (buf itself does not limit, D-24): when the budget is exhausted,
// datagrams are dropped; once buffers are released, traffic flows again.
//
// Checks: INT-07
func TestBudgetExhaustionDropsThenRecovers(t *testing.T) {
	budget := limit.NewBudget(int64(buf.Capacity(1))) // room for one held 1-byte datagram
	pool := buf.NewPool()
	held := make(chan *buf.Buf, 4)
	release := func(b *buf.Buf) {
		budget.Release(int64(buf.Capacity(b.Cap())))
		b.Release()
	}
	var overBudget atomic.Int32
	conn, err := udp.Listen(context.Background(), "udp", "127.0.0.1:0", func(c *udp.Conn, p *udp.Packet) {
		if !budget.Reserve(int64(buf.Capacity(len(p.Data)))) {
			overBudget.Add(1)
			return
		}
		b := pool.Copy(p.Data)
		held <- b
		_ = c.Reply(p, []byte("ok"))
	}, udp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	go conn.Serve(context.Background())
	defer conn.Close()
	cl, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(conn.LocalAddr()))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	in := make([]byte, 8)
	cl.SetDeadline(time.Now().Add(2 * time.Second))

	cl.Write([]byte("a"))
	if _, err := cl.Read(in); err != nil {
		t.Fatal("first datagram:", err)
	}
	cl.Write([]byte("b")) // budget full: dropped
	eventually(t, func() bool { return overBudget.Load() == 1 })

	release(<-held) // gateway finishes with the first one
	cl.Write([]byte("c"))
	if _, err := cl.Read(in); err != nil {
		t.Fatal("traffic did not recover:", err)
	}
	release(<-held)
	if budget.Used() != 0 {
		t.Fatalf("used=%d", budget.Used())
	}
}

// Long-lived TCP handlers react to Draining by telling the client to move,
// and the runtime's graceful shutdown completes without aborting them.
//
// Checks: INT-08
func TestTCPDrainingGoAway(t *testing.T) {
	var aborted atomic.Int32
	ln, err := tcp.Listen(context.Background(), "tcp", "127.0.0.1:0", func(ctx context.Context, c *tcp.Conn) {
		select {
		case <-c.Draining():
			_, _ = io.WriteString(c, "GOAWAY\n")
		case <-ctx.Done():
			aborted.Add(1)
		}
	}, tcp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	srv := netx.NewServer(ln)
	srv.ShutdownTimeout = 2 * time.Second
	done := make(chan error)
	go func() { done <- srv.LoopContext(context.Background()) }()

	var clients []*bufio.Reader
	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		clients = append(clients, bufio.NewReader(c))
	}
	eventually(t, func() bool { return ln.Stats().Active == 3 })

	start := time.Now()
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i, r := range clients {
		if line, err := r.ReadString('\n'); err != nil || line != "GOAWAY\n" {
			t.Fatalf("client %d: %q %v", i, line, err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if aborted.Load() != 0 {
		t.Fatalf("%d handlers aborted instead of draining", aborted.Load())
	}
	if time.Since(start) > time.Second {
		t.Fatal("drain waited for the timeout")
	}
}

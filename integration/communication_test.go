// Package integration_test composes every go-netx package the way a gateway
// would. Run with -tags netxdebug to also check that no buffer leaks.
package integration_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
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

// Checks: INT-01, TCP-03
func TestTCPLineEcho(t *testing.T) {
	ctx := context.Background()
	ln, err := tcp.Listen(ctx, "tcp", "127.0.0.1:0", func(_ context.Context, c *tcp.Conn) {
		sc := bufio.NewScanner(c)
		for sc.Scan() {
			if _, err := fmt.Fprintf(c, "echo:%s\n", sc.Text()); err != nil {
				return
			}
		}
	}, tcp.Options{Admit: limit.NewConcurrency(16)})
	if err != nil {
		t.Fatal(err)
	}
	srv := netx.NewServer(ln)
	done := make(chan error)
	go func() { done <- srv.LoopContext(ctx) }()

	const clients, messages = 8, 20
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for id := 0; id < clients; id++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			r := bufio.NewReader(conn)
			for m := 0; m < messages; m++ {
				msg := fmt.Sprintf("client-%d-%d", id, m)
				fmt.Fprintln(conn, msg)
				line, err := r.ReadString('\n')
				if err != nil || strings.TrimSpace(line) != "echo:"+msg {
					errs <- fmt.Errorf("%q %v", line, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// peer is per-flow gateway state: a bounded outbound queue and its sender.
type peer struct {
	out *queue.Ring[*buf.Buf]
}

// TestUDPGatewayComposition: the read loop never blocks. Each datagram is
// copied into an owned buffer and handed to the sender flow's queue; a
// per-flow goroutine writes it back. Flows expire through flow.Table.
// Checks: BUF-06, INT-03, INT-04, INT-06
func TestUDPGatewayComposition(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := buf.NewPool(limit.NewBudget(1 << 20))
	var senders sync.WaitGroup

	var conn *udp.Conn
	flows := flow.NewTable(flow.TableOptions[netip.AddrPort, *peer]{
		IdleTimeout: time.Minute,
		OnClose:     func(_ netip.AddrPort, f *flow.Flow[*peer]) { f.Value.out.Close() },
	})
	newPeer := func(to netip.AddrPort) *peer {
		p := &peer{out: queue.New(queue.Options[*buf.Buf]{
			Cap: 64, Policy: queue.DropOldest, OnDrop: (*buf.Buf).Release,
		})}
		senders.Add(1)
		go func() {
			defer senders.Done()
			defer p.out.Drain()
			for {
				b, err := p.out.PopWait(ctx)
				if err != nil {
					return
				}
				_ = conn.WriteTo(b.B, to)
				b.Release()
			}
		}()
		return p
	}

	conn, err := udp.Listen(ctx, "udp", "127.0.0.1:0", func(_ *udp.Conn, p *udp.Packet) {
		f, _ := flows.GetOrOpen(p.From, func() *peer { return newPeer(p.From) })
		f.Touch()              // a datagram arrived: record the traffic for idle expiry
		b := pool.Copy(p.Data) // borrowed → owned
		if b == nil {
			return // over budget: drop
		}
		if !f.Value.out.Push(b) { // transfer failed: still ours
			b.Release()
		}
	}, udp.Options{Readers: 2})
	if err != nil {
		t.Fatal(err)
	}
	srv := netx.NewServer(netx.Group(conn, netx.ServiceFuncs{ServeFunc: flows.Run}))
	done := make(chan error)
	go func() { done <- srv.LoopContext(ctx) }()

	for c := 0; c < 4; c++ {
		cl, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(conn.LocalAddr()))
		if err != nil {
			t.Fatal(err)
		}
		cl.SetDeadline(time.Now().Add(2 * time.Second))
		in := make([]byte, 64)
		for i := 0; i < 20; i++ {
			msg := fmt.Sprintf("c%d-%d", c, i)
			cl.Write([]byte(msg))
			n, err := cl.Read(in)
			if err != nil || string(in[:n]) != msg {
				t.Fatalf("%q %v", in[:n], err)
			}
		}
		// Kept open until the end: a closed client's port can be handed to
		// the next one (seen on macOS CI), which would merge two flows.
		defer cl.Close()
	}
	if flows.Len() != 4 {
		t.Fatalf("flows=%d", flows.Len())
	}

	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	cancel()
	senders.Wait()
	if buf.Debug && buf.Outstanding() != 0 {
		t.Fatalf("%d buffers leaked", buf.Outstanding())
	}
}

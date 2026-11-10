//go:build stress

package integration_test

import (
	"context"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ninepeach/go-netx"
	"github.com/ninepeach/go-netx/buf"
	"github.com/ninepeach/go-netx/flow"
	"github.com/ninepeach/go-netx/internal/leak"
	"github.com/ninepeach/go-netx/queue"
	"github.com/ninepeach/go-netx/udp"
)

func stressDuration() time.Duration {
	if s, err := strconv.Atoi(os.Getenv("NETX_STRESS_SECONDS")); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	return 3 * time.Second
}

// The full gateway composition under sustained churn: clients keep
// appearing and vanishing, flows expire, per-flow senders start and stop,
// queues overflow. Afterwards nothing is left behind: no buffers,
// no flows, no goroutines, no sockets.
//
// Checks: INT-09
func TestStressGatewayLongRun(t *testing.T) {
	base := leak.Take()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := buf.Outstanding()
	pool := buf.NewPool()
	var senders sync.WaitGroup
	var conn *udp.Conn
	flows := flow.NewTable(flow.TableOptions[netip.AddrPort, *peer]{
		IdleTimeout: 50 * time.Millisecond,
		OnClose:     func(_ netip.AddrPort, f *flow.Flow[*peer]) { f.Value.out.Close() },
	})
	newPeer := func(to netip.AddrPort) *peer {
		p := &peer{out: queue.New(queue.Options[*buf.Buf]{Cap: 8, Policy: queue.DropOldest, OnDrop: (*buf.Buf).Release})}
		senders.Add(1)
		go func() {
			defer senders.Done()
			defer p.out.Drain()
			for {
				b, err := p.out.PopWait(ctx)
				if err != nil {
					return
				}
				_ = conn.WriteTo(b.Bytes(), to)
				b.Release()
			}
		}()
		return p
	}
	conn, err := udp.Listen(ctx, "udp", "127.0.0.1:0", func(_ *udp.Conn, p *udp.Packet) {
		f, _ := flows.GetOrOpen(p.From, func() *peer { return newPeer(p.From) })
		f.Touch() // traffic keeps the flow from idle expiry
		if b := pool.Copy(p.Data); !f.Value.out.Push(b) {
			b.Release()
		}
	}, udp.Options{Readers: 2})
	if err != nil {
		t.Fatal(err)
	}
	srv := netx.NewServer(netx.Group(conn, netx.ServiceFuncs{ServeFunc: flows.Run}))
	done := make(chan error, 1)
	go func() { done <- srv.LoopContext(ctx) }()

	deadline := time.Now().Add(stressDuration())
	var clients, replies atomic.Int64
	var cw sync.WaitGroup
	for w := 0; w < 8; w++ {
		cw.Add(1)
		go func() {
			defer cw.Done()
			in := make([]byte, 64)
			for time.Now().Before(deadline) {
				cl, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(conn.LocalAddr()))
				if err != nil {
					t.Error(err)
					return
				}
				clients.Add(1)
				for i := 0; i < 20; i++ {
					cl.Write([]byte("payload"))
					cl.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
					if _, err := cl.Read(in); err == nil {
						replies.Add(1)
					}
				}
				cl.Close() // the flow expires on its own
			}
		}()
	}
	cw.Wait()
	t.Logf("clients=%d replies=%d flows peak-left=%d", clients.Load(), replies.Load(), flows.Len())
	if replies.Load() < clients.Load()*15 {
		t.Fatalf("too few replies: %d for %d clients", replies.Load(), clients.Load())
	}

	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	cancel()
	senders.Wait()
	if flows.Len() != 0 {
		t.Fatalf("%d flows left", flows.Len())
	}
	if held := buf.Outstanding() - before; held != 0 {
		t.Fatalf("%d buffers leaked", held)
	}
	if err := base.Check(5*time.Second, 2); err != nil {
		t.Fatal(err)
	}
}

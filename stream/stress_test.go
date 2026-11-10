//go:build stress

package stream_test

import (
	"bufio"
	"context"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/ninepeach/go-netx/buf"
	"github.com/ninepeach/go-netx/internal/leak"
	"github.com/ninepeach/go-netx/stream"
	"github.com/ninepeach/go-netx/tcp"
)

func heapInUse() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse
}

// Many idle connections: a stream.Reader holds only its idle buffer, a
// fixed 4KB bufio.Reader holds 4KB. After a burst of large frames every
// grown buffer goes back to the pool.
//
// Checks: STREAM-08
func TestStressIdleReaderMemory(t *testing.T) {
	const n = 4000
	measure := func(name string, handler func(*tcp.Conn)) float64 {
		base := heapInUse()
		s, err := tcp.Listen(context.Background(), "tcp", "127.0.0.1:0", func(_ context.Context, c *tcp.Conn) { handler(c) }, tcp.Options{})
		if err != nil {
			t.Fatal(err)
		}
		go s.Serve(context.Background())
		clients := make([]net.Conn, 0, n)
		for i := 0; i < n; i++ {
			c, err := net.Dial("tcp", s.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			clients = append(clients, c)
		}
		for s.Stats().Active != n {
			time.Sleep(10 * time.Millisecond)
		}
		per := float64(heapInUse()-base) / n
		for _, c := range clients {
			c.Close()
		}
		s.Close()
		for s.Stats().Active != 0 {
			time.Sleep(10 * time.Millisecond)
		}
		t.Logf("%-22s %6.0f bytes heap per connection (client side included)", name, per)
		return per
	}
	base := leak.Take()
	pool := buf.NewPool()
	none := measure("no reader", func(c *tcp.Conn) { c.Read(make([]byte, 1)) })
	fixed := measure("bufio 4KB", func(c *tcp.Conn) { bufio.NewReaderSize(c, 4096).Peek(4) })
	adaptive := measure("stream.Reader 512B", func(c *tcp.Conn) {
		r := stream.NewReader(c, pool, stream.ReaderOptions{})
		defer r.Release()
		r.Peek(4)
	})
	saved := fixed - adaptive
	t.Logf("stream.Reader saves %.0f bytes per idle connection over bufio 4KB; costs %.0f over no reader", saved, adaptive-none)
	if saved < 3000 {
		t.Fatalf("expected about 3.5KB saved per idle connection, got %.0f", saved)
	}
	if err := base.Check(5*time.Second, 2); err != nil {
		t.Fatal(err)
	}
}

// After a burst of large frames on every connection, grown buffers return
// to the pool: none is still held (counted in netxdebug builds; make
// stress runs both).
//
// Checks: STREAM-03
func TestStressBurstThenIdleReturnsBuffers(t *testing.T) {
	const n = 500
	before := buf.Outstanding()
	held := func() int64 { return buf.Outstanding() - before }
	pool := buf.NewPool()
	frame := make([]byte, 10_000)
	got := make(chan struct{}, n)
	s, err := tcp.Listen(context.Background(), "tcp", "127.0.0.1:0", func(_ context.Context, c *tcp.Conn) {
		r := stream.NewReader(c, pool, stream.ReaderOptions{})
		defer r.Release()
		if _, err := r.Peek(len(frame)); err != nil {
			return
		}
		r.Discard(len(frame))
		got <- struct{}{}
		r.Peek(1) // a light read follows, then the connection idles
		r.Discard(1)
		got <- struct{}{}
		r.Peek(1) // idle: wait
	}, tcp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(context.Background())
	defer s.Close()
	clients := make([]net.Conn, n)
	for i := range clients {
		if clients[i], err = net.Dial("tcp", s.Addr().String()); err != nil {
			t.Fatal(err)
		}
		defer clients[i].Close()
		clients[i].Write(frame)
	}
	for i := 0; i < n; i++ {
		<-got
	}
	if buf.Debug && held() == 0 {
		t.Fatal("no buffer was grown by the burst")
	}
	for _, c := range clients {
		c.Write([]byte{1})
	}
	for i := 0; i < n; i++ {
		<-got
	}
	if held() != 0 {
		t.Fatalf("%d buffers still borrowed by %d idle connections", held(), n)
	}
}

package stream_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/ninepeach/go-netx/buf"
	"github.com/ninepeach/go-netx/stream"
	"github.com/ninepeach/go-netx/tcp"
)

// sink returns a *tcp.Conn whose peer discards everything.
func sink(b *testing.B) (*tcp.Conn, func()) {
	conns := make(chan *tcp.Conn, 1)
	hold := make(chan struct{})
	s, err := tcp.Listen(context.Background(), "tcp", "127.0.0.1:0", func(_ context.Context, c *tcp.Conn) {
		conns <- c
		<-hold
	}, tcp.Options{})
	if err != nil {
		b.Fatal(err)
	}
	go s.Serve(context.Background())
	peer, err := net.Dial("tcp", s.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	go io.Copy(io.Discard, peer)
	return <-conns, func() { close(hold); peer.Close(); s.Close() }
}

// One Write per message against one Writer.Flush per backlog, both through
// a *tcp.Conn.
//
// Checks: STREAM-15
func BenchmarkBacklog(b *testing.B) {
	pool := buf.NewPool()
	for _, batch := range []int{1, 8, 32} {
		msg := make([]byte, 64)
		b.Run(fmt.Sprintf("batch=%d/write", batch), func(b *testing.B) {
			c, stop := sink(b)
			defer stop()
			b.ReportAllocs()
			b.SetBytes(int64(64 * batch))
			for i := 0; i < b.N; i++ {
				for j := 0; j < batch; j++ {
					m := pool.Copy(msg)
					c.Write(m.Bytes())
					m.Release()
				}
			}
		})
		b.Run(fmt.Sprintf("batch=%d/writer", batch), func(b *testing.B) {
			c, stop := sink(b)
			defer stop()
			w := stream.NewWriter(c)
			b.ReportAllocs()
			b.SetBytes(int64(64 * batch))
			for i := 0; i < b.N; i++ {
				for j := 0; j < batch; j++ {
					w.Add(pool.Copy(msg))
				}
				if _, err := w.Flush(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

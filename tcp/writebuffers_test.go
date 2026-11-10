package tcp_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"

	"github.com/ninepeach/go-netx/buf"
	"github.com/ninepeach/go-netx/stream"
	"github.com/ninepeach/go-netx/tcp"
)

var _ stream.BuffersWriter = (*tcp.Conn)(nil)

// A handler that sends several owned buffers through stream.Writer over a
// *tcp.Conn delivers them intact and in order.
//
// Checks: STREAM-14
func TestConnWriteBuffers(t *testing.T) {
	pool := buf.NewPool()
	var want []byte
	parts := make([][]byte, 50)
	for i := range parts {
		parts[i] = bytes.Repeat([]byte{byte('a' + i%26)}, 100+i*37)
		want = append(want, parts[i]...)
	}
	s, _ := start(t, func(_ context.Context, c *tcp.Conn) {
		w := stream.NewWriter(c)
		for _, p := range parts {
			w.Add(pool.Copy(p))
		}
		if _, err := w.Flush(); err != nil {
			t.Error(err)
		}
	}, tcp.Options{})
	c := dial(t, s)
	got, err := io.ReadAll(c)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("received %d bytes (want %d): %v", len(got), len(want), err)
	}
	// Direct call with an empty and a nil-element-free vector.
	var empty net.Buffers
	if n, err := (&tcp.Conn{Conn: c}).WriteBuffers(&empty); n != 0 || err != nil {
		t.Fatalf("empty WriteBuffers: %d %v", n, err)
	}
}

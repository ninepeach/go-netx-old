package stream

import (
	"io"
	"net"

	"github.com/ninepeach/go-netx/buf"
)

// BuffersWriter is implemented by connections that can send several
// buffers with one system call. *tcp.Conn implements it; so do wrappers
// that want Writer to keep using writev. Go's own net.Buffers only uses
// writev on the standard library's connection types, so a wrapper that
// does not implement this silently falls back to one Write per buffer.
type BuffersWriter interface {
	WriteBuffers(*net.Buffers) (int64, error)
}

// Writer collects buffers and sends them in one writev. It is not safe for
// concurrent use; one goroutine owns a connection's Writer.
//
// Ownership: Add takes ownership of the buffer. Flush and Discard release
// every added buffer, whether or not it was written: after a failed write
// the stream is broken and the data is of no further use. AddShared only
// borrows: the Writer sends the bytes and never releases or keeps them.
type Writer struct {
	dst  io.Writer
	held []*buf.Buf  // owned buffers to release
	vec  net.Buffers // everything to send, in Add/AddShared order; backing array reused
	tmp  net.Buffers // the view WriteTo consumes; a field so it does not escape per call
	size int
}

// NewWriter returns a Writer that sends to dst.
func NewWriter(dst io.Writer) *Writer { return &Writer{dst: dst} }

// Add queues b for the next Flush and takes ownership of it.
func (w *Writer) Add(b *buf.Buf) {
	w.held = append(w.held, b)
	w.vec = append(w.vec, b.Bytes())
	w.size += b.Len()
}

// AddShared queues p for the next Flush without taking ownership: p is
// sent in order with the added buffers and never released. It is meant
// for immutable data shared by many connections, such as an encoded game
// configuration or a broadcast payload, which is then sent with no copy
// and no per-connection buffer. p must not change until Flush or Discard
// returns; after that the Writer holds no reference to it. An empty p is
// ignored.
func (w *Writer) AddShared(p []byte) {
	if len(p) == 0 {
		return
	}
	w.vec = append(w.vec, p)
	w.size += len(p)
}

// Pending returns how many buffers (owned and shared) and bytes are queued.
func (w *Writer) Pending() (buffers, bytes int) { return len(w.vec), w.size }

// Flush writes everything queued in order, then releases the owned
// buffers. A single buffer is written with Write (writev only pays off for
// several). Flush either writes every queued byte or returns an error: a
// destination that reports fewer bytes with a nil error, which io.Writer
// forbids but some wrappers do, yields io.ErrShortWrite.
func (w *Writer) Flush() (int64, error) {
	var n int64
	var err error
	switch len(w.vec) {
	case 0:
		return 0, nil
	case 1:
		var k int
		k, err = w.dst.Write(w.vec[0])
		n = int64(k)
	default:
		w.tmp = w.vec
		if bw, ok := w.dst.(BuffersWriter); ok {
			n, err = bw.WriteBuffers(&w.tmp)
		} else {
			n, err = w.tmp.WriteTo(w.dst)
		}
	}
	if err == nil && n < int64(w.size) {
		err = io.ErrShortWrite
	}
	w.Discard()
	return n, err
}

// Discard drops everything queued without writing it, releasing the owned
// buffers; shared data is only forgotten.
func (w *Writer) Discard() {
	buf.ReleaseMulti(w.held)
	w.held = w.held[:0]
	clear(w.vec) // drop references to released and shared memory
	w.vec = w.vec[:0]
	w.tmp = nil
	w.size = 0
}

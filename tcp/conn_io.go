package tcp

import (
	"errors"
	"fmt"
	"io"
)

// Half-close and copy methods. Conn embeds net.Conn, an interface, so the
// methods of the connection inside (a *net.TCPConn from Listen) are hidden
// unless Conn forwards them. Each forwards to the embedded connection
// itself, never to anything beneath it: Conn adds nothing to Read and
// Write, and a caller's wrapper (from NewServer with its own listener)
// keeps its logic.

// CloseWrite shuts down the sending side (a TCP FIN): the peer reads EOF
// and can still send. It returns errors.ErrUnsupported, and closes
// nothing, when the embedded connection has no half-close.
func (c *Conn) CloseWrite() error {
	if hc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return hc.CloseWrite()
	}
	return fmt.Errorf("tcp: CloseWrite: %w", errors.ErrUnsupported)
}

// CloseRead shuts down the receiving side: later Reads return at once.
// It returns errors.ErrUnsupported, and closes nothing, when the embedded
// connection has no half-close.
func (c *Conn) CloseRead() error {
	if hc, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return hc.CloseRead()
	}
	return fmt.Errorf("tcp: CloseRead: %w", errors.ErrUnsupported)
}

// ReadFrom writes to the connection everything read from r until EOF. It
// makes io.Copy reach the embedded connection's own ReadFrom: for a
// *net.TCPConn on Linux that splices in the kernel when r is a TCP
// connection, as the standard library decides. Otherwise it copies
// through Write.
func (c *Conn) ReadFrom(r io.Reader) (int64, error) {
	if rc, ok := r.(*Conn); ok {
		r = rc.Conn // our own layer adds nothing; let the standard library see the TCP source
	}
	if rf, ok := c.Conn.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(writerOnly{c.Conn}, r)
}

// WriteTo writes to w everything read from the connection until EOF. It
// makes io.Copy reach the embedded connection's own WriteTo, which for a
// *net.TCPConn hands a TCP destination its kernel copy path. Otherwise it
// copies through Read.
func (c *Conn) WriteTo(w io.Writer) (int64, error) {
	if wc, ok := w.(*Conn); ok {
		w = wc.Conn
	}
	if wt, ok := c.Conn.(io.WriterTo); ok {
		return wt.WriteTo(w)
	}
	return io.Copy(w, readerOnly{c.Conn})
}

// writerOnly and readerOnly hide ReadFrom and WriteTo from io.Copy, so the
// fallbacks cannot call back into themselves.
type writerOnly struct{ io.Writer }

type readerOnly struct{ io.Reader }

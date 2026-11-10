// Package stream reads and writes byte streams (TCP, TLS, Unix sockets)
// with memory proportional to the data in flight, not to the number of
// connections.
//
// Reader keeps a small buffer while a connection is idle and borrows a
// larger one from a buf.Pool only while data is pending, so many mostly
// idle connections cost little. Writer sends a backlog of buffers with one
// writev system call instead of one Write each.
//
// Neither knows any protocol: where a frame ends is the caller's decision.
// stream depends on buf and the standard library only; transports do not
// depend on stream.
package stream

import (
	"errors"
	"io"

	"github.com/ninepeach/go-netx/buf"
)

var (
	// ErrTooLarge is returned when more than ReaderOptions.Max bytes would
	// have to be buffered, for example a frame longer than the protocol
	// allows. The connection should usually be closed.
	ErrTooLarge = errors.New("stream: data exceeds Max")
	// ErrNoBuffer is returned when the pool's budget cannot provide a larger
	// buffer. Buffered data is kept; retry later or close the connection.
	ErrNoBuffer = errors.New("stream: no buffer within budget")
	// ErrReleased is returned by a Reader after Release.
	ErrReleased = errors.New("stream: reader released")
	// ErrNegativeCount is returned by Peek for a negative count.
	ErrNegativeCount = errors.New("stream: negative count")
)

// defaultMax is ReaderOptions.Max when unset. It is fixed at 64KB rather
// than following buf.Large, so raising the largest pooled class (to 128KB in
// #35) does not silently double the frame size every Reader accepts.
const defaultMax = 64 << 10

// ReaderOptions configures a Reader.
type ReaderOptions struct {
	// IdleSize is the buffer kept while no data is pending. Default 512.
	IdleSize int
	// Max is the most data the Reader will buffer, typically the largest
	// frame the protocol allows. Default 64KB.
	Max int
}

// Reader is a buffered reader whose buffer grows while data is pending and
// shrinks back to IdleSize once it is consumed and traffic is light. It is
// not safe for concurrent use.
type Reader struct {
	src  io.Reader
	pool *buf.Pool
	idle []byte   // owned, kept for the Reader's lifetime
	big  *buf.Buf // borrowed from pool while grown, else nil
	data []byte   // idle or big's full capacity
	r, w int      // data[r:w] is buffered
	last int      // bytes returned by the most recent read
	err  error    // sticky error from src
	max  int
	done bool
}

// NewReader returns a Reader over src that borrows larger buffers from
// pool. It panics if IdleSize or Max is negative or Max < IdleSize.
func NewReader(src io.Reader, pool *buf.Pool, o ReaderOptions) *Reader {
	if o.IdleSize == 0 {
		o.IdleSize = 512
	}
	if o.Max == 0 {
		o.Max = defaultMax
	}
	if o.IdleSize < 0 || o.Max < o.IdleSize || pool == nil {
		panic("stream: invalid ReaderOptions or nil pool")
	}
	idle := make([]byte, o.IdleSize)
	return &Reader{src: src, pool: pool, idle: idle, data: idle, max: o.Max}
}

// Buffered returns the data read but not yet discarded. The slice is
// borrowed: valid until the next call that reads, discards or releases.
func (r *Reader) Buffered() []byte { return r.data[r.r:r.w] }

// Len returns the number of buffered bytes.
func (r *Reader) Len() int { return r.w - r.r }

// Peek returns the next n bytes without consuming them, reading from the
// source as needed. The slice is borrowed: valid until the next call that
// reads, discards or releases. If fewer than n bytes can be had, Peek
// returns what is buffered and the error that stopped it. A negative n
// returns ErrNegativeCount and changes nothing.
func (r *Reader) Peek(n int) ([]byte, error) {
	if r.done {
		return nil, ErrReleased
	}
	if n < 0 {
		return nil, ErrNegativeCount
	}
	if n > r.max {
		return r.Buffered(), ErrTooLarge
	}
	for r.Len() < n {
		if err := r.ensure(n); err != nil {
			return r.Buffered(), err
		}
		if err := r.readOnce(); err != nil {
			if r.Len() >= n {
				break
			}
			return r.Buffered(), err
		}
	}
	return r.data[r.r : r.r+n], nil
}

// Fill reads once more from the source, appending to the buffered data,
// and grows the buffer first if it is full. Use it when the frame length
// is not known yet.
func (r *Reader) Fill() error {
	if r.done {
		return ErrReleased
	}
	if err := r.ensure(r.Len() + 1); err != nil {
		return err
	}
	return r.readOnce()
}

// Discard consumes n buffered bytes (fewer if fewer are buffered) and
// returns how many it consumed. When the buffer empties after a light read,
// a grown buffer goes back to the pool. n <= 0 consumes nothing.
func (r *Reader) Discard(n int) int {
	if n <= 0 {
		return 0
	}
	if n > r.Len() {
		n = r.Len()
	}
	r.r += n
	if r.r == r.w {
		r.r, r.w = 0, 0
		if r.big != nil && r.last <= len(r.idle) {
			r.shrink()
		}
	}
	return n
}

// Shrink returns a grown buffer to the pool if nothing is buffered, and
// reports whether it did. Discard does this by itself only after a light
// read, so that steady traffic does not trade buffers back and forth; a
// connection that goes quiet right after a large frame keeps the large
// buffer until its next small read. Call Shrink when the caller knows the
// connection is going idle, for example after handling a frame and before
// a long wait for the next one, so it holds only IdleSize while idle. With
// data buffered, or after Release, it does nothing.
func (r *Reader) Shrink() bool {
	if r.done || r.big == nil || r.Len() != 0 {
		return false
	}
	r.r, r.w = 0, 0
	r.shrink()
	return true
}

// Read implements io.Reader: it hands out buffered data first, then reads
// from the source.
func (r *Reader) Read(p []byte) (int, error) {
	if r.done {
		return 0, ErrReleased
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.Len() == 0 {
		if err := r.readOnce(); err != nil && r.Len() == 0 {
			return 0, err
		}
	}
	n := copy(p, r.Buffered())
	r.Discard(n)
	return n, nil
}

// Release returns any borrowed buffer to the pool. The Reader must not be
// used afterwards; later calls return ErrReleased.
func (r *Reader) Release() {
	if r.done {
		return
	}
	r.done = true
	if r.big != nil {
		r.big.Release()
		r.big = nil
	}
	r.data, r.r, r.w = nil, 0, 0
}

// ensure makes room for n buffered bytes with space left to read into:
// it grows when the buffer is smaller than n, and otherwise compacts or
// grows only when the free space at the end has run out.
func (r *Reader) ensure(n int) error {
	if n > r.max {
		return ErrTooLarge
	}
	if len(r.data) < n {
		return r.grow(n)
	}
	if r.w < len(r.data) && len(r.data)-r.r >= n {
		return nil // room at the end, and n fits from r
	}
	if r.r > 0 { // compact
		r.w = copy(r.data, r.data[r.r:r.w])
		r.r = 0
		if r.w < len(r.data) {
			return nil
		}
	}
	return r.grow(len(r.data) + 1)
}

// grow moves the buffered data into a buffer of at least need bytes: the
// pool's class for need (at least one class above the current buffer), never
// exceeding max. Classes double, so a frame arriving in pieces grows the
// buffer a logarithmic number of times.
func (r *Reader) grow(need int) error {
	size := min(buf.Capacity(max(need, len(r.data)+1)), r.max)
	if size <= len(r.data) {
		return ErrTooLarge
	}
	b := r.pool.Get(size)
	if b == nil {
		return ErrNoBuffer
	}
	r.w = copy(b.B, r.data[r.r:r.w])
	r.r = 0
	if r.big != nil {
		r.big.Release()
	}
	r.big, r.data = b, b.B
	return nil
}

// shrink returns to the idle buffer; only called when nothing is buffered.
func (r *Reader) shrink() {
	r.big.Release()
	r.big, r.data = nil, r.idle
}

// maxEmptyReads is how many (0, nil) reads in a row readOnce accepts
// before giving up with io.ErrNoProgress, as bufio does.
const maxEmptyReads = 100

// readOnce reads from the source once, retrying reads that return no data
// and no error. A source that keeps doing so yields io.ErrNoProgress, which
// is not sticky: a later call reads again.
func (r *Reader) readOnce() error {
	if r.err != nil {
		return r.err
	}
	var n int
	var err error
	for i := 0; ; i++ {
		n, err = r.src.Read(r.data[r.w:])
		if n != 0 || err != nil {
			break
		}
		if i == maxEmptyReads-1 {
			return io.ErrNoProgress
		}
	}
	if n < 0 || n > len(r.data)-r.w {
		r.err = errors.New("stream: invalid Read count from source")
		return r.err
	}
	r.w += n
	r.last = n
	if err != nil {
		r.err = err
		if n > 0 {
			return nil // hand out the data first; the error comes next time
		}
		return err
	}
	return nil
}

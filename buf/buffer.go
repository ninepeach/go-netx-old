// Package buf provides pooled byte buffers with explicit ownership.
//
// The design follows sing's common/buf: an allocator with power-of-two size
// classes (64B to 64KB, one sync.Pool per class) under a cursor buffer that
// can keep room in front of the data, so protocol layers add a header with
// ExtendHeader, strip one with Advance and append with Extend or Write,
// without copying the payload. Unlike sing, the Buf itself is pooled with
// its memory, so Get and Release allocate nothing.
//
// buf does not limit memory (D-24): Get always returns a buffer. Bound
// memory in the application, for example with tcp.Options.Admit and
// stream.ReaderOptions.Max, or with a limit.Budget around Get.
//
// Ownership rules:
//
//  1. Borrowed: data handed to a callback (for example udp.Packet.Data) is
//     valid only until the callback returns. Never Release it; Copy it to keep it.
//  2. Owned: a *Buf from Get, GetHeadroom or Copy has exactly one owner, who
//     must call Release exactly once.
//  3. Transfer: handing a *Buf to someone else (for example pushing it into a
//     queue) transfers ownership. If the transfer fails, ownership stays with
//     the caller, who must still Release it.
//  4. Unmanaged: As and With wrap memory buf does not own; Release on them
//     only forgets it.
//
// Build with -tags netxdebug to panic on double Release, count outstanding
// buffers and poison released memory.
package buf

import (
	"errors"
	"io"
	"net"
	"strconv"
	"unicode/utf8"
)

// ErrInvalidCount is returned when a source or destination reports a count
// outside 0..len(p), which the io contracts forbid. The buffer is left
// unchanged.
var ErrInvalidCount = errors.New("buf: invalid count from Read, ReadFrom or Write")

// Buf is a byte buffer with a start and an end cursor over one block of
// memory:
//
//	[ headroom | start … data … end | free | reserved ]
//
// Bytes returns the data. Write, Extend and the Read*From methods append
// at end; ExtendHeader grows the data backwards into the headroom; Advance
// and Truncate drop data from the front and the back. Slices returned by
// these methods alias the buffer: they are valid until the next call that
// changes it, and never after Release.
//
// Reading consumes, writing out does not: Read, ReadByte and ReadBytes move
// the start cursor past what they return, while Bytes and WriteTo leave
// the data in place. io.Copy(w, b) takes WriteTo, so a second io.Copy
// sends the same data again, whereas io.ReadAll(b) empties b. This is
// sing's behaviour (D-29); after WriteTo, call Advance(n) or Reset to
// drop what was written.
//
// A Buf is not safe for concurrent use.
type Buf struct {
	data     []byte // the whole block; for a pooled buffer, its class size
	start    int
	end      int
	capacity int  // writes stop here; below size after Reserve
	size     int  // headroom plus requested size: the usable block
	head     int  // headroom the buffer was obtained with, restored by Reset
	class    int8 // index into the size classes, -1 if not pooled
	pool     *Pool
	debugState
}

// Release returns a pooled buffer to its pool; on an unmanaged buffer (As,
// With) it only drops the reference. The caller must not touch b, or any
// slice obtained from it, afterwards. Releasing twice is a bug (debug
// builds panic). Release of a nil *Buf does nothing.
func (b *Buf) Release() {
	if b == nil {
		return
	}
	if b.pool == nil {
		*b = Buf{} // As / With: forget the caller's memory
		return
	}
	b.onRelease()
	b.start, b.end, b.capacity, b.size, b.head = 0, 0, 0, 0, 0
	if b.class >= 0 {
		b.pool.pools[b.class].Put(b)
	} else {
		b.data = nil // unpooled: a *Buf kept after Release must not pin it
	}
}

// Len returns the number of data bytes.
func (b *Buf) Len() int { return b.end - b.start }

// Cap returns how far the data may extend: the size asked for plus the
// headroom, minus any Reserve.
func (b *Buf) Cap() int { return b.capacity }

// Start returns the headroom left in front of the data.
func (b *Buf) Start() int { return b.start }

// Bytes returns the data.
func (b *Buf) Bytes() []byte { return b.data[b.start:b.end] }

// FreeLen returns how many bytes can still be appended.
func (b *Buf) FreeLen() int { return b.capacity - b.end }

// FreeBytes returns the space after the data, up to Cap, for reading into
// directly; follow with Extend to account for what was written.
func (b *Buf) FreeBytes() []byte { return b.data[b.end:b.capacity] }

// IsEmpty reports whether there is no data.
func (b *Buf) IsEmpty() bool { return b.end == b.start }

// IsFull reports whether nothing more can be appended.
func (b *Buf) IsFull() bool { return b.end == b.capacity }

// Extend appends n bytes to the data and returns them for writing. It
// panics if they do not fit: header and field sizes are the caller's
// constants, so overflowing is a programming error.
func (b *Buf) Extend(n int) []byte {
	if n < 0 || n > b.capacity-b.end {
		panic("buf: Extend(" + strconv.Itoa(n) + ") with " + strconv.Itoa(b.capacity-b.end) + " bytes free")
	}
	ext := b.data[b.end : b.end+n]
	b.end += n
	return ext
}

// ExtendHeader prepends n bytes to the data, taken from the headroom, and
// returns them for writing. It panics if the headroom is smaller than n.
func (b *Buf) ExtendHeader(n int) []byte {
	if n < 0 || n > b.start {
		panic("buf: ExtendHeader(" + strconv.Itoa(n) + ") with " + strconv.Itoa(b.start) + " bytes of headroom")
	}
	b.start -= n
	return b.data[b.start : b.start+n]
}

// Advance drops n bytes from the front of the data; they become headroom.
// It drops everything if n exceeds Len and panics if n < 0.
func (b *Buf) Advance(n int) {
	if n < 0 {
		panic("buf: negative Advance")
	}
	b.start += min(n, b.end-b.start) // never start+n: n may be up to MaxInt
}

// Truncate keeps the first n bytes of the data. It panics unless
// 0 <= n <= Len.
func (b *Buf) Truncate(n int) {
	if n < 0 || n > b.Len() {
		panic("buf: Truncate(" + strconv.Itoa(n) + ") of " + strconv.Itoa(b.Len()) + " bytes")
	}
	b.end = b.start + n
}

// Reserve sets aside the last n bytes of room, for a trailer added after
// the payload such as an AEAD tag: appends stop n bytes earlier until
// Unreserve. It panics if n exceeds FreeLen.
func (b *Buf) Reserve(n int) {
	if n < 0 || n > b.FreeLen() {
		panic("buf: Reserve(" + strconv.Itoa(n) + ") with " + strconv.Itoa(b.FreeLen()) + " bytes free")
	}
	b.capacity -= n
}

// Unreserve gives back n bytes set aside by Reserve. It panics if that
// would pass the size the buffer was obtained with.
func (b *Buf) Unreserve(n int) {
	if n < 0 || n > b.size-b.capacity {
		panic("buf: Unreserve(" + strconv.Itoa(n) + ") of " + strconv.Itoa(b.size-b.capacity) + " reserved bytes")
	}
	b.capacity += n
}

// Reset empties the buffer: the data starts at the original headroom again
// and any Reserve is given back.
func (b *Buf) Reset() {
	b.start, b.end, b.capacity = b.head, b.head, b.size
}

// FullReset empties the buffer and drops the headroom too: the data starts
// at the very beginning, so all RawCap bytes are room, and any Reserve is
// given back. (sing's Reset; go-netx's Reset keeps the headroom.)
func (b *Buf) FullReset() {
	b.start, b.end, b.capacity = 0, 0, b.size
}

// RawCap returns the room the buffer was obtained with, headroom included,
// whatever Reserve has set aside: the most Cap can be.
func (b *Buf) RawCap() int { return b.size }

// Resize places the data at n bytes starting at position start of the
// buffer, moving both cursors: for a protocol that fills a buffer out of
// order. The arguments are a start and a length, as in sing. Bytes that
// were data keep their value; bytes that were not become zero, because a
// pooled block still holds an earlier owner's bytes there (unlike sing,
// which exposes them; REG-40). To take back bytes dropped with Advance or
// Read, use ExtendHeader, which keeps them; to keep bytes written through
// FreeBytes, use Extend. Unmanaged buffers (As, With) are the caller's
// memory and keep their bytes. It panics unless 0 <= start, 0 <= n and
// start+n <= Cap.
func (b *Buf) Resize(start, n int) {
	if start < 0 || n < 0 || start > b.capacity || n > b.capacity-start {
		panic("buf: Resize(" + strconv.Itoa(start) + ", " + strconv.Itoa(n) + ") with capacity " + strconv.Itoa(b.capacity))
	}
	end := start + n
	if b.pool != nil {
		if hi := min(end, b.start); start < hi {
			clear(b.data[start:hi]) // in front of the old data
		}
		if lo := max(start, b.end); lo < end {
			clear(b.data[lo:end]) // behind it
		}
	}
	b.start, b.end = start, end
}

// Write appends p. It writes what fits and returns io.ErrShortBuffer if not
// all of p did.
func (b *Buf) Write(p []byte) (int, error) {
	n := copy(b.data[b.end:b.capacity], p)
	b.end += n
	if n < len(p) {
		return n, io.ErrShortBuffer
	}
	return n, nil
}

// WriteString appends s, like Write.
func (b *Buf) WriteString(s string) (int, error) {
	n := copy(b.data[b.end:b.capacity], s)
	b.end += n
	if n < len(s) {
		return n, io.ErrShortBuffer
	}
	return n, nil
}

// WriteByte appends c, or returns io.ErrShortBuffer if the buffer is full.
func (b *Buf) WriteByte(c byte) error {
	if b.IsFull() {
		return io.ErrShortBuffer
	}
	b.data[b.end] = c
	b.end++
	return nil
}

// Read copies data into p and consumes what it copied, like io.Reader: it
// returns io.EOF when the buffer is empty. Consuming moves the start
// cursor (the bytes become headroom); nothing is copied within the buffer.
// WriteTo, which io.Copy(w, b) takes instead of Read, does not consume:
// see Buf.
func (b *Buf) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.IsEmpty() {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.start:b.end])
	b.start += n
	return n, nil
}

// ReadByte consumes and returns the first byte, or returns io.EOF.
func (b *Buf) ReadByte() (byte, error) {
	if b.IsEmpty() {
		return 0, io.EOF
	}
	c := b.data[b.start]
	b.start++
	return c, nil
}

// ReadBytes consumes the first n bytes and returns them as a view of the
// buffer, not a copy: valid until the buffer is written to, reset or
// released. With fewer than n bytes it consumes nothing and returns io.EOF
// if the buffer is empty, io.ErrUnexpectedEOF otherwise. It panics if n is
// negative.
func (b *Buf) ReadBytes(n int) ([]byte, error) {
	if n < 0 {
		panic("buf: ReadBytes(" + strconv.Itoa(n) + ")")
	}
	if n > b.Len() {
		if b.IsEmpty() {
			return nil, io.EOF
		}
		return nil, io.ErrUnexpectedEOF
	}
	v := b.data[b.start : b.start+n : b.start+n]
	b.start += n
	return v, nil
}

// Byte returns the data byte at position i. It panics unless 0 <= i < Len.
func (b *Buf) Byte(i int) byte {
	b.checkIndex("Byte", i, b.Len()-1)
	return b.data[b.start+i]
}

// SetByte sets the data byte at position i. It panics unless 0 <= i < Len.
func (b *Buf) SetByte(i int, c byte) {
	b.checkIndex("SetByte", i, b.Len()-1)
	b.data[b.start+i] = c
}

// To returns the first n bytes of the data. Like From and Range it returns
// a view, with no capacity beyond its length, and panics outside 0..Len.
func (b *Buf) To(n int) []byte {
	b.checkIndex("To", n, b.Len())
	return b.data[b.start : b.start+n : b.start+n]
}

// From returns the data from position i to its end.
func (b *Buf) From(i int) []byte {
	b.checkIndex("From", i, b.Len())
	return b.data[b.start+i : b.end : b.end]
}

// Range returns the data from position i up to, not including, position j.
func (b *Buf) Range(i, j int) []byte {
	b.checkIndex("Range", j, b.Len())
	b.checkIndex("Range", i, j)
	return b.data[b.start+i : b.start+j : b.start+j]
}

// checkIndex panics unless 0 <= i <= max. Positions are checked against the
// data, not the memory under it, so a wrong offset cannot reach the
// headroom, the free space or a neighbour's bytes left in the class.
func (b *Buf) checkIndex(op string, i, max int) {
	if i < 0 || i > max {
		panic("buf: " + op + " position " + strconv.Itoa(i) + " outside 0.." + strconv.Itoa(max))
	}
}

// WriteRune appends the UTF-8 encoding of r (of U+FFFD if r is not a valid
// rune) and returns its length. It writes nothing and returns
// io.ErrShortBuffer if the whole encoding does not fit.
func (b *Buf) WriteRune(r rune) (int, error) {
	n := utf8.RuneLen(r)
	if n < 0 {
		n = utf8.RuneLen(utf8.RuneError)
	}
	if n > b.FreeLen() {
		return 0, io.ErrShortBuffer
	}
	utf8.AppendRune(b.data[b.end:b.end], r)
	b.end += n
	return n, nil
}

// WriteZero appends one zero byte, like WriteByte(0).
func (b *Buf) WriteZero() error { return b.WriteByte(0) }

// WriteZeroN appends n zero bytes. It writes nothing and returns
// io.ErrShortBuffer if they do not fit, and panics if n is negative.
func (b *Buf) WriteZeroN(n int) error {
	if n < 0 {
		panic("buf: WriteZeroN(" + strconv.Itoa(n) + ")")
	}
	if n > b.FreeLen() {
		return io.ErrShortBuffer
	}
	clear(b.data[b.end : b.end+n])
	b.end += n
	return nil
}

// ReadOnceFrom reads once from r into the free space and appends what it
// read. It returns io.ErrShortBuffer, without reading, if the buffer is
// full, and ErrInvalidCount, appending nothing, if r reports a count
// outside the space it was given.
func (b *Buf) ReadOnceFrom(r io.Reader) (int, error) {
	if b.IsFull() {
		return 0, io.ErrShortBuffer
	}
	n, err := r.Read(b.data[b.end:b.capacity])
	if n < 0 || n > b.capacity-b.end {
		return 0, ErrInvalidCount
	}
	b.end += n
	return n, err
}

// ReadFullFrom reads exactly n bytes from r and appends them, or returns
// io.ErrShortBuffer, without reading, if n does not fit. On a short read
// the bytes read are kept and the error is io.ErrUnexpectedEOF or r's.
func (b *Buf) ReadFullFrom(r io.Reader, n int) (int, error) {
	if n < 0 || n > b.FreeLen() {
		return 0, io.ErrShortBuffer
	}
	return b.readAtLeast(r, n, n)
}

// ReadAtLeastFrom reads from r into the free space until at least min
// bytes have been appended, and keeps more if r offers it. It returns
// io.ErrShortBuffer, without reading, if min does not fit. Like
// io.ReadAtLeast, a source that ends early gives io.EOF if nothing was
// read and io.ErrUnexpectedEOF otherwise; what was read is kept. With
// min <= 0 it reads once, like ReadOnceFrom.
func (b *Buf) ReadAtLeastFrom(r io.Reader, min int) (int, error) {
	if min <= 0 {
		return b.ReadOnceFrom(r)
	}
	if min > b.FreeLen() {
		return 0, io.ErrShortBuffer
	}
	return b.readAtLeast(r, min, b.FreeLen())
}

// ReadFrom appends everything r has, until r returns io.EOF, which is
// success (io.ReaderFrom, so io.Copy into a Buf uses it). The buffer does
// not grow: if r has more than fits, ReadFrom fills the buffer and returns
// io.ErrShortBuffer with the count stored. To tell that case from data
// that fits exactly, it reads one more byte from r once the buffer is
// full. On io.ErrShortBuffer that byte is given back if r is an
// io.ByteScanner (bufio.Reader, bytes.Reader, strings.Reader) and is
// otherwise gone from r: on a stream such as a net.Conn, where losing it
// desynchronises the protocol, read with ReadAtLeastFrom or ReadOnceFrom
// instead. Like the other Read*From methods it returns ErrInvalidCount for
// a count outside the space given and io.ErrNoProgress for a source that
// keeps returning (0, nil).
func (b *Buf) ReadFrom(r io.Reader) (int64, error) {
	var n int64
	var probe []byte // allocated once, only when the buffer is full
	empty := 0
	for {
		p := b.data[b.end:b.capacity]
		full := len(p) == 0
		if full {
			if probe == nil {
				probe = make([]byte, 1)
			}
			p = probe
		}
		k, err := r.Read(p)
		if k < 0 || k > len(p) {
			return n, ErrInvalidCount
		}
		if full && k > 0 {
			if s, ok := r.(io.ByteScanner); ok {
				_ = s.UnreadByte() // best effort: r keeps the byte if it can
			}
			return n, io.ErrShortBuffer
		}
		b.end += k
		n += int64(k)
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if k > 0 {
			empty = 0
		} else if empty++; empty >= maxEmptyReads {
			return n, io.ErrNoProgress
		}
	}
}

// maxEmptyReads is how many reads in a row may return (0, nil) before the
// source is given up on with io.ErrNoProgress (bufio uses the same count).
const maxEmptyReads = 100

// readAtLeast appends at least min and at most max bytes from r; the
// caller has checked 0 < min <= max <= FreeLen (min may be 0 only with
// max 0). Unlike io.ReadAtLeast it does not trust r: a count outside the
// space given returns ErrInvalidCount, leaving what was read before it,
// and a run of empty reads returns io.ErrNoProgress instead of spinning.
func (b *Buf) readAtLeast(r io.Reader, min, max int) (int, error) {
	limit := b.end + max
	n, empty := 0, 0
	for n < min {
		k, err := r.Read(b.data[b.end:limit])
		if k < 0 || k > limit-b.end {
			return n, ErrInvalidCount
		}
		b.end += k
		n += k
		if err != nil {
			switch {
			case n >= min:
				return n, nil
			case err == io.EOF && n > 0:
				return n, io.ErrUnexpectedEOF
			}
			return n, err
		}
		if k > 0 {
			empty = 0
		} else if empty++; empty >= maxEmptyReads {
			return n, io.ErrNoProgress
		}
	}
	return n, nil
}

// ReadPacketFrom reads one datagram from c into the free space and appends
// it. A datagram larger than FreeLen is truncated by the system. Like
// ReadOnceFrom, it returns ErrInvalidCount for a count outside the space.
func (b *Buf) ReadPacketFrom(c net.PacketConn) (int, net.Addr, error) {
	if b.IsFull() {
		return 0, nil, io.ErrShortBuffer
	}
	n, addr, err := c.ReadFrom(b.data[b.end:b.capacity])
	if n < 0 || n > b.capacity-b.end {
		return 0, addr, ErrInvalidCount
	}
	b.end += n
	return n, addr, err
}

// WriteTo writes the data to w. Unlike bytes.Buffer.WriteTo it does not
// consume it, so the data can be sent again (to several peers, or after a
// retryable error); call Advance(n) with the count to drop what was
// written. A short write that
// w reports with a nil error, which io.Writer forbids but some wrappers do,
// returns io.ErrShortWrite; a count outside 0..Len returns ErrInvalidCount.
func (b *Buf) WriteTo(w io.Writer) (int64, error) {
	p := b.Bytes()
	n, err := w.Write(p)
	if n < 0 || n > len(p) {
		return 0, ErrInvalidCount
	}
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	return int64(n), err
}

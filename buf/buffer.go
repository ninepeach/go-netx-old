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
	k, err := io.ReadFull(r, b.data[b.end:b.end+n])
	b.end += k
	return k, err
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

// WriteTo writes the data to w. It does not consume it. A short write that
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

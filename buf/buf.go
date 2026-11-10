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
	"io"
	"math/bits"
	"net"
	"strconv"
	"sync"
)

// Size classes are powers of two from MinSize to Large: 64B, 128B, …, 64KB.
// A request takes the smallest class that holds it, so a buffer's memory
// is less than twice its size (except below MinSize), and the class is one
// bit operation away. Small, Medium and Large name common sizes.
const (
	MinSize = 64       // smallest class: one cache line
	Small   = 2 << 10  // one MTU-sized datagram
	Medium  = 16 << 10 // a typical stream read
	Large   = 64 << 10 // largest pooled class (D-26); larger requests are not pooled
)

const (
	minShift   = 6 // log2(MinSize)
	numClasses = 11
)

// Capacity returns the memory a buffer of n bytes (headroom included)
// occupies: the smallest class holding n, or n itself above Large, where
// buffers are allocated directly and not pooled.
func Capacity(n int) int {
	if c := classOf(n); c >= 0 {
		return MinSize << c
	}
	return n
}

// classOf returns the class index for n, or -1 above Large.
func classOf(n int) int {
	if n > Large {
		return -1
	}
	if n <= MinSize {
		return 0
	}
	return bits.Len(uint(n-1)) - minShift
}

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

// Pool hands out buffers in power-of-two size classes. A Pool is safe for
// concurrent use. The zero value is not usable; call NewPool, or use
// Default through the package-level Get, GetHeadroom and Copy.
type Pool struct {
	pools [numClasses]sync.Pool
}

// NewPool returns an empty pool.
func NewPool() *Pool {
	p := &Pool{}
	for i := range p.pools {
		size, class := MinSize<<i, int8(i)
		p.pools[i].New = func() any {
			return &Buf{data: make([]byte, size), class: class, pool: p}
		}
	}
	return p
}

// Default is the pool behind the package-level functions.
var Default = NewPool()

// Get returns an empty buffer from Default with room for size bytes.
func Get(size int) *Buf { return Default.Get(size) }

// GetHeadroom returns an empty buffer from Default with head bytes kept
// in front for ExtendHeader and room for size bytes of data.
func GetHeadroom(head, size int) *Buf { return Default.GetHeadroom(head, size) }

// Copy returns a buffer from Default holding a copy of data.
func Copy(data []byte) *Buf { return Default.Copy(data) }

// Get returns an empty buffer with room for size bytes: Len 0, Cap and
// FreeLen size. It never returns nil and panics if size is negative.
// Contents are not zeroed: write before you read or send.
func (p *Pool) Get(size int) *Buf { return p.GetHeadroom(0, size) }

// GetHeadroom returns an empty buffer whose data starts head bytes in, so
// up to head bytes of headers can be added in front with ExtendHeader, with
// room for size bytes after that. Cap is head+size. It panics if either is
// negative.
func (p *Pool) GetHeadroom(head, size int) *Buf {
	if head < 0 || size < 0 || head+size < 0 {
		panic("buf: negative size")
	}
	total := head + size
	var b *Buf
	if class := classOf(total); class >= 0 {
		b = p.pools[class].Get().(*Buf)
	} else {
		b = &Buf{data: make([]byte, total), class: -1, pool: p}
	}
	b.start, b.end, b.head = head, head, head
	b.capacity, b.size = total, total
	b.onGet()
	return b
}

// Copy returns a buffer holding a copy of data.
func (p *Pool) Copy(data []byte) *Buf {
	b := p.Get(len(data))
	b.end += copy(b.data[:b.capacity], data)
	return b
}

// As wraps data as a buffer that holds it (Bytes returns data). The memory
// stays the caller's: Release only forgets it.
func As(data []byte) *Buf {
	return &Buf{data: data, end: len(data), capacity: len(data), size: len(data), class: -1}
}

// With wraps data as an empty buffer that writes into it. The memory stays
// the caller's: Release only forgets it.
func With(data []byte) *Buf {
	return &Buf{data: data, capacity: len(data), size: len(data), class: -1}
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
	b.start = min(b.start+n, b.end)
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
// full.
func (b *Buf) ReadOnceFrom(r io.Reader) (int, error) {
	if b.IsFull() {
		return 0, io.ErrShortBuffer
	}
	n, err := r.Read(b.data[b.end:b.capacity])
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
// it. A datagram larger than FreeLen is truncated by the system.
func (b *Buf) ReadPacketFrom(c net.PacketConn) (int, net.Addr, error) {
	if b.IsFull() {
		return 0, nil, io.ErrShortBuffer
	}
	n, addr, err := c.ReadFrom(b.data[b.end:b.capacity])
	b.end += n
	return n, addr, err
}

// WriteTo writes the data to w. It does not consume it.
func (b *Buf) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(b.Bytes())
	return int64(n), err
}

# Package buf — design

Overall design: [ARCHITECTURE.md](../../ARCHITECTURE.md).

## 1. Scope

| Owns | Does not own |
|---|---|
| Pooled byte buffers in power-of-two size classes (64B…64KB) | I/O (see `stream`) |
| A cursor buffer with headroom for adding and stripping protocol headers | Encoding |
| The ownership rules every go-netx package follows | Memory limits: the application's (D-24) |
| Debug-build ownership checks | |

Imports only the standard library (`io`, `math/bits`, `net`, `strconv`,
`sync`).

## 2. API

```go
const MinSize, Small, Medium, Large = 64, 2 << 10, 16 << 10, 64 << 10
func Capacity(n int) int                     // memory a buffer of n bytes occupies

func NewPool() *Pool
var Default *Pool                            // behind the package-level functions
func Get(size int) *Buf                      // = Default.Get
func GetHeadroom(head, size int) *Buf        // = Default.GetHeadroom
func Copy(data []byte) *Buf                  // = Default.Copy
func (*Pool) Get(size int) *Buf              // empty, Cap == size; never nil
func (*Pool) GetHeadroom(head, size int) *Buf // empty, head bytes in front for ExtendHeader
func (*Pool) Copy(data []byte) *Buf          // borrowed → owned
func As(data []byte) *Buf                    // unmanaged: holds data
func With(data []byte) *Buf                  // unmanaged: empty, writes into data

// [ headroom | start … data … end | free | reserved ]
func (*Buf) Len() int; Cap() int; Start() int
func (*Buf) Bytes() []byte; FreeBytes() []byte; FreeLen() int
func (*Buf) IsEmpty() bool; IsFull() bool
func (*Buf) Extend(n int) []byte             // append n bytes; panics past Cap
func (*Buf) ExtendHeader(n int) []byte       // prepend n bytes from the headroom; panics past it
func (*Buf) Advance(n int)                   // drop n bytes in front
func (*Buf) Truncate(n int)                  // keep the first n bytes
func (*Buf) Reserve(n int); Unreserve(n int) // set aside / give back room at the end
func (*Buf) Reset()                          // empty, original headroom and room
func (*Buf) Write([]byte) (int, error)       // io.ErrShortBuffer when it does not fit
func (*Buf) WriteString(string) (int, error); WriteByte(byte) error
func (*Buf) ReadOnceFrom(io.Reader) (int, error)
func (*Buf) ReadFullFrom(io.Reader, n int) (int, error)
func (*Buf) ReadPacketFrom(net.PacketConn) (int, net.Addr, error)
func (*Buf) WriteTo(io.Writer) (int64, error)
func (*Buf) Release()                        // nil-safe; unmanaged: only forgets

func LenMulti([]*Buf) int; IsEmptyMulti([]*Buf) bool
func ToSliceMulti([]*Buf) [][]byte           // aliases; net.Buffers for writev
func CopyMulti(dst []byte, bufs []*Buf) int
func ReleaseMulti([]*Buf)                    // releases each once, clears the slice
const Debug bool                             // true with -tags netxdebug
const Poison = 0xDB                          // debug builds fill released buffers with it
func Outstanding() int64                     // debug builds
var ErrInvalidCount error                    // a Read/ReadFrom/Write count outside 0..len(p)
```

Files, as in sing: `alloc.go` (size classes, `Capacity`), `pool.go`
(`Pool`, `Default`, `Get`, `GetHeadroom`, `Copy`, `As`, `With`),
`buffer.go` (`Buf` and its methods), `multi.go` (the `*Multi` helpers),
`debug.go`/`nodebug.go` (ownership checks).

## 3. Ownership rules

The rules (borrowed, owned, transfer, shared) are defined for every package
in [ARCHITECTURE.md](../../ARCHITECTURE.md), "Ownership rules". buf provides
the types they are about: a `*Buf` from `Get`, `GetHeadroom` or `Copy` is
owned; `Release` ends ownership; handing a `*Buf` on (`queue.Ring.Push`,
`stream.Writer.Add`) transfers it, and a failed transfer leaves it with the
caller. `As` and `With` wrap memory buf does not own; `Release` on them only
drops the reference, so code that releases every `*Buf` it is given works
for both.

Slices from `Bytes`, `FreeBytes`, `Extend` and `ExtendHeader` alias the
buffer: valid until the next call that changes it, never after `Release`.

## 4. Design

**After sing's `common/buf`.** The design follows sing (SagerNet): the same
power-of-two allocator under a cursor buffer, and the same method names
(`Extend`, `ExtendHeader`, `Advance`, `Truncate`, `Reserve`, `FreeBytes`,
`ReadOnceFrom`, `As`, `With`), so code written against one reads naturally
against the other. The cursor buffer is what proxies and gateways need for
layered protocols: TLS, a proxy protocol and a mux each add or strip a
header, and with headroom (`GetHeadroom`) that never moves the payload.
Where go-netx differs:

- *The `*Buf` is pooled with its memory.* sing pools `*[N]byte` arrays and
  allocates a new `Buffer` struct on every `New`; here `Get` and `Release`
  allocate nothing.
- *A `*Buf` knows its class.* sing's `Put([]byte)` accepts any slice whose
  capacity is a power of two, so foreign memory can enter the pool; here
  only buffers a Pool handed out go back to it.
- *No reference counting (D-25).* sing's `IncRef`/`DecRef` only count:
  `Release` is skipped while the count is above zero and nothing releases
  when it reaches zero, so a missed call leaks. Shared immutable data uses
  `stream.Writer.AddShared`; real shared ownership waits for a case that
  needs it, and would release on the last `Release`.
- *No fixed buffer sizes or build tags* (`BufferSize`, `UDPBufferSize`,
  `with_low_memory`): sizes are the application's choice.
- *Debug builds* check ownership (below); sing's `Leak` only panics when
  called.

**No memory limit in buf (D-24).** `Get` always returns a buffer. Memory is
bounded where the application decides: `tcp.Options.Admit` and
`MaxConnsPerIP` cap connections, `stream.ReaderOptions.Max` caps what one
connection buffers, so the worst case is about connections × Max. A gateway
that wants a byte budget reserves from a `limit.Budget` before `Get` and
releases after `Release` (INT-07 shows the pattern).

**Power-of-two classes, 64B to 64KB (D-21, D-26).** Eleven classes, one
`sync.Pool` each, as in sing and smux. A request (headroom included) takes
the smallest class that holds it, found with one `bits.Len`, so its memory
is under twice its size from 64B up. The earlier three classes (2KB, 16KB,
64KB) wasted up to 8×. 64KB is the largest pooled class: a pooled buffer
stays on the heap for two GC cycles after its last use, so pooling rare
large ones keeps a burst's worth of them alive and raises the next GC
target (the reason `fmt` drops buffers over 64KB, golang/go#23199). Larger
requests are allocated directly and dropped on `Release`. The default
`stream.ReaderOptions.Max` is pinned at 64KB on its own.

**Cap is what was asked for, not the class.** `Get(1000)` has `Cap` 1000
even though it sits in a 1KB class: a buffer never offers more room than
the caller planned for, and `Reset` and `Unreserve` cannot pass it.

**Overflow.** `Extend`, `ExtendHeader`, `Truncate`, `Reserve` and
`Unreserve` panic on sizes that do not fit: those sizes are the caller's
constants, so a mismatch is a programming error, as in sing. `Write`,
`WriteString`, `WriteByte` and the `Read*From` methods take data from
outside and return `io.ErrShortBuffer` instead. `Advance` takes any
non-negative n, up to `math.MaxInt`, and drops at most `Len` bytes; it never
computes start+n, which could overflow.

**Counts from outside are checked.** A reader, `PacketConn` or writer that
reports a count outside 0..len(p) breaks the io contract; `ReadOnceFrom`,
`ReadPacketFrom` and `WriteTo` return `ErrInvalidCount` and leave the
cursors alone rather than move them past `Cap` or before the data.
`WriteTo` turns a short write with a nil error into `io.ErrShortWrite`, as
`stream.Writer.Flush` does (REG-30, REG-36).

**Contents are not zeroed** (cost). A new buffer is empty; callers write
before they send. Sending bytes that were never written would leak a
previous owner's data, which debug poisoning makes visible.

**Debug builds** (`-tags netxdebug`): each Buf tracks whether it is owned;
releasing twice panics, `Outstanding()` counts unreleased buffers, and
Release fills the whole block with `Poison` (0xDB), so a read after Release,
or bytes sent without being written, show up as runs of 0xDB in tests.
Release builds compile these checks away. Tests and stress runs use both.

## 5. Concurrency

`Pool` is safe for concurrent use. A `Buf` belongs to one owner at a time
and is not safe for concurrent use.

## 6. Performance (2 vCPU)

Get + Release about 5–6ns at every pooled size from 64B to 64KB, 0
allocations (`BenchmarkGetReleaseBySize`, medians of 6, 2026-11-11).
Unpooled requests cost an allocation and its zeroing: 70KB about 15µs,
140KB about 28µs. The write path of a two-layer protocol (1200B payload,
20 bytes of headers added in front with `ExtendHeader`, a 16-byte tag
behind, then `WriteTo`) costs about 14ns with 0 allocations and no copy of
the payload (`BenchmarkHeaderPath`).

| request | memory, three classes | memory now |
|---|---|---|
| 64B | 2KB | 64B |
| 200B | 2KB | 256B |
| 1200B | 2KB | 2KB |
| 3000B | 16KB | 4KB |
| 9000B | 16KB | 16KB |
| 40000B | 64KB | 64KB |
| 70000B | unpooled | unpooled |

## 7. Decisions

D-21 (power-of-two classes), D-24 (no memory limit in buf; supersedes
D-05), D-25 (no reference counting), D-26 (largest pooled class 64KB).

## 8. Testing

BUF-01…BUF-25, NF-11 (fuzz against a slice model of every cursor
operation) in [docs/TESTING.md](../TESTING.md).

## 9. Open items

None.

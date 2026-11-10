# Package stream — design

Overall design: [ARCHITECTURE.md](../../ARCHITECTURE.md). Origin of the
ideas: ARCHITECTURE.md, "Connection model compared with cloudwego/netpoll".

## 1. Scope

| Owns | Does not own |
|---|---|
| A stream reader whose memory follows the data in flight | Where a frame ends (the decoder's) |
| Sending a backlog of owned buffers with one writev | When to flush (the gateway's policy) |
| The `BuffersWriter` contract for wrapped connections | Connections themselves |

Imports `buf` only. Transports do not import `stream`; gateways compose
them, for example `stream.NewReader(conn, pool, …)` over a `*tcp.Conn`.
It is a package of its own (D-12): `buf` manages memory only, transports
hold no buffers. It serves byte streams (TCP, TLS, Unix sockets); UDP
handlers work on datagrams inline and do not need it.

## 2. API

```go
func NewReader(src io.Reader, pool *buf.Pool, o ReaderOptions) *Reader
type ReaderOptions struct {
    IdleSize int // buffer kept while idle; default 512
    Max      int // most bytes ever buffered (largest frame); default buf.Large
}
func (*Reader) Peek(n int) ([]byte, error) // borrowed; reads until n bytes or error; n < 0: ErrNegativeCount
func (*Reader) Fill() error                // one more read, growing if full
func (*Reader) Buffered() []byte           // borrowed
func (*Reader) Len() int
func (*Reader) Discard(n int) int          // n <= 0 consumes nothing
func (*Reader) Read(p []byte) (int, error) // io.Reader
func (*Reader) Release()
var ErrTooLarge, ErrNoBuffer, ErrReleased

func NewWriter(dst io.Writer) *Writer
func (*Writer) Add(b *buf.Buf)            // takes ownership
func (*Writer) AddShared(p []byte)        // borrows immutable shared data; never released
func (*Writer) Flush() (int64, error)     // writes all in order, releases the owned buffers
func (*Writer) Discard()                  // drops all without writing, releases the owned
func (*Writer) Pending() (buffers, bytes int)
type BuffersWriter interface{ WriteBuffers(*net.Buffers) (int64, error) }
```

## 3. Reader design

**Memory follows pending data.** The Reader owns a small idle buffer for
its whole life and borrows a larger one from the pool only when needed:

- *Grow* when `Peek(n)` needs more than the buffer holds, or a `Fill` finds
  it full: move to the pool's class for the frame (`buf.Capacity`: powers of
  two from 64B to 64KB, at least one class up), or `Max` if
  larger), copying the buffered bytes, and release the previous borrowed
  buffer.
- *Compact* before growing when consumed bytes sit at the front.
- *Shrink* when `Discard` empties the buffer and the last read was no larger
  than the idle buffer: release the borrowed buffer and go back to idle.
  Requiring a light last read keeps a busy connection from bouncing between
  sizes on every frame.

**Limits.** `Peek(n)` with `n > Max`, or growth past `Max`, returns
`ErrTooLarge`: a frame larger than the protocol allows should close the
connection rather than grow memory. When the pool's budget cannot supply a
larger buffer, `ErrNoBuffer` is returned and buffered data is kept intact,
so the caller can retry later or close.

**Errors.** A read that returns data and an error hands the data out first;
the error is sticky and returned on the next read. Reads that return no
data and no error are retried up to 100 times in a row, as in bufio, then
`io.ErrNoProgress` is returned (REG-22); it is not sticky, so a later call
reads again.

**Borrowed results.** Slices from `Peek` and `Buffered` stay valid until the
next call that reads, discards or releases, the same rule as
`udp.Packet.Data`.

## 4. Writer design

`Add` appends a `*buf.Buf` and takes ownership. `AddShared` appends a
plain `[]byte` without taking ownership. Both go into one send vector in
call order, so a frame header from the pool and a shared payload are sent
together. `Flush`:

- 0 slices: nothing.
- 1 slice: one `Write` (writev costs more than it saves for one slice).
- 2+ slices: one `net.Buffers` write. If the destination implements
  `BuffersWriter` it is used, otherwise `net.Buffers.WriteTo`.

Every buffer from `Add` is released after `Flush`, whether the write
succeeded or not: after a failed or partial write the stream is broken.
Shared slices are never released; the caller must not change them until
`Flush` or `Discard` returns, after which the Writer keeps no reference.
The vector slices are reused and cleared, so steady-state `Add`+`Flush`
and `AddShared`+`Flush` allocate nothing.

**Shared data.** Data that is the same for every connection — an encoded
game configuration loaded at login, a broadcast message — should be
encoded once into an immutable `[]byte` and sent with `AddShared` to each
connection, instead of being copied into a pooled buffer per connection.
Measured (2 vCPU): allocating 1MB costs about 140µs and 4MB about 1.5ms,
so 10,000 players reconnecting after a restart would spend about 1.4s of
CPU and 10GB of allocation copying a 1MB configuration; shared, it costs
one copy in memory. A hot reload swaps in a new slice; writes still using
the old one finish with it. Read-only sharing across goroutines is
race-free (STREAM-20).

**Why `BuffersWriter`.** `net.Buffers` uses writev only on the standard
library's own connection types. Any wrapper (`*tcp.Conn`, a metrics
wrapper) silently loses it and falls back to one Write per buffer. Wrappers
implement `WriteBuffers` and pass the vector to the inner connection. They
must never "unwrap" to the raw socket instead: for a TLS connection that
would write plaintext.

## 5. Concurrency

Neither type is safe for concurrent use: one goroutine owns a connection's
Reader and another (or the same) its Writer.

## 6. Performance (2 vCPU)

- 4000 idle connections: about 3.6KB less heap per connection than a 4KB
  `bufio.Reader`; about 430B more than holding no buffer.
- Through a `*tcp.Conn`, 64B messages: 8 queued about 6× faster than one
  `Write` each, 32 queued about 9–10×; one queued equals a plain `Write`;
  0 allocations.

## 7. Decisions

D-12 (separate package). Not adopted from netpoll: the linked-chunk
`LinkBuffer`, which exists because netpoll's poller reads before a handler
runs; here the handler reads for itself and one contiguous buffer suffices.

## 8. Testing

STREAM-01…STREAM-20, NF-20 (fuzz) in [docs/TESTING.md](../TESTING.md).

## 9. Open items

None.

## Cross-repository target-design alignment (2026-11-10)

The [canonical target design](https://github.com/ninepeach/architecture-docs/blob/main/projects/go-netx/design.md) and [phased implementation plan](https://github.com/ninepeach/architecture-docs/blob/main/projects/go-netx/implementation-steps.zh.md) describe additional planned abstractions and acceptance criteria. This document specifies the **current module contract**. Current stream Reader/Writer handles byte-stream buffering and writes, not FrameCodec, logical Flow lifecycle, or the canonical plan's per-Flow writer queue. Do not treat target APIs as implemented or change this module's boundaries without an explicit cross-repository design decision.

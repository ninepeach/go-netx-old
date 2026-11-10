# Package buf — design

Overall design: [ARCHITECTURE.md](../../ARCHITECTURE.md).

## 1. Scope

| Owns | Does not own |
|---|---|
| Pooled byte buffers in power-of-two size classes (64B…64KB) | I/O (see `stream`) |
| The ownership rules every go-netx package follows | Encoding |
| Charging in-use memory to a `limit.Budget` | Process memory limits |
| Debug-build ownership checks | |

Imports `limit` only (and `math/bits`, `sync`).

## 2. API

```go
const MinSize, Small, Medium, Large = 64, 2 << 10, 16 << 10, 64 << 10
func Capacity(n int) int                 // capacity (and budget cost) of Get(n)
func NewPool(budget *limit.Budget) *Pool // budget may be nil
func (*Pool) Get(n int) *Buf             // len(B) == n, cap(B) == Capacity(n); nil when over budget
func (*Pool) Copy(data []byte) *Buf      // borrowed → owned
type Buf struct{ B []byte /* … */ }
func (*Buf) Release()                    // nil-safe
const Debug bool                          // true with -tags netxdebug
const Poison = 0xDB                       // debug builds fill released buffers with it
func Outstanding() int64                  // debug builds
```

## 3. Ownership rules

1. **Borrowed**: data handed to a callback (`udp.Packet.Data`,
   `stream.Reader.Peek`) is valid only until the callback returns or the
   next call. Never release it; `Copy` it to keep it.
2. **Owned**: a `*Buf` from `Get` or `Copy` has exactly one owner, who
   releases it exactly once.
3. **Transfer**: handing a `*Buf` on (`queue.Ring.Push`, `stream.Writer.Add`)
   transfers ownership. If the transfer fails, the caller still owns it.
4. **Shared**: immutable data many connections send (`stream.Writer.AddShared`)
   is a plain `[]byte` outside the pool: never owned, never released, not
   changed while queued.

Discard paths are where leaks happen, which is why `queue` has `OnDrop` and
`stream.Writer` releases on every Flush outcome.

## 4. Design

**Power-of-two classes, 64B to 64KB (D-21).** Eleven classes, one
`sync.Pool` each. A request takes the smallest class that holds it, found
with one `bits.Len`, so capacity is under twice the length (from 64B up)
and the budget, which charges capacity, holds at least half data. This is
what mainstream Go pools do (bytedance/gopkg `mcache` used by netpoll,
valyala/bytebufferpool used by fasthttp, xtaci/smux, gnet, libp2p). The
earlier three classes (2KB, 16KB, 64KB) wasted up to 8×: a 3KB IM message
held 16KB and a 50B game packet 2KB; now they hold 4KB and 64B. More
classes cost nothing measurable: each class is its own per-P pool, and Get
+ Release stayed at about 9ns. 64B is the floor because smaller buffers
save little next to the `Buf` header; 64KB is the largest UDP datagram.
Requests above `Large` are allocated directly and not pooled, but still
budgeted. `Capacity(n)` exposes the rounding so callers can plan budgets
and `stream.Reader` can grow class by class.

**`*Buf`, not `[]byte`, in the pool.** Putting a slice into `sync.Pool`
allocates a header on every Put; a pointer does not. `Get` sets `B` to the
requested length over the full class capacity; `Release` sets `B` to nil so
a stale reference fails fast instead of reading reused memory.

**Budget = in-use memory only.** `Get` charges the class capacity before
taking a buffer and `Release` refunds it; a refused `Get` charges nothing.
Released buffers may stay in `sync.Pool` until the GC reclaims them, so the
budget bounds how much data the gateway holds at once, not process RSS.

**Contents are not zeroed** on Release (cost); `Get(n)` exposes only `n`
bytes, and callers overwrite what they use. Sending a buffer that was not
fully written would leak a previous owner's bytes, which is what the debug
poisoning below is for.

**`Release` of nil is a no-op**, so `b := pool.Get(n); defer b.Release()`
is safe when the budget refuses and Get returns nil.

**Debug builds** (`-tags netxdebug`): each Buf tracks whether it is owned;
releasing twice panics, `Outstanding()` counts unreleased buffers, and
Release fills the whole buffer with `Poison` (0xDB), so a read after
Release, or bytes sent without being written, show up as runs of 0xDB in
tests. Release builds compile these checks away. Tests and stress runs use
both.

## 5. Concurrency

`Pool` is safe for concurrent use. A `Buf` belongs to one owner at a time.

## 6. Performance (2 vCPU)

Get + Release about 9ns at every pooled size from 64B to 64KB, 0
allocations, the same as the three-class version measured in the same
run (`BenchmarkGetReleaseBySize`, medians of 6). Unpooled (70KB) about
11µs and 2 allocations, unchanged.

| request | capacity before | capacity now |
|---|---|---|
| 64B | 2KB | 64B |
| 200B | 2KB | 256B |
| 1200B | 2KB | 2KB |
| 3000B | 16KB | 4KB |
| 9000B | 16KB | 16KB |
| 40000B | 64KB | 64KB |

## 7. Decisions

D-05 (budget counts in-use memory only), D-21 (power-of-two classes).

## 8. Testing

BUF-01…BUF-17, NF-11 (fuzz) in [docs/TESTING.md](../TESTING.md).

## 9. Open items

None.

## Cross-repository target-design alignment (2026-11-10)

The [canonical target design](https://github.com/ninepeach/architecture-docs/blob/main/projects/go-netx/design.md) and [phased implementation plan](https://github.com/ninepeach/architecture-docs/blob/main/projects/go-netx/implementation-steps.zh.md) describe additional planned abstractions and acceptance criteria. This document specifies the **current module contract**. Current 64B–64KB sync.Pool classes and in-use Budget do not establish the canonical target's max retained bytes or per-class retained count. The target's larger size classes are examples, not the current API. Do not treat target APIs as implemented or change this module's boundaries without an explicit cross-repository design decision.

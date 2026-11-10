# Package queue — design

Overall design: [ARCHITECTURE.md](../../ARCHITECTURE.md).

## 1. Scope

| Owns | Does not own |
|---|---|
| A bounded FIFO with an explicit overflow policy | Understanding items (coalescing, priorities) |
| Backpressure: refusal or eviction, never growth | Admission and resource totals (see `limit`) |
| Handing discarded items back for release | Scheduling consumers |

`queue` imports nothing. A Media jitter buffer or a Game queue that merges
position updates needs the item contents and stays in the gateway.

## 2. API

```go
func New[T any](Options[T]) *Ring[T]
type Options[T any] struct {
    Cap    int    // rounded up to a power of two; must be positive
    Policy Policy // Reject (default) or DropOldest
    OnDrop func(T) // every evicted or drained item
}
func (*Ring[T]) Push(T) bool                      // never blocks
func (*Ring[T]) PushWait(ctx, T) error            // blocks for room; ignores Policy
func (*Ring[T]) Pop() (T, bool)
func (*Ring[T]) PopWait(ctx) (T, error)
func (*Ring[T]) Close()                            // refuse new items, wake waiters
func (*Ring[T]) Drain() int                        // give remaining items to OnDrop
func (*Ring[T]) Len() int; Cap() int; Stats() Stats
var ErrClosed
```

## 3. Design

**Ownership through the queue.** Push accepting an item transfers it to the
queue. `Push` returning false (Reject, or closed) leaves it with the caller,
who must still release it. Under DropOldest the evicted item goes to
`OnDrop`. After `Close`, consumers may still pop what is queued; `Drain`
hands anything left to `OnDrop`. For `*buf.Buf`, `OnDrop` is
`(*buf.Buf).Release`, so every discard path frees memory.

**Structure.** A ring of `Cap` slots (power of two, index by mask) under one
mutex. Popped slots are zeroed so the queue does not keep released memory
reachable. `OnDrop` runs outside the lock.

**Waiting.** Two capacity-1 signal channels (not-empty, not-full) and a
`done` channel closed by `Close`. A waiter that wakes and makes progress
re-signals, so one signal can wake a chain of waiters without losing
wake-ups. Signals are never sent on a closed channel; `Close` closes only
`done`.

**PushWait** blocks for room regardless of Policy and must never be used on
a network read loop: a slow consumer would stall all traffic.

## 4. Concurrency

Safe for any number of producers and consumers. Bookkeeping and the item
array share one lock; for the per-connection send queues it is designed for,
contention is low.

## 5. Observability

`Stats()`: Len, Pushed, Popped, Dropped (evicted or drained), Rejected
(refused or pushed after close).

## 6. Performance (2 vCPU)

Push + Pop about 39ns, 0 allocations.

## 7. Testing

QUEUE-01…QUEUE-17, NF-06 (stress), NF-10 (fuzz) in
[docs/TESTING.md](../TESTING.md).

## 8. Open items

None. A lock-free SPSC variant is not planned until a benchmark shows the
mutex matters.

## Cross-repository target-design alignment (2026-11-10)

The [canonical target design](https://github.com/ninepeach/architecture-docs/blob/main/projects/go-netx/design.md) and [phased implementation plan](https://github.com/ninepeach/architecture-docs/blob/main/projects/go-netx/implementation-steps.zh.md) describe additional planned abstractions and acceptance criteria. This document specifies the **current module contract**. The bounded generic Ring is not the canonical plan's per-Flow CloseableBoundedWriterQueue; writer waiter caps, WriteItem ownership and drain semantics require separate decisions. Do not treat target APIs as implemented or change this module's boundaries without an explicit cross-repository design decision.

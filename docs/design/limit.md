# Package limit — design

Overall design: [ARCHITECTURE.md](../../ARCHITECTURE.md).

## 1. Scope

| Owns | Does not own |
|---|---|
| Admission: may new work enter now? | Queuing work that may not enter (see `queue`) |
| Totals: how much of a resource may be held at once? | Distributed or cross-node limits |

`limit` imports nothing. Every call answers immediately; nothing waits.

## 2. API

```go
type Limiter interface{ TryAcquire() bool; Release() }
func NewConcurrency(max int) *Concurrency // at most max holders
func NewRate(perSec float64, burst int) *Rate // token bucket; Release is a no-op
func All(...Limiter) Limiter              // all must admit; partial success undone
func NewPerKey[K comparable](max int) *PerKey[K] // TryAcquire(k), Release(k), Keys(), Max()
func NewBudget(max int64) *Budget         // Reserve(n), Release(n), Used(), Max()
// Concurrency also has Max(); Keys() is the number of keys currently held
```

## 3. Design

**Never above the maximum, even for an instant.** `Concurrency.TryAcquire`
and `Budget.Reserve` use CAS loops: they check before they add. An
optimistic "add, then subtract on failure" lets concurrent failures push
`InUse`/`Used` past the maximum and, near `MaxInt64`, overflow and admit
(REG-12). `Reserve` checks `n > max - used`, which cannot overflow.

**Misuse panics.** Negative reservations are refused. Releasing more than
was acquired panics for `Concurrency`, `PerKey` and `Budget` (REG-13):
silently going negative would raise the limit. Constructors panic on a
non-positive maximum.

**PerKey forgets.** Counts live in a map; a key is deleted when its count
reaches zero, so memory follows the keys currently held, not every key ever
seen (one entry per active client address for `tcp.MaxConnsPerIP`). A mutex
guards the map: it is used on the accept path, not per packet.

**Rate** is a mutex-guarded token bucket on the monotonic clock. `Release`
does nothing: tokens refill with time.

**All** acquires in order and, on the first refusal, releases what it has
already acquired, so it never leaks a permit.

## 4. Use in go-netx

- `tcp.Options.Admit` takes any `Limiter` (total connections, rate of new
  connections, or `All` of both).
- `tcp.Options.MaxConnsPerIP` uses a `PerKey[netip.Addr]`.
- A gateway can bound the bytes it holds with a `Budget` reserved around
  `buf.Get`; buf itself does not limit (D-24).

## 5. Decisions

D-04 (`TryAcquire` never blocks: admission answers "now or not"; waiting is
`queue`'s job), D-10 (per-IP cap is local, not distributed).

## 6. Testing

RES-01…RES-18 in [docs/TESTING.md](../TESTING.md).

## 7. Open items

None.

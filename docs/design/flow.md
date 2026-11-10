# Package flow — design

Overall design: [ARCHITECTURE.md](../../ARCHITECTURE.md).

## 1. Scope

| Owns | Does not own |
|---|---|
| Transport identity: a flow ID that doubles as an epoch | I/O (transports move bytes) |
| The close state machine (Open → Closing → Closed) | Sessions: users, login, reconnects |
| A sharded table keyed by anything comparable, with idle expiry | Authentication |

A flow is one TCP connection or one UDP source address; it ends when the
transport ends. A session is a logged-in user that survives reconnects and
belongs to the gateway. `flow` imports nothing from go-netx, and transports
do not import `flow`: a gateway looks up `udp.Packet.From` or
`tcp.Conn.ID()` in a `Table`.

## 2. API

```go
func NextID() uint64                    // process-wide, strictly increasing, never 0
type Flow[V any] struct{ Value V /* … */ }
func New[V any](value V) *Flow[V]
func (*Flow[V]) ID() uint64
func (*Flow[V]) State() State           // Open, Closing, Closed
func (*Flow[V]) BeginClose() bool       // Open → Closing, true once
func (*Flow[V]) Close() bool            // → Closed, true exactly once
func (*Flow[V]) Touch()                 // record activity; no effect unless Open and in its table
func (*Flow[V]) LastSeen() int64        // last Touch, or creation time

func NewTable[K comparable, V any](TableOptions[K, V]) *Table[K, V]
type TableOptions[K comparable, V any] struct {
    Shards      int           // power of two, default 64
    IdleTimeout time.Duration // 0 = no expiry
    OnClose     func(K, *Flow[V]) // exactly once per flow leaving the table
    MaxFlows    int               // 0 = no limit; full → GetOrOpen returns nil, false
}
func (*Table) Get(K) (*Flow[V], bool)          // pure lookup, LastSeen unchanged
func (*Table) GetAndTouch(K) (*Flow[V], bool)  // Get, then Touch: the per-packet path
func (*Table) GetOrOpen(K, newValue func() V) (*Flow[V], bool) // new flow: LastSeen = table clock; nil after Close or when full
func (*Table) Remove(K, id uint64) bool // only if the ID still matches
func (*Table) Sweep(now time.Time) int
func (*Table) Run(context.Context) error // clock + sweeps; Close on exit
func (*Table) Close()                   // permanent: no flows afterwards
func (*Table) Closed() bool
func (*Table) Refused() uint64          // GetOrOpen calls refused by MaxFlows
func (*Table) Len() int
func (*Table) Range(func(K, *Flow[V]) bool)
```

## 3. Design

**ID as epoch.** IDs are global and strictly increasing, so a stale callback
that remembers `(key, id)` can never remove a newer flow that reused the
key: `Remove(key, id)` checks the ID. No separate epoch field is needed.

**Exactly-once close.** `Close` uses CAS; exactly one caller gets `true`, so
releasing resources on `true` is safe. The table calls `OnClose` after the
flow is Closed and outside shard locks, for every path out of the table:
`Remove`, idle expiry, `Close`. Exactly-once comes from the table, not from
`Close`'s return value (REG-28): a flow is deleted from its shard under the
lock once, and the path that deleted it calls `OnClose`, even if the caller
had already closed the flow. A gateway therefore releases a flow's
resources in one place — `OnClose`, or the `true` of `Flow.Close` — not
both.

**Callbacks and locks.** `GetOrOpen`'s `newValue` and `Range`'s `fn` run
under a shard lock, released by `defer`, so a panic in them propagates
without leaving the shard locked (REG-29); a panicking `newValue` inserts
nothing. `defer` costs nothing measurable here (`GetOrOpen` on an existing
key 53–60ns before and after, 0 allocations, 2 vCPU). `OnClose` runs
outside locks; a panic in it propagates, and the flows `Sweep` or `Close`
had not yet reported are no longer in the table and are not reported.

**Sharding.** Keys hash with `maphash.Comparable` to one of a power-of-two
number of shards, each a mutex and a map, padded to its own cache line.
A lookup locks one shard.

**Lookup and activity are separate.** `Get` is a pure lookup and
`GetOrOpen` changes `LastSeen` only by creating a flow (initialised to the
table clock). `Touch` is the only operation that records activity:
`LastSeen` is the last activity the system explicitly recorded, and
`IdleTimeout` expires flows by it. The transport does not touch flows —
it does not import `flow` — so the gateway calls `Touch` (or
`GetAndTouch`) when the transport delivers real data. Background queries,
`Range`, metrics and management code must not touch, or they would keep
idle flows alive. There is no automatic heartbeat.

**Touch cannot revive a leaving flow.** `LastSeen` and a frozen flag share
one atomic word (the flag is the lowest bit; the clock is coarse anyway).
`BeginClose`, `Close` and `Remove` set the flag, and `Sweep` evicts an idle
flow by setting it with a CAS on the value it judged idle. `Touch` is a CAS
loop that gives up when the flag is set. So a Touch that lands before
Sweep decides makes Sweep's CAS fail and the flow stays; a Touch after the
decision, or after Close or Remove, has no effect and `LastSeen` keeps its
old value. A Closing or Closed flow still in the table is evicted by its
old `LastSeen`. `Touch` never moves `LastSeen` backwards, and when the
coarse clock has not moved since the last Touch — the common case per
packet — it is a single atomic load.

**Coarse clock.** Touches record the table's clock, not `time.Now()`, which
would cost a call per packet (a flow made by `New` outside a table uses
`time.Now()`). `Run` advances the clock every quarter of
`IdleTimeout` (clamped to 10ms…1s) and sweeps. `Sweep(now)` also advances
the clock, so a table swept by hand without `Run` does not evict touched
flows (REG-11); the clock never moves backwards.

**Lookup races.** A returned flow can still be closed later by `Remove`,
`Sweep` or `Close`; callers that care check `State`. Before v1.1 the
touch happened inside `Get` under the shard lock so that `Sweep` could not
evict a flow between lookup and touch (REG-15); the CAS above gives the
same guarantee for a separate `Touch`: if Sweep wins, the Touch is a no-op
and the flow is visibly not Open.

**Close is permanent (D-22).** `Close` marks every shard closed under its
lock while emptying it, so a `GetOrOpen` racing it either lands before the
shard is emptied (and is closed with it) or sees the mark and creates
nothing. That makes "after Run returns the table is empty and every flow
got its OnClose" hold even while UDP readers are still running (REG-33).

**MaxFlows (D-23).** An optional cap against spoofed-source floods. The
count is one atomic, touched only when a flow is created or leaves; a full
table refuses new keys (`Refused` counts them) and keeps serving existing
flows. There is no eviction of old flows to make room: that would let a
flood push out real sessions.

## 4. Performance (2 vCPU)

- `Get` on `netip.AddrPort` (a pure lookup): about 48ns at 1k flows, 80ns
  at 100k, 112ns at 1M, 0 allocations; `GetAndTouch` on one key 51ns and
  `GetOrOpen` on an existing key 50ns (medians of 5, 2 vCPU, measured with
  the Touch change; `Get` used to store `LastSeen` on every call and was
  52/69/112/210ns in the same run of the old code).
- `Sweep` with nothing to evict (`BenchmarkSweepBySize`): about 10ns per
  flow at 10k (0.1ms per sweep) and 13.5ns at 100k (1.4ms), stable across
  runs. At 1M the table no longer fits in the CPU caches and the cost
  depends on the machine: 19–50ms per sweep across runs on one 2 vCPU VM
  (Intel Xeon 2.1GHz, Linux 6.18, Go 1.24, 2026-11-06; six separate runs
  37–46ms), 105ms on another 2 vCPU VM (2026-11-05). At the 1s maximum
  tick a million flows cost about 2–10% of a core; plan with the upper
  end.

## 5. Decisions

D-06 (pending): sweep stays a full scan until a gateway approaches a
million flows, then becomes incremental (a few shards per tick).

D-20: `Get` is a pure lookup; `Touch` is the only operation that records
activity.

D-22: `Close` is permanent. D-23: optional `MaxFlows`, refusing new keys.

## 6. Testing

FLOW-01…FLOW-33, NF-05 (stress), NF-09 (fuzz), NF-14 in
[docs/TESTING.md](../TESTING.md).

## 7. Open items

- Incremental sweep (D-06).

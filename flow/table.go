package flow

import (
	"context"
	"hash/maphash"
	"sync"
	"sync/atomic"
	"time"
)

// TableOptions configures a Table.
type TableOptions[K comparable, V any] struct {
	// Shards is rounded up to a power of two. Default 64.
	Shards int
	// IdleTimeout evicts flows not touched for this long. Zero disables
	// idle eviction. Eviction only runs while Run is active.
	IdleTimeout time.Duration
	// OnClose is called exactly once for every flow that leaves the table,
	// whether by Remove, idle eviction or Close, after it is Closed. That
	// holds even if the caller closed the flow itself before it left: the
	// table calls OnClose because the flow left, not because Table closed
	// it. So release a flow's resources in one place, OnClose or the true
	// return of Flow.Close, not both. It runs outside table locks.
	OnClose func(K, *Flow[V])
	// MaxFlows caps the number of flows. When the table is full, GetOrOpen
	// for a new key creates nothing and returns (nil, false); existing flows
	// are still found. It bounds memory against a flood of spoofed source
	// addresses, each of which would otherwise get a flow until it idles
	// out. Zero means no limit; negative values panic in NewTable.
	MaxFlows int
}

// Table maps keys (for example netip.AddrPort) to flows. It is safe for
// concurrent use; lookups lock one shard only.
type Table[K comparable, V any] struct {
	shards  []shard[K, V]
	mask    uint64
	seed    maphash.Seed
	idle    int64
	onClose func(K, *Flow[V])
	now     atomic.Int64 // coarse clock, advanced by Run
	max     int64
	count   atomic.Int64  // flows in the table, kept only when max > 0
	refused atomic.Uint64 // GetOrOpen calls refused by MaxFlows
	shut    atomic.Bool
}

type shard[K comparable, V any] struct {
	mu     sync.Mutex
	m      map[K]*Flow[V]
	closed bool     // set under mu by Table.Close; no inserts afterwards
	_      [40]byte // keep neighbouring shard locks off one cache line
}

// NewTable returns an empty table. It panics if MaxFlows is negative.
func NewTable[K comparable, V any](o TableOptions[K, V]) *Table[K, V] {
	if o.MaxFlows < 0 {
		panic("flow: negative MaxFlows")
	}
	n := o.Shards
	if n <= 0 {
		n = 64
	}
	size := 1
	for size < n {
		size <<= 1
	}
	t := &Table[K, V]{
		shards: make([]shard[K, V], size), mask: uint64(size - 1),
		seed: maphash.MakeSeed(), idle: int64(o.IdleTimeout), onClose: o.OnClose,
		max: int64(o.MaxFlows),
	}
	for i := range t.shards {
		t.shards[i].m = make(map[K]*Flow[V])
	}
	t.now.Store(time.Now().UnixNano())
	return t
}

func (t *Table[K, V]) shard(k K) *shard[K, V] {
	return &t.shards[maphash.Comparable(t.seed, k)&t.mask]
}

// Get returns the flow for k. It is a pure lookup: it does not change
// LastSeen, so management and background code can inspect flows without
// keeping them alive. Call Flow.Touch (or use GetAndTouch) when the lookup
// is for real traffic. A returned flow can still be closed later by Remove,
// Sweep or Close; check State before relying on it.
func (t *Table[K, V]) Get(k K) (*Flow[V], bool) {
	s := t.shard(k)
	s.mu.Lock()
	f, ok := s.m[k]
	s.mu.Unlock()
	return f, ok
}

// GetAndTouch is Get followed by Flow.Touch, for the per-packet path. If
// Sweep evicts the flow between the two, the Touch has no effect and the
// returned flow is no longer Open.
func (t *Table[K, V]) GetAndTouch(k K) (*Flow[V], bool) {
	f, ok := t.Get(k)
	if ok {
		f.Touch()
	}
	return f, ok
}

// GetOrOpen returns the flow for k, creating it with newValue if absent. A
// new flow's LastSeen starts at the table clock; finding an existing flow
// does not change its LastSeen. newValue runs under the shard lock and must
// be fast. If it panics, the panic propagates, no flow is inserted and the
// shard stays usable.
//
// It returns (nil, false) without calling newValue when no flow can be
// created: after Close, or when MaxFlows flows already exist (counted by
// Refused). Callers drop the packet in that case.
func (t *Table[K, V]) GetOrOpen(k K, newValue func() V) (f *Flow[V], created bool) {
	s := t.shard(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.m[k]; ok {
		return f, false
	}
	if s.closed {
		return nil, false
	}
	if t.max > 0 {
		if t.count.Add(1) > t.max {
			t.count.Add(-1)
			t.refused.Add(1)
			return nil, false
		}
		defer func() {
			if f == nil { // newValue panicked: give the slot back
				t.count.Add(-1)
			}
		}()
	}
	var v V
	if newValue != nil {
		v = newValue()
	}
	f = newInTable(v, &t.now)
	s.m[k] = f
	return f, true
}

// Refused returns how many GetOrOpen calls MaxFlows refused.
func (t *Table[K, V]) Refused() uint64 { return t.refused.Load() }

// Closed reports whether Close has started. It does not wait for all shards
// to be emptied or for OnClose callbacks to finish. Once true, Close will
// eventually remove every flow, and no new flow survives the close.
func (t *Table[K, V]) Closed() bool { return t.shut.Load() }

// left records that n flows left the table.
func (t *Table[K, V]) left(n int) {
	if t.max > 0 && n > 0 {
		t.count.Add(int64(-n))
	}
}

// Remove closes and removes the flow for k only if its ID is id, so a stale
// caller cannot remove a newer flow that reused the key. It reports whether
// a flow was removed.
func (t *Table[K, V]) Remove(k K, id uint64) bool {
	s := t.shard(k)
	s.mu.Lock()
	f, ok := s.m[k]
	if !ok || f.id != id {
		s.mu.Unlock()
		return false
	}
	f.freeze() // no Touch from here on
	delete(s.m, k)
	t.left(1) // release capacity while holding the shard lock
	s.mu.Unlock()
	t.closed(k, f)
	return true
}

// closed finishes a flow that the caller has just deleted from its shard.
// Deleting under the lock happens once per flow, so the caller is the only
// one to get here for f and OnClose runs exactly once, whether or not the
// flow was already Closed by someone else (REG-28).
func (t *Table[K, V]) closed(k K, f *Flow[V]) {
	f.Close()
	if t.onClose != nil {
		t.onClose(k, f)
	}
}

// Len returns the number of flows.
func (t *Table[K, V]) Len() int {
	n := 0
	for i := range t.shards {
		s := &t.shards[i]
		s.mu.Lock()
		n += len(s.m)
		s.mu.Unlock()
	}
	return n
}

// Range calls fn for every flow until fn returns false. fn runs under a
// shard lock and must not call back into the table. If fn panics, the panic
// propagates and the shard stays usable.
func (t *Table[K, V]) Range(fn func(K, *Flow[V]) bool) {
	for i := range t.shards {
		if !t.shards[i].rangeLocked(fn) {
			return
		}
	}
}

// rangeLocked runs fn over one shard; the deferred unlock keeps the shard
// usable if fn panics.
func (s *shard[K, V]) rangeLocked(fn func(K, *Flow[V]) bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, f := range s.m {
		if !fn(k, f) {
			return false
		}
	}
	return true
}

// Sweep evicts flows whose LastSeen is older than IdleTimeout as of now and
// returns how many it evicted. A flow touched before Sweep decides is kept;
// a Touch after the decision has no effect. It also advances the table clock to now, so touches made
// after a manual Sweep are recorded at the new time; without that, a table
// swept by hand instead of by Run would see every flow as idle. Run calls
// Sweep periodically.
//
// Sweep visits every flow, holding one shard lock at a time. Cost grows
// with table size, and faster than linearly once the table outgrows the
// CPU caches: about 0.1ms for 10k flows and 1.4ms for 100k; at 1M it
// depends heavily on the machine's memory, 19-50ms on one 2 vCPU VM and
// 105ms on another. With Run's 1s maximum tick, a million flows cost about
// 2-10% of one core. See BenchmarkSweepBySize.
func (t *Table[K, V]) Sweep(now time.Time) int {
	t.advance(now.UnixNano())
	if t.idle <= 0 {
		return 0
	}
	cutoff := now.UnixNano() - t.idle
	type victim struct {
		k K
		f *Flow[V]
	}
	var victims []victim
	for i := range t.shards {
		s := &t.shards[i]
		s.mu.Lock()
		for k, f := range s.m {
			if f.expire(cutoff) {
				delete(s.m, k)
				t.left(1) // keep capacity in sync with map removal
				victims = append(victims, victim{k, f})
			}
		}
		s.mu.Unlock()
	}
	for _, v := range victims {
		t.closed(v.k, v.f)
	}
	return len(victims)
}

// Run advances the table's coarse clock and evicts idle flows until ctx is
// done, then closes the table (see Close). The clock resolution is a quarter
// of IdleTimeout, clamped to [10ms, 1s].
func (t *Table[K, V]) Run(ctx context.Context) error {
	tick := time.Second
	if t.idle > 0 {
		tick = time.Duration(t.idle / 4)
		tick = min(max(tick, 10*time.Millisecond), time.Second)
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Close()
			return nil
		case now := <-ticker.C:
			t.Sweep(now)
		}
	}
}

// advance moves the coarse clock forward, never back.
func (t *Table[K, V]) advance(ns int64) {
	for {
		cur := t.now.Load()
		if ns <= cur || t.now.CompareAndSwap(cur, ns) {
			return
		}
	}
}

// Close removes and closes every flow and closes the table: afterwards
// GetOrOpen creates nothing, so a reader still running after Close (or
// after Run returned) cannot leave flows behind that nothing will sweep
// or close. Get, Remove, Range and Len keep working on the empty table.
// Close is idempotent; OnClose runs once for each flow it removes.
// Concurrent Close calls and Remove/Sweep callbacks can still be running
// after this call returns. Callers requiring callback completion must first
// stop concurrent table operations and wait for them to finish.
func (t *Table[K, V]) Close() {
	t.shut.Store(true)
	for i := range t.shards {
		s := &t.shards[i]
		s.mu.Lock()
		m := s.m
		s.closed = true
		s.m = make(map[K]*Flow[V])
		t.left(len(m)) // capacity is released before the shard unlocks
		s.mu.Unlock()
		for k, f := range m {
			t.closed(k, f)
		}
	}
}

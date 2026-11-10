package flow

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Checks: FLOW-30
func TestCloseIsPermanent(t *testing.T) {
	var closes atomic.Int32
	tb := NewTable[int, int](TableOptions[int, int]{OnClose: func(int, *Flow[int]) { closes.Add(1) }})
	tb.GetOrOpen(1, nil)
	tb.GetOrOpen(2, nil)
	if tb.Closed() {
		t.Fatal("Closed before Close")
	}
	tb.Close()
	if !tb.Closed() || tb.Len() != 0 || closes.Load() != 2 {
		t.Fatalf("after Close: closed=%v len=%d OnClose=%d", tb.Closed(), tb.Len(), closes.Load())
	}
	called := false
	f, created := tb.GetOrOpen(3, func() int { called = true; return 3 })
	if f != nil || created || called {
		t.Fatalf("GetOrOpen after Close: f=%v created=%v newValue called=%v", f, created, called)
	}
	if _, ok := tb.Get(3); ok || tb.Len() != 0 {
		t.Fatal("a flow was inserted after Close")
	}
	tb.Close() // idempotent
	if closes.Load() != 2 {
		t.Fatalf("second Close ran OnClose: %d", closes.Load())
	}
	if tb.Remove(1, 0) {
		t.Fatal("Remove found a flow in a closed table")
	}
}

// Writers still running while Run returns used to leave flows behind that
// nothing would ever sweep or close (REG-33).
//
// Checks: FLOW-31
func TestNoFlowsSurviveRunExit(t *testing.T) {
	for round := 0; round < 50; round++ {
		var opened, closed atomic.Int64
		tb := NewTable[int, int](TableOptions[int, int]{
			IdleTimeout: time.Hour,
			OnClose:     func(int, *Flow[int]) { closed.Add(1) },
		})
		ctx, cancel := context.WithCancel(context.Background())
		ran := make(chan struct{})
		go func() { tb.Run(ctx); close(ran) }()
		var wg sync.WaitGroup
		stop := make(chan struct{})
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					if _, created := tb.GetOrOpen(w<<24|i, nil); created {
						opened.Add(1)
					}
				}
			}(w)
		}
		time.Sleep(time.Millisecond)
		cancel()
		<-ran
		time.Sleep(time.Millisecond) // writers keep going after Run returned
		close(stop)
		wg.Wait()
		if n := tb.Len(); n != 0 {
			t.Fatalf("round %d: %d flows left after Run returned", round, n)
		}
		if opened.Load() != closed.Load() {
			t.Fatalf("round %d: opened %d, OnClose %d", round, opened.Load(), closed.Load())
		}
	}
}

// Checks: FLOW-32
func TestMaxFlows(t *testing.T) {
	tb := NewTable[int, int](TableOptions[int, int]{MaxFlows: 3, IdleTimeout: time.Minute})
	ids := map[int]uint64{}
	for k := 0; k < 3; k++ {
		f, created := tb.GetOrOpen(k, nil)
		if !created {
			t.Fatal("not created under the cap")
		}
		ids[k] = f.ID()
	}
	called := false
	if f, created := tb.GetOrOpen(9, func() int { called = true; return 0 }); f != nil || created || called {
		t.Fatalf("full table created a flow: %v %v %v", f, created, called)
	}
	if f, created := tb.GetOrOpen(1, nil); f == nil || created {
		t.Fatal("existing flow not found while full")
	}
	if tb.Refused() != 1 || tb.Len() != 3 {
		t.Fatalf("refused=%d len=%d", tb.Refused(), tb.Len())
	}
	// Every way out of the table frees a slot.
	tb.Remove(0, ids[0])
	if _, created := tb.GetOrOpen(10, nil); !created {
		t.Fatal("slot not freed by Remove")
	}
	tb.Sweep(time.Now().Add(time.Hour)) // evicts all three
	for k := 20; k < 23; k++ {
		if _, created := tb.GetOrOpen(k, nil); !created {
			t.Fatal("slots not freed by Sweep")
		}
	}
	// A panicking newValue gives its slot back.
	tb.Remove(20, mustGet(t, tb, 20).ID())
	func() {
		defer func() { recover() }()
		tb.GetOrOpen(30, func() int { panic("boom") })
	}()
	if _, created := tb.GetOrOpen(31, nil); !created {
		t.Fatal("slot leaked by a panicking newValue")
	}
}

func mustGet(t *testing.T, tb *Table[int, int], k int) *Flow[int] {
	t.Helper()
	f, ok := tb.Get(k)
	if !ok {
		t.Fatalf("no flow %d", k)
	}
	return f
}

// Checks: FLOW-33
func TestMaxFlowsConcurrent(t *testing.T) {
	const max = 100
	tb := NewTable[int, int](TableOptions[int, int]{MaxFlows: max})
	var created atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				if _, ok := tb.GetOrOpen(w*1000+i, nil); ok {
					created.Add(1)
				}
			}
		}(w)
	}
	wg.Wait()
	if created.Load() != max || tb.Len() != max || tb.Refused() != 8000-max {
		t.Fatalf("created=%d len=%d refused=%d", created.Load(), tb.Len(), tb.Refused())
	}
	tb.Close()
	if tb.Len() != 0 {
		t.Fatal("Close left flows")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("negative MaxFlows did not panic")
		}
	}()
	NewTable[int, int](TableOptions[int, int]{MaxFlows: -1})
}

// TestCloseCanReturnWhileConcurrentOnCloseRuns documents the deliberately
// non-joining Close contract. Stop concurrent operations before Close if
// callback completion is required.
func TestCloseCanReturnWhileConcurrentOnCloseRuns(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	removed := make(chan struct{})
	tb := NewTable[int, int](TableOptions[int, int]{
		OnClose: func(int, *Flow[int]) {
			close(entered)
			<-release
		},
	})
	f, created := tb.GetOrOpen(1, nil)
	if !created {
		t.Fatal("flow not created")
	}
	go func() {
		defer close(removed)
		tb.Remove(1, f.ID())
	}()
	<-entered // flow removed; callback still pending
	tb.Close()
	if !tb.Closed() || tb.Len() != 0 {
		t.Fatal("Close failed to close table")
	}
	close(release)
	<-removed
}

// TestCloseBlocksNewFlowsAfterReturning ensures a concurrent writer cannot
// recreate a flow after Close has returned.
func TestCloseBlocksNewFlowsAfterReturning(t *testing.T) {
	tb := NewTable[int, int](TableOptions[int, int]{MaxFlows: 1})
	tb.GetOrOpen(1, nil)
	tb.Close()
	for i := 0; i < 100; i++ {
		f, created := tb.GetOrOpen(i, nil)
		if f != nil || created {
			t.Fatalf("flow %d created after Close", i)
		}
	}
	if tb.Len() != 0 {
		t.Fatal("flows survived Close")
	}
}

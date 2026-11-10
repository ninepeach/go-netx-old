package buf

import (
	"runtime"
	"strconv"
	"sync"
	"testing"

	"github.com/ninepeach/go-netx/limit"
)

// Checks: BUF-01, BUF-02
func TestGetClasses(t *testing.T) {
	p := NewPool(nil)
	for _, tc := range []struct{ n, cap int }{
		{0, MinSize}, {1, MinSize}, {64, 64}, {65, 128}, {1500, Small}, {Small + 1, 4 << 10},
		{Medium + 1, 32 << 10}, {Large, Large}, {Large + 1, Large + 1},
	} {
		b := p.Get(tc.n)
		if len(b.B) != tc.n || cap(b.full) != tc.cap {
			t.Fatalf("Get(%d): len=%d cap=%d want cap %d", tc.n, len(b.B), cap(b.full), tc.cap)
		}
		b.Release()
	}
}

// Checks: BUF-04
func TestBudgetChargesCapacity(t *testing.T) {
	budget := limit.NewBudget(Small * 2)
	p := NewPool(budget)
	a, b := p.Get(Small), p.Get(Small-100) // both cost a 2KB class
	if a == nil || b == nil {
		t.Fatal("expected two small buffers")
	}
	if p.Get(1) != nil {
		t.Fatal("budget exceeded")
	}
	a.Release()
	b.Release()
	if budget.Used() != 0 {
		t.Fatalf("used=%d", budget.Used())
	}
}

// Checks: BUF-03
func TestCopy(t *testing.T) {
	p := NewPool(nil)
	src := []byte("hello")
	b := p.Copy(src)
	src[0] = 'x'
	if string(b.B) != "hello" {
		t.Fatal("copy aliases source")
	}
	b.Release()
}

// Checks: BUF-05
func TestDoubleReleasePanicsInDebug(t *testing.T) {
	if !Debug {
		t.Skip("needs -tags netxdebug")
	}
	p := NewPool(nil)
	b := p.Get(1)
	b.Release()
	defer func() {
		if recover() == nil {
			t.Fatal("double Release did not panic")
		}
	}()
	b.Release()
}

// Checks: BUF-12, NF-16
func BenchmarkGetRelease(b *testing.B) {
	p := NewPool(nil)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			p.Get(1200).Release()
		}
	})
}

// Checks: BUF-04, BUF-13
func TestReleaseClearsViewAndRefundsUnpooledCapacity(t *testing.T) {
	const size = Large + 1
	budget := limit.NewBudget(size)
	p := NewPool(budget)
	b := p.Get(size)
	if b == nil {
		t.Fatal("expected unpooled buffer")
	}
	if budget.Used() != size {
		t.Fatalf("used=%d want=%d", budget.Used(), size)
	}
	b.Release()
	if b.B != nil {
		t.Fatal("Release did not clear the public buffer view")
	}
	if budget.Used() != 0 {
		t.Fatalf("budget not refunded: used=%d", budget.Used())
	}
}

// Checks: BUF-04, BUF-14
func TestBudgetRejectsLargeBufferWithoutPartialCharge(t *testing.T) {
	budget := limit.NewBudget(Large)
	p := NewPool(budget)
	if b := p.Get(Large + 1); b != nil {
		b.Release()
		t.Fatal("buffer larger than remaining budget was allocated")
	}
	if budget.Used() != 0 {
		t.Fatalf("failed Get charged budget: used=%d", budget.Used())
	}
}

// Checks: BUF-09
func TestGetNegativeSizePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Get(-1) did not panic")
		}
	}()
	NewPool(nil).Get(-1)
}

// Checks: BUF-08
func TestConcurrentGetNeverShares(t *testing.T) {
	p := NewPool(nil)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				b := p.Get(1200)
				mark := byte(g*31 + i)
				for j := range b.B {
					b.B[j] = mark
				}
				runtime.Gosched() // let others Get while we hold b
				for j := range b.B {
					if b.B[j] != mark {
						t.Errorf("buffer shared with another owner")
						return
					}
				}
				b.Release()
			}
		}(g)
	}
	wg.Wait()
}

// Checks: BUF-11
func TestPoolCorrectAcrossGC(t *testing.T) {
	budget := limit.NewBudget(1 << 20)
	p := NewPool(budget)
	for round := 0; round < 5; round++ {
		held := make([]*Buf, 0, 16)
		for i := 0; i < 16; i++ {
			b := p.Get(1000 + i)
			if b == nil || len(b.B) != 1000+i {
				t.Fatalf("round %d: bad buffer", round)
			}
			held = append(held, b)
		}
		for _, b := range held {
			b.Release()
		}
		runtime.GC() // sync.Pool may drop everything; the pool must not care
		runtime.GC()
		if budget.Used() != 0 {
			t.Fatalf("round %d: used=%d", round, budget.Used())
		}
	}
}

// Checks: BUF-12, NF-16
func BenchmarkGetReleaseBySize(b *testing.B) {
	for _, n := range []int{64, 200, 1200, 3000, 9000, 40000, 70000, 140000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			p := NewPool(nil)
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					p.Get(n).Release()
				}
			})
		})
	}
}

// Every class is a power of two from MinSize to Large; a buffer's capacity
// is under twice its length (from MinSize up) and is what the budget pays.
// Checks: BUF-15
func TestPowerOfTwoClasses(t *testing.T) {
	budget := limit.NewBudget(1 << 30)
	p := NewPool(budget)
	seen := map[int]bool{}
	for n := 0; n <= Large+3; n++ {
		c := Capacity(n)
		switch {
		case n > Large:
			if c != n {
				t.Fatalf("Capacity(%d) = %d, want n (unpooled)", n, c)
			}
			continue
		case c < MinSize || c&(c-1) != 0 || c < n:
			t.Fatalf("Capacity(%d) = %d: not a power of two >= max(n, MinSize)", n, c)
		case n > MinSize && c >= 2*n:
			t.Fatalf("Capacity(%d) = %d: at least twice the request", n, c)
		}
		seen[c] = true
		if n%97 == 0 || c == n { // spot-check Get against Capacity
			b := p.Get(n)
			if len(b.B) != n || cap(b.B) != c || budget.Used() != int64(c) {
				t.Fatalf("Get(%d): len %d cap %d budget %d, want cap %d", n, len(b.B), cap(b.B), budget.Used(), c)
			}
			b.Release()
		}
	}
	if len(seen) != 12 {
		t.Fatalf("%d classes, want 12 (64B…128KB)", len(seen))
	}
}

// Checks: BUF-16
func TestReleaseNilIsNoop(t *testing.T) {
	budget := limit.NewBudget(MinSize)
	p := NewPool(budget)
	held := p.Get(1)
	refused := p.Get(1) // over budget: nil
	if refused != nil {
		t.Fatal("expected the budget to refuse")
	}
	refused.Release() // must not panic
	var nilBuf *Buf
	nilBuf.Release()
	held.Release()
	if budget.Used() != 0 {
		t.Fatalf("used=%d", budget.Used())
	}
}

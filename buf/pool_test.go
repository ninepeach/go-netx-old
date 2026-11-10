package buf

import (
	"runtime"
	"strconv"
	"sync"
	"testing"
)

// Checks: BUF-03
func TestCopy(t *testing.T) {
	src := []byte("hello")
	b := NewPool().Copy(src)
	src[0] = 'x'
	if string(b.Bytes()) != "hello" || b.Cap() != 5 || !b.IsFull() {
		t.Fatalf("Copy: %q cap %d", b.Bytes(), b.Cap())
	}
	b.Release()
	c := Copy(nil)
	if c.Len() != 0 {
		t.Fatal("Copy(nil) not empty")
	}
	c.Release()
}

// Checks: BUF-13
func TestReleaseResetsAndDropsUnpooled(t *testing.T) {
	p := NewPool()
	big := p.Get(Large + 1)
	big.Write([]byte("x"))
	big.Release()
	if big.Len() != 0 || big.Cap() != 0 {
		t.Fatalf("released buffer still has len %d cap %d", big.Len(), big.Cap())
	}
	if big.data != nil { // a *Buf kept after Release must not pin its memory
		t.Fatalf("released unpooled buffer still references %d bytes", len(big.data))
	}
	small := p.Get(10)
	small.Write([]byte("abc"))
	small.Release()
	next := p.Get(10)
	defer next.Release()
	if next.Len() != 0 || next.Cap() != 10 || next.Start() != 0 {
		t.Fatalf("reused buffer: len %d cap %d start %d", next.Len(), next.Cap(), next.Start())
	}
}

// Checks: BUF-09
func TestGetNegativeSizePanics(t *testing.T) {
	p := NewPool()
	mustPanic(t, "Get(-1)", func() { p.Get(-1) })
	mustPanic(t, "GetHeadroom(-1, 1)", func() { p.GetHeadroom(-1, 1) })
	mustPanic(t, "GetHeadroom(1, -1)", func() { p.GetHeadroom(1, -1) })
	mustPanic(t, "GetHeadroom overflow", func() { p.GetHeadroom(int(^uint(0)>>1), 1) })
	mustPanic(t, "Capacity(-1)", func() { Capacity(-1) })
}

// Checks: BUF-08
func TestConcurrentGetNeverShares(t *testing.T) {
	p := NewPool()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				b := p.Get(1200)
				mark := byte(g*31 + i)
				for j := range b.Extend(1200) {
					b.Bytes()[j] = mark
				}
				runtime.Gosched() // let others Get while we hold b
				for _, v := range b.Bytes() {
					if v != mark {
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
	p := NewPool()
	for round := 0; round < 5; round++ {
		held := make([]*Buf, 0, 16)
		for i := 0; i < 16; i++ {
			b := p.Get(1000 + i)
			if b.Cap() != 1000+i || b.Len() != 0 {
				t.Fatalf("round %d: bad buffer", round)
			}
			held = append(held, b)
		}
		for _, b := range held {
			b.Release()
		}
		runtime.GC() // sync.Pool may drop everything; the pool must not care
		runtime.GC()
	}
}

// The package-level functions use Default.
//
// Checks: BUF-18
func TestDefaultPool(t *testing.T) {
	a := Get(100)
	if a.pool != Default || a.Cap() != 100 {
		t.Fatal("Get does not use Default")
	}
	h := GetHeadroom(8, 100)
	if h.pool != Default || h.Start() != 8 || h.Cap() != 108 {
		t.Fatal("GetHeadroom does not use Default")
	}
	c := Copy([]byte("ab"))
	if c.pool != Default || string(c.Bytes()) != "ab" {
		t.Fatal("Copy does not use Default")
	}
	a.Release()
	h.Release()
	c.Release()
}

// As and With wrap the caller's memory: no pool, and Release only forgets.
//
// Checks: BUF-22
func TestUnmanagedWrappers(t *testing.T) {
	before := Outstanding()
	data := []byte("shared")
	a := As(data)
	if string(a.Bytes()) != "shared" || a.Cap() != 6 || !a.IsFull() {
		t.Fatalf("As: %q cap %d", a.Bytes(), a.Cap())
	}
	a.Advance(2)
	a.Release()
	a.Release() // forgetting twice is fine
	if string(data) != "shared" {
		t.Fatal("Release changed the caller's memory")
	}

	space := make([]byte, 4)
	w := With(space)
	if w.Len() != 0 || w.Cap() != 4 {
		t.Fatalf("With: len %d cap %d", w.Len(), w.Cap())
	}
	w.WriteString("ab")
	if string(space[:2]) != "ab" {
		t.Fatal("With does not write into the caller's memory")
	}
	w.Release()
	if Outstanding() != before {
		t.Fatalf("unmanaged buffers counted as outstanding: %d", Outstanding()-before)
	}
}

// Checks: BUF-12, NF-16
func BenchmarkGetRelease(b *testing.B) {
	p := NewPool()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			p.Get(1200).Release()
		}
	})
}

// Checks: BUF-12, NF-16
func BenchmarkGetReleaseBySize(b *testing.B) {
	for _, n := range []int{64, 200, 1200, 3000, 9000, 40000, 70000, 140000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			p := NewPool()
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					p.Get(n).Release()
				}
			})
		})
	}
}

package buf

import (
	"bytes"
	"errors"
	"io"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// mustPanic runs fn and fails unless it panics.
func mustPanic(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s did not panic", what)
		}
	}()
	fn()
}

// Checks: BUF-01, BUF-02
func TestGetClasses(t *testing.T) {
	p := NewPool()
	for _, tc := range []struct{ n, mem int }{
		{0, MinSize}, {1, MinSize}, {64, 64}, {65, 128}, {1500, Small}, {Small + 1, 4 << 10},
		{Medium + 1, 32 << 10}, {Large, Large}, {Large + 1, Large + 1},
	} {
		b := p.Get(tc.n)
		if b.Len() != 0 || b.Cap() != tc.n || b.FreeLen() != tc.n || len(b.data) != tc.mem {
			t.Fatalf("Get(%d): len %d cap %d free %d mem %d, want empty, cap %d, mem %d",
				tc.n, b.Len(), b.Cap(), b.FreeLen(), len(b.data), tc.n, tc.mem)
		}
		if pooled := tc.n <= Large; (b.class >= 0) != pooled {
			t.Fatalf("Get(%d): pooled=%v", tc.n, b.class >= 0)
		}
		b.Release()
	}
}

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

// Checks: BUF-05
func TestDoubleReleasePanicsInDebug(t *testing.T) {
	if !Debug {
		t.Skip("needs -tags netxdebug")
	}
	b := NewPool().Get(1)
	b.Release()
	mustPanic(t, "double Release", b.Release)
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

// A protocol stack's write path: payload in the middle, two headers in
// front, a tag behind, then the whole frame is sent. Zero allocations.
//
// Checks: BUF-12, NF-16
func BenchmarkHeaderPath(b *testing.B) {
	p := NewPool()
	payload := make([]byte, 1200)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			x := p.GetHeadroom(32, 1200+16)
			x.Write(payload)
			x.Extend(16) // tag
			x.ExtendHeader(4)
			x.ExtendHeader(16)
			x.WriteTo(io.Discard)
			x.Release()
		}
	})
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

// Every class is a power of two from MinSize to Large; a buffer's memory
// is under twice its size (from MinSize up).
//
// Checks: BUF-15
func TestPowerOfTwoClasses(t *testing.T) {
	p := NewPool()
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
			if len(b.data) != c || b.Cap() != n {
				t.Fatalf("Get(%d): mem %d cap %d, want mem %d", n, len(b.data), b.Cap(), c)
			}
			b.Release()
		}
	}
	if len(seen) != 11 {
		t.Fatalf("%d classes, want 11 (64B…64KB, D-26)", len(seen))
	}
	// Headroom counts toward the class.
	b := p.GetHeadroom(100, 1000)
	if len(b.data) != Capacity(1100) || b.Cap() != 1100 || b.Start() != 100 {
		t.Fatalf("GetHeadroom(100, 1000): mem %d cap %d start %d", len(b.data), b.Cap(), b.Start())
	}
	b.Release()
}

// Checks: BUF-16
func TestReleaseNilIsNoop(t *testing.T) {
	var b *Buf
	b.Release()
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

// Headers go in front without moving the payload; Advance strips them on
// the way in; Truncate and Reserve handle a trailer.
//
// Checks: BUF-10, BUF-19
func TestHeadroomAndCursors(t *testing.T) {
	b := NewPool().GetHeadroom(8, 16)
	defer b.Release()
	b.WriteString("payload")
	payloadAt := &b.Bytes()[0]
	copy(b.ExtendHeader(2), "h2")
	copy(b.ExtendHeader(3), "h1-")
	if got := string(b.Bytes()); got != "h1-h2payload" {
		t.Fatalf("after headers: %q", got)
	}
	if &b.Bytes()[5] != payloadAt {
		t.Fatal("ExtendHeader moved the payload")
	}
	if b.Start() != 3 {
		t.Fatalf("headroom left %d, want 3", b.Start())
	}
	mustPanic(t, "ExtendHeader past the headroom", func() { b.ExtendHeader(4) })

	// Reserve 4 bytes for a tag; appends stop before it.
	b.Reserve(4)
	if b.FreeLen() != 16-7-4 {
		t.Fatalf("FreeLen after Reserve = %d", b.FreeLen())
	}
	if n, err := b.Write(make([]byte, 10)); n != 5 || !errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("Write past the reserve = %d, %v", n, err)
	}
	b.Truncate(b.Len() - 5)
	b.Unreserve(4)
	copy(b.Extend(4), "TAG!")
	if got := string(b.Bytes()); got != "h1-h2payloadTAG!" {
		t.Fatalf("with tag: %q", got)
	}
	mustPanic(t, "Unreserve past the end", func() { b.Unreserve(100) })

	// Receive path: strip the headers.
	b.Advance(5)
	if got := string(b.Bytes()); got != "payloadTAG!" {
		t.Fatalf("after Advance: %q", got)
	}
	b.Advance(100)
	if !b.IsEmpty() {
		t.Fatal("Advance past the end left data")
	}
	mustPanic(t, "negative Advance", func() { b.Advance(-1) })
	mustPanic(t, "Truncate past Len", func() { b.Truncate(1) })
	mustPanic(t, "Extend past Cap", func() { b.Extend(b.FreeLen() + 1) })

	// Reset restores the original headroom and room.
	b.Reserve(2)
	b.Reset()
	if b.Start() != 8 || b.Len() != 0 || b.FreeLen() != 16 {
		t.Fatalf("after Reset: start %d len %d free %d", b.Start(), b.Len(), b.FreeLen())
	}
}

// Checks: BUF-20
func TestWriteAndReadMethods(t *testing.T) {
	b := NewPool().Get(8)
	defer b.Release()
	if err := b.WriteByte('a'); err != nil {
		t.Fatal(err)
	}
	if n, err := b.WriteString("bcdefgh"); n != 7 || err != nil {
		t.Fatal(n, err)
	}
	if err := b.WriteByte('z'); !errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("WriteByte on a full buffer = %v", err)
	}
	if n, err := b.WriteString("z"); n != 0 || !errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("WriteString on a full buffer = %d, %v", n, err)
	}
	if n, err := b.ReadOnceFrom(strings.NewReader("z")); n != 0 || !errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("ReadOnceFrom on a full buffer = %d, %v", n, err)
	}
	var out bytes.Buffer
	if n, err := b.WriteTo(&out); n != 8 || err != nil || out.String() != "abcdefgh" || b.Len() != 8 {
		t.Fatalf("WriteTo = %d, %v, %q, len after %d", n, err, out.String(), b.Len())
	}

	r := NewPool().Get(10)
	defer r.Release()
	if n, err := r.ReadOnceFrom(strings.NewReader("hello")); n != 5 || err != nil || string(r.Bytes()) != "hello" {
		t.Fatalf("ReadOnceFrom = %d, %v, %q", n, err, r.Bytes())
	}
	if n, err := r.ReadFullFrom(strings.NewReader("12345"), 6); n != 0 || !errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("ReadFullFrom past Cap = %d, %v", n, err)
	}
	if n, err := r.ReadFullFrom(strings.NewReader("12"), 3); n != 2 || !errors.Is(err, io.ErrUnexpectedEOF) || string(r.Bytes()) != "hello12" {
		t.Fatalf("short ReadFullFrom = %d, %v, %q", n, err, r.Bytes())
	}
	if n, err := r.ReadFullFrom(strings.NewReader("xyz"), 3); n != 3 || err != nil || string(r.Bytes()) != "hello12xyz" {
		t.Fatalf("ReadFullFrom = %d, %v, %q", n, err, r.Bytes())
	}

	// FreeBytes + Extend: read into the free space directly.
	f := NewPool().Get(4)
	defer f.Release()
	n := copy(f.FreeBytes(), "wxyz!")
	f.Extend(n)
	if string(f.Bytes()) != "wxyz" {
		t.Fatalf("FreeBytes/Extend: %q", f.Bytes())
	}
}

// Checks: BUF-21
func TestReadPacketFrom(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	cl, err := net.Dial("udp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	cl.Write([]byte("datagram"))
	b := NewPool().GetHeadroom(4, 64)
	defer b.Release()
	n, from, err := b.ReadPacketFrom(pc)
	if err != nil || n != 8 || string(b.Bytes()) != "datagram" || from.String() != cl.LocalAddr().String() {
		t.Fatalf("ReadPacketFrom = %d, %v, %v, %q", n, from, err, b.Bytes())
	}
	b.Extend(b.FreeLen())
	if _, _, err := b.ReadPacketFrom(pc); !errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("ReadPacketFrom on a full buffer = %v", err)
	}
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

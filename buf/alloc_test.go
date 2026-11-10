package buf

import (
	"testing"
)

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

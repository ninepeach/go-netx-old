package buf

import (
	"bytes"
	"net"
	"testing"
)

// The batch helpers agree with doing the same per buffer: LenMulti and
// IsEmptyMulti count data only (not headroom), ToSliceMulti aliases each
// buffer in order, CopyMulti stops at len(dst), and ReleaseMulti releases
// every buffer once and clears the slice, skipping nil entries.
//
// Checks: BUF-23
func TestMulti(t *testing.T) {
	before := Outstanding()
	p := NewPool()
	a := p.GetHeadroom(8, 16)
	a.WriteString("hello ")
	b := p.Get(4) // empty
	c := p.Copy([]byte("world"))
	bufs := []*Buf{a, b, c}

	if n := LenMulti(bufs); n != 11 {
		t.Fatalf("LenMulti = %d, want 11", n)
	}
	if IsEmptyMulti(bufs) || !IsEmptyMulti([]*Buf{b}) || !IsEmptyMulti(nil) {
		t.Fatal("IsEmptyMulti wrong")
	}
	vec := ToSliceMulti(bufs)
	if len(vec) != 3 || !bytes.Equal(bytes.Join(vec, nil), []byte("hello world")) {
		t.Fatalf("ToSliceMulti = %q", vec)
	}
	vec[0][0] = 'H' // aliases the buffer
	if string(a.Bytes()) != "Hello " {
		t.Fatalf("ToSliceMulti copied: %q", a.Bytes())
	}
	var w bytes.Buffer
	nb := net.Buffers(ToSliceMulti(bufs))
	if _, err := nb.WriteTo(&w); err != nil || w.String() != "Hello world" {
		t.Fatalf("as net.Buffers: %q", w.String())
	}

	dst := make([]byte, 32)
	if n := CopyMulti(dst, bufs); n != 11 || string(dst[:n]) != "Hello world" {
		t.Fatalf("CopyMulti = %d %q", n, dst[:n])
	}
	short := make([]byte, 8)
	if n := CopyMulti(short, bufs); n != 8 || string(short) != "Hello wo" {
		t.Fatalf("CopyMulti short = %d %q", n, short)
	}
	if n := CopyMulti(nil, bufs); n != 0 {
		t.Fatalf("CopyMulti(nil) = %d", n)
	}

	withNil := []*Buf{a, nil, b, c}
	ReleaseMulti(withNil)
	for i, x := range withNil {
		if x != nil {
			t.Fatalf("entry %d not cleared", i)
		}
	}
	ReleaseMulti(withNil) // cleared: nothing released twice
	if got := Outstanding() - before; got != 0 {
		t.Fatalf("%d buffers outstanding after ReleaseMulti", got)
	}
}

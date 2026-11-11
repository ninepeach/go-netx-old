package buf

import (
	"bytes"
	"errors"
	"io"
	"math"
	"net"
	"strings"
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

// Checks: BUF-05
func TestDoubleReleasePanicsInDebug(t *testing.T) {
	if !Debug {
		t.Skip("needs -tags netxdebug")
	}
	b := NewPool().Get(1)
	b.Release()
	mustPanic(t, "double Release", b.Release)
}

// Checks: BUF-16
func TestReleaseNilIsNoop(t *testing.T) {
	var b *Buf
	b.Release()
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

// Advance by any non-negative n, up to math.MaxInt, drops at most Len
// bytes: start+n must not overflow into a negative cursor.
//
// Checks: BUF-24
func TestAdvanceHugeDoesNotOverflow(t *testing.T) {
	b := GetHeadroom(16, 32)
	defer b.Release()
	b.WriteString("abcdef")
	b.Advance(math.MaxInt)
	if b.Len() != 0 || b.Start() != 22 {
		t.Fatalf("after Advance(MaxInt): Len %d Start %d, want 0 22", b.Len(), b.Start())
	}
	b.Advance(math.MaxInt) // empty: still nothing to drop
	if b.Start() != 22 {
		t.Fatalf("second Advance moved start to %d", b.Start())
	}
	copy(b.ExtendHeader(2), "hi") // the cursor is still usable
	if string(b.Bytes()) != "hi" {
		t.Fatalf("Bytes = %q", b.Bytes())
	}
}

// badReader returns a fixed count, whatever it was given.
type badReader struct{ n int }

func (r badReader) Read([]byte) (int, error) { return r.n, nil }

// badPacketConn returns a fixed count from ReadFrom.
type badPacketConn struct {
	net.PacketConn
	n int
}

func (c badPacketConn) ReadFrom([]byte) (int, net.Addr, error) { return c.n, nil, nil }

// countWriter returns a fixed count and no error from Write.
type countWriter struct{ n int }

func (w countWriter) Write([]byte) (int, error) { return w.n, nil }

// A source or destination that reports a count outside 0..len(p) breaks
// the io contract. ReadOnceFrom and ReadPacketFrom return ErrInvalidCount
// and leave the buffer unchanged instead of moving the cursor past Cap or
// before the data; WriteTo returns ErrInvalidCount for such a count and
// io.ErrShortWrite for a short write with a nil error.
//
// Checks: BUF-25
func TestInvalidIOCounts(t *testing.T) {
	b := Get(8)
	defer b.Release()
	b.WriteString("ab")
	for _, n := range []int{-1, 7, math.MaxInt} { // 6 bytes free
		if k, err := b.ReadOnceFrom(badReader{n}); k != 0 || !errors.Is(err, ErrInvalidCount) {
			t.Fatalf("ReadOnceFrom count %d = %d, %v", n, k, err)
		}
		if k, _, err := b.ReadPacketFrom(badPacketConn{n: n}); k != 0 || !errors.Is(err, ErrInvalidCount) {
			t.Fatalf("ReadPacketFrom count %d = %d, %v", n, k, err)
		}
		if string(b.Bytes()) != "ab" || b.FreeLen() != 6 {
			t.Fatalf("buffer changed after count %d: %q free %d", n, b.Bytes(), b.FreeLen())
		}
	}
	if k, err := b.ReadOnceFrom(badReader{6}); k != 6 || err != nil || b.Len() != 8 {
		t.Fatalf("valid count 6 = %d, %v, Len %d", k, err, b.Len())
	}

	b.Truncate(2)
	if n, err := b.WriteTo(countWriter{1}); n != 1 || err != io.ErrShortWrite {
		t.Fatalf("short write = %d, %v; want 1, io.ErrShortWrite", n, err)
	}
	for _, n := range []int{-1, 3} {
		if k, err := b.WriteTo(countWriter{n}); k != 0 || !errors.Is(err, ErrInvalidCount) {
			t.Fatalf("WriteTo count %d = %d, %v", n, k, err)
		}
	}
	if n, err := b.WriteTo(countWriter{2}); n != 2 || err != nil {
		t.Fatalf("full write = %d, %v", n, err)
	}
	if string(b.Bytes()) != "ab" {
		t.Fatalf("WriteTo consumed data: %q", b.Bytes())
	}
}

// Read, ReadByte and ReadBytes consume from the front by moving the start
// cursor, never the memory: the consumed bytes become headroom. Read is an
// io.Reader (io.EOF when empty, 0 and nil for an empty p); ReadByte an
// io.ByteReader; ReadBytes returns a view of the next n bytes, valid until
// the buffer changes, or consumes nothing and returns io.EOF (empty) or
// io.ErrUnexpectedEOF (fewer than n).
//
// Checks: BUF-27
func TestConsumingReads(t *testing.T) {
	var _ io.Reader = (*Buf)(nil)
	var _ io.ByteReader = (*Buf)(nil)
	b := GetHeadroom(4, 32)
	defer b.Release()
	b.WriteString("hello world!")
	orig := b.Bytes() // to check that reads move the cursor, not the bytes

	p := make([]byte, 5)
	if n, err := b.Read(p); n != 5 || err != nil || string(p) != "hello" {
		t.Fatalf("Read = %d, %v, %q", n, err, p)
	}
	if b.Start() != 9 || string(b.Bytes()) != " world!" || &b.Bytes()[0] != &orig[5] {
		t.Fatalf("after Read: start %d data %q; the data must not move", b.Start(), b.Bytes())
	}
	if n, err := b.Read(nil); n != 0 || err != nil {
		t.Fatalf("Read(nil) = %d, %v", n, err)
	}
	if c, err := b.ReadByte(); c != ' ' || err != nil {
		t.Fatalf("ReadByte = %q, %v", c, err)
	}
	view, err := b.ReadBytes(5)
	if err != nil || string(view) != "world" || &view[0] != &orig[6] {
		t.Fatalf("ReadBytes = %q, %v; want a view of the buffer", view, err)
	}
	if cap(view) != 5 {
		t.Fatalf("ReadBytes view has cap %d: appending to it would overwrite the buffer", cap(view))
	}
	if v, err := b.ReadBytes(0); len(v) != 0 || err != nil {
		t.Fatalf("ReadBytes(0) = %q, %v", v, err)
	}
	// One byte left: asking for two consumes nothing.
	if v, err := b.ReadBytes(2); v != nil || err != io.ErrUnexpectedEOF || b.Len() != 1 {
		t.Fatalf("short ReadBytes = %q, %v, Len %d", v, err, b.Len())
	}
	big := make([]byte, 10)
	if n, err := b.Read(big); n != 1 || err != nil || big[0] != '!' {
		t.Fatalf("last Read = %d, %v", n, err)
	}
	if n, err := b.Read(big); n != 0 || err != io.EOF {
		t.Fatalf("Read when empty = %d, %v", n, err)
	}
	if _, err := b.ReadByte(); err != io.EOF {
		t.Fatalf("ReadByte when empty = %v", err)
	}
	if _, err := b.ReadBytes(1); err != io.EOF {
		t.Fatalf("ReadBytes when empty = %v", err)
	}
	mustPanic(t, "ReadBytes(-1)", func() { b.ReadBytes(-1) })

	// The consumed bytes are headroom again.
	if b.Start() != 16 {
		t.Fatalf("start %d after consuming 12 bytes from 4", b.Start())
	}
	copy(b.ExtendHeader(2), "ok")
	if string(b.Bytes()) != "ok" {
		t.Fatalf("ExtendHeader after reads = %q", b.Bytes())
	}

	// io.Copy takes WriteTo, which leaves the data (as in sing); Read is
	// the consuming path.
	var out bytes.Buffer
	if n, err := io.Copy(&out, b); n != 2 || err != nil || b.Len() != 2 {
		t.Fatalf("io.Copy = %d, %v, Len after %d; WriteTo must not consume", n, err, b.Len())
	}
}

// Byte, SetByte, From, To and Range index the data, not the memory under
// it: positions are relative to the start cursor and anything outside
// 0..Len panics, including the headroom and the free space. The slices are
// views whose capacity ends where they do.
//
// Checks: BUF-28
func TestIndexedAccess(t *testing.T) {
	b := GetHeadroom(8, 32)
	defer b.Release()
	b.WriteString("0123456789")
	if b.Byte(0) != '0' || b.Byte(9) != '9' {
		t.Fatalf("Byte = %q, %q", b.Byte(0), b.Byte(9))
	}
	b.SetByte(3, 'x')
	if string(b.Bytes()) != "012x456789" {
		t.Fatalf("after SetByte: %q", b.Bytes())
	}
	if string(b.To(3)) != "012" || string(b.From(7)) != "789" || string(b.Range(3, 6)) != "x45" {
		t.Fatalf("To %q From %q Range %q", b.To(3), b.From(7), b.Range(3, 6))
	}
	if len(b.To(0)) != 0 || len(b.From(10)) != 0 || len(b.Range(4, 4)) != 0 {
		t.Fatal("empty views at the edges")
	}
	for name, v := range map[string][]byte{"To": b.To(3), "From": b.From(7), "Range": b.Range(3, 6)} {
		if cap(v) != len(v) {
			t.Fatalf("%s view: len %d cap %d", name, len(v), cap(v))
		}
	}
	b.Range(3, 6)[0] = '3' // a view, not a copy
	if b.Byte(3) != '3' {
		t.Fatal("Range returned a copy")
	}
	b.Advance(2) // positions follow the start cursor
	if b.Byte(0) != '2' || string(b.To(2)) != "23" {
		t.Fatalf("after Advance: Byte(0) %q To(2) %q", b.Byte(0), b.To(2))
	}
	for name, fn := range map[string]func(){
		"Byte(-1)":       func() { b.Byte(-1) },
		"Byte(Len)":      func() { b.Byte(b.Len()) },
		"SetByte(Len)":   func() { b.SetByte(b.Len(), 0) },
		"To(Len+1)":      func() { b.To(b.Len() + 1) },
		"To(-1)":         func() { b.To(-1) },
		"From(Len+1)":    func() { b.From(b.Len() + 1) },
		"From(-1)":       func() { b.From(-1) },
		"Range(5,4)":     func() { b.Range(5, 4) },
		"Range(-1,2)":    func() { b.Range(-1, 2) },
		"Range(0,Len+1)": func() { b.Range(0, b.Len()+1) },
	} {
		mustPanic(t, name, fn)
	}
}

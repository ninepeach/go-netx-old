package buf

import (
	"bytes"
	"io"
	"testing"
)

// stale plants a previous owner's bytes in b's whole block, as a pooled
// block holds them after Release (Get does not clear memory).
func stale(b *Buf) {
	for i := range b.data {
		b.data[i] = 'S'
	}
}

func noStale(t *testing.T, what string, p []byte) {
	t.Helper()
	if i := bytes.IndexByte(p, 'S'); i >= 0 {
		t.Fatalf("%s exposes a previous owner's byte at %d: %q", what, i, p)
	}
}

// Resize never turns bytes the current owner has not had as data, or been
// given to write (FreeBytes), into data: they read as zero. Bytes the owner
// did have, before Truncate, Advance, Read or a Reset, come back intact,
// so rewinding keeps working. Unmanaged buffers (As, With) are the
// caller's own memory and are left as they are. Resize allocates nothing.
//
// Checks: BUF-33
func TestResizeNeverExposesStaleBytes(t *testing.T) {
	p := NewPool()

	b := p.Get(64)
	stale(b)
	b.Resize(0, 64)
	noStale(t, "Resize over a fresh buffer", b.Bytes())
	if !bytes.Equal(b.Bytes(), make([]byte, 64)) {
		t.Fatalf("fresh bytes = %q, want zeros", b.Bytes())
	}
	b.Release()

	// The owner's own bytes survive Truncate, Advance and FullReset.
	b = p.Get(64)
	stale(b)
	b.WriteString("hello world")
	b.Truncate(5)
	b.Resize(0, 11)
	if string(b.Bytes()) != "hello world" {
		t.Fatalf("after Truncate, Resize = %q", b.Bytes())
	}
	b.Advance(6)
	b.Resize(0, 11)
	if string(b.Bytes()) != "hello world" {
		t.Fatalf("rewind after Advance = %q", b.Bytes())
	}
	if _, err := b.ReadBytes(6); err != nil {
		t.Fatal(err)
	}
	b.FullReset()
	b.Resize(0, 20)
	if string(b.To(11)) != "hello world" {
		t.Fatalf("after FullReset = %q", b.Bytes())
	}
	noStale(t, "Resize past what was written", b.Bytes())
	b.Release()

	// Headroom: a header written with ExtendHeader is kept, the headroom
	// in front of it that nobody wrote is not exposed.
	b = p.GetHeadroom(8, 16)
	stale(b)
	b.WriteString("body")
	copy(b.ExtendHeader(2), "hd")
	b.Resize(0, 16)
	noStale(t, "Resize into untouched headroom", b.Bytes())
	if string(b.Range(6, 12)) != "hdbody" {
		t.Fatalf("header and body = %q", b.Bytes())
	}
	b.Release()

	// FreeBytes hands the space out for writing: what was put there is
	// the owner's and Resize keeps it.
	b = p.Get(16)
	stale(b)
	copy(b.FreeBytes(), "raw")
	b.Resize(0, 3)
	if string(b.Bytes()) != "raw" {
		t.Fatalf("after FreeBytes = %q", b.Bytes())
	}
	b.Release()

	// Unmanaged memory is the caller's own: left untouched.
	mem := []byte("caller's bytes")
	w := With(mem)
	w.Resize(0, len(mem))
	if string(w.Bytes()) != "caller's bytes" {
		t.Fatalf("With + Resize = %q", w.Bytes())
	}

	b = p.Get(64)
	defer b.Release()
	if allocs := testing.AllocsPerRun(100, func() {
		b.FullReset()
		b.Resize(0, 64)
	}); allocs != 0 {
		t.Fatalf("Resize allocates %v times", allocs)
	}
}

// A full buffer probes the source for more data. The probe costs at most
// one allocation per call, however often the source returns (0, nil), and
// a source that can take a byte back (io.ByteScanner, such as bufio.Reader
// or bytes.Reader) gets the probe byte back, so nothing is lost from it.
//
// Checks: BUF-32
func TestReadFromProbe(t *testing.T) {
	b := Get(8)
	defer b.Release()
	b.WriteString("12345678")

	if allocs := testing.AllocsPerRun(20, func() { b.ReadFrom(&stallReader{}) }); allocs > 1 {
		t.Fatalf("full buffer, stalled source: %v allocs per ReadFrom", allocs)
	}

	src := bytes.NewReader([]byte("xy"))
	if n, err := b.ReadFrom(src); n != 0 || err != io.ErrShortBuffer {
		t.Fatalf("full buffer, more data = %d, %v", n, err)
	}
	if src.Len() != 2 {
		t.Fatalf("source lost the probe byte: %d left, want 2", src.Len())
	}
	rest, _ := io.ReadAll(src)
	if string(rest) != "xy" {
		t.Fatalf("source continues with %q", rest)
	}

	// Overflow while filling: the stored count is right and the source
	// resumes right after it.
	b.Truncate(6)
	src = bytes.NewReader([]byte("abcdef"))
	if n, err := b.ReadFrom(src); n != 2 || err != io.ErrShortBuffer || string(b.Bytes()) != "123456ab" {
		t.Fatalf("overflow = %d, %v, %q", n, err, b.Bytes())
	}
	if rest, _ := io.ReadAll(src); string(rest) != "cdef" {
		t.Fatalf("source continues with %q, want cdef", rest)
	}
}

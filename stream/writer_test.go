package stream

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/ninepeach/go-netx/buf"
)

func bufs(pool *buf.Pool, parts ...string) []*buf.Buf {
	out := make([]*buf.Buf, len(parts))
	for i, p := range parts {
		out[i] = pool.Copy([]byte(p))
	}
	return out
}

// countingWriter records each Write call.
type countingWriter struct {
	bytes.Buffer
	writes int
	err    error
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.writes++
	if c.err != nil {
		return 0, c.err
	}
	return c.Buffer.Write(p)
}

// vecWriter implements BuffersWriter and records how it was called.
type vecWriter struct {
	countingWriter
	vecCalls int
}

func (v *vecWriter) WriteBuffers(b *net.Buffers) (int64, error) {
	v.vecCalls++
	return b.WriteTo(&v.countingWriter)
}

// Checks: STREAM-10
func TestFlushWritesInOrderAndReleases(t *testing.T) {
	held := heldSince()
	pool := buf.NewPool()
	dst := &vecWriter{}
	w := NewWriter(dst)
	for _, b := range bufs(pool, "one,", "two,", "three") {
		w.Add(b)
	}
	if n, size := w.Pending(); n != 3 || size != 13 {
		t.Fatalf("Pending=%d,%d", n, size)
	}
	n, err := w.Flush()
	if err != nil || n != 13 || dst.String() != "one,two,three" {
		t.Fatalf("Flush: %d %v %q", n, err, dst.String())
	}
	if held() != 0 {
		t.Fatalf("buffers not released: %d", held())
	}
	if n, _ := w.Pending(); n != 0 {
		t.Fatal("queue not cleared")
	}
	if n, err := w.Flush(); n != 0 || err != nil { // empty Flush
		t.Fatal("empty flush")
	}
}

// Checks: STREAM-11
func TestFlushFailureAndDiscardRelease(t *testing.T) {
	held := heldSince()
	pool := buf.NewPool()
	boom := errors.New("broken pipe")
	w := NewWriter(&countingWriter{err: boom})
	for _, b := range bufs(pool, "a", "b", "c") {
		w.Add(b)
	}
	if _, err := w.Flush(); err != boom {
		t.Fatalf("Flush error: %v", err)
	}
	if held() != 0 {
		t.Fatalf("failed Flush kept %d bytes", held())
	}
	ok := &countingWriter{}
	w2 := NewWriter(ok)
	for _, b := range bufs(pool, "x", "y") {
		w2.Add(b)
	}
	w2.Discard()
	if held() != 0 || ok.writes != 0 {
		t.Fatalf("Discard: used=%d writes=%d", held(), ok.writes)
	}
}

// Checks: STREAM-12
func TestSingleBufferUsesWriteAndFlushIsAllocFree(t *testing.T) {
	pool := buf.NewPool()
	dst := &vecWriter{}
	w := NewWriter(dst)
	w.Add(pool.Copy([]byte("solo")))
	w.Flush()
	if dst.vecCalls != 0 || dst.writes != 1 {
		t.Fatalf("single buffer: vec=%d writes=%d", dst.vecCalls, dst.writes)
	}
	msgs := [][]byte{seq(64), seq(64), seq(64), seq(64)}
	sink := &vecWriter{}
	w = NewWriter(sink)
	add := func() {
		for _, m := range msgs {
			w.Add(pool.Copy(m))
		}
	}
	add()
	w.Flush() // warm the reused slices
	allocs := testing.AllocsPerRun(1000, func() {
		add()
		sink.Reset()
		if _, err := w.Flush(); err != nil {
			t.Fatal(err)
		}
	})
	if raceEnabled {
		return // sync.Pool drops items on purpose under -race, so Copy allocates
	}
	if allocs != 0 {
		t.Fatalf("%.1f allocs per Add+Flush", allocs)
	}
}

// Checks: STREAM-13
func TestBuffersWriterPreferredPlainWriterFallsBack(t *testing.T) {
	pool := buf.NewPool()
	vec := &vecWriter{}
	w := NewWriter(vec)
	for _, b := range bufs(pool, "a", "b", "c") {
		w.Add(b)
	}
	w.Flush()
	if vec.vecCalls != 1 || vec.String() != "abc" {
		t.Fatalf("BuffersWriter: calls=%d %q", vec.vecCalls, vec.String())
	}
	plain := &countingWriter{}
	w = NewWriter(plain)
	for _, b := range bufs(pool, "a", "b", "c") {
		w.Add(b)
	}
	if n, err := w.Flush(); err != nil || n != 3 || plain.String() != "abc" {
		t.Fatalf("plain writer: %d %v %q", n, err, plain.String())
	}
	if plain.writes != 3 {
		t.Fatalf("plain writer got %d writes, want one per buffer", plain.writes)
	}
}

// shortWriter writes at most max bytes per call and reports no error, which
// io.Writer forbids but some wrappers do.
type shortWriter struct {
	bytes.Buffer
	max int
}

func (s *shortWriter) Write(p []byte) (int, error) {
	return s.Buffer.Write(p[:min(len(p), s.max)])
}

// shortVecWriter is a BuffersWriter that stops after max bytes, no error.
type shortVecWriter struct {
	bytes.Buffer
	max int
}

func (s *shortVecWriter) WriteBuffers(b *net.Buffers) (int64, error) {
	var n int64
	for _, p := range *b {
		k, _ := s.Buffer.Write(p[:min(len(p), s.max-int(n))])
		n += int64(k)
	}
	return n, nil
}

func (s *shortVecWriter) Write(p []byte) (int, error) { return s.Buffer.Write(p) }

// Checks: STREAM-21
func TestFlushShortWriteIsAnError(t *testing.T) {
	held := heldSince()
	pool := buf.NewPool()
	cases := []struct {
		name  string
		dst   io.Writer
		parts []string
	}{
		{"single Write", &shortWriter{max: 2}, []string{"abcd"}},
		{"plain writer, several", &shortWriter{max: 2}, []string{"abcd", "efgh", "ijkl"}},
		{"BuffersWriter", &shortVecWriter{max: 3}, []string{"ab", "cd", "ef"}},
	}
	for _, c := range cases {
		w := NewWriter(c.dst)
		for _, b := range bufs(pool, c.parts...) {
			w.Add(b)
		}
		w.AddShared([]byte("!"))
		n, err := w.Flush()
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("%s: Flush = %d, %v; want io.ErrShortWrite", c.name, n, err)
		}
		if held() != 0 {
			t.Fatalf("%s: short write kept %d bytes", c.name, held())
		}
		if k, size := w.Pending(); k != 0 || size != 0 {
			t.Fatalf("%s: queue not cleared: %d,%d", c.name, k, size)
		}
	}
}

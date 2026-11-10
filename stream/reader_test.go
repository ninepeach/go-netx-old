package stream

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/ninepeach/go-netx/buf"
	"github.com/ninepeach/go-netx/limit"
)

// chunks returns its byte slices one Read at a time, then err (io.EOF by
// default).
type chunks struct {
	parts [][]byte
	err   error
	reads int
}

func (c *chunks) Read(p []byte) (int, error) {
	c.reads++
	for len(c.parts) > 0 && len(c.parts[0]) == 0 {
		c.parts = c.parts[1:]
	}
	if len(c.parts) == 0 {
		if c.err == nil {
			return 0, io.EOF
		}
		return 0, c.err
	}
	n := copy(p, c.parts[0])
	c.parts[0] = c.parts[0][n:]
	return n, nil
}

func seq(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

// Checks: STREAM-01
func TestPeekAccumulatesAcrossReads(t *testing.T) {
	frame := seq(1000)
	src := &chunks{parts: [][]byte{frame[:3], frame[3:400], frame[400:999], frame[999:]}}
	r := NewReader(src, buf.NewPool(nil), ReaderOptions{})
	got, err := r.Peek(1000)
	if err != nil || !bytes.Equal(got, frame) {
		t.Fatalf("Peek: %v, equal=%v", err, bytes.Equal(got, frame))
	}
	if r.Len() != 1000 {
		t.Fatalf("Len=%d", r.Len())
	}
}

// Checks: STREAM-02
func TestDiscardAndRead(t *testing.T) {
	data := seq(300)
	r := NewReader(&chunks{parts: [][]byte{data[:100], data[100:]}}, buf.NewPool(nil), ReaderOptions{})
	head, err := r.Peek(10)
	if err != nil || !bytes.Equal(head, data[:10]) {
		t.Fatal("peek head")
	}
	if r.Discard(10) != 10 {
		t.Fatal("discard")
	}
	rest, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(rest, data[10:]) {
		t.Fatalf("ReadAll: %v len=%d", err, len(rest))
	}
	if r.Discard(5) != 0 {
		t.Fatal("discard past end")
	}
	if n, err := r.Read(nil); n != 0 || err != nil {
		t.Fatal("zero-length Read")
	}
}

// Checks: STREAM-03
func TestGrowsWhenNeededAndShrinksWhenQuiet(t *testing.T) {
	budget := limit.NewBudget(1 << 20)
	big := seq(5000)
	src := &chunks{parts: [][]byte{big, []byte("ping")}}
	r := NewReader(src, buf.NewPool(budget), ReaderOptions{IdleSize: 512})
	if len(r.data) != 512 || r.big != nil || budget.Used() != 0 {
		t.Fatal("not idle-sized at start")
	}
	got, err := r.Peek(5000) // a large frame: grows through the size classes
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("large peek: %v", err)
	}
	if want := buf.Capacity(5000); r.big == nil || len(r.data) != want || budget.Used() != int64(want) { // 8KB
		t.Fatalf("grown to %d, budget %d", len(r.data), budget.Used())
	}
	r.Discard(5000) // drained, but the last read was large: stays grown
	if r.big == nil {
		t.Fatal("shrank right after heavy traffic")
	}
	if b, err := r.Peek(4); err != nil || string(b) != "ping" {
		t.Fatalf("small message: %q %v", b, err)
	}
	r.Discard(4) // drained after a light read: back to idle
	if r.big != nil || len(r.data) != 512 || budget.Used() != 0 {
		t.Fatalf("not shrunk: big=%v len=%d budget=%d", r.big != nil, len(r.data), budget.Used())
	}
}

// Checks: STREAM-04
func TestMaxBoundsBuffering(t *testing.T) {
	r := NewReader(&chunks{parts: [][]byte{seq(3000)}}, buf.NewPool(nil), ReaderOptions{IdleSize: 256, Max: 1024})
	if _, err := r.Peek(1025); err != ErrTooLarge {
		t.Fatalf("Peek over Max: %v", err)
	}
	if b, err := r.Peek(1024); err != nil || len(b) != 1024 {
		t.Fatalf("Peek at Max: %d %v", len(b), err)
	}
	if err := r.Fill(); err != ErrTooLarge { // full at Max, nothing consumed
		t.Fatalf("Fill when full at Max: %v", err)
	}
	if len(r.data) > 1024 {
		t.Fatalf("buffer grew past Max: %d", len(r.data))
	}
}

// Checks: STREAM-05
func TestBudgetExhaustedKeepsData(t *testing.T) {
	budget := limit.NewBudget(buf.Small) // room for one Small buffer
	pool := buf.NewPool(budget)
	hog := pool.Get(buf.Small) // take it all
	data := seq(3000)
	r := NewReader(&chunks{parts: [][]byte{data}}, pool, ReaderOptions{IdleSize: 512})
	if _, err := r.Peek(100); err != nil { // fills the idle buffer: 512 bytes
		t.Fatal(err)
	}
	got, err := r.Peek(1000) // needs a larger buffer the budget cannot give
	if err != ErrNoBuffer {
		t.Fatalf("got %v, want ErrNoBuffer", err)
	}
	if !bytes.Equal(got, data[:512]) {
		t.Fatal("buffered data lost or changed")
	}
	hog.Release()
	if got, err = r.Peek(1000); err != nil || !bytes.Equal(got, data[:1000]) {
		t.Fatalf("did not recover: %v", err)
	}
	r.Release()
	if budget.Used() != 0 {
		t.Fatalf("used=%d after Release", budget.Used())
	}
}

// Checks: STREAM-06
func TestSourceErrorsAfterData(t *testing.T) {
	boom := errors.New("boom")
	r := NewReader(&chunks{parts: [][]byte{[]byte("abc")}, err: boom}, buf.NewPool(nil), ReaderOptions{})
	got, err := r.Peek(10)
	if err != boom || string(got) != "abc" {
		t.Fatalf("Peek: %q %v", got, err)
	}
	// Data first, then the sticky error.
	p := make([]byte, 10)
	if n, err := r.Read(p); n != 3 || err != nil {
		t.Fatalf("Read: %d %v", n, err)
	}
	if _, err := r.Read(p); err != boom {
		t.Fatalf("second Read: %v", err)
	}
	if err := r.Fill(); err != boom {
		t.Fatalf("Fill: %v", err)
	}

	eof := NewReader(&chunks{parts: [][]byte{[]byte("xy")}}, buf.NewPool(nil), ReaderOptions{})
	if got, err := eof.Peek(3); err != io.EOF || string(got) != "xy" {
		t.Fatalf("EOF: %q %v", got, err)
	}
}

// Checks: STREAM-07
func TestReleaseAndOptions(t *testing.T) {
	budget := limit.NewBudget(1 << 20)
	r := NewReader(&chunks{parts: [][]byte{seq(4000)}}, buf.NewPool(budget), ReaderOptions{})
	r.Peek(4000)
	r.Release()
	r.Release() // idempotent
	if budget.Used() != 0 {
		t.Fatalf("used=%d", budget.Used())
	}
	if _, err := r.Peek(1); err != ErrReleased {
		t.Fatal(err)
	}
	if _, err := r.Read(make([]byte, 1)); err != ErrReleased {
		t.Fatal(err)
	}
	if err := r.Fill(); err != ErrReleased {
		t.Fatal(err)
	}
	for _, o := range []ReaderOptions{{IdleSize: -1}, {IdleSize: 1024, Max: 512}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("%+v accepted", o)
				}
			}()
			NewReader(&chunks{}, buf.NewPool(nil), o)
		}()
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("nil pool accepted")
			}
		}()
		NewReader(&chunks{}, nil, ReaderOptions{})
	}()
}

// emptyReader returns (0, nil) for its first empty reads, then data.
type emptyReader struct {
	empty, calls int
	data         string
}

func (e *emptyReader) Read(p []byte) (int, error) {
	e.calls++
	if e.calls <= e.empty {
		return 0, nil
	}
	return copy(p, e.data), nil
}

// Checks: STREAM-16
func TestEmptyReadsDoNotSpin(t *testing.T) {
	pool := buf.NewPool(nil)
	for name, call := range map[string]func(*Reader) error{
		"Peek": func(r *Reader) error { _, err := r.Peek(4); return err },
		"Fill": func(r *Reader) error { return r.Fill() },
		"Read": func(r *Reader) error { _, err := r.Read(make([]byte, 4)); return err },
	} {
		src := &emptyReader{empty: 1 << 30}
		r := NewReader(src, pool, ReaderOptions{})
		if err := call(r); !errors.Is(err, io.ErrNoProgress) {
			t.Fatalf("%s: err = %v after %d reads, want io.ErrNoProgress", name, err, src.calls)
		}
		if src.calls != 100 {
			t.Fatalf("%s: %d reads, want 100", name, src.calls)
		}
		// Not sticky: once the source produces data, the Reader works.
		src.empty, src.data = src.calls, "data"
		if err := call(r); err != nil {
			t.Fatalf("%s after progress: %v", name, err)
		}
		r.Release()
	}
	// Fewer than 100 empty reads are retried transparently.
	r := NewReader(&emptyReader{empty: 99, data: "abcd"}, pool, ReaderOptions{})
	if b, err := r.Peek(4); err != nil || string(b) != "abcd" {
		t.Fatalf("Peek after 99 empty reads: %q, %v", b, err)
	}
	r.Release()
}

// Checks: STREAM-17
func TestNegativeCounts(t *testing.T) {
	r := NewReader(bytes.NewReader([]byte("hello world")), buf.NewPool(nil), ReaderOptions{})
	defer r.Release()
	if b, err := r.Peek(-1); !errors.Is(err, ErrNegativeCount) || b != nil {
		t.Fatalf("Peek(-1) = %q, %v; want ErrNegativeCount", b, err)
	}
	if b, err := r.Peek(5); err != nil || string(b) != "hello" {
		t.Fatalf("Peek(5) = %q, %v", b, err)
	}
	r.Discard(2)
	before := string(r.Buffered())
	for _, n := range []int{-1, -10, math.MinInt + 1, 0} {
		if got := r.Discard(n); got != 0 {
			t.Fatalf("Discard(%d) = %d, want 0", n, got)
		}
		if b, err := r.Peek(n - 1); !errors.Is(err, ErrNegativeCount) || b != nil {
			t.Fatalf("Peek(%d) = %q, %v; want ErrNegativeCount", n-1, b, err)
		}
		if got := string(r.Buffered()); got != before || r.Len() != len(before) {
			t.Fatalf("after Discard(%d): buffered %q (Len %d), want %q", n, got, r.Len(), before)
		}
	}
	// The reader still works normally afterwards.
	if b, err := r.Peek(9); err != nil || string(b) != "llo world" {
		t.Fatalf("Peek(9) = %q, %v", b, err)
	}
	if r.Discard(9) != 9 || r.Len() != 0 {
		t.Fatal("Discard after negative calls")
	}
}

// Checks: STREAM-22
func TestShrinkReturnsGrownBufferWhenEmpty(t *testing.T) {
	budget := limit.NewBudget(1 << 20)
	big := seq(10000)
	src := &chunks{parts: [][]byte{big, []byte("abcdef")}}
	r := NewReader(src, buf.NewPool(budget), ReaderOptions{})
	if r.Shrink() {
		t.Fatal("Shrink on the idle buffer reported a change")
	}
	if _, err := r.Peek(10000); err != nil {
		t.Fatal(err)
	}
	if r.Shrink() {
		t.Fatal("Shrink with data buffered")
	}
	r.Discard(10000) // the read was large: Discard keeps the buffer
	if r.big == nil || budget.Used() == 0 {
		t.Fatal("expected the grown buffer to be kept after a heavy read")
	}
	if !r.Shrink() || r.big != nil || budget.Used() != 0 || len(r.data) != 512 {
		t.Fatalf("Shrink: big=%v budget=%d len=%d", r.big != nil, budget.Used(), len(r.data))
	}
	if b, err := r.Peek(6); err != nil || string(b) != "abcdef" { // still works
		t.Fatalf("after Shrink: %q %v", b, err)
	}
	r.Release()
	if r.Shrink() {
		t.Fatal("Shrink after Release")
	}
	// Steady state costs nothing: grow, drain, Shrink, repeat.
	pool := buf.NewPool(nil)
	loop := &chunks{}
	r2 := NewReader(loop, pool, ReaderOptions{})
	one := [][]byte{big}
	if !raceEnabled {
		allocs := testing.AllocsPerRun(100, func() {
			one[0] = big
			loop.parts = one
			r2.Peek(len(big))
			r2.Discard(len(big))
			r2.Shrink()
		})
		if allocs != 0 {
			t.Fatalf("grow/Shrink cycle allocates %.1f", allocs)
		}
	}
}

// The default Max stays 64KB when buf's largest pooled class grows (128KB
// since #35): a Reader built with zero options must not start accepting
// frames twice as large.
//
// Checks: STREAM-23
func TestDefaultMaxIs64KB(t *testing.T) {
	r := NewReader(&chunks{}, buf.NewPool(nil), ReaderOptions{})
	if _, err := r.Peek(64<<10 + 1); err != ErrTooLarge {
		t.Fatalf("Peek(64KB+1) = %v, want ErrTooLarge", err)
	}
	if buf.Large <= 64<<10 {
		t.Skip("buf.Large no longer above 64KB; nothing to pin")
	}
}

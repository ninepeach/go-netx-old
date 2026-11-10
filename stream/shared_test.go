package stream

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"

	"github.com/ninepeach/go-netx/buf"
)

// Checks: STREAM-18
func TestAddSharedInterleavesWithoutReleasing(t *testing.T) {
	held := heldSince()
	pool := buf.NewPool()
	shared := []byte("CONFIG-v7")
	orig := append([]byte(nil), shared...)
	for _, dst := range []interface {
		io.Writer
		Bytes() []byte
	}{&vecWriter{}, &countingWriter{}} {
		w := NewWriter(dst)
		w.Add(pool.Copy([]byte("hdr|")))
		w.AddShared(shared)
		w.AddShared(nil) // ignored
		w.Add(pool.Copy([]byte("|tail")))
		if n, size := w.Pending(); n != 3 || size != 4+len(shared)+5 {
			t.Fatalf("Pending = %d, %d", n, size)
		}
		if _, err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		if got := string(dst.Bytes()); got != "hdr|CONFIG-v7|tail" {
			t.Fatalf("%T: wrote %q", dst, got)
		}
		if held() != 0 {
			t.Fatalf("owned buffers not released: %d", held())
		}
		if !bytes.Equal(shared, orig) {
			t.Fatal("shared data changed (released or poisoned?)")
		}
		for i, v := range w.vec[:cap(w.vec)] {
			if v != nil {
				t.Fatalf("Writer still references queued slice %d after Flush", i)
			}
		}
	}
	// Only shared data: a single Write; several: one writev.
	one := &vecWriter{}
	w := NewWriter(one)
	w.AddShared(shared)
	w.Flush()
	if one.writes != 1 || one.vecCalls != 0 || one.String() != "CONFIG-v7" {
		t.Fatalf("one shared slice: writes=%d vec=%d %q", one.writes, one.vecCalls, one.String())
	}
	many := &vecWriter{}
	w = NewWriter(many)
	w.AddShared(shared)
	w.AddShared(shared)
	w.Flush()
	if many.vecCalls != 1 || many.String() != "CONFIG-v7CONFIG-v7" {
		t.Fatalf("two shared slices: vec=%d %q", many.vecCalls, many.String())
	}
}

// Checks: STREAM-19
func TestAddSharedOnFailureAndDiscard(t *testing.T) {
	held := heldSince()
	pool := buf.NewPool()
	shared := []byte("shared")
	fail := &countingWriter{err: errors.New("broken pipe")}
	w := NewWriter(fail)
	w.Add(pool.Copy([]byte("a")))
	w.AddShared(shared)
	if _, err := w.Flush(); err == nil {
		t.Fatal("expected the write error")
	}
	w.AddShared(shared)
	w.Add(pool.Copy([]byte("b")))
	w.Discard()
	if n, size := w.Pending(); held() != 0 || n != 0 || size != 0 {
		t.Fatalf("after failure and Discard: used=%d pending=%d/%d", held(), n, size)
	}
	if string(shared) != "shared" {
		t.Fatal("shared data changed")
	}
	// The Writer keeps working.
	ok := &countingWriter{}
	w = NewWriter(ok)
	w.AddShared(shared)
	w.Flush()
	if ok.String() != "shared" {
		t.Fatalf("%q", ok.String())
	}
}

// One immutable payload sent to many connections at once, the game
// configuration case: no copies, no per-connection buffers, no races.
// Checks: STREAM-20
func TestSharedPayloadToManyConnections(t *testing.T) {
	payload := seq(1 << 20) // 1MB
	const conns = 16
	var wg sync.WaitGroup
	errs := make(chan error, conns)
	for i := 0; i < conns; i++ {
		a, b := net.Pipe()
		wg.Add(2)
		go func() {
			defer wg.Done()
			defer a.Close()
			w := NewWriter(a)
			w.AddShared([]byte{0xC0, 0xF1})
			w.AddShared(payload)
			if _, err := w.Flush(); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			got, err := io.ReadAll(b)
			if err != nil || len(got) != 2+len(payload) || !bytes.Equal(got[2:], payload) {
				errs <- errors.New("payload mismatch")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, seq(1<<20)) {
		t.Fatal("shared payload modified")
	}
	// Steady state: queuing shared data allocates nothing.
	sink := &vecWriter{}
	w := NewWriter(sink)
	w.AddShared(payload[:64])
	w.AddShared(payload[64:128])
	w.Flush()
	allocs := testing.AllocsPerRun(1000, func() {
		sink.Reset()
		w.AddShared(payload[:64])
		w.AddShared(payload[64:128])
		w.Flush()
	})
	if allocs != 0 {
		t.Fatalf("%.1f allocs per AddShared+Flush", allocs)
	}
}

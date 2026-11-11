package buf

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

// Random Get/Release sequences with cursor operations, checked against a
// plain-slice model: each held buffer's data matches its model after every
// operation (appends, consuming reads, cursor moves), operations that must
// fail fail without changing anything, no two held buffers share memory,
// and every Get is released.
//
// Checks: NF-11
func FuzzGetRelease(f *testing.F) {
	f.Add([]byte{0, 10, 0, 0, 200, 0, 1, 0, 0, 1})
	f.Add([]byte{0, 255, 255, 0, 0, 1, 0, 255, 255, 1, 0})
	f.Add([]byte{0, 40, 0, 2, 3, 0, 3, 2, 0, 4, 1, 0, 5, 1, 0, 6, 2, 0, 7, 1, 0, 8, 0, 0, 1, 0, 0})
	f.Fuzz(func(t *testing.T, ops []byte) {
		p := NewPool()
		before := Outstanding()
		type model struct {
			b          *Buf
			head, size int // as obtained
			start      int // headroom left
			data       []byte
			reserved   int
		}
		var live []*model
		check := func(m *model) {
			t.Helper()
			if !bytes.Equal(m.b.Bytes(), m.data) || m.b.Start() != m.start ||
				m.b.Cap() != m.head+m.size-m.reserved || m.b.FreeLen() != m.head+m.size-m.reserved-m.start-len(m.data) {
				t.Fatalf("buffer %q start %d cap %d free %d; model %q start %d head %d size %d reserved %d",
					m.b.Bytes(), m.b.Start(), m.b.Cap(), m.b.FreeLen(), m.data, m.start, m.head, m.size, m.reserved)
			}
		}
		panics := func(fn func()) (p bool) {
			defer func() { p = recover() != nil }()
			fn()
			return false
		}
		free := func(m *model) int { return m.head + m.size - m.reserved - m.start - len(m.data) }
		for i := 0; i+2 < len(ops); i += 3 {
			op, arg := ops[i]%16, int(binary.LittleEndian.Uint16(ops[i+1:i+3]))
			if op == 0 || len(live) == 0 { // Get, sometimes with headroom, up to ~128KB
				head := 0
				if arg%3 == 0 {
					head = arg % 64
				}
				size := arg * 2
				b := p.GetHeadroom(head, size)
				m := &model{b: b, head: head, size: size, start: head}
				check(m)
				live = append(live, m)
				continue
			}
			m := live[arg%len(live)]
			n := arg % 300
			mark := bytes.Repeat([]byte{byte(i)}, n)
			switch op {
			case 1: // Release
				m.b.Release()
				for k, x := range live {
					if x == m {
						live = append(live[:k], live[k+1:]...)
						break
					}
				}
				continue
			case 2: // Write
				k, err := m.b.Write(mark)
				want := min(n, free(m))
				if k != want || (err != nil) != (want < n) {
					t.Fatalf("Write(%d) = %d, %v; free %d", n, k, err, free(m))
				}
				m.data = append(m.data, mark[:k]...)
			case 3: // Extend
				if n > free(m) {
					if !panics(func() { m.b.Extend(n) }) {
						t.Fatal("Extend past Cap did not panic")
					}
				} else {
					copy(m.b.Extend(n), mark)
					m.data = append(m.data, mark...)
				}
			case 4: // ExtendHeader
				n %= 40
				if n > m.start {
					if !panics(func() { m.b.ExtendHeader(n) }) {
						t.Fatal("ExtendHeader past the headroom did not panic")
					}
				} else {
					copy(m.b.ExtendHeader(n), mark)
					m.start -= n
					m.data = append(append([]byte{}, mark[:n]...), m.data...)
				}
			case 5: // Advance
				k := min(n, len(m.data))
				m.b.Advance(n)
				m.start += k
				m.data = m.data[k:]
			case 6: // Truncate
				if n > len(m.data) {
					if !panics(func() { m.b.Truncate(n) }) {
						t.Fatal("Truncate past Len did not panic")
					}
				} else {
					m.b.Truncate(n)
					m.data = m.data[:n]
				}
			case 7: // Reserve / Unreserve
				if arg%2 == 0 {
					if n > free(m) {
						if !panics(func() { m.b.Reserve(n) }) {
							t.Fatal("Reserve past FreeLen did not panic")
						}
					} else {
						m.b.Reserve(n)
						m.reserved += n
					}
				} else {
					if n > m.reserved {
						if !panics(func() { m.b.Unreserve(n) }) {
							t.Fatal("Unreserve past the reserve did not panic")
						}
					} else {
						m.b.Unreserve(n)
						m.reserved -= n
					}
				}
			case 8: // Reset
				m.b.Reset()
				m.start, m.data, m.reserved = m.head, nil, 0
			case 9: // ReadFullFrom
				k, err := m.b.ReadFullFrom(bytes.NewReader(mark), n)
				if n > free(m) {
					if k != 0 || err != io.ErrShortBuffer {
						t.Fatalf("ReadFullFrom past Cap = %d, %v", k, err)
					}
				} else {
					if k != n || err != nil {
						t.Fatalf("ReadFullFrom(%d) = %d, %v", n, k, err)
					}
					m.data = append(m.data, mark...)
				}
			case 10: // Read: consumes from the front
				p := make([]byte, n)
				k, err := m.b.Read(p)
				want := min(n, len(m.data))
				if k != want || !bytes.Equal(p[:k], m.data[:want]) || (err == io.EOF) != (n > 0 && len(m.data) == 0) {
					t.Fatalf("Read(%d) = %d, %v with %d bytes", n, k, err, len(m.data))
				}
				m.start += k
				m.data = m.data[k:]
			case 11: // ReadBytes: a view, or nothing consumed
				v, err := m.b.ReadBytes(n)
				if n > len(m.data) {
					if v != nil || err == nil {
						t.Fatalf("ReadBytes(%d) of %d bytes = %q, %v", n, len(m.data), v, err)
					}
				} else {
					if err != nil || !bytes.Equal(v, m.data[:n]) {
						t.Fatalf("ReadBytes(%d) = %q, %v", n, v, err)
					}
					m.start += n
					m.data = m.data[n:]
				}
			case 12: // FullReset: no headroom, no reserve
				m.b.FullReset()
				m.start, m.data, m.reserved = 0, nil, 0
			case 13: // WriteZeroN
				err := m.b.WriteZeroN(n)
				if (err != nil) != (n > free(m)) {
					t.Fatalf("WriteZeroN(%d) = %v; free %d", n, err, free(m))
				}
				if err == nil {
					m.data = append(m.data, make([]byte, n)...)
				}
			case 14: // ReadFrom: to EOF, or full
				k, err := m.b.ReadFrom(bytes.NewReader(mark))
				want := min(n, free(m))
				if int(k) != want || (err != nil) != (want < n) {
					t.Fatalf("ReadFrom(%d bytes) = %d, %v; free %d", n, k, err, free(m))
				}
				m.data = append(m.data, mark[:want]...)
			case 15: // Resize: the cursors move, the bytes stay
				start, length := arg%64, n
				room := m.head + m.size - m.reserved
				if start > room || length > room-start {
					if !panics(func() { m.b.Resize(start, length) }) {
						t.Fatal("Resize past Cap did not panic")
					}
				} else {
					m.b.Resize(start, length)
					m.start = start
					m.data = append([]byte(nil), m.b.Bytes()...)
					if len(m.data) != length {
						t.Fatalf("Resize(%d, %d) left %d bytes", start, length, len(m.data))
					}
				}
			}
			check(m)
		}
		// No two held buffers share memory: overwrite each, then verify.
		for k, m := range live {
			for j := range m.b.data {
				m.b.data[j] = byte(k)
			}
		}
		for k, m := range live {
			for _, v := range m.b.data {
				if v != byte(k) {
					t.Fatal("two held buffers share memory")
				}
			}
			m.b.Release()
		}
		if Outstanding() != before {
			t.Fatalf("outstanding after releasing all: %d", Outstanding()-before)
		}
	})
}

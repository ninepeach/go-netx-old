package buf

import (
	"encoding/binary"
	"testing"

	"github.com/ninepeach/go-netx/limit"
)

// Random Get/Release sequences: the budget always equals the capacity of
// the buffers held, Get refuses exactly when the budget is short, and no
// two held buffers share memory.
//
// Checks: NF-11
func FuzzGetRelease(f *testing.F) {
	f.Add([]byte{0, 10, 0, 0, 200, 0, 1, 0, 0, 1})
	f.Add([]byte{0, 255, 255, 0, 0, 1, 0, 255, 255, 1, 0})
	f.Fuzz(func(t *testing.T, ops []byte) {
		const max = 200 << 10
		budget := limit.NewBudget(max)
		p := NewPool(budget)
		type held struct {
			b    *Buf
			mark byte
			cost int64
		}
		var live []held
		var used int64
		for i := 0; i+2 < len(ops); i += 3 {
			arg := int(binary.LittleEndian.Uint16(ops[i+1 : i+3]))
			if ops[i]%2 == 0 { // Get
				size := arg * 2     // up to ~128KB, past the largest class (64KB)
				cost := int64(size) // model: next power of two, at least 64, up to 64KB
				if size <= Large {
					cost = MinSize
					for cost < int64(size) {
						cost *= 2
					}
				}
				if got := Capacity(size); int64(got) != cost {
					t.Fatalf("Capacity(%d) = %d, model %d", size, got, cost)
				}
				b := p.Get(size)
				if (b == nil) != (used+cost > max) {
					t.Fatalf("Get(%d): nil=%v used=%d cost=%d", size, b == nil, used, cost)
				}
				if b == nil {
					continue
				}
				if len(b.B) != size {
					t.Fatalf("len %d want %d", len(b.B), size)
				}
				mark := byte(i)
				for j := range b.B {
					b.B[j] = mark
				}
				live = append(live, held{b, mark, cost})
				used += cost
			} else if len(live) > 0 { // Release
				k := arg % len(live)
				h := live[k]
				for _, v := range h.b.B {
					if v != h.mark {
						t.Fatal("held buffer overwritten by another owner")
					}
				}
				h.b.Release()
				used -= h.cost
				live = append(live[:k], live[k+1:]...)
			}
			if budget.Used() != used {
				t.Fatalf("budget %d, model %d", budget.Used(), used)
			}
		}
		for _, h := range live {
			h.b.Release()
		}
		if budget.Used() != 0 {
			t.Fatalf("budget not returned: %d", budget.Used())
		}
	})
}

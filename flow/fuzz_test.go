package flow

import "testing"

// Random operation sequences against a model map: the table must agree with
// the model on which key maps to which flow, Remove must honour epochs,
// OnClose must fire exactly once per flow that leaves, a closed table
// creates nothing, and MaxFlows (picked by the first byte) is never exceeded.
//
// Checks: NF-09
func FuzzTableOps(f *testing.F) {
	f.Add([]byte{0, 1, 0, 1, 2, 1, 1, 1})
	f.Add([]byte{0, 0, 3, 0, 0, 0, 2, 0, 4, 0})
	f.Add([]byte{2, 0, 0, 0, 1, 0, 2, 0, 3, 0, 1, 2, 0, 0, 4})
	f.Fuzz(func(t *testing.T, ops []byte) {
		maxFlows := 0
		if len(ops) > 0 {
			maxFlows, ops = int(ops[0]%5), ops[1:] // 0 = unlimited
		}
		closed := map[uint64]int{}
		tb := NewTable(TableOptions[byte, int]{
			Shards:   4,
			MaxFlows: maxFlows,
			OnClose:  func(_ byte, fl *Flow[int]) { closed[fl.ID()]++ },
		})
		shut := false
		var refused uint64
		model := map[byte]uint64{} // key → current flow ID
		var removed []uint64       // IDs that left the table
		for i := 0; i+1 < len(ops); i += 2 {
			op, key := ops[i]%6, ops[i+1]%8
			switch op {
			case 0: // GetOrOpen
				fl, created := tb.GetOrOpen(key, nil)
				id, had := model[key]
				full := maxFlows > 0 && len(model) >= maxFlows
				switch {
				case had:
					if created || fl == nil || fl.ID() != id {
						t.Fatalf("GetOrOpen(%d) of an existing flow: %v %v", key, fl, created)
					}
				case shut || full:
					if created || fl != nil {
						t.Fatalf("GetOrOpen(%d) created a flow (closed=%v full=%v)", key, shut, full)
					}
					if full && !shut {
						refused++
					}
				default:
					if !created || fl == nil {
						t.Fatalf("GetOrOpen(%d) did not create a flow", key)
					}
					model[key] = fl.ID()
				}
			case 1: // Get
				fl, ok := tb.Get(key)
				id, had := model[key]
				if ok != had || (ok && fl.ID() != id) {
					t.Fatalf("Get(%d) disagrees with model", key)
				}
			case 2: // Remove with the current ID
				if id, had := model[key]; had {
					if !tb.Remove(key, id) {
						t.Fatalf("Remove(%d, current) failed", key)
					}
					delete(model, key)
					removed = append(removed, id)
				}
			case 3: // Remove with a stale ID must never remove
				if len(removed) > 0 {
					stale := removed[int(key)%len(removed)]
					if tb.Remove(key, stale) {
						t.Fatalf("stale ID %d removed key %d", stale, key)
					}
				}
			case 5: // a lookup never changes LastSeen; Touch never moves it back
				if fl, ok := tb.Get(key); ok {
					before := fl.LastSeen()
					tb.Get(key)
					tb.GetOrOpen(key, nil)
					if fl.LastSeen() != before {
						t.Fatalf("lookup of %d changed LastSeen", key)
					}
					fl.Touch()
					if fl.LastSeen() < before {
						t.Fatalf("Touch moved LastSeen of %d backwards", key)
					}
				}
			case 4: // Close everything
				for k, id := range model {
					removed = append(removed, id)
					delete(model, k)
				}
				tb.Close()
				shut = true
			}
			if tb.Len() != len(model) || tb.Closed() != shut || tb.Refused() != refused {
				t.Fatalf("Len=%d model=%d closed=%v/%v refused=%d/%d", tb.Len(), len(model), tb.Closed(), shut, tb.Refused(), refused)
			}
		}
		for _, id := range removed {
			if closed[id] != 1 {
				t.Fatalf("flow %d OnClose called %d times", id, closed[id])
			}
		}
		if len(closed) != len(removed) {
			t.Fatalf("OnClose for %d flows, %d removed", len(closed), len(removed))
		}
	})
}

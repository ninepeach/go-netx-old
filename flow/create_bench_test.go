package flow

import "testing"

// GetOrOpen creating a new flow each time, with and without MaxFlows.
//
// Checks: FLOW-32
func BenchmarkGetOrOpenCreate(b *testing.B) {
	for _, max := range []int{0, 1 << 30} {
		name := "unlimited"
		if max > 0 {
			name = "MaxFlows"
		}
		b.Run(name, func(b *testing.B) {
			tb := NewTable[int, int](TableOptions[int, int]{MaxFlows: max})
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				tb.GetOrOpen(i, nil)
			}
		})
	}
}

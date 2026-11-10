package buf

import "math/bits"

// Size classes are powers of two from MinSize to Large: 64B, 128B, …, 64KB.
// A request takes the smallest class that holds it, so a buffer's memory
// is less than twice its size (except below MinSize), and the class is one
// bit operation away. Small, Medium and Large name common sizes.
const (
	MinSize = 64       // smallest class: one cache line
	Small   = 2 << 10  // one MTU-sized datagram
	Medium  = 16 << 10 // a typical stream read
	Large   = 64 << 10 // largest pooled class (D-26); larger requests are not pooled
)

const (
	minShift   = 6 // log2(MinSize)
	numClasses = 11
)

// Capacity returns the memory a buffer of n bytes (headroom included)
// occupies: the smallest class holding n, or n itself above Large, where
// buffers are allocated directly and not pooled.
func Capacity(n int) int {
	if c := classOf(n); c >= 0 {
		return MinSize << c
	}
	return n
}

// classOf returns the class index for n, or -1 above Large.
func classOf(n int) int {
	if n > Large {
		return -1
	}
	if n <= MinSize {
		return 0
	}
	return bits.Len(uint(n-1)) - minShift
}

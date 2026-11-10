package buf

// Helpers for a batch of buffers, as in sing's multi.go: a frame split over
// several buffers, or a writev queue.

// LenMulti returns the total data length of bufs.
func LenMulti(bufs []*Buf) int {
	n := 0
	for _, b := range bufs {
		n += b.Len()
	}
	return n
}

// IsEmptyMulti reports whether no buffer in bufs holds data.
func IsEmptyMulti(bufs []*Buf) bool {
	for _, b := range bufs {
		if !b.IsEmpty() {
			return false
		}
	}
	return true
}

// ToSliceMulti returns the data of each buffer, in order, as a new slice
// (convert it to net.Buffers to write it with writev). The slices alias the
// buffers: valid until the buffers change or are released.
func ToSliceMulti(bufs []*Buf) [][]byte {
	out := make([][]byte, len(bufs))
	for i, b := range bufs {
		out[i] = b.Bytes()
	}
	return out
}

// CopyMulti copies the data of bufs, in order, into dst and returns the
// number of bytes copied: LenMulti(bufs), or len(dst) if that is smaller.
func CopyMulti(dst []byte, bufs []*Buf) int {
	n := 0
	for _, b := range bufs {
		if n == len(dst) {
			break
		}
		n += copy(dst[n:], b.Bytes())
	}
	return n
}

// ReleaseMulti releases every buffer in bufs and sets its entry to nil, so
// the slice cannot release a buffer twice. Nil entries are skipped.
func ReleaseMulti(bufs []*Buf) {
	for i, b := range bufs {
		b.Release()
		bufs[i] = nil
	}
}

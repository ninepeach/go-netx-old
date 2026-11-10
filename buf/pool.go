package buf

import "sync"

// Pool hands out buffers in power-of-two size classes. A Pool is safe for
// concurrent use. The zero value is not usable; call NewPool, or use
// Default through the package-level Get, GetHeadroom and Copy.
type Pool struct {
	pools [numClasses]sync.Pool
}

// NewPool returns an empty pool.
func NewPool() *Pool {
	p := &Pool{}
	for i := range p.pools {
		size, class := MinSize<<i, int8(i)
		p.pools[i].New = func() any {
			return &Buf{data: make([]byte, size), class: class, pool: p}
		}
	}
	return p
}

// Default is the pool behind the package-level functions.
var Default = NewPool()

// Get returns an empty buffer from Default with room for size bytes.
func Get(size int) *Buf { return Default.Get(size) }

// GetHeadroom returns an empty buffer from Default with head bytes kept
// in front for ExtendHeader and room for size bytes of data.
func GetHeadroom(head, size int) *Buf { return Default.GetHeadroom(head, size) }

// Copy returns a buffer from Default holding a copy of data.
func Copy(data []byte) *Buf { return Default.Copy(data) }

// Get returns an empty buffer with room for size bytes: Len 0, Cap and
// FreeLen size. It never returns nil and panics if size is negative.
// Contents are not zeroed: write before you read or send.
func (p *Pool) Get(size int) *Buf { return p.GetHeadroom(0, size) }

// GetHeadroom returns an empty buffer whose data starts head bytes in, so
// up to head bytes of headers can be added in front with ExtendHeader, with
// room for size bytes after that. Cap is head+size. It panics if either is
// negative.
func (p *Pool) GetHeadroom(head, size int) *Buf {
	if head < 0 || size < 0 || head+size < 0 {
		panic("buf: negative size")
	}
	total := head + size
	var b *Buf
	if class := classOf(total); class >= 0 {
		b = p.pools[class].Get().(*Buf)
	} else {
		b = &Buf{data: make([]byte, total), class: -1, pool: p}
	}
	b.start, b.end, b.head = head, head, head
	b.capacity, b.size = total, total
	b.onGet()
	return b
}

// Copy returns a buffer holding a copy of data.
func (p *Pool) Copy(data []byte) *Buf {
	b := p.Get(len(data))
	b.end += copy(b.data[:b.capacity], data)
	return b
}

// As wraps data as a buffer that holds it (Bytes returns data). The memory
// stays the caller's: Release only forgets it.
func As(data []byte) *Buf {
	return &Buf{data: data, end: len(data), capacity: len(data), size: len(data), class: -1}
}

// With wraps data as an empty buffer that writes into it. The memory stays
// the caller's: Release only forgets it.
func With(data []byte) *Buf {
	return &Buf{data: data, capacity: len(data), size: len(data), class: -1}
}

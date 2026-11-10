// Package udp is a datagram transport with no per-packet goroutines, no
// per-packet allocation and no internal queue.
//
// Each reader goroutine owns preallocated receive buffers (Options.Batch of
// them, for recvmmsg on Linux) and calls the Handler inline. Concurrency comes from Options.Readers: with
// socket.Options.ReusePort each reader gets its own socket and the kernel
// spreads flows across them; without it the readers share one socket. When
// handlers fall behind, the kernel receive buffer fills and drops datagrams;
// Stats.KernelDrops reports those drops on Linux.
//
// UDP stays a datagram transport here: nothing is reordered, retransmitted
// or presented as a stream.
package udp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ninepeach/go-netx/internal/backoff"
	"github.com/ninepeach/go-netx/internal/counter"
	"github.com/ninepeach/go-netx/internal/sys"
	"github.com/ninepeach/go-netx/socket"
)

var (
	// ErrClosed is returned by Serve after Close or Shutdown, and by writes
	// on a closed Conn.
	ErrClosed = errors.New("udp: closed")
	// ErrTooLarge is returned when writing more than MaxDatagram bytes.
	ErrTooLarge = errors.New("udp: datagram exceeds MaxDatagram")
)

// MaxDatagram is the largest datagram go-netx sends or receives, in both
// directions. WriteTo and Reply refuse longer payloads with ErrTooLarge;
// Options.MaxPacket defaults to it and may not exceed it, so received
// datagrams longer than this are always dropped as Truncated.
const MaxDatagram = 65000

// maxBatch bounds Options.Batch on every platform (the kernel's UIO_MAXIOV
// limit on recvmmsg), so a configuration valid on Linux is valid anywhere.
const maxBatch = 1024

// Packet is one received datagram. The *Packet itself is borrowed like
// Data: the reader reuses it for the next datagram, so a Handler must not
// keep the pointer; copy the fields it needs.
type Packet struct {
	// Data is borrowed: it is valid only until the Handler returns.
	// Copy it (for example with buf.Pool.Copy) to keep it.
	Data []byte
	// From is the sender. IPv4-mapped IPv6 addresses are unmapped so the
	// same client always yields the same key.
	From netip.AddrPort
	// Reader is the index of the reader that received the datagram, in
	// [0, Options.Readers). With ReusePort the kernel keeps a flow on one
	// reader, so gateways can shard per-flow state by Reader without locks.
	Reader int
}

// Handler is called inline on the reader goroutine for every datagram.
//
// It must finish in bounded time and must not perform blocking operations it
// cannot bound. While it runs its reader receives nothing, and neither
// Shutdown nor Close can interrupt it: closing the socket only stops the
// next read, so a handler that never returns keeps Serve from returning.
// There is deliberately no context. Work that may wait belongs on a
// gateway-owned goroutine fed through a bounded queue (see package queue).
// Panics are not recovered.
type Handler func(c *Conn, p *Packet)

// Options configures a Conn.
type Options struct {
	// Readers is the number of reader goroutines, each holding Batch
	// receive buffers of MaxPacket bytes; with ReusePort, also the number
	// of sockets. Default 1.
	// On macOS ReusePort does not spread datagrams, so one reader gets them all.
	// go-netx does not cap it: sizing it to the host is the caller's job,
	// typically no more than the number of CPUs.
	Readers int
	// MaxPacket is the receive buffer size per reader, at most
	// MaxDatagram (also the default). Larger incoming datagrams are
	// dropped and counted as Truncated. Sends are limited by MaxDatagram,
	// not by MaxPacket.
	//
	// Size it to the protocol: gateways whose datagrams fit in one MTU
	// (game, media) do well with 1500-2048, which keeps reader buffers
	// cache-friendly and surfaces oversized senders in Stats.Truncated.
	// The buffers are allocated once per reader: Batch × MaxPacket bytes,
	// about 520KB per reader with the Linux defaults (8 × 65000) and 16KB
	// with MaxPacket 2048.
	MaxPacket int
	// Batch is how many datagrams a reader receives per system call (Linux
	// recvmmsg). A batch never waits to fill: a call returns what is
	// queued, so batching adds no latency. The handler still sees one
	// datagram at a time, and Packet.Data stays valid only for that call.
	// Default 8 on Linux, where the measured receive cost per queued
	// datagram drops by about 44%; at most 1024. When several readers share
	// one socket (Readers > 1 without ReusePort) the default is 1: a batch
	// would give one reader several datagrams to handle in turn while the
	// others sit idle. An explicit value is used as given. Batching only changes
	// performance, so where recvmmsg is unavailable (macOS, Windows) any
	// Batch reads one datagram per call, and the same configuration runs
	// on a development Mac and a Linux server (D-18). Conn.Batch reports
	// the size in effect.
	Batch  int
	Socket socket.Options
}

// Stats is a point-in-time snapshot.
type Stats struct {
	RecvPackets uint64
	RecvBytes   uint64
	SendPackets uint64
	SendBytes   uint64
	Truncated   uint64 // datagrams larger than MaxPacket, dropped (all platforms)
	RecvErrors  uint64 // transient errors retried with backoff, plus a final fatal one
	SendErrors  uint64
	// KernelDrops counts datagrams the kernel dropped because the receive
	// queue was full. The kernel reports the count along with the next
	// datagram it queues, so drops after the last queued datagram appear
	// only once another one arrives.
	KernelDrops uint64
	// KernelDropsEnabled reports whether the kernel agreed to report drops
	// on every socket (Linux SO_RXQ_OVFL). When false, KernelDrops is 0
	// because it is not measured, not because nothing was dropped.
	KernelDropsEnabled bool
}

type readerStats struct {
	recvPackets, recvBytes, truncated, recvErrors counter.Counter
}

// dropCounter turns the kernel's wrapping 32-bit cumulative drop count into
// a 64-bit total. Readers sharing a socket may report counts out of order,
// so only forward movement (as a signed 32-bit delta) is accumulated.
type dropCounter struct {
	last  atomic.Uint32
	total counter.Counter
}

func (d *dropCounter) observe(raw uint32) {
	for {
		prev := d.last.Load()
		delta := int32(raw - prev) // wrap-safe
		if delta <= 0 {
			return // unchanged or a stale report from another reader
		}
		if d.last.CompareAndSwap(prev, raw) {
			d.total.Add(uint64(delta))
			return
		}
	}
}

type sendStats struct {
	packets, bytes, errors counter.Counter
}

// Conn is a bound UDP endpoint. It implements the netx Service contract
// through Serve and Shutdown.
type Conn struct {
	socks   []*net.UDPConn
	drops   []dropCounter // per socket
	sends   []sendStats   // per socket
	readers []readerStats
	handler Handler
	max     int
	batch   int
	local   netip.AddrPort

	mu        sync.Mutex
	serving   bool
	closed    bool
	fatal     error         // first unrecoverable read error
	done      chan struct{} // closed when Serve returns
	closing   chan struct{} // closed by Close; wakes readers in backoff
	closeOnce sync.Once
	dropsOn   bool // kernel drop counting enabled on every socket
}

// Listen binds network ("udp", "udp4", "udp6") at address.
func Listen(ctx context.Context, network, address string, h Handler, o Options) (*Conn, error) {
	if h == nil {
		return nil, errors.New("udp: nil handler")
	}
	if o.Readers < 0 || o.MaxPacket < 0 || o.Batch < 0 {
		return nil, errors.New("udp: options must not be negative")
	}
	if o.MaxPacket > MaxDatagram {
		return nil, fmt.Errorf("udp: MaxPacket %d exceeds MaxDatagram %d", o.MaxPacket, MaxDatagram)
	}
	if o.Batch > maxBatch {
		return nil, fmt.Errorf("udp: Batch %d exceeds %d", o.Batch, maxBatch)
	}
	if o.Readers == 0 {
		o.Readers = 1
	}
	if o.MaxPacket == 0 {
		o.MaxPacket = MaxDatagram
	}
	nsock := 1
	if o.Socket.ReusePort {
		nsock = o.Readers
	}
	switch {
	case !sys.BatchSupported:
		o.Batch = 1 // no recvmmsg: one datagram per call
	case o.Batch == 0 && o.Readers > nsock:
		// Readers share a socket: a batch would hand several datagrams to
		// one reader to handle in turn while the others wait on an empty
		// queue, so by default each reader takes one.
		o.Batch = 1
	case o.Batch == 0:
		o.Batch = sys.DefaultBatch
	}
	lc := socket.ListenConfig(o.Socket)
	c := &Conn{handler: h, max: o.MaxPacket, batch: o.Batch, done: make(chan struct{}), closing: make(chan struct{}), dropsOn: true}
	for i := 0; i < nsock; i++ {
		pc, err := lc.ListenPacket(ctx, network, address)
		if err != nil {
			c.closeSockets()
			return nil, err
		}
		uc := pc.(*net.UDPConn)
		c.socks = append(c.socks, uc)
		if err := raiseSendBuffer(uc, o.Socket.SendBuffer); err != nil {
			c.closeSockets()
			return nil, err
		}
		if i == 0 {
			// Bind the remaining sockets to the resolved port so ":0" works.
			c.local = uc.LocalAddr().(*net.UDPAddr).AddrPort()
			address = c.local.String()
		}
		if rc, err := uc.SyscallConn(); err != nil || !sys.EnableDropCount(rc) {
			c.dropsOn = false
		}
	}
	c.drops = make([]dropCounter, nsock)
	c.sends = make([]sendStats, nsock)
	c.readers = make([]readerStats, o.Readers)
	return c, nil
}

// Batch returns the number of datagrams a reader receives per system call:
// Options.Batch, its default, or 1 where recvmmsg is unavailable.
func (c *Conn) Batch() int { return c.batch }

// LocalAddr returns the bound address.
func (c *Conn) LocalAddr() netip.AddrPort { return c.local }

// Serve runs the readers until Shutdown or Close, or until ctx is cancelled,
// which closes the Conn. It returns after every reader has stopped: nil on
// a requested stop, or the first fatal read error, which also closes the
// Conn so no reader keeps running on a broken socket.
func (c *Conn) Serve(ctx context.Context) error {
	c.mu.Lock()
	switch {
	case c.closed:
		c.mu.Unlock()
		return ErrClosed
	case c.serving:
		c.mu.Unlock()
		return errors.New("udp: Serve called more than once")
	}
	c.serving = true
	c.mu.Unlock()
	defer close(c.done)

	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()

	var wg sync.WaitGroup
	for i := range c.readers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := i % len(c.socks)
			c.read(i, s)
		}(i)
	}
	wg.Wait()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fatal
}

func (c *Conn) read(reader, sock int) {
	if c.batch > 1 {
		c.readBatch(reader, sock)
		return
	}
	uc := c.socks[sock]
	st := &c.readers[reader]
	drops := &c.drops[sock]
	data := make([]byte, c.max)
	oob := make([]byte, sys.OOBSize)
	p := &Packet{Reader: reader}
	var backoff time.Duration
	for {
		n, oobn, flags, from, err := uc.ReadMsgUDPAddrPort(data, oob)
		if err != nil {
			if c.readFailed(st, err, &backoff) {
				return
			}
			continue
		}
		backoff = 0
		if oobn > 0 {
			if d, ok := sys.DropCount(oob[:oobn]); ok {
				drops.observe(d)
			}
		}
		if flags&sys.MsgTrunc != 0 {
			st.truncated.Inc()
			continue
		}
		st.recvPackets.Inc()
		st.recvBytes.Add(uint64(n))
		p.Data = data[:n]
		p.From = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		c.handler(c, p)
	}
}

// readBatch is read with recvmmsg: up to c.batch datagrams per system
// call, each handed to the handler in arrival order.
func (c *Conn) readBatch(reader, sock int) {
	st := &c.readers[reader]
	drops := &c.drops[sock]
	raw, err := c.socks[sock].SyscallConn()
	if err != nil {
		var backoff time.Duration
		c.readFailed(st, err, &backoff)
		return
	}
	mem := make([]byte, c.batch*c.max)
	bufs := make([][]byte, c.batch)
	for i := range bufs {
		bufs[i] = mem[i*c.max : (i+1)*c.max : (i+1)*c.max]
	}
	br := sys.NewBatchReader(raw, bufs, sys.OOBSize)
	p := &Packet{Reader: reader}
	var backoff time.Duration
	for {
		n, err := br.Read()
		if err != nil {
			if c.readFailed(st, err, &backoff) {
				return
			}
			continue
		}
		backoff = 0
		for i := 0; i < n; i++ {
			size, oob, flags, from := br.Message(i)
			if len(oob) > 0 {
				if d, ok := sys.DropCount(oob); ok {
					drops.observe(d)
				}
			}
			if flags&sys.MsgTrunc != 0 {
				st.truncated.Inc()
				continue
			}
			st.recvPackets.Inc()
			st.recvBytes.Add(uint64(size))
			p.Data = bufs[i][:size]
			p.From = from
			c.handler(c, p)
		}
	}
}

// readFailed handles a read error and reports whether the reader must stop.
func (c *Conn) readFailed(st *readerStats, err error, delay *time.Duration) bool {
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	switch sys.ClassifyRead(err) {
	case sys.Truncated:
		st.truncated.Inc()
		return false
	case sys.Transient:
		st.recvErrors.Inc()
		*delay = backoff.Next(*delay)
		// Close during the backoff stops the reader now, not after it.
		return !backoff.Wait(*delay, c.closing)
	}
	st.recvErrors.Inc()
	c.mu.Lock()
	if c.fatal == nil && !c.closed {
		c.fatal = fmt.Errorf("udp: read: %w", err)
	}
	c.mu.Unlock()
	_ = c.Close()
	return true
}

// WriteTo sends b to addr. It is safe for concurrent use and does not
// allocate. Writes are spread across sockets by destination port.
func (c *Conn) WriteTo(b []byte, to netip.AddrPort) error {
	return c.write(int(to.Port())%len(c.socks), b, to)
}

// ErrNilPacket is returned by Reply for a nil packet.
var ErrNilPacket = errors.New("udp: nil packet")

// Reply sends b to p.From through the socket that received p. A Reader
// index this Conn did not assign falls back to WriteTo's socket choice.
func (c *Conn) Reply(p *Packet, b []byte) error {
	if p == nil {
		return ErrNilPacket
	}
	if p.Reader < 0 || p.Reader >= len(c.readers) {
		return c.WriteTo(b, p.From)
	}
	return c.write(p.Reader%len(c.socks), b, p.From)
}

func (c *Conn) write(sock int, b []byte, to netip.AddrPort) error {
	if len(b) > MaxDatagram {
		return ErrTooLarge
	}
	st := &c.sends[sock]
	n, err := c.socks[sock].WriteToUDPAddrPort(b, to)
	if err != nil {
		st.errors.Inc()
		if errors.Is(err, net.ErrClosed) {
			return ErrClosed
		}
		return err
	}
	st.packets.Inc()
	st.bytes.Add(uint64(n))
	return nil
}

// Shutdown closes the sockets and waits until Serve has returned or ctx is
// done. UDP has no connections to drain, so this is immediate unless a
// handler is still running.
func (c *Conn) Shutdown(ctx context.Context) error {
	_ = c.Close()
	c.mu.Lock()
	serving := c.serving
	c.mu.Unlock()
	if !serving {
		return nil
	}
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close closes the sockets without waiting.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		close(c.closing)
		c.closeSockets()
	})
	return nil
}

func (c *Conn) closeSockets() {
	for _, s := range c.socks {
		_ = s.Close()
	}
}

// Stats returns a snapshot summed over readers and sockets.
func (c *Conn) Stats() Stats {
	s := Stats{KernelDropsEnabled: c.dropsOn}
	for i := range c.readers {
		r := &c.readers[i]
		s.RecvPackets += r.recvPackets.Load()
		s.RecvBytes += r.recvBytes.Load()
		s.Truncated += r.truncated.Load()
		s.RecvErrors += r.recvErrors.Load()
	}
	for i := range c.sends {
		w := &c.sends[i]
		s.SendPackets += w.packets.Load()
		s.SendBytes += w.bytes.Load()
		s.SendErrors += w.errors.Load()
		s.KernelDrops += c.drops[i].total.Load()
	}
	return s
}

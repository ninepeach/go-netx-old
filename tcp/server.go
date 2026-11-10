// Package tcp is a stream transport: an accept loop with admission control,
// one goroutine per connection, and three-phase shutdown.
//
// Shutdown runs StopAccept → Drain → Abort. StopAccept closes the listener
// and closes Draining so long-lived handlers can ask clients to move. Drain
// waits for handlers to return. Abort, when the shutdown context ends,
// cancels the handler context and closes every connection.
package tcp

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
	"github.com/ninepeach/go-netx/limit"
	"github.com/ninepeach/go-netx/socket"
)

// ErrClosed is returned by Serve after Close or Shutdown.
var ErrClosed = errors.New("tcp: closed")

// Conn is an accepted connection. The server owns it and closes it after
// the Handler returns.
type Conn struct {
	net.Conn
	id    uint64
	srv   *Server
	ipKey netip.Addr // valid when keyed: holds a MaxConnsPerIP slot
	keyed bool
}

// ID is unique among connections accepted by this process.
func (c *Conn) ID() uint64 { return c.id }

// Draining is closed when the server stops accepting and starts draining.
func (c *Conn) Draining() <-chan struct{} { return c.srv.draining }

// WriteBuffers writes all of v with one writev on the underlying TCP
// connection where the platform supports it. It implements
// stream.BuffersWriter: net.Buffers only uses writev on the standard
// library's own connection types, so without it a *Conn would fall back to
// one Write per buffer.
func (c *Conn) WriteBuffers(v *net.Buffers) (int64, error) { return v.WriteTo(c.Conn) }

// Handler serves one connection. ctx is cancelled only on Abort, not when
// draining starts; watch c.Draining() for that. Panics are not recovered.
type Handler func(ctx context.Context, c *Conn)

// Options configures a Server.
type Options struct {
	// Admit is consulted for every accepted connection; rejected
	// connections are closed immediately and counted. Nil admits all.
	Admit limit.Limiter
	// MaxConnsPerIP caps concurrent connections from one client address;
	// zero means no cap. Rejected connections are closed immediately and
	// counted in Stats.RejectedPerIP. The limit is local to this server:
	// behind a load balancer, hash by source IP so each address always
	// reaches the same node, and make sure the real client address arrives
	// (an L4 balancer that rewrites it would make every client one IP).
	MaxConnsPerIP int
	// IPv6PrefixBits groups IPv6 clients by prefix for MaxConnsPerIP,
	// because one client can hold a whole /64 and rotate addresses in it.
	// Zero means 64; 128 counts each address separately. IPv4 (including
	// IPv4-mapped IPv6) is always counted per address.
	IPv6PrefixBits int
	// Socket applies to the listener created by Listen; accepted
	// connections inherit its buffer sizes. NewServer ignores it.
	Socket socket.Options
}

// Stats is a point-in-time snapshot.
type Stats struct {
	Accepted      uint64
	Rejected      uint64 // refused by Admit
	RejectedPerIP uint64 // refused by MaxConnsPerIP
	Active        uint64
	Closed        uint64
	AcceptErrors  uint64 // temporary accept failures, retried with backoff
}

var lastConnID atomic.Uint64

// Server accepts TCP connections. It implements the netx Service contract.
type Server struct {
	ln      net.Listener
	handler Handler
	opts    Options

	accepted, rejected, rejectedPerIP, active, closed, acceptErrors counter.Counter
	perIP                                                           *limit.PerKey[netip.Addr]
	v6bits                                                          int

	mu       sync.Mutex
	serving  bool
	stopped  bool // listener closed
	conns    map[*Conn]struct{}
	ctx      context.Context // handler context, cancelled on Abort
	abort    context.CancelFunc
	draining chan struct{}
	done     chan struct{} // closed when Serve returns
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// Listen binds network ("tcp", "tcp4", "tcp6") at address.
func Listen(ctx context.Context, network, address string, h Handler, o Options) (*Server, error) {
	ln, err := socket.Listen(ctx, network, address, o.Socket)
	if err != nil {
		return nil, err
	}
	s, err := NewServer(ln, h, o)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	return s, nil
}

// NewServer serves an existing listener, which the server then owns.
func NewServer(ln net.Listener, h Handler, o Options) (*Server, error) {
	if ln == nil || h == nil {
		return nil, errors.New("tcp: nil listener or handler")
	}
	if o.MaxConnsPerIP < 0 {
		return nil, errors.New("tcp: MaxConnsPerIP must not be negative")
	}
	if o.IPv6PrefixBits < 0 || o.IPv6PrefixBits > 128 {
		return nil, errors.New("tcp: IPv6PrefixBits must be within 0..128")
	}
	ctx, abort := context.WithCancel(context.Background())
	s := &Server{
		ln: ln, handler: h, opts: o, conns: make(map[*Conn]struct{}),
		ctx: ctx, abort: abort, draining: make(chan struct{}), done: make(chan struct{}),
	}
	if o.MaxConnsPerIP > 0 {
		s.perIP = limit.NewPerKey[netip.Addr](o.MaxConnsPerIP)
		s.v6bits = o.IPv6PrefixBits
		if s.v6bits == 0 {
			s.v6bits = 64
		}
	}
	return s, nil
}

func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Draining is closed when the server stops accepting.
func (s *Server) Draining() <-chan struct{} { return s.draining }

// Serve accepts until Shutdown or Close, or until ctx is cancelled, which
// aborts. It returns after every handler has returned. A permanent accept
// error aborts all connections and is returned. Values in ctx are visible
// to handlers.
func (s *Server) Serve(ctx context.Context) error {
	s.mu.Lock()
	switch {
	case s.stopped:
		s.mu.Unlock()
		return ErrClosed
	case s.serving:
		s.mu.Unlock()
		return errors.New("tcp: Serve called more than once")
	}
	s.serving = true
	s.ctx, s.abort = context.WithCancel(context.WithoutCancel(ctx))
	s.mu.Unlock()
	defer close(s.done)

	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()

	err := s.acceptLoop()
	if err != nil {
		// The listener is broken: nothing new can arrive and the process
		// will be restarted, so abort instead of waiting on handlers that
		// may never finish.
		s.abortAll()
	}
	s.wg.Wait()
	s.abort()
	return err
}

func (s *Server) acceptLoop() error {
	var delay time.Duration
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			if s.isStopped() {
				return nil
			}
			var temp interface{ Temporary() bool }
			if errors.As(err, &temp) && temp.Temporary() {
				s.acceptErrors.Inc()
				delay = backoff.Next(delay)
				if !backoff.Wait(delay, s.draining) {
					return nil // stopped while backing off
				}
				continue
			}
			s.stopAccept()
			return fmt.Errorf("tcp: accept: %w", err)
		}
		delay = 0
		c := &Conn{Conn: nc, srv: s}
		if s.perIP != nil {
			if c.ipKey, c.keyed = ipKey(nc.RemoteAddr(), s.v6bits); c.keyed && !s.perIP.TryAcquire(c.ipKey) {
				c.keyed = false // holds no slot
				s.rejectedPerIP.Inc()
				_ = nc.Close()
				continue
			}
		}
		if s.opts.Admit != nil && !s.opts.Admit.TryAcquire() {
			s.releasePerIP(c)
			s.rejected.Inc()
			_ = nc.Close()
			continue
		}
		c.id = lastConnID.Add(1)
		if !s.track(c) {
			s.release(c)
			_ = nc.Close()
			return nil
		}
		s.accepted.Inc()
		s.active.Inc()
		s.wg.Add(1)
		go s.serveConn(c)
	}
}

func (s *Server) serveConn(c *Conn) {
	// Deferred in reverse: close, return admission slots, then count the
	// connection as closed, so a Closed count never runs ahead of the slots
	// it frees.
	defer s.wg.Done()
	defer s.untrack(c)
	defer s.release(c)
	defer c.Close()
	s.handler(s.ctx, c)
}

// release returns every admission slot c holds.
func (s *Server) release(c *Conn) {
	if s.opts.Admit != nil {
		s.opts.Admit.Release()
	}
	s.releasePerIP(c)
}

func (s *Server) releasePerIP(c *Conn) {
	if c.keyed {
		c.keyed = false
		s.perIP.Release(c.ipKey)
	}
}

// ipKey returns the MaxConnsPerIP key for a remote address: the IPv4
// address, or the IPv6 address masked to v6bits. Non-TCP addresses (custom
// listeners) have no key and are not limited.
func ipKey(a net.Addr, v6bits int) (netip.Addr, bool) {
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		return netip.Addr{}, false
	}
	ip, ok := netip.AddrFromSlice(ta.IP)
	if !ok {
		return netip.Addr{}, false
	}
	ip = ip.Unmap()
	if ip.Is6() && v6bits < 128 {
		p, err := ip.Prefix(v6bits)
		if err != nil {
			return netip.Addr{}, false
		}
		ip = p.Addr()
	}
	return ip, true
}

func (s *Server) track(c *Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *Server) untrack(c *Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	s.active.Dec()
	s.closed.Inc()
}

func (s *Server) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// stopAccept closes the listener and signals Draining.
func (s *Server) stopAccept() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
		_ = s.ln.Close()
		close(s.draining)
	})
}

// abortAll cancels handlers and closes every connection.
func (s *Server) abortAll() {
	s.mu.Lock()
	s.abort()
	conns := make([]*Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Conn.Close()
	}
}

// Shutdown stops accepting, waits for handlers until ctx is done, then
// aborts the rest and returns ctx.Err().
func (s *Server) Shutdown(ctx context.Context) error {
	s.stopAccept()
	s.mu.Lock()
	serving := s.serving
	s.mu.Unlock()
	if !serving {
		return nil
	}
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		s.abortAll()
		return ctx.Err()
	}
}

// Close stops accepting and aborts every connection without waiting.
func (s *Server) Close() error {
	s.stopAccept()
	s.abortAll()
	return nil
}

func (s *Server) Stats() Stats {
	return Stats{
		Accepted: s.accepted.Load(), Rejected: s.rejected.Load(), RejectedPerIP: s.rejectedPerIP.Load(),
		Active: s.active.Load(), Closed: s.closed.Load(),
		AcceptErrors: s.acceptErrors.Load(),
	}
}

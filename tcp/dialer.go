package tcp

import (
	"context"
	"net"
	"time"

	"github.com/ninepeach/go-netx/socket"
)

type DialEvent struct {
	Network    string
	Address    string
	StartedAt  time.Time
	Duration   time.Duration
	LocalAddr  net.Addr
	RemoteAddr net.Addr
	Err        error
}

// Dialer dials TCP with socket options and an optional per-attempt event.
// netx does not aggregate, score, log or retry targets.
type Dialer struct {
	inner  net.Dialer
	onDial func(DialEvent)
	err    error // options no dial can use; returned before any network work
}

type DialOptions struct {
	Timeout time.Duration
	// KeepAlive is the keep-alive period, as in net.Dialer. Socket.KeepAlive,
	// when set, takes precedence and allows idle, interval and count.
	KeepAlive time.Duration
	Socket    socket.Options
	OnDial    func(DialEvent)
}

// Validate reports whether a Dialer made from o can dial at all. With
// Socket.FastOpen.Enabled (unsupported on dialers, D-19) it returns the
// *socket.UnsupportedOptionError every DialContext would return. Call it at
// startup to reject such a configuration before the first dial.
func (o DialOptions) Validate() error { return socket.CheckDialer(o.Socket) }

// NewDialer returns a Dialer. It does not fail: when Validate would return
// an error, every DialContext returns that error at once, before resolving
// the address or opening a socket, and reports it through OnDial.
func NewDialer(opts DialOptions) *Dialer {
	base := net.Dialer{Timeout: opts.Timeout, KeepAlive: opts.KeepAlive}
	base = socket.Dialer(base, opts.Socket)
	return &Dialer{inner: base, onDial: opts.OnDial, err: opts.Validate()}
}

func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	startedAt := time.Now()
	var conn net.Conn
	err := d.err
	if err == nil {
		conn, err = d.inner.DialContext(ctx, network, address)
	}
	if d.onDial != nil {
		event := DialEvent{
			Network: network, Address: address, StartedAt: startedAt,
			Duration: time.Since(startedAt), Err: err,
		}
		if conn != nil {
			event.LocalAddr = conn.LocalAddr()
			event.RemoteAddr = conn.RemoteAddr()
		}
		d.onDial(event)
	}
	return conn, err
}

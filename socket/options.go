package socket

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"syscall"
	"time"
)

// FastOpenOptions controls TCP Fast Open on listeners. Server-side Fast
// Open only saves a round trip, so where the platform or kernel lacks it
// the listener is created without it (see Supported); any other error from
// enabling it is returned. Backlog applies on Linux only.
//
// Dialers refuse it with UnsupportedOptionError (D-19): once a TFO cookie
// is cached, a dial with Fast Open returns success without a handshake, so
// a closed or dead peer looks connected until the first Read fails, and
// the dial timeout and DialEvent no longer reflect reachability.
type FastOpenOptions struct {
	Enabled bool
	Backlog int
}

// Options contains portable socket configuration. One Options value works
// on every supported platform: performance options the platform lacks run
// without the optimisation (Supported reports which are implemented),
// while options that change behaviour fail with UnsupportedOptionError.
type Options struct {
	FastOpen FastOpenOptions
	// ReusePort lets several sockets bind the same address. Linux and
	// FreeBSD (SO_REUSEPORT_LB) spread incoming connections and datagrams
	// across them; macOS allows the binding but does not spread load, so
	// one socket receives everything. Other platforms return an
	// UnsupportedOptionError.
	ReusePort bool
	// RecvBuffer and SendBuffer set SO_RCVBUF and SO_SNDBUF in bytes when
	// positive, before bind. The size is read back: when the kernel grants
	// less (Linux caps it at net.core.rmem_max and wmem_max without
	// failing), Linux retries with SO_RCVBUFFORCE or SO_SNDBUFFORCE, which
	// needs CAP_NET_ADMIN, and if the size is still short the socket is
	// not created and a *BufferSizeError is returned. On a TCP listener
	// they are inherited by every accepted connection, and setting them
	// before listen lets TCP window scaling take them into account.
	RecvBuffer int
	SendBuffer int
	// KeepAlive configures TCP keep-alive probes. On a listener it applies
	// to every accepted connection; on a Dialer to every dialed one. The
	// zero value keeps Go's default (enabled, 15s).
	KeepAlive KeepAlive
	// Interface, when not empty, binds the socket to the named network
	// interface: its traffic leaves through that interface whatever the
	// routing table says (multi-homed hosts, keeping a proxy's own traffic
	// off a VPN). Linux uses SO_BINDTODEVICE, which also restricts what the
	// socket receives; macOS IP_BOUND_IF / IPV6_BOUND_IF; Windows
	// IP_UNICAST_IF / IPV6_UNICAST_IF, which affect only outgoing unicast
	// traffic. An unknown name fails the dial or listen. Other platforms
	// return an UnsupportedOptionError.
	Interface string
	// RoutingMark, when not zero, sets SO_MARK (Linux only), which policy
	// routing and netfilter rules can match, for example to keep a
	// transparent proxy's own connections out of its redirect rules. It
	// needs CAP_NET_ADMIN; without it the socket is not created (EPERM).
	// Other platforms return an UnsupportedOptionError.
	RoutingMark uint32
	// DontFragment sets the IP don't-fragment bit on UDP sockets, so a
	// datagram larger than the path MTU fails to send (EMSGSIZE) instead of
	// being fragmented: path MTU discovery for QUIC and media. On an IPv6
	// socket it covers IPv4-mapped traffic too where the platform allows
	// (Linux, Windows). It is refused on TCP, and returns an
	// UnsupportedOptionError on platforms without it.
	DontFragment bool
	// BindAddressNoPort, on a dialer with a LocalAddr whose port is 0,
	// defers choosing the local port from bind to connect (Linux
	// IP_BIND_ADDRESS_NO_PORT), so many outbound TCP connections from one
	// source address do not run out of ports. A performance option: other
	// platforms dial without it (Supported().BindAddressNoPort). It is
	// refused on listeners and on UDP.
	BindAddressNoPort bool
}

// KeepAlive configures TCP keep-alive probes, which let a server notice a
// peer that vanished without closing the connection (for example a phone
// that switched networks).
type KeepAlive struct {
	// Disable turns keep-alive probes off. The other fields are ignored.
	Disable bool
	// Idle is how long a connection must be idle before the first probe.
	Idle time.Duration
	// Interval is the time between probes.
	Interval time.Duration
	// Count is how many unanswered probes close the connection.
	Count int
}

// IsZero reports whether k leaves Go's default behaviour in place.
func (k KeepAlive) IsZero() bool { return k == KeepAlive{} }

// apply returns the net package's keep-alive duration and configuration.
// Zero fields in an enabled configuration fall back to Go's defaults.
func (k KeepAlive) apply() (time.Duration, net.KeepAliveConfig) {
	switch {
	case k.Disable:
		return -1, net.KeepAliveConfig{}
	case k.IsZero():
		return 0, net.KeepAliveConfig{}
	}
	return 0, net.KeepAliveConfig{Enable: true, Idle: k.Idle, Interval: k.Interval, Count: k.Count}
}

// BufferSetter is implemented by *net.UDPConn and *net.TCPConn.
type BufferSetter interface {
	SetReadBuffer(int) error
	SetWriteBuffer(int) error
}

// SetBuffers applies RecvBuffer and SendBuffer to an existing connection,
// for sockets not created through ListenConfig or Dialer. When c also
// implements syscall.Conn (as *net.UDPConn and *net.TCPConn do), the sizes
// are verified as in Options and a short size is a *BufferSizeError;
// otherwise they are only set. A nil c, including a typed nil such as a
// nil *net.UDPConn, is an error.
func SetBuffers(c BufferSetter, opts Options) error {
	if isNil(c) {
		return errors.New("socket: SetBuffers: nil connection")
	}
	if sc, ok := c.(syscall.Conn); ok && (opts.RecvBuffer > 0 || opts.SendBuffer > 0) {
		raw, err := sc.SyscallConn()
		if err != nil {
			return err
		}
		var applyErr error
		if err := raw.Control(func(fd uintptr) {
			applyErr = applyBuffers(fd, Options{RecvBuffer: opts.RecvBuffer, SendBuffer: opts.SendBuffer})
		}); err != nil {
			return err
		}
		return applyErr
	}
	if opts.RecvBuffer > 0 {
		if err := c.SetReadBuffer(opts.RecvBuffer); err != nil {
			return err
		}
	}
	if opts.SendBuffer > 0 {
		if err := c.SetWriteBuffer(opts.SendBuffer); err != nil {
			return err
		}
	}
	return nil
}

// UnsupportedOptionError reports that a requested option is unavailable:
// Platform is the operating system, "network <name>" when the option does
// not apply to that kind of socket, or "dialed connections" for Fast Open
// on a Dialer.
type UnsupportedOptionError struct {
	Option   string
	Platform string
}

func (e *UnsupportedOptionError) Error() string {
	return fmt.Sprintf("socket: %s is unsupported on %s", e.Option, e.Platform)
}

// IsUnsupported reports whether err represents an unavailable socket option.
func IsUnsupported(err error) bool {
	var target *UnsupportedOptionError
	return errors.As(err, &target)
}

// ListenConfig returns a net.ListenConfig configured with the requested
// pre-bind socket options. For TCP prefer Listen: on macOS Fast Open can
// only be enabled on a socket that is already listening, so a listener
// made from ListenConfig alone runs without it.
func ListenConfig(opts Options) net.ListenConfig {
	keepAlive, keepAliveConfig := opts.KeepAlive.apply()
	return net.ListenConfig{Control: control(opts, false), KeepAlive: keepAlive, KeepAliveConfig: keepAliveConfig}
}

// Listen creates a TCP listener with opts: ListenConfig(opts).Listen, then
// the options a platform accepts only on a listening socket (TCP Fast Open
// on macOS). tcp.Listen uses it.
func Listen(ctx context.Context, network, address string, opts Options) (net.Listener, error) {
	lc := ListenConfig(opts)
	ln, err := lc.Listen(ctx, network, address)
	if err != nil {
		return nil, err
	}
	if err := afterListen(ln, opts); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// Dialer applies outbound socket options to base. The returned value is a
// copy; the supplied dialer is not mutated. Options a dialer cannot use
// (see CheckDialer) make every dial fail; check them first with
// CheckDialer. A Control or ControlContext
// already set on base keeps working: it runs first (ControlContext when
// both are set, as in the net package), then the options are applied, all
// through the returned dialer's ControlContext.
func Dialer(base net.Dialer, opts Options) net.Dialer {
	ours := control(opts, true)
	userCtx, userCtl := base.ControlContext, base.Control
	base.Control = nil
	base.ControlContext = func(ctx context.Context, network, address string, c syscall.RawConn) error {
		var err error
		switch {
		case userCtx != nil:
			err = userCtx(ctx, network, address, c)
		case userCtl != nil:
			err = userCtl(network, address, c)
		}
		if err != nil {
			return err
		}
		return ours(network, address, c)
	}
	if !opts.KeepAlive.IsZero() {
		base.KeepAlive, base.KeepAliveConfig = opts.KeepAlive.apply()
	}
	return base
}

// CheckDialer reports, without dialing, whether opts can be used for dialed
// connections: it returns the *UnsupportedOptionError every dial would
// return, currently for FastOpen.Enabled (D-19), or nil. Call it at startup
// so a bad configuration fails there instead of on the first dial; Dialer
// itself has no error to return.
func CheckDialer(opts Options) error {
	if opts.FastOpen.Enabled {
		return &UnsupportedOptionError{Option: "TCP Fast Open", Platform: "dialed connections"}
	}
	return nil
}

func control(opts Options, outbound bool) func(string, string, syscall.RawConn) error {
	return func(network, _ string, raw syscall.RawConn) error {
		if err := checkNetwork(network, opts); err != nil {
			return err
		}
		if outbound {
			if err := CheckDialer(opts); err != nil {
				return err
			}
		} else if opts.BindAddressNoPort {
			return &UnsupportedOptionError{Option: "BindAddressNoPort", Platform: "listeners"}
		}
		if opts == (Options{}) {
			return nil
		}
		ipv6 := strings.HasSuffix(network, "6")
		var ifindex int
		if opts.Interface != "" && interfaceByIndex {
			ifi, err := net.InterfaceByName(opts.Interface)
			if err != nil {
				return fmt.Errorf("socket: interface %q: %w", opts.Interface, err)
			}
			ifindex = ifi.Index
		}
		var applyErr error
		if err := raw.Control(func(fd uintptr) {
			if applyErr = applyBuffers(fd, opts); applyErr == nil {
				if applyErr = applyOptions(fd, opts); applyErr == nil {
					applyErr = applyControl(fd, opts, ipv6, ifindex)
				}
			}
		}); err != nil {
			return err
		}
		return applyErr
	}
}

// checkNetwork refuses options that cannot apply to the network instead of
// dropping them silently: any option on a network other than TCP or UDP,
// and the TCP-only options (Fast Open, keep-alive) on UDP. This is a
// configuration error and always fails explicitly.
func checkNetwork(network string, o Options) error {
	isTCP := network == "tcp" || network == "tcp4" || network == "tcp6"
	isUDP := network == "udp" || network == "udp4" || network == "udp6"
	where := "network " + network
	switch {
	case !isTCP && !isUDP:
		if o != (Options{}) {
			return &UnsupportedOptionError{Option: "socket options", Platform: where}
		}
	case isUDP && o.FastOpen.Enabled:
		return &UnsupportedOptionError{Option: "TCP Fast Open", Platform: where}
	case isUDP && !o.KeepAlive.IsZero():
		return &UnsupportedOptionError{Option: "TCP keep-alive", Platform: where}
	case isUDP && o.BindAddressNoPort:
		return &UnsupportedOptionError{Option: "BindAddressNoPort", Platform: where}
	case isTCP && o.DontFragment:
		return &UnsupportedOptionError{Option: "DontFragment", Platform: where}
	}
	return nil
}

// isNil reports whether c is nil or an interface holding a nil pointer.
func isNil(c BufferSetter) bool {
	if c == nil {
		return true
	}
	v := reflect.ValueOf(c)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return v.IsNil()
	}
	return false
}

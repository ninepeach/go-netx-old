# Package socket — design

Overall design: [ARCHITECTURE.md](../../ARCHITECTURE.md).

## 1. Scope

| Owns | Does not own |
|---|---|
| Socket options applied before bind/connect | Server or connection lifecycles |
| Platform differences and explicit unsupported-option errors | Choosing values for a workload |
| Building `net.ListenConfig` and `net.Dialer` | Accepting, reading, writing |

`socket` imports nothing from go-netx; `tcp` and `udp` use it.

## 2. API

```go
type Options struct {
    FastOpen   FastOpenOptions // TCP listeners only; runs without it where unavailable
    ReusePort  bool            // Linux, macOS, FreeBSD
    RecvBuffer, SendBuffer int // SO_RCVBUF / SO_SNDBUF when > 0; verified
    KeepAlive  KeepAlive       // Disable, Idle, Interval, Count; zero = Go default
    Interface  string          // bind to a named interface; Linux, macOS, Windows
    RoutingMark uint32         // SO_MARK; Linux, CAP_NET_ADMIN
    DontFragment bool          // UDP: DF bit / no fragmentation
    BindAddressNoPort bool     // dialers: local port chosen at connect; Linux
}
type FastOpenOptions struct { Enabled bool; Backlog int }
func ListenConfig(Options) net.ListenConfig
func Listen(ctx, network, address string, Options) (net.Listener, error) // + post-listen options
func Dialer(base net.Dialer, Options) net.Dialer // returns a copy
func CheckDialer(Options) error                  // the error every dial would return, without dialing
func SetBuffers(BufferSetter, Options) error     // for sockets made elsewhere
func IsUnsupported(error) bool                   // *UnsupportedOptionError
func Supported() Capabilities                    // FastOpenListen, ReusePortBalanced, BindAddressNoPort
type BufferSizeError struct{ Option string; Requested, Actual int }
```

## 3. Design

**Everything before bind, with one exception.** Options are applied in the
`Control` hook of `net.ListenConfig` / `net.Dialer`, before the socket
binds or connects. The exception is Fast Open on macOS, which the kernel
refuses with EINVAL until the socket is listening (measured on macOS 26,
REG-31); `Listen` sets it right after listen(2), so a listener made from
`ListenConfig` alone runs without it. `tcp.Listen` uses `Listen`.
That is what makes them work: a TCP listener's buffer sizes and keep-alive
are inherited by accepted connections (no per-connection syscalls), and
TCP window scaling is negotiated with the larger buffer already in place.

**The caller's hooks keep working (REG-20).** `Dialer(base, opts)` runs the
hook already on `base` first — `ControlContext`, or `Control` when only
that is set, matching the net package, which ignores `Control` when
`ControlContext` is set — and then applies the options, all through the
returned dialer's `ControlContext`. Replacing `Control` would have dropped
the caller's hook, and with a `ControlContext` on `base` every option
would have been skipped silently.

**Buffer sizes are verified (D-16).** Kernels cap `SO_RCVBUF` and
`SO_SNDBUF` without failing setsockopt: Linux silently limits them to
`net.core.rmem_max` / `wmem_max` (measured: 8 MiB requested with
rmem_max = 4 MiB gives 4 MiB, error nil). After setting a size, go-netx
reads it back (on Linux, half of what getsockopt reports, because the
kernel doubles it for bookkeeping). If it is short, Linux retries once with
`SO_RCVBUFFORCE` / `SO_SNDBUFFORCE`, which work only with CAP_NET_ADMIN;
if the size is still short, the socket is not created and the caller gets
`*BufferSizeError{Option, Requested, Actual}` with a hint (raise the
sysctl, or grant the capability). `SetBuffers` verifies the same way when
the connection exposes `SyscallConn`, and returns an error for a nil or
typed-nil connection instead of panicking (REG-24). A gateway that sizes buffers for its
peak load learns at startup that the host is not configured for it,
instead of discovering it later as kernel drops. The logic is one function
over injected set/get calls, so the same code is tested on every platform.

**Keep-alive** maps to Go's own fields: `Disable` sets `KeepAlive = -1`; any
other non-zero setting becomes `net.KeepAliveConfig{Enable: true, …}`, and
fields left at zero keep Go's defaults. The zero value changes nothing
(Go's default: enabled, 15s). The listener's setting reaches accepted
connections because Go applies `ListenConfig.KeepAliveConfig` on accept.

**One configuration everywhere (D-18).** The same `Options` must run on a
development Mac and a Linux server, so the rule depends on what an option
does. Options that only affect performance run without the optimisation
where the platform or kernel lacks it: Fast Open (saves a round trip;
kernel errors meaning "unavailable" are treated as success, any other
error is returned) and ReusePort's load spreading on macOS. `Supported()`
reports what the platform implements, so a gateway can log it at startup.
Options that change behaviour fail with `*UnsupportedOptionError`: options
on non-TCP/UDP networks, TCP-only options on UDP (D-14), and ReusePort
where a second bind would fail. There is no caller-chosen ignore policy
(D-13). Where the platform can do it, the option is implemented rather
than skipped: macOS listeners get Fast Open through `TCP_FASTOPEN`
(0x105), set after listen.

**No Fast Open on dialers (D-19, REG-19).** Measured on Linux: after a TFO
cookie is cached, a Fast Open dial to a port that is no longer listening
returns success in about 100µs without a handshake, the first Write
succeeds too, and the failure appears only at Read. The dial timeout and
`tcp.DialEvent` then say nothing about reachability, and a gateway would
treat a dead upstream as up. That changes behaviour, so a Dialer with
`FastOpen.Enabled` returns `*UnsupportedOptionError` with `Platform`
`"dialed connections"` on every platform; listeners keep Fast Open.
`CheckDialer` returns that error without dialing, so a configuration can
be rejected at startup; `tcp.DialOptions.Validate` wraps it, and a
`tcp.Dialer` built from such options fails every dial at once, before
resolving the address or opening a socket (DIAL-09). `socket.Dialer`
itself returns a plain `net.Dialer`, which can only fail per dial.

**ReusePort.** Linux uses `SO_REUSEPORT`; FreeBSD uses
`SO_REUSEPORT_LB` to distribute connections and datagrams. macOS accepts
`SO_REUSEPORT` and permits multiple binds, but does not distribute load
across sockets.

**Interface, RoutingMark, DontFragment, BindAddressNoPort (D-28).** The
platform controls a proxy needs, covering what sing's `common/control`
offers for binding and marking. They are written from the operating
systems' documentation, not from sing's source (GPL-3.0). By D-18 the
first three change behaviour, so they fail where unsupported, and
BindAddressNoPort only saves ports, so it runs without where unsupported.
`Interface` is looked up before the socket is touched, so an unknown name
fails the dial or listen with the lookup error. On an IPv6 socket the IPv6
option is set; Linux and Windows apply IPv4-level options to IPv4-mapped
traffic on a dual-stack socket, so there both are set, while macOS and
FreeBSD refuse IPv4 options on an IPv6 socket and get only the IPv6 one.
Windows' `IP_UNICAST_IF` takes the index in network byte order and, unlike
`SO_BINDTODEVICE`, only steers outgoing unicast traffic. `DontFragment` is
UDP-only (TCP does path MTU discovery itself) and `BindAddressNoPort`
dialer- and TCP-only; elsewhere they are configuration errors.

## 4. Platform notes

| Option | Linux | macOS | FreeBSD | Windows |
|---|---|---|---|---|
| RecvBuffer / SendBuffer | ✓ verified; reading halved (kernel doubles); FORCE retry when capped | ✓ verified (cap: `kern.ipc.maxsockbuf`) | ✓ verified | ✓ verified |
| KeepAlive idle/interval/count | ✓ | ✓ | ✓ | ✓ |
| ReusePort | ✓ | ✓ (bind only) | ✓ (load-balanced) | unsupported error |
| FastOpen (listeners; dialers refuse it everywhere) | ✓ (kernel may disable: runs without) | ✓ (`TCP_FASTOPEN`) | runs without | runs without |
| Interface | ✓ `SO_BINDTODEVICE` | ✓ `IP_BOUND_IF` / `IPV6_BOUND_IF` | unsupported error | ✓ `IP_UNICAST_IF` / `IPV6_UNICAST_IF` (outgoing only) |
| RoutingMark | ✓ `SO_MARK` (CAP_NET_ADMIN) | unsupported error | unsupported error | unsupported error |
| DontFragment (UDP) | ✓ `IP(V6)_MTU_DISCOVER` = DO, both on dual-stack | ✓ `IP(V6)_DONTFRAG`, per family | ✓ `IP(V6)_DONTFRAG`, per family | ✓ `IP_DONTFRAGMENT` / `IPV6_DONTFRAG`, both on dual-stack |
| BindAddressNoPort (dialers) | ✓ `IP_BIND_ADDRESS_NO_PORT` | runs without | runs without | runs without |

## 5. Decisions

- One `Options` struct shared by TCP and UDP, so a gateway configures
  sockets the same way everywhere.
- D-18: performance options degrade where unavailable; behaviour options fail.
- D-19: no Fast Open on dialers.
- macOS ReusePort is accepted for configuration parity, but callers must
  not assume load balancing; use Linux or FreeBSD when per-socket load
  distribution is required.
- D-13: no caller-chosen ignore policy; `UnsupportedPolicy` and
  `UnsupportedIgnore` were removed.
- D-16: a buffer smaller than requested is an error, after one FORCE retry
  on Linux.
- D-28: sing `common/control` capabilities are implemented independently
  in `socket.Options`, not copied (sing is GPL-3.0).

## 6. Testing

SOCK-01…SOCK-35 in [docs/TESTING.md](../TESTING.md). Platform-specific
options are verified with `getsockopt` and real bind attempts. SOCK-24/25
run on the real Linux kernel; run as root and again under
`setpriv --bounding-set -net_admin` they cover both the FORCE success and
the error path.

## 7. Open items

- PROXY protocol, if gateways run behind L4 balancers that hide the source
  address (see ARCHITECTURE.md).

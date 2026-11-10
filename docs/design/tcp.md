# Package tcp — design

Overall design: [ARCHITECTURE.md](../../ARCHITECTURE.md). Comparison with
net/http and fasthttp: section 7 below.

## 1. Scope

| Owns | Does not own |
|---|---|
| Listening, the accept loop, admission, connection tracking | Framing, protocols, sessions |
| Three-phase shutdown (StopAccept → Drain → Abort) | Read/write buffers (see `stream`) |
| Connection IDs, drain signal, writev passthrough | Per-connection timeouts (the handler's) |
| Dialing with per-attempt events | Retries, target scoring |

Imports: `socket`, `limit`, `internal/backoff`, `internal/counter`.

## 2. API

```go
func Listen(ctx, network, address string, h Handler, o Options) (*Server, error)
func NewServer(ln net.Listener, h Handler, o Options) (*Server, error)
type Handler func(ctx context.Context, c *Conn) // ctx cancelled only on abort

type Options struct {
    Admit          limit.Limiter // total admission; nil = all
    MaxConnsPerIP  int           // per-address cap; 0 = none
    IPv6PrefixBits int           // IPv6 grouping for the cap; 0 = 64
    Socket         socket.Options
}
func (*Server) Serve(context.Context) error   // returns after every handler
func (*Server) Shutdown(context.Context) error
func (*Server) Close() error                  // abort now
func (*Server) Draining() <-chan struct{}
func (*Server) Stats() Stats // Accepted, Rejected, RejectedPerIP, Active, Closed, AcceptErrors

type Conn struct{ net.Conn /* … */ }
func (*Conn) ID() uint64                      // unique per process
func (*Conn) Draining() <-chan struct{}
func (*Conn) WriteBuffers(*net.Buffers) (int64, error)

func NewDialer(DialOptions) *Dialer           // Timeout, KeepAlive, Socket (no FastOpen, D-19), OnDial
func (*Dialer) DialContext(ctx, network, address string) (net.Conn, error)
var ErrClosed
```

## 3. Design

**Connection model.** One goroutine per connection on Go's netpoller. The
server holds no per-connection buffer; reading is the handler's job (D-11
explains why not an event loop; D-07 why not a worker pool).

**Accept loop.**

1. `Accept`. Temporary errors back off 5ms doubling to 1s and are counted;
   the wait ends at once when shutdown starts (REG-02). A permanent error
   stops accepting, aborts every connection and is returned by `Serve`.
2. Per-IP check (`MaxConnsPerIP`): key = IPv4 address, or IPv6 address masked
   to `IPv6PrefixBits`; IPv4-mapped addresses count as IPv4. Non-TCP remote
   addresses (custom listeners) are not keyed.
3. `Admit.TryAcquire()`. On refusal the per-IP slot is returned.
4. Track the connection; if shutdown started meanwhile, release and close.
5. Start the handler goroutine.

Refusals are counted, never logged and never answered: the transport does
not speak the protocol (D-08).

**Connection teardown** runs, in order: close the socket, return admission
slots, count the connection closed, `wg.Done`. Slots come back before
`Closed` is counted, so a `Closed` count never runs ahead of the slots it
frees.

**Shutdown.** `StopAccept` closes the listener and the `Draining` channel.
Handlers keep their context during the drain; long-lived ones select on
`Draining()` and tell clients to move. When the shutdown context ends,
`Abort` cancels the handler context and closes every tracked connection.
`Serve` returns only after all handlers have returned.

**writev passthrough.** `net.Buffers.WriteTo` uses writev only for the
standard library's own connection types; `*Conn` wraps one, so it would
fall back to one Write per buffer. `WriteBuffers` hands the vector to the
inner connection (`stream.BuffersWriter`).

## 4. Concurrency

`Serve` once per server (second call errors; after Close: `ErrClosed`).
`Shutdown` and `Close` are safe concurrently and repeatedly. Handler panics
are not recovered.

## 5. Observability

`Stats()` snapshot: Accepted, Rejected (Admit), RejectedPerIP, Active,
Closed, AcceptErrors. `DialEvent` per dial attempt: target, timing,
addresses, error.

## 6. Performance (2 vCPU, loopback)

- Accept + handler + close: about 79–89µs and 23 allocations per connection
  (`BenchmarkAccept`).
- Idle connection: about 2KB stack + under 1KB heap (D-11).

## 7. Decisions

D-07 (no worker pool), D-08 (no protocol reply on refusal), D-09 (no
per-connection context), D-10 (per-IP cap is local), D-11 (goroutine per
connection; the netpoll comparison is in ARCHITECTURE.md, "Connection
model").

### Compared with net/http and fasthttp

The TCP transport was reviewed against the two most used Go HTTP servers
(net/http as of Go 1.24.7, fasthttp `000816e`, 2026-11). Gin has no transport
of its own: `gin.Run` builds an `http.Server`, so it shares net/http's.

**The workloads differ, and that decides every item below.** fasthttp is
built for web applications: many short-lived connections and a high request
rate, so it optimises the cost of each new connection and request. go-netx
serves IM, game and media gateways: relatively few long-lived connections
that stay open, mostly idle, for minutes or hours. A fasthttp idea is
adopted when it also holds, or matters more, for long-lived connections,
and rejected when it only pays off for short ones.

| | net/http | fasthttp | go-netx |
|---|---|---|---|
| Connection model | goroutine per conn | worker pool reusing goroutines (LIFO) | goroutine per conn |
| Temporary accept errors | 5ms→1s backoff, `time.Sleep` | `Timeout` only, fixed 1s sleep | 5ms→1s backoff, interruptible |
| Permanent error detection | `Temporary()` | string match on the error text | `errors.As` plus a stopped flag |
| Concurrency cap | none | `Concurrency`, replies 503 | `Admit`, closes and counts |
| Per-IP cap | none | `MaxConnsPerIP` | `MaxConnsPerIP`, IPv6 grouped by /64 |
| TCP keepalive | default 15s, fixed | on/off and period | `socket.Options.KeepAlive`: off, idle, interval, count |
| Graceful shutdown | close listener, poll idle conns | close listener, poll every 100ms | `Draining()` channel, no polling |
| After drain timeout | conns keep running | conns keep running | conns aborted automatically |
| Idle connection memory | buffers held for life | `ReduceMemoryUsage` frees them | none held by the server |
| Error reporting | logs | logs, rate-limited | counters (`Stats`) |

#### Adopted

| fasthttp idea | Problem it solves there | For long-lived gateways | Decision |
|---|---|---|---|
| Worker pool | creating goroutines for many short connections | connections are rare events; ~3µs each is noise | rejected (D-07) |
| 503 on overload | browsers need a status code | the protocol is the gateway's | rejected (D-08) |
| `MaxConnsPerIP` | one source exhausting connections | same need on a public gateway | adopted (v1.1.0) |
| Keepalive settings | managing reused idle connections | must detect peers that vanished | adopted (v1.1.0) |
| `ReduceMemoryUsage` | idle keep-alive connections holding buffers | connections are idle most of the time: the gain is larger | adopted as `stream.Reader` |

From net/http and from go-netx's own requirements:

- **net/http's backoff curve**, but interruptible: a shutdown never waits
  out a sleep (REG-02).
- **A drain signal instead of hooks or polling**: long-lived handlers select
  on `Conn.Draining()` and tell clients to move; Serve returns through a
  channel when the last handler ends.
- **Abort after the drain timeout.** Both references return `ctx.Err()` and
  leave connections running for the caller to clean up; go-netx closes them
  and cancels handler contexts, so a timed-out shutdown leaves nothing
  behind (verified by the leak checks, RUN-16).
- **Counters, not logs**, on the accept path: a flood of rejected or failed
  accepts cannot flood the log.

#### Rejected

Recorded as design decisions D-07 to D-09 in [docs/TESTING.md](../TESTING.md).

- **Worker pool (fasthttp).** It saves goroutine creation and stack growth:
  measured about 3.5–4.2µs and 1 allocation for a new goroutine against
  about 0.6µs and none for a pooled handoff (2 vCPU, a handler with some
  stack depth), so about 3µs per connection. A full accept measures about
  79µs (`BenchmarkAccept`), so the pool would save about 4%: roughly 3% of
  a core at 10k new connections per second. This is an optimisation for
  fasthttp's workload, many short connections; with long-lived IM and game
  connections new connections are rare and it does not pay for an
  idle-worker lifecycle. Revisit only if a gateway has to serve short-lived
  connections at tens of thousands per second, with a benchmark of that
  workload.
- **Protocol replies on rejection** (fasthttp's 503). The transport does not
  speak the protocol; a gateway that wants to answer before closing does so
  in its own admission logic.
- **Per-connection contexts** (net/http's `ConnContext`). One allocation per
  connection for something gateways rarely need: shared values come from the
  Serve context, and identity from `Conn.ID()`.
- **Matching error strings, fixed sleeps, polling shutdown**: already done
  better here, as the table shows.

#### Added in v1.1.0

1. **Configurable TCP keepalive.** `socket.Options.KeepAlive` sets idle
   time, probe interval and probe count (`net.KeepAliveConfig`), or
   disables probes. On a listener it applies to every accepted connection,
   on a dialer to every dialed one; the zero value keeps Go's default
   (enabled, 15s). IM gateways use it to notice peers that vanished without
   a FIN, such as a phone switching networks.
2. **Per-IP admission, local to one server.** `tcp.Options.MaxConnsPerIP`
   caps concurrent connections per client address, checked before `Admit`;
   refusals are counted in `Stats.RejectedPerIP`, and the slot is returned
   when the connection closes or `Admit` refuses it. IPv6 clients are
   grouped by prefix (`IPv6PrefixBits`, default 64) because one client can
   rotate addresses within a /64; IPv4 and IPv4-mapped addresses count per
   address. The counter is `limit.PerKey`, which forgets keys at zero so
   memory follows current clients only.

   It is deliberately not distributed (D-10): admission runs in the accept
   loop and must stay local and non-blocking. With a load balancer that
   hashes by source IP, every address always reaches the same node and the
   local cap is the global one. Limits per user or session across nodes
   belong to the gateway. Behind an L4 load balancer, make sure the real
   client address arrives (source preservation, or a future PROXY protocol
   option); otherwise every client looks like the balancer.

## 8. Testing

TCP-01…TCP-27, DIAL-01…DIAL-08, STREAM-14, OBS-01/02/06, NF-03/13 in
[docs/TESTING.md](../TESTING.md).

## 9. Open items

- PROXY protocol for the real client address behind L4 balancers.

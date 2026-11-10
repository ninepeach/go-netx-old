# Package tcp — design

Overall design: [ARCHITECTURE.md](../../ARCHITECTURE.md). Comparison with
net/http and fasthttp: ARCHITECTURE.md, "TCP compared with…".

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
connection).

## 8. Testing

TCP-01…TCP-27, DIAL-01…DIAL-08, STREAM-14, OBS-01/02/06, NF-03/13 in
[docs/TESTING.md](../TESTING.md).

## 9. Open items

- PROXY protocol for the real client address behind L4 balancers.

## Cross-repository target-design alignment (2026-11-10)

The [canonical target design](https://github.com/ninepeach/architecture-docs/blob/main/projects/go-netx/design.md) and [phased implementation plan](https://github.com/ninepeach/architecture-docs/blob/main/projects/go-netx/implementation-steps.zh.md) describe additional planned abstractions and acceptance criteria. This document specifies the **current module contract**. Current TCP transport handles connection lifecycle and admission. It does not import flow or instantiate the canonical plan's logical read/write Flow; gateways compose these layers. Do not treat target APIs as implemented or change this module's boundaries without an explicit cross-repository design decision.

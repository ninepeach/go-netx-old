# go-netx architecture

go-netx is the shared network foundation for the IM, Game and Media
gateways. Each gateway owns its protocol, deployment, resource budget and
failure domain; go-netx owns only what all of them need and none of them
should reimplement.

**This layout is frozen.** Changes to it follow the freeze rules below. This
document is the overall design; each package has its own (see Module designs).

## Design principles

1. **Hot paths do not create goroutines, allocate or queue.** Anything that
   cannot meet this does not go into a transport.
2. **No business semantics.** No message types, sessions, rooms, users or
   authentication. If code needs to understand packet contents, it belongs
   in a gateway.
3. **Memory is preallocated when its count scales with cores, pooled when it
   scales with connections, and never allocated per packet.**
4. **Zero external dependencies.** Observability is exposed as snapshots,
   not tied to a monitoring product.

## Packages

| Package | Domain | Owns | Does not own |
|---|---|---|---|
| `netx` (root) | Runtime | Start, signals, three-phase shutdown, `Group` | Connections, protocols |
| `socket/` | Transport | Socket options, platform differences | Server lifecycles |
| `tcp/` | Transport | Listener, accept loop, `Conn` with ID, Dialer | Sessions, framing |
| `udp/` | Transport | Sockets, reader loops, datagram send/receive | Reliability, streams |
| `flow/` | Flow | Flow ID (doubles as epoch), close state machine, idle-expiring `Table` | I/O, authentication, user identity |
| `limit/` | Resource control | Admission (`Limiter`), totals (`Budget`) | Queues |
| `queue/` | Resource control | Bounded `Ring`, overflow policy, backpressure | Global budgets |
| `buf/` | Buffer management | `Pool`, power-of-two size classes (64B…64KB), ownership rules | Encoding, I/O |
| `stream/` | Stream I/O | `Reader` that grows and shrinks with pending data, `Writer` that sends a backlog with one writev | Framing, protocols |
| `internal/` | — | Syscalls, padded counters | Anything public |

Observability has no package: every component exposes `Stats()` returning a
value snapshot, and errors are exported sentinels per package (`udp.ErrClosed`,
`tcp.ErrClosed`, `queue.ErrClosed`, …), matched with `errors.Is`.

**Session is not here.** A flow is transport identity: one TCP connection or
one UDP source address, ending when the transport ends. A session is business
identity: a logged-in user that survives reconnects. Sessions belong to each
gateway.

## Module designs

This document is the overall design. Each package has its own design
document in [docs/design](docs/design): scope, API, internal design,
concurrency, ownership, performance, decisions and the checkpoints that test
it. `go test ./...` fails if a package lacks one (DOC-02).

| Package | Design |
|---|---|
| `netx` (root) | [docs/design/runtime.md](docs/design/runtime.md) |
| `socket` | [docs/design/socket.md](docs/design/socket.md) |
| `tcp` | [docs/design/tcp.md](docs/design/tcp.md) |
| `udp` | [docs/design/udp.md](docs/design/udp.md) |
| `stream` | [docs/design/stream.md](docs/design/stream.md) |
| `flow` | [docs/design/flow.md](docs/design/flow.md) |
| `limit` | [docs/design/limit.md](docs/design/limit.md) |
| `queue` | [docs/design/queue.md](docs/design/queue.md) |
| `buf` | [docs/design/buf.md](docs/design/buf.md) |
| `internal/*` | [docs/design/internal.md](docs/design/internal.md) |

## Dependency direction

```
netx (root)        → nothing
limit, queue, flow → nothing
buf                → nothing
stream             → buf
socket             → nothing
tcp                → socket, limit, internal
udp                → socket, internal
```

- Transports do not import `flow`, `queue`, `buf` or `stream`. Gateways
  compose them (for example a `stream.Reader` over a `*tcp.Conn`).
- `tcp` and `udp` never import each other. The root never imports them;
  they satisfy `netx.Service` structurally.
- No cycles, ever.

## Runtime contract

```go
type Service interface {
    Serve(context.Context) error    // blocks until fully stopped; ctx cancel = abort
    Shutdown(context.Context) error // stop accepting → drain until ctx → abort
}
```

`netx.Server` runs one service (use `netx.Group` for several):

1. A stop signal (OS signal, parent ctx, `Stop`, `Shutdown`) starts the drain.
   It does **not** cancel the service context.
2. `Service.Shutdown` runs with `ShutdownTimeout`. TCP closes its listener and
   closes `Draining()`, so long-lived handlers can tell clients to move.
3. After Shutdown returns, the service context is cancelled (abort). Serve
   must return within `AbortGrace` or the server returns `ErrAbortTimeout`.

## Transport behaviour

The transports share one shape; the details are in each package's design.

- **TCP** ([tcp.md](docs/design/tcp.md)): one goroutine per connection on
  the Go netpoller; admission (`Admit`, `MaxConnsPerIP`) at accept time,
  refusals counted, not answered; a `Draining()` signal, then abort after
  the drain timeout. The server holds no per-connection buffers.
- **UDP** ([udp.md](docs/design/udp.md)): `Readers` goroutines call the
  handler inline with preallocated buffers, no internal queue (the kernel
  receive buffer is the queue, drops counted); `ReusePort` gives each
  reader its own socket so per-flow state shards without locks; `recvmmsg`
  batches on Linux. `udp.MaxDatagram` (65000) bounds both directions.
- **Socket options** ([socket.md](docs/design/socket.md)): one `Options`
  for both; performance options degrade where a platform lacks them,
  behaviour options fail (D-18).

How the TCP transport compares with net/http and fasthttp, and why it
adopts or rejects each of their ideas, is in [tcp.md](docs/design/tcp.md).

## Connection model compared with cloudwego/netpoll

netpoll (`d63d249`, 2026-08) replaces Go's netpoller with its own epoll
loops: a poller reads incoming data into the connection's `LinkBuffer`, then
a pooled goroutine runs `OnRequest`. An idle connection therefore holds no
goroutine at all; with goroutine-per-connection it holds one blocked in
`Read`.

Measured for go-netx (9000 idle connections, small read buffer): about 2KB
of stack and under 1KB of heap per connection, so about 300MB per 100k
connections, more if handlers grow their stacks. netpoll saves most of
that, and IM's many mostly-idle connections are exactly its strength.

go-netx keeps goroutine-per-connection (D-11): at up to around 100k
connections per node the cost is acceptable, and the standard model keeps
TLS, Windows and macOS development, the `net.Conn` ecosystem, zero
dependencies and simple code. netpoll in turn is Linux/BSD only, has no TLS,
depends on ByteDance's gopkg, and requires `OnRequest` to drain all input or
it spins. A gateway that needs 500k–1M connections per node should use
netpoll (or gnet) directly rather than go-netx growing its own event loop.

Adopted from netpoll: the adaptive read buffer and gathered writes, as
`stream` ([stream.md](docs/design/stream.md)). Not adopted: its poller, its goroutine pool (D-07), and
`LinkBuffer`'s linked-chunk API, which exists because netpoll's poller reads
before a handler exists; with the handler reading for itself, one
contiguous buffer is enough.

## Ownership rules

1. **Borrowed** — data passed to a callback (`udp.Packet.Data`) is valid only
   until the callback returns. Never Release it; `buf.Pool.Copy` it to keep it.
2. **Owned** — a `*buf.Buf` from `Get`/`GetHeadroom`/`Copy` has exactly one
   owner, who Releases it exactly once. `buf.As`/`buf.With` wrap memory buf
   does not own; their Release only forgets it.
3. **Transfer** — handing a `*buf.Buf` on (for example `queue.Ring.Push`)
   transfers ownership. If the transfer fails (`Push` returns false), the
   caller still owns it.
4. **Shared** — immutable data many connections send, such as an encoded
   configuration, stays a plain `[]byte` outside the pool and goes out with
   `stream.Writer.AddShared`: nobody owns or releases it, and it must not
   change while queued.

Discard paths are where leaks happen, so `queue.Options.OnDrop` receives
every evicted or drained item. For buffers, pass `(*buf.Buf).Release`.

These rules are defined here for every package; [buf.md](docs/design/buf.md)
describes the types they apply to and how debug builds (`-tags netxdebug`)
check them: double Release panics, `buf.Outstanding()` counts unreleased
buffers, and released memory is poisoned. Tests and load tests run with it.

## Where shared structures stop

A structure goes into `queue/`, `flow/` or `limit/` if it depends only on
capacity, policy and keys. If it must understand packet contents, it stays in
the gateway: a Media jitter buffer (timestamps, sequence numbers) or a Game
send queue that coalesces position updates are gateway code.

## Freeze rules

1. **Dependency direction is one-way** as drawn above. A change that adds an
   arrow needs a written reason in this file.
2. **New code goes into an existing package first.** A new package needs a
   reason why none fits, recorded here.
3. **Exported names outside `internal/` do not change within a major
   version.**

## Out of scope until measured

- UDP send batching (`sendmmsg`, GSO): receive batching is done
  (`udp.Options.Batch`); sending waits for a measured Media need.
- io_uring: only if batching is proven to be the bottleneck.
- Reliable UDP: belongs to a protocol layer, not here.

## Documents

`ninepeach/go-netx` is the single source of truth for go-netx's design.
This file is the overall design; each package's design is in
[docs/design](docs/design) (one file per package, `go test` enforces it,
DOC-02); tests and decisions are in [docs/TESTING.md](docs/TESTING.md);
measured baselines and the tuning guide in
[docs/PERFORMANCE.md](docs/PERFORMANCE.md);
working rules in [docs/CONTRIBUTING.md](docs/CONTRIBUTING.md). A design
change is made here, in the same pull request as the code. Copies
elsewhere, such as the claude.ai Project or older design archives, are
not authoritative.

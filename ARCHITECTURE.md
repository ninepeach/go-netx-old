# go-netx architecture

go-netx is the shared network foundation for the IM, Game and Media
gateways. Each gateway owns its protocol, deployment, resource budget and
failure domain; go-netx owns only what all of them need and none of them
should reimplement.

**This layout is frozen.** Changes to it follow the rules at the end.

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
buf                → limit
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

**UDP.** `Options.Readers` goroutines each own preallocated receive buffers
and call the handler inline; on Linux each system call receives up to
`Options.Batch` (default 8) queued datagrams with `recvmmsg`, which roughly
halves receive cost under load without adding latency. With `ReusePort` each reader has its own socket
and the kernel pins each flow to one reader (`Packet.Reader`), so gateways can
shard per-flow state without locks. There is no internal queue: the kernel
receive buffer is the queue (`socket.Options.RecvBuffer`), and overflows are
reported in `Stats.KernelDrops` on Linux (the kernel reports drops with the
next queued datagram, so the count lags once traffic stops).
`udp.MaxDatagram` (65000) is the size limit in both directions; `MaxPacket`
defaults to it and may not exceed it. Datagrams larger than `MaxPacket` are
dropped and counted, never delivered truncated.

**TCP.** One goroutine per connection on the Go netpoller. `Options.Admit`
decides admission at accept time; rejections are counted, not logged. The
handler context is cancelled only on abort. Temporary accept errors back off
5ms doubling to 1s, and Close or Shutdown cuts the backoff short. The server
holds no per-connection buffers: reading is the handler's job.

## TCP compared with net/http and fasthttp

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

### Adopted

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

### Rejected

Recorded as design decisions D-07 to D-09 in docs/TESTING.md.

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

### Added in v1.1.0

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

### Stream I/O (`stream`, v1.1.0)

Two buffer behaviours from netpoll and fasthttp, built on go-netx's own
model instead of their event loops (see the netpoll comparison below):

- **`stream.Reader` holds memory only while data is pending.** It keeps a
  512B buffer while the connection is idle, moves to `buf.Pool`'s size
  classes when a frame needs more, and goes back once the data is consumed
  after a light read. `Peek`/`Discard` let a gateway decoder accumulate a
  frame across reads; where the frame ends stays the decoder's decision.
  `Max` bounds a frame (`ErrTooLarge`); a short budget gives `ErrNoBuffer`
  without losing buffered data. Measured with 4000 idle connections: about
  3.6KB less per connection than a 4KB `bufio.Reader`.
- **`stream.Writer` sends a backlog with one writev.** `Add` takes ownership
  of a `*buf.Buf`; `Flush` writes them in order and releases them all,
  written or not. A single buffer uses `Write`. Measured through a
  `*tcp.Conn`: 8 queued messages about 6× faster than one `Write` each, 32
  about 9–10×, with no allocations.

`net.Buffers` only uses writev on the standard library's own connection
types, so wrapping a `net.Conn` silently loses it. `*tcp.Conn` therefore
implements `stream.BuffersWriter`, passing writev through to the socket.
Wrappers must do the same, never unwrap: unwrapping a TLS connection would
bypass encryption.

`stream` is a package of its own (D-12): `buf` manages memory only and the
transports hold no buffers, so either would lose its single job.

### Still planned

- **PROXY protocol** support, if gateways run behind L4 load balancers that
  do not preserve the source address.

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
`stream`. Not adopted: its poller, its goroutine pool (D-07), and
`LinkBuffer`'s linked-chunk API, which exists because netpoll's poller reads
before a handler exists; with the handler reading for itself, one
contiguous buffer is enough.

## Ownership rules

1. **Borrowed** — data passed to a callback (`udp.Packet.Data`) is valid only
   until the callback returns. Never Release it; `buf.Pool.Copy` it to keep it.
2. **Owned** — a `*buf.Buf` from `Get`/`Copy` has exactly one owner, who
   Releases it exactly once.
3. **Transfer** — handing a `*buf.Buf` on (for example `queue.Ring.Push`)
   transfers ownership. If the transfer fails (`Push` returns false), the
   caller still owns it.
4. **Shared** — immutable data many connections send, such as an encoded
   configuration, stays a plain `[]byte` outside the pool and goes out with
   `stream.Writer.AddShared`: nobody owns or releases it, and it must not
   change while queued.

Discard paths are where leaks happen, so `queue.Options.OnDrop` receives
every evicted or drained item. For buffers, pass `(*buf.Buf).Release`.

Build with `-tags netxdebug` to panic on double Release and count outstanding
buffers (`buf.Outstanding()`). Tests and load tests always run with it.

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

- `recvmmsg`/`sendmmsg` batching: next step for UDP; the API does not change.
- io_uring: only if batching is proven to be the bottleneck.
- Reliable UDP: belongs to a protocol layer, not here.

## Canonical target design and cross-repository alignment

The long-term target design lives in [architecture-docs / projects/go-netx/design.md](https://github.com/ninepeach/architecture-docs/blob/main/projects/go-netx/design.md) ([Chinese](https://github.com/ninepeach/architecture-docs/blob/main/projects/go-netx/design.zh.md)); the phased implementation plan is [implementation-steps.zh.md](https://github.com/ninepeach/architecture-docs/blob/main/projects/go-netx/implementation-steps.zh.md). This file and `docs/design/` describe **current package and implementation contracts**, not automatic completion of the target design.

As of 2026-11-10, the two designs are **not fully equivalent**. In particular:

- `flow` currently models transport identity/state; the target design also describes logical read/write Flow and Session/epoch semantics. Reconcile the abstraction before claiming parity.
- `buf` uses 64B–64KB size classes, `sync.Pool`, and an **in-use** `limit.Budget`; the target's retained-pool bytes and per-class retained-count bounds are a separate, unresolved requirement.
- `limit.Budget` provides accounting, but is not itself the target's idempotent ownership-bearing `MemoryLease`.
- `queue.Ring` is a bounded generic queue, not a complete per-Flow writer queue with all waiter/ownership requirements.
- SessionManager, FrameCodec, reliability, deduplication, and epoch validation remain target capabilities unless separately implemented and tested.

**Change control:** Preserve correctness, bounded resources, ownership, and shutdown invariants. When the target design and implementation contract conflict, document and approve a design decision in both repositories before treating either side as authoritative for the disputed requirement. Documentation synchronization is not proof of implementation or test completion.

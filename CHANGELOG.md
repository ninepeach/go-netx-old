# Changelog

## v1.1.0 (unreleased)

### Added

- socket: `CheckDialer(Options)` returns, without dialing, the error every
  dial with these options would return (Fast Open on a dialer, D-19).
- tcp: `DialOptions.Validate()` checks a dialer configuration at startup.
  A `Dialer` built from options that fail it now returns that error from
  `DialContext` immediately, before resolving the address or opening a
  socket (it used to fail only after both), and still reports it through
  `OnDial`.

- flow: `TableOptions.MaxFlows` caps the number of flows; a full table's
  `GetOrOpen` returns `(nil, false)` for new keys and `Table.Refused`
  counts them. `Table.Closed` reports whether Close was called.
- stream: `Reader.Shrink()` returns a grown buffer to the pool when
  nothing is buffered, so a connection going idle right after a large
  frame holds only IdleSize. The automatic shrink in `Discard` is unchanged.
- stream: `Writer.AddShared([]byte)` queues immutable data shared by many
  connections (an encoded game configuration, a broadcast) in order with
  owned buffers, without copying it or taking ownership; `Pending` counts
  it. Steady-state `AddShared` + `Flush` allocates nothing.
- buf: `Capacity(n)` returns the capacity, and budget cost, of `Get(n)`;
  `MinSize` (64) is the smallest class.
- buf: debug builds (`-tags netxdebug`) fill released buffers with
  `Poison` (0xDB), so use after Release and unwritten bytes show up in tests.
- flow: `Flow.Touch` records activity and is now the only way `LastSeen`
  changes after creation; `Table.GetAndTouch` is the lookup-plus-Touch
  shortcut for the per-packet path. A Touch after `BeginClose`, `Close`,
  `Remove` or eviction has no effect, and a Touch racing `Sweep` either
  keeps the flow or is discarded, decided by one CAS.
- `socket.Options.KeepAlive`: TCP keep-alive idle time, probe interval and
  probe count, or `Disable`. Applies to listeners (inherited by accepted
  connections) and dialers; the zero value keeps Go's default.
- `tcp.Options.MaxConnsPerIP` and `IPv6PrefixBits`: per-client-address
  connection cap, local to one server, with IPv6 grouped by prefix
  (default /64). `tcp.Stats.RejectedPerIP` counts refusals.
- `limit.PerKey[K]`: per-key concurrency limiter that forgets keys at zero.
- `stream` package: `Reader` keeps a 512B buffer while idle and borrows
  larger ones from a `buf.Pool` only while data is pending (`Peek`,
  `Discard`, `Fill`, `Read`, `Max`, `ErrTooLarge`, `ErrNoBuffer`);
  `Writer` sends queued `*buf.Buf`s with one writev and releases them.
- `tcp.Conn.WriteBuffers`, so writev survives the `*tcp.Conn` wrapper
  (`stream.BuffersWriter`).
- `make verify` also runs the tests without `-race`, where allocation
  assertions are not skipped; `make fuzz` caps input minimization
  (`FUZZMIN`).

- udp: `Options.Batch` receives up to that many queued datagrams per system
  call with Linux `recvmmsg` (default 8 on Linux, 1 elsewhere, max 1024).
  Under load the receive cost per datagram drops from about 2.2µs to
  1.2µs (2 vCPU), with no added latency and 0 allocations; the handler
  contract is unchanged. Memory per reader becomes `Batch × MaxPacket`
  (about 520KB with the defaults); set `MaxPacket` for small datagrams.
  Where `recvmmsg` is unavailable (macOS, Windows) any `Batch` reads one
  datagram per call; `Conn.Batch()` reports the size in effect.
- socket: `Supported()` reports which performance options the platform
  implements (`FastOpenListen`, `ReusePortBalanced`).
- socket: TCP Fast Open on macOS listeners (`TCP_FASTOPEN`).
- socket: `Listen(ctx, network, address, Options)` creates a TCP listener
  and also applies options a platform accepts only on a listening socket
  (Fast Open on macOS). `tcp.Listen` uses it.

### Changed

- buf: size classes are powers of two from 64B to 64KB (11 classes)
  instead of 2KB, 16KB and 64KB, so a buffer is less than twice its
  request: a 3KB message holds 4KB instead of 16KB and a 50B packet 64B
  instead of 2KB. Get + Release is unchanged at about 9ns and 0 allocs.
  **Behaviour change:** budgets now cost `buf.Capacity(n)` per buffer;
  code that computed budgets from `Small`/`Medium` should use `Capacity`.
- buf: `Release` on a nil `*Buf` does nothing.
- flow: `Table.Close` (and `Run` on exit) closes the table for good:
  afterwards `GetOrOpen` creates nothing and returns `(nil, false)`.
  Before, flows inserted during or after Close stayed in the table with
  no sweep and no OnClose. **Behaviour change:** create a new table
  instead of reusing a closed one, and handle a nil flow from
  `GetOrOpen`. `Closed` means Close has started; Close does not wait for
  OnClose callbacks run by a concurrent Remove, Sweep or Close.
- stream: `Reader` grows through the power-of-two classes.
- flow: `Table.Get` is a pure lookup and `GetOrOpen` no longer refreshes
  an existing flow's `LastSeen`; a new flow's `LastSeen` starts at the
  table clock. **Behaviour change:** code that relied on lookups to keep
  flows from idle expiry must call `Flow.Touch` (or `GetAndTouch`) when
  data arrives. `Get` is faster as a result (112ns instead of 210ns at 1M
  flows, 2 vCPU).
- flow: `TableOptions.OnClose` now runs for every flow that leaves the
  table, also when the caller had already closed it with `Flow.Close`;
  before, such flows left without `OnClose`. **Behaviour change:** release
  a flow's resources in `OnClose` or on `Flow.Close` returning true, not
  both.
- flow: a panic in `GetOrOpen`'s `newValue` or in `Range`'s `fn` no longer
  leaves the shard locked forever; the panic propagates as before.
- netx: `Server.OnError` is also called when `OnStart` fails, and a panic
  in `OnError` is recovered and appended to the returned error instead of
  crashing `LoopContext`.
- stream: `Reader.Peek` with a negative count returns the new
  `ErrNegativeCount` instead of panicking, and `Discard` with a count of 0
  or less consumes nothing; before, a negative `Discard` moved the read
  position backwards and corrupted the Reader.
- socket: `SetBuffers` returns an error for a nil or typed-nil connection
  instead of panicking (or, with no sizes, reporting success).
- socket: `Dialer` refuses `FastOpen` with `UnsupportedOptionError`
  (D-19). With a cached TFO cookie a Fast Open dial to a closed port
  succeeded and the error appeared only at Read, so dial timeouts and
  `tcp.DialEvent` no longer reflected reachability. Listeners keep Fast
  Open. **Breaking:** remove `FastOpen` from dialer options.
- socket: `Dialer` keeps the caller's `Control` / `ControlContext` and runs
  it before applying the options. Before, `Control` was replaced, and a
  `ControlContext` on the base dialer made every option silently skipped.
- udp: when several readers share one socket (`Readers` > 1 without
  `ReusePort`), `Batch` defaults to 1 so handlers keep running in
  parallel; an explicit `Batch` is used as given.
- stream: `Reader` returns `io.ErrNoProgress` after 100 reads in a row
  that return no data and no error, instead of spinning.
- socket: remove `UnsupportedPolicy` and `UnsupportedIgnore`. One
  configuration now runs on every platform: options that only affect
  performance (Fast Open, `udp.Options.Batch`) run without the
  optimisation where unavailable, and options that change behaviour fail
  with `UnsupportedOptionError`. **Breaking:** see Migration.
- tcp: admission slots are returned before a connection is counted as
  `Closed`, so a `Closed` count never runs ahead of the slots it frees.
- socket: options that do not fit the network are refused with
  `UnsupportedOptionError` instead of being silently ignored: any option on
  a network other than TCP/UDP, and FastOpen or KeepAlive on UDP.
  **Behaviour change:** a single `Options` with TCP-only settings can no
  longer be passed to UDP listeners.
- socket: `RecvBuffer` and `SendBuffer` are read back after being set.
  When the kernel grants less (Linux caps them at `net.core.rmem_max` /
  `wmem_max` without an error), Linux retries with `SO_RCVBUFFORCE` /
  `SO_SNDBUFFORCE` (needs CAP_NET_ADMIN), and a size that is still short
  returns the new `*socket.BufferSizeError`. `SetBuffers` verifies the same
  way. **Behaviour change:** a buffer larger than the host allows now fails
  Listen instead of being silently reduced; raise the sysctl.
- socket: `ReusePort` is supported on macOS (binds, does not balance load)
  and on FreeBSD via `SO_REUSEPORT_LB` (balanced).

### Fixed

- tcp, udp: a `Shutdown` that runs before `Serve` was first called makes
  that `Serve` return nil instead of `ErrClosed`. A stop arriving during
  startup (a SIGTERM while `OnStart` runs) used to make `netx.Server`
  report `tcp: closed` / `udp: closed` through `OnError` and return it,
  turning a clean stop into a failure. `Close` before `Serve`, and any
  later `Serve`, still return `ErrClosed`.

- stream: `Writer.Flush` returns `io.ErrShortWrite` when the destination
  reports fewer bytes than queued with a nil error (which `io.Writer`
  forbids but some wrappers do). It used to return nil, so the caller
  believed a truncated or, through the per-buffer fallback, garbled
  stream had been sent.
- socket: Fast Open on macOS is set after listen(2). macOS refuses
  `TCP_FASTOPEN` with EINVAL before that, so any Listen with Fast Open
  enabled failed on a Mac.
- udp: on macOS `Listen` raises `SO_SNDBUF` to 64KB unless
  `Socket.SendBuffer` is set. The default of 9216 bytes made every
  datagram over 9216 bytes fail with EMSGSIZE.

### Migration

- Remove the `Unsupported` field from `FastOpenOptions`. Nothing else is
  needed: where Fast Open is unavailable the socket is created without it,
  as `UnsupportedIgnore` did; use `socket.Supported()` to check.

## v1.0.0

First stable release. Rewrite around the frozen package layout in
ARCHITECTURE.md; not compatible with v0.x.

Every checkpoint in docs/TESTING.md is covered except UDP-20 (IPv6 send
boundary: test written, not yet run on an IPv6 host). Coverage includes
fuzz tests (`make fuzz`), stress tests with goroutine and file-descriptor
leak checks (`make stress`) and benchmarks. The document is kept in sync
with the tests mechanically (`make verify`).

### Added

- `flow`: process-wide flow IDs that double as epochs, Open → Closing →
  Closed state machine, sharded `Table` with idle eviction and epoch-checked
  `Remove`.
- `limit`: `Limiter` with `Concurrency`, token-bucket `Rate` and `All`;
  `Budget` for total resource caps.
- `queue`: bounded `Ring[T]` with `Reject`/`DropOldest`, `OnDrop`,
  `PushWait`/`PopWait`, `Close`/`Drain` and stats.
- `buf`: `Pool` with three size classes, optional `Budget`, ownership rules,
  and `-tags netxdebug` double-release and leak checks.
- `netx.Group` to run several services under one runtime.
- `socket.Options.RecvBuffer` and `SendBuffer`, applied before bind; TCP
  listeners pass them to accepted connections.
- `udp.MaxDatagram` (65000): the size limit in both directions. `MaxPacket`
  defaults to it and may not exceed it.
- `udp.ErrNilPacket`, `udp.Stats.KernelDropsEnabled`.
- `Stats.KernelDrops` survives the kernel's 32-bit counter wraparound.
- `Stats()` snapshots on `tcp.Server`, `udp.Conn` and `queue.Ring`.

### Changed

- `udp`: reader goroutines call the handler inline with a borrowed buffer
  (`Options.Readers`, one socket per reader with `ReusePort`). No
  per-packet goroutine or allocation; `netip.AddrPort` addresses; oversized
  datagrams dropped and counted; Linux kernel drop counts.
- `tcp`: handler receives `*tcp.Conn` with `ID()` and `Draining()`; admission
  through `Options.Admit`; three-phase shutdown where the handler context is
  cancelled only on abort; `Serve` returns after all handlers.
- `netx.Server`: stop signals start the drain instead of cancelling the
  service; abort waits at most `AbortGrace`; a second signal aborts at once;
  `OnStop` gets its own `ShutdownTimeout` budget.
- Retry backoff in the TCP accept loop and UDP readers stops immediately on
  Close or Shutdown.
- `limit`: `Budget` and `Concurrency` are overflow-safe and never exceed
  their maximum, even transiently; releasing more than was acquired panics.
- `socket`: `UnsupportedIgnore` ignores only "not supported" errors; other
  setsockopt errors are returned.
- `flow`: `Sweep` advances the table clock, so hand-driven sweeps do not
  evict touched flows; touches happen under the shard lock.
- `udp`: fatal read errors close the Conn and are returned by `Serve`;
  `Reply` never panics on a nil or foreign packet.

### Removed

- `mux` package (protocol layer).
- `netx.TCPServer`, `netx.UDPServer`, `ListenAndServeTCP/UDP`,
  `NewTCPService/NewUDPService`: transports implement `netx.Service` directly.
- `udp.Client` and `Exchange`; `tcp.Dial`; `OnError`, `OnOpen`, `OnClose`
  hooks; `MaxConnections` and `MaxHandlers` (use `limit`);
  `Server.OnServe/OnShutdown` (use `ServiceFuncs`).

## v0.2.0

- Add TCP `OnOpen` and `OnClose` connection lifecycle hooks.
- Add optional TCP `DialEvent` callbacks with target, timing, addresses, and
  dial result.
- Add context-aware UDP listener creation with `udp.ListenContext`.
- Add UDP socket configuration through `udp.ListenOptions` and
  `netx.UDPServerOptions.Socket`.
- Preserve the v0.1 TCP, UDP, lifecycle, and high-level server APIs.

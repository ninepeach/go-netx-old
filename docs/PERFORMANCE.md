# go-netx performance

This file has two parts. Section 1 records measured baselines and the
machine they came from. Section 2 is a tuning guide for gateways. Every
number here was measured; none is estimated (CONTRIBUTING §2.4). To
reproduce the gateway-scale numbers, run `make baseline`
(`internal/tools/baseline`). For the micro benchmarks, run `make bench`.

Unless noted otherwise, the measurements were taken on 2026-11-10 on a
2 vCPU Intel Xeon @ 2.10GHz VM with 8GB RAM, Linux 6.18 and Go 1.24.7,
with a file-descriptor limit of 20000 and IPv4 loopback. Client and
server ran in the same process, so they shared the two CPUs. Latency
therefore covers both ends, and throughput is a lower bound for a
dedicated server.

## 1. Baselines

### 1.1 Idle TCP connections (`make baseline`, 9000 connections)

Each server handler is blocked in a read. The figures are the memory the
server side adds per connection: heap plus goroutine stack, measured
after `runtime.GC()`. Each mode runs in its own process. The numbers
varied by less than 1% across three runs.

| Handler | Heap | Stack | Total per connection | 10k connections |
|---|---|---|---|---|
| `c.Read` into a 4-byte slice | 1.3KB | 2–4KB | 3.3–5.4KB | 33–54MB |
| `stream.Reader`, idle (512B) | 1.8KB | 4.1KB | **5.9KB** | 59MB |
| `bufio.Reader` 4KB | 5.3KB | 4.1KB | 9.4KB | 94MB |
| `stream.Reader` after one 10KB frame | 18.2KB | 4.1KB | **22.3KB** | 223MB |
| … then `Reader.Shrink()` | 1.7KB | 4.1KB | **5.8KB** | 58MB |

- The goroutine stack (2KB to start, 4KB once the read path is a few calls
  deep) costs as much as all of go-netx's own state. That is the price of
  goroutine-per-connection (D-11).
- A connection that goes quiet right after a large frame keeps a pooled
  16KB buffer. 9000 such connections held 147MB of pool memory. Call
  `Shrink` when a connection goes idle (§2.2).
- Accept plus connect: 14–38k connections/s. Closing 9000 connections and
  running `Shutdown` took about 110ms.

### 1.2 TCP request/response (`make baseline`)

Length-prefixed frames echoed through `stream.Reader` + `stream.Writer`.
Each client keeps one request outstanding. Duration 5s.

| Clients | Frame | Requests/s | p50 | p99 | p99.9 |
|---|---|---|---|---|---|
| 1 | 64B | 178k | 4µs | 16µs | 55µs |
| 16 | 64B | 222k | 53µs | 191µs | 4.1ms |
| 128 | 64B | 194k | 620µs | 1.7ms | 4.8ms |
| 16 | 4KB | 174k | 66µs | 280µs | 4.2ms |

The ~4ms p99.9 appears whenever there are more runnable goroutines than
the 2 CPUs. These are scheduler and GC pauses shared by both ends, not
time spent inside go-netx.

### 1.3 A client that stops reading (`make baseline`)

One client never reads while the server pushes 256KB responses to it,
setting a 2s write deadline before each `Flush`. Sixteen other clients
run request/response at the same time.

- The other clients were unaffected: 204k req/s, p50 55µs, p99 216µs,
  in line with 16 clients without a slow one (§1.2).
- The slow client's `Flush` returned `i/o timeout` after 2.00s, and every
  pooled buffer was back (pool in use: 0B).
- `Shutdown` with a 500ms budget, while a handler was still blocked writing
  to a client that does not read, returned after 551ms with
  `context deadline exceeded`. The abort closed the connection and the
  handler returned. Pool in use afterwards: 0B.

go-netx sets no write timeout of its own. A handler that writes must set
a deadline (§2.3).

### 1.4 Micro benchmarks (`make bench`, medians of 3)

| Benchmark | Time | Allocs |
|---|---|---|
| `buf` Get + Release, any class 64B–128KB | 6–10ns | 0 |
| `buf` Get + Release, 140KB (over `Large`, not pooled; 2026-11-11) | 24µs | 2 |
| `flow` Get on `netip.AddrPort`, 1k / 100k / 1M flows | 66 / 124 / 228ns | 0 |
| `flow` GetAndTouch / GetOrOpen (existing key) | 51 / 53ns | 0 |
| `flow` Sweep, 10k / 100k / 1M flows | 0.12 / 2.7 / 77ms | 0 |
| `queue` Push + Pop | 33ns | 0 |
| `internal/counter` Inc | 24ns | 0 |
| `stream.Writer` backlog of 8 / 32 over TCP, vs one Write each | 5.8× / 11× faster | 0 |
| `udp` receive per queued datagram, 64B, Batch 1 / 8 / 32 (`BenchmarkRecv`, median of 8, 1s) | 720 / 439 / 439ns | 0 |
| `udp` receive per queued datagram, 1200B, Batch 1 / 8 / 32 (same) | 794 / 558 / 594ns | 0 |
| `udp` echo round trip, 512B | 4.6µs | 0 |
| `tcp` accept + close | 27µs | 23 |

Run-to-run noise on this VM is about ±10% for the nanosecond benchmarks
and up to ±50% for benchmarks that go through real sockets. Compare
before and after only in interleaved runs on one machine.

No comparison with netpoll or gnet was run, because this environment
cannot download modules. Their connection models differ anyway: see
ARCHITECTURE.md, "Connection model compared with cloudwego/netpoll".

## 2. Tuning guide

### 2.1 Per-connection memory

- Use `stream.Reader` instead of `bufio.Reader`. It saves about 3.5KB per
  idle connection (§1.1). Set `ReaderOptions.Max` to the largest frame your
  protocol allows: anything larger fails with `ErrTooLarge` instead of
  growing memory.
- Give `buf.Pool` a `limit.Budget`, so a burst of large frames across many
  connections fails with `ErrNoBuffer` instead of exhausting memory.
- Budget per connection at about 6KB for idle TCP, plus whatever your
  handler keeps.

### 2.2 Large frames on mostly idle connections

`Discard` shrinks back to the idle buffer on its own only after a light
read. This keeps busy connections from trading buffers back and forth.
When a protocol sends occasional large messages (a game configuration, a
history sync) and the connection then idles, call `r.Shrink()` after
handling the message, or before a long wait such as a heartbeat interval.
In §1.1 this took 9000 such connections from 22.3KB to 5.8KB each.

### 2.3 Slow and stuck clients

- Set a write deadline before `Writer.Flush`, for example
  `c.SetWriteDeadline(time.Now().Add(2 * time.Second))`. A client that
  stops reading then costs one blocked goroutine for at most that long.
  When `Flush` fails, it still releases every buffer.
- Set a read deadline, or use `socket.KeepAlive`, to find clients that
  vanished without closing.
- Give `Server.ShutdownTimeout`, or the context passed to `Shutdown`, a
  bound. Abort closes the connections that are still open, which unblocks
  handlers stuck in a write.

### 2.4 Admission

- `tcp.Options.Admit` (`limit.NewConcurrency`) caps the total number of
  connections. `MaxConnsPerIP` caps a single address, with IPv6 grouped by
  /64. Both reject before the handler runs.
- For UDP, cap the number of flows with `flow.TableOptions.MaxFlows`.
  Spoofed source addresses otherwise get one flow each, until the flows
  idle out. Watch `Table.Refused()`.

### 2.5 UDP

- On Linux, `udp.Options.Batch` (default 8) cuts the receive cost per
  queued datagram by 30–39% in §1.4 (44% in the original D-17
  measurement; the same benchmark varies that much between runs). It
  helps only when datagrams queue up: with one in flight, as in an echo,
  it changes nothing. Memory per reader is
  Batch × MaxPacket. For small-packet gateways, set `MaxPacket`.
- `Socket.RecvBuffer` is verified (D-16). On Linux, raise
  `net.core.rmem_max` or grant CAP_NET_ADMIN, or `Listen` fails.
- On macOS, `udp.Listen` raises the send buffer so that `MaxDatagram`
  sends work (REG-32). It does not batch, and `ReusePort` does not spread
  load there. Use a Mac for development, not for benchmarks.
- Watch `udp.Stats.KernelDrops`. It is available on Linux only and is
  updated when the next datagram arrives.

### 2.6 Broadcast

Encode the payload once and send it to every connection with
`Writer.AddShared`. There is no copy and no per-connection buffer, and
`AddShared` + `Flush` allocates nothing (STREAM-20).

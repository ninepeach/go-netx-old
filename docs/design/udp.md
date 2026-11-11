# Package udp — design

Overall design: [ARCHITECTURE.md](../../ARCHITECTURE.md).

## 1. Scope

| Owns | Does not own |
|---|---|
| Binding, reader loops, inline dispatch | Reliability, ordering, sessions |
| Datagram size limits in both directions | Per-flow state (see `flow`) |
| Kernel drop and truncation accounting | Queuing work for later (see `queue`) |

Imports: `socket`, `internal/sys`, `internal/backoff`, `internal/counter`.

## 2. API

```go
func Listen(ctx, network, address string, h Handler, o Options) (*Conn, error)
type Handler func(c *Conn, p *Packet) // runs inline on the reader goroutine
type Packet struct {
    Data   []byte         // borrowed: valid until the handler returns (so is *Packet)
    From   netip.AddrPort // IPv4-mapped addresses unmapped
    Reader int            // reader index; flow-sticky with ReusePort
}
type Options struct {
    Readers   int // default 1; with ReusePort, one socket each
    MaxPacket int // size of each receive buffer; default and max MaxDatagram
    Batch     int // datagrams per recvmmsg; max 1024; Linux default 8, or 1 when readers share a socket; 1 in effect elsewhere
    Socket    socket.Options
}
const MaxDatagram = 65000 // size limit for sends and receives
func (*Conn) Serve(context.Context) error
func (*Conn) Shutdown(context.Context) error
func (*Conn) Close() error
func (*Conn) WriteTo(b []byte, to netip.AddrPort) error
func (*Conn) Reply(p *Packet, b []byte) error
func (*Conn) LocalAddr() netip.AddrPort
func (*Conn) Batch() int // batch size in effect
func (*Conn) Stats() Stats
var ErrClosed, ErrTooLarge, ErrNilPacket
```

## 3. Design

**No goroutine, allocation or queue per datagram.** Each reader goroutine
owns `Batch` buffers of `MaxPacket` bytes, allocated once, and calls the
handler inline. `Packet` is reused; `From` uses `netip.AddrPort`, which
needs no allocation. Concurrency comes from `Readers`, not from goroutines
per packet.

**Batched reads (Linux, D-17).** With `Batch` > 1 a reader calls
`recvmmsg` (`internal/sys.BatchReader`), which returns up to `Batch`
datagrams already queued. It never waits for the batch to fill, so a
lone datagram is delivered as soon as it arrives; batching only helps when
datagrams queue up, which is exactly when receive cost matters. The
handler is still called once per datagram, in arrival order, and
`Packet.Data` points into that datagram's buffer, which is reused only on
the next system call, after every handler of the batch has returned. Each
slot has its own address and control buffer, so `From`, `Truncated` and
`KernelDrops` work per datagram as with one read. When several readers
share one socket (`Readers` > 1 without ReusePort) the default is 1
(REG-21): a batch would give one reader several datagrams to handle in
turn while the other readers wait on an empty queue, lowering handler
parallelism; an explicit `Batch` is used as given. `Batch: 1` keeps the
single `ReadMsgUDPAddrPort` loop. Batching only changes performance, so
on platforms without `recvmmsg` (macOS, Windows) any `Batch` runs as 1 and
the same configuration works on a development Mac (D-18); the bounds
(0…1024) are checked everywhere, and `Conn.Batch()` reports the size in
effect. Memory per reader is `Batch × MaxPacket`: about 520KB with the
defaults (8 × 65000), 16KB with `MaxPacket: 2048`, so a gateway with
small datagrams should set `MaxPacket`. Link-local IPv6 senders get their
zone from the interface name, looked up once per interface and cached per
reader.

**Readers and sockets.** With `ReusePort`, `Listen` binds one socket per
reader to the same resolved port (`:0` works); the kernel hashes each
4-tuple to one socket, so a flow always reaches the same reader and a
gateway can shard per-flow state by `Packet.Reader` without locks. Without
it, readers share one socket. On macOS `ReusePort` binds but does not spread
datagrams: one reader receives everything (D-15), so use it there only to
run the production configuration, not for throughput.

**The kernel buffer is the queue.** A slow handler stops its reader; the
socket's receive queue (`socket.Options.RecvBuffer`) fills and the kernel
drops. On Linux `SO_RXQ_OVFL` reports the cumulative drop count with each
datagram; `KernelDrops` accumulates it wrap-safe across the 32-bit counter
and ignores stale values from readers sharing a socket. The kernel attaches
the count to the next queued datagram, so it lags once traffic stops.

**Truncation.** A datagram larger than `MaxPacket` is dropped and counted,
never delivered cut short: Linux reports `MSG_TRUNC`, Windows a
`WSAEMSGSIZE` read error; both become `Truncated`.

**Read errors** (`internal/sys.ClassifyRead`): transient errors (resource
pressure, ICMP errors) back off 5ms→1s, interrupted by Close; truncation is
counted; a fatal error closes the Conn and is returned by `Serve`.

**Sending.** `WriteTo` spreads writes over sockets by destination port;
`Reply` uses the socket that received the packet. Payloads over
`MaxDatagram` return `ErrTooLarge`. A nil packet or a foreign `Reader`
index never panics. macOS refuses a datagram larger than `SO_SNDBUF`, whose
default is 9216 bytes (`net.inet.udp.maxdgram`), so on macOS `Listen` raises
`SO_SNDBUF` to 64KB unless `Socket.SendBuffer` is set; otherwise anything
over 9216 bytes failed there with EMSGSIZE while it worked on Linux (REG-32).

## 4. Concurrency and handler contract

Handlers must finish in bounded time and must not block on anything they
cannot bound: Shutdown and Close cannot interrupt a running handler, and a
handler that never returns keeps `Serve` from returning. There is no
context by design (D-03). Waiting work belongs on a gateway goroutine fed
through a bounded queue. Panics are not recovered.

`Serve` once per Conn (second call errors; after Close: `ErrClosed`). A
`Shutdown` that runs before `Serve` was ever called is a requested stop:
that first `Serve` returns nil without reading, so a stop that arrives
during startup is a clean exit for `netx.Server` (REG-34). `Close` before
`Serve` still yields `ErrClosed`, as does every later call.

## 5. Observability

`Stats()`: RecvPackets/Bytes, SendPackets/Bytes, Truncated, RecvErrors,
SendErrors, KernelDrops, KernelDropsEnabled (false means "not measured",
not "no drops"). Counters are padded per reader and per socket.

## 6. Performance (2 vCPU, Linux 6.18, Go 1.24, loopback)

Echo round trip (512B) about 10–12µs with 0 allocations (`BenchmarkEcho`),
against 32.5µs and 5 allocations in v0.2; batching does not change it,
since an echo has one datagram in flight.

Receive cost per datagram with a queue to drain (`BenchmarkRecv`: the
handler holds the reader while 256 datagrams queue, then the reader drains
them; median of 5 runs, 0 allocations in every case):

| Batch | 64B | 1200B | 64B, 2026-11-10 | 1200B, 2026-11-10 |
|---|---|---|---|---|
| 1 (v1.1 behaviour) | 2176 ns | 2373 ns | 720 ns | 794 ns |
| 8 (Linux default) | 1210 ns (−44%) | 1302 ns (−45%) | 439 ns (−39%) | 558 ns (−30%) |
| 32 | 1156 ns | 1233 ns | 439 ns | 594 ns |

The first two columns are the measurement behind D-17; the last two are
the same benchmark re-run on 2026-11-10 (median of 8, `-benchtime 1s`).
The absolute cost varies a lot between runs and VM states, and so does
the saving: 30–45% for Batch 8. Both are this queue-drain benchmark; no
end-to-end figure is claimed for batching.

Batching saves more than the bare system call (measured −34% for
recvmmsg vs recvmsg in C-like Go code) because the Go poller and
scheduler work per read is spread over the batch too. 32 adds little over
8 for its 4× memory, which is why the default is 8.

## 7. Decisions

D-01 (`Readers` not capped), D-02 (one limit, 65000, both directions), D-03
(no handler context), D-17 (recvmmsg batching on by default on Linux), D-18 (performance
options degrade where unavailable).

## 8. Testing

UDP-01…UDP-38, SYS-05/06, OBS-03…OBS-05, NF-04/08/12 in
[docs/TESTING.md](../TESTING.md). UDP-20 (IPv6) is written but has not
run on an IPv6 host yet.

## 9. Open items

- Sending in batches (GSO `UDP_SEGMENT` or sendmmsg) for media bursts to
  one peer; see the QUIC research note.
- PKTINFO, so `Reply` uses the local address the datagram was sent to on
  multi-homed hosts bound to a wildcard address.

## V1 stabilization addendum — datagram integrity and packet ownership

Keep the existing reader-goroutine/inline-handler model. Sing's packet-buffer patterns are a comparison point, not a reason to introduce a generic packet queue or a per-datagram goroutine.

### Packet contract

- `Packet` and `Packet.Data` are **borrowed**. The complete Packet (including address metadata) is valid only during its Handler invocation. A handler retaining data must copy it before returning. Do not expose a Release method for borrowed receive slots.
- A read buffer slot cannot be reused until its corresponding inline Handler has returned. With `recvmmsg`, every slot of a batch is distinct, and no subsequent read overwrites a still-running callback's bytes.
- A truncated datagram is **never delivered** as complete data. Distinguish an empty valid datagram from truncation and from a socket read error. `MaxPacket` is receive capacity, while `MaxDatagram` bounds public send/receive policy; document how oversize inbound packets are counted.
- `Reply` must use the receiving socket, and its **source/local address semantics must be honest**: on a wildcard-bound multi-homed socket, socket affinity alone does not guarantee the original destination IP. Until PKTINFO is implemented and tested, explicitly document that limitation; do not claim correct local-source selection. PKTINFO support is a separately reviewed behavior change, not an implicit part of batch optimization.
- Linux `recvmmsg` and drop counters are optional performance/observability capabilities. macOS must keep equivalent packet correctness via the single-message path. A reader's long-running Handler can delay Close/Shutdown; do not claim it is forcibly interruptible.
- Keep send batching/GSO, retransmission, ordering, fragmentation/reassembly, internal queues, and flow/session tables out of V1.

### Acceptance

Test 0/1/max/oversize payloads, truncated packets, IPv4 and IPv6 including zones, wildcard/multihomed replies (document unsupported semantics), close during blocked read, close during Handler, repeated shutdown, shared socket vs ReusePort, Linux batch sizes 1/8/32, kernel drop counter wrap and reader affinity. Compare B/op and allocs/op on hot receive paths; zero **per-datagram** allocation remains the target, verified rather than assumed. Mark IPv6 coverage unverified until run on a capable host.

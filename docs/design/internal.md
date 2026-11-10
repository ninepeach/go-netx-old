# Internal packages — design

Overall design: [ARCHITECTURE.md](../../ARCHITECTURE.md). Packages under
`internal/` can change without breaking the public API; each exists because
more than one public package needs it or because it must stay hidden.

## internal/sys — platform syscalls for the transports

| API | Purpose |
|---|---|
| `MsgTrunc` | recvmsg flag for a truncated datagram (unix; 0 elsewhere) |
| `EnableDropCount(RawConn) bool`, `DropCount(oob) (uint32, bool)`, `OOBSize` | Linux `SO_RXQ_OVFL`: enable it, parse the count from control data |
| `ClassifyRead(error) ReadErr` | Transient, Truncated or Fatal, per platform |

| `BatchReader` (`NewBatchReader`, `Read`, `Message`), `BatchSupported`, `DefaultBatch` | Linux `recvmmsg` into caller-owned buffers; elsewhere a stub and `BatchSupported = false` |

- `BatchReader` owns one `mmsghdr`, iovec, sockaddr and control slot per
  buffer, built once. `Read` runs `recvmmsg` through `RawConn.Read`, so it
  waits on the Go poller (EAGAIN returns false), retries EINTR, and is
  woken by Close; the read callback is a method value built in the
  constructor, so a call does not allocate. The address and control
  lengths are reset before every call because the kernel overwrites them.
  Sockaddrs are decoded without allocating; IPv4-mapped addresses are
  unmapped and an IPv6 scope ID becomes the interface name (the number when
  unknown), cached per reader.
- `DropCount` walks `cmsghdr` records with fixed offsets and `unsafe`, without
  allocating; it rejects truncated buffers and lengths that point outside
  the buffer. It parses kernel data, so it is fuzzed (NF-07).
- Read-error policy: on unix, `EINTR`, `EAGAIN`, `ENOBUFS`, `ENOMEM` and
  ICMP-driven errors (`ECONNREFUSED`, `EHOSTUNREACH`, `ENETUNREACH`) are
  transient; everything else is fatal. On Windows, `WSAEMSGSIZE` is
  truncation and `WSAECONNRESET`/`WSAENETRESET` (ICMP on an unconnected
  socket) are transient. The Windows policy is a pure function
  (`classifyWinsock`) compiled and tested on every platform.

Checkpoints: SYS-01…SYS-06, UDP-08, UDP-30, NF-07.

## internal/backoff — retry delay shared by TCP and UDP

`Next(d)`: 5ms first, doubling, capped at 1s (net/http's curve).
`Wait(d, stop)`: sleeps unless `stop` closes first. Both transports wait on
their shutdown channel, so Close never waits out a backoff (REG-02, REG-03).
Checkpoints: TCP-07/08, UDP-09/10.

## internal/counter — padded atomic counters

`Counter` is an `atomic.Uint64` padded to its own cache line (`Add`, `Inc`,
`Dec` for gauges, `Load`, `Store`, `Max`). Counters written by different
goroutines (per UDP reader, per socket) never share a line. Checkpoints:
SYS-04, NF-19.

## internal/leak — goroutine and file-descriptor leak check

`Take()` records goroutines and open FDs (Linux `/proc/self/fd`; -1
elsewhere); `Snapshot.Check(timeout, slack)` waits for both to return to
the baseline and reports a goroutine dump if they do not. It replaces goleak
to keep go-netx dependency-free. Used after shutdown (RUN-16) and in every
stress test. Checkpoint: NF-18.

## internal/tools/testdoc — test/document correspondence

Reads every test function's `// Checks:` directive (ignoring build tags) and
regenerates the test column of [docs/TESTING.md](../TESTING.md); its unit
test (DOC-01) fails `go test` on any mismatch, and DOC-02 requires a design
document for every package. Rules: [docs/CONTRIBUTING.md](../CONTRIBUTING.md).

## internal/tools/baseline — gateway-scale measurements

`make baseline` runs it. It measures, on one host:

- Server-side memory per idle TCP connection (heap plus stack, after GC)
  for a raw read, `stream.Reader`, `bufio.Reader`, and `stream.Reader`
  after one 10KB frame with and without `Shrink`. Each mode runs in a
  fresh process.
- Request/response throughput and latency percentiles through
  `stream.Reader` and `stream.Writer`.
- A client that stops reading, while other clients keep running.

The results, and the machine they came from, are recorded in
[docs/PERFORMANCE.md](../PERFORMANCE.md). It is a measurement tool and not
a test, so it has no checkpoints.


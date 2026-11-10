# go-netx

Minimal, high-performance TCP and UDP foundation for Go gateways: no
per-packet goroutines, no per-packet allocation, no internal queues, no
dependencies. See [ARCHITECTURE.md](ARCHITECTURE.md) for the overall design
and its frozen rules, [docs/design](docs/design) for each package's design,
and [docs/TESTING.md](docs/TESTING.md) for the test checklist.

```
netx    runtime: start, signals, stop accepting → drain → abort
tcp     accept loop, admission, Conn IDs, Dialer
udp     multi-reader datagram transport
socket  socket options (ReusePort, buffers, TCP Fast Open)
flow    flow IDs, close state machine, idle-expiring table
limit   admission limiters and budgets
queue   bounded ring with overflow policy
buf     pooled buffers with explicit ownership
stream  idle-cheap stream reader and writev writer
```

## TCP

```go
ln, err := tcp.Listen(ctx, "tcp", ":9000", func(ctx context.Context, c *tcp.Conn) {
	_, _ = io.Copy(c, c) // the server closes c when the handler returns
}, tcp.Options{
	Admit:         limit.NewConcurrency(10_000),
	MaxConnsPerIP: 20, // per client address; IPv6 grouped by /64
	Socket: socket.Options{KeepAlive: socket.KeepAlive{
		Idle: 30 * time.Second, Interval: 10 * time.Second, Count: 3,
	}},
})
if err != nil {
	log.Fatal(err)
}
log.Fatal(netx.NewServer(ln).Loop()) // SIGINT/SIGTERM → graceful shutdown
```

Long-lived handlers watch `c.Draining()` to move clients away during a
graceful shutdown; `ctx` is cancelled only when the drain times out.

## UDP

```go
conn, err := udp.Listen(ctx, "udp", ":9001", func(c *udp.Conn, p *udp.Packet) {
	_ = c.Reply(p, p.Data) // p.Data is borrowed: valid only in this call
}, udp.Options{Readers: 4, Socket: socket.Options{ReusePort: true, RecvBuffer: 8 << 20}})
// RecvBuffer is verified: on Linux it needs net.core.rmem_max >= 8 MiB
// (or CAP_NET_ADMIN), otherwise Listen returns a *socket.BufferSizeError.
if err != nil {
	log.Fatal(err)
}
log.Fatal(netx.NewServer(conn).Loop())
```

The handler runs inline on its reader. To keep data or hand it to another
goroutine, copy it into an owned buffer:

```go
b := pool.Copy(p.Data)       // owned
if !q.Push(b) {              // transfer failed: still ours
	b.Release()
}
```

## Stream I/O

```go
r := stream.NewReader(conn, pool, stream.ReaderOptions{Max: 64 << 10})
defer r.Release()
for {
	hdr, err := r.Peek(4)                // waits on a 512B buffer while idle
	if err != nil {
		return err                       // io.EOF, ErrNoBuffer, ...
	}
	size := int(binary.BigEndian.Uint32(hdr))
	frame, err := r.Peek(4 + size)       // grows from the pool only now
	if err != nil {
		return err                       // ErrTooLarge if size exceeds Max
	}
	handle(frame[4:])                    // frame is valid until the next call
	r.Discard(4 + size)                  // shrinks back once quiet
}

w := stream.NewWriter(conn)              // conn is a *tcp.Conn
for _, m := range backlog {
	w.Add(m)                             // takes ownership of each *buf.Buf
}
if _, err := w.Flush(); err != nil {     // one writev, then all released
	return err
}
```

## Several services

```go
netx.NewServer(netx.Group(tcpServer, udpConn, netx.ServiceFuncs{ServeFunc: flows.Run})).Loop()
```

## Observability

Every component has `Stats()` returning a snapshot (`udp.Stats.KernelDrops`,
`tcp.Stats.Rejected`, `queue.Stats.Dropped`, …). Export them however you like.

## Performance

UDP echo round trip on loopback (2 vCPU, Go 1.24, 512-byte payload):

| | ns/op | allocs/op |
|---|---|---|
| v0.2 | 32,500 | 5 |
| v1.0 | 10,800 | 0 |

## Examples

```sh
make examples
./build/examples/tcp-echo     # tcp-client, udp-echo, udp-client,
                              # http-server, socks5-server
```

## Verification

```sh
make check   # test, race (with and without -tags netxdebug), vet
```

Before pushing, run `make verify` (gofmt, vet, doc check, race with and
without `netxdebug`, cross-compile). Contribution and testing rules are in
[docs/CONTRIBUTING.md](docs/CONTRIBUTING.md); the per-module test matrix,
regression list and a general checklist for network libraries are in
[docs/TESTING.md](docs/TESTING.md).

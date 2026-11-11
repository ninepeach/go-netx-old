# Package bufio — V1 target design (not implemented)

> **Status: design-only.** The implementation and authoritative current API remain in [stream.md](stream.md). Step 2 evaluates consolidation, and must not create duplicate runtime implementations. This document defines the intended destination and the migration acceptance gate.

Overall architecture: [ARCHITECTURE.md](../../ARCHITECTURE.md).

## 1. Goals and non-goals

Provide a small adaptive buffered Reader and gathered-write Writer over generic `io.Reader` / `io.Writer`, backed by `buf.Pool`. Keep the mature semantics of `stream`, its 512B idle read allocation policy and configurable 64KiB default maximum. Do not implement framing, protocol parsing, TLS, transport lifecycles, an internal send queue, asynchronous I/O, or a custom event loop.

The package is independent of `tcp` and `udp`. Applications may wrap a `tcp.Conn`, a TLS connection, or any suitable Reader/Writer.

## 2. Public API freeze gate

Before coding, inventory all exported symbols in `stream` and their callers. Prefer a mechanical move retaining exported method signatures and behaviors. There is no bounded-pool API in V1 (D-24); do not introduce fallible constructors or change method signatures solely for a hypothetical bounded pool. In particular, retain `Add(b *buf.Buf)` and `AddShared(p []byte)` without error returns. Do not introduce a package named `bufio` alongside `stream` as two maintained implementations.

The name `bufio` is also the name of Go's standard package. In importing applications use an explicit alias (for example `netxbufio "github.com/ninepeach/go-netx/bufio"`) wherever both are imported; do not claim they are interchangeable.

## 3. Reader contract

- `Peek` and `Fill` expose borrowed views valid until the next operation that may modify or recycle the backing buffer, or `Release`. Document precise lifetime in the exported method comments.
- Preserve unread bytes across growth. `Max` limits logical buffered bytes, not necessarily the backing class capacity. Shrink is opportunistic and must never discard unread data.
- Respect `io.Reader` semantics: data with a non-nil error is delivered before the error is reported where required by the existing contract; avoid busy loops on repeated `(0,nil)` and report `io.ErrNoProgress` according to a documented retry bound.
- Use the existing unbounded `buf.Pool` contract. The application may separately enforce admission or memory budgets with `limit.Budget` (INT-07); a fallible bounded Reader is outside V1.
- Release returns owned backing memory exactly once; do not release borrowed external storage.

## 4. Writer contract

- `Add(b *buf.Buf)` unconditionally queues the buffer and transfers ownership; it has no normal failure path and returns no error. `AddShared(p []byte)` queues a borrowed immutable slice and also returns no error. The caller must not modify shared storage until `Flush` or `Discard` returns.
- Gather writes using `net.Buffers` where supported without bypassing wrappers or deadlines. Provide a safe ordinary `Write` fallback.
- `Flush` attempts the queued write in order and returns the acknowledged byte count and any error. On **any** write error or short write (including `(0, nil)` where bytes remain), report the error (`io.ErrShortWrite` for a nil-error short write), **release every owned buffer and clear all queued/shared references**, including unsent data. The Writer does not retry a failed batch; the caller decides whether to close the connection. This is an explicit Writer policy, not a claim that every `io.Writer` is irrecoverable.
- Validate the reported count for both `io.Writer` and `BuffersWriter`: a count outside `0..bytes offered` is invalid and must not corrupt accounting. Preserve wrapper/TLS safety; do not bypass a wrapper to obtain writev.
- `Discard` releases every queued owned buffer exactly once and clears shared references without writing. After `Flush` or `Discard`, no borrowed slice remains referenced by the Writer.
- Reader and Writer instances are not concurrent-use-safe unless a method specifically documents otherwise; the caller serializes access.

## 5. Memory budget integration

D-24 remains in force: `buf.Pool` is unbounded and the V1 migration does
not add a bounded pool. Applications may reserve from `limit.Budget`
before allocation and return the reservation after `Release` (INT-07).
When planning budgets, consider simultaneous old/new backing storage
during Reader growth. The `bufio` Reader retains the current `stream`
growth and error semantics; it does not introduce a new fallible pool
constructor or silently change `Get`.

## 6. Performance and acceptance

- Preserve existing stream unit tests and add error-injection tests for EOF with data, short writes, `(0,nil)`, mid-flush failures, writev fallback, wrappers/TLS, buffer ownership, invalid write counts, and UDP truncation behavior in `buf` tests.
- Benchmark Reader and Writer on small/large payloads with ns/op, B/op, allocs/op, throughput and peak outstanding memory; explain regressions.
- Run `go test ./...`, `go test -race ./...`, relevant `netxdebug` tests on Linux/macOS; Windows compile only. FreeBSD is not a V1 target.
- Step 2 is complete only after there is **one** maintained implementation, imports and documentation are consistent, and tests pass. Do not gate the package migration on a bounded-pool API, which is deferred.

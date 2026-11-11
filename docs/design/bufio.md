# Package bufio — V1 target design (not implemented)

> **Status: design-only.** The implementation and authoritative current API remain in [stream.md](stream.md). Step 2 evaluates consolidation, and must not create duplicate runtime implementations. This document defines the intended destination and the migration acceptance gate.

Overall architecture: [ARCHITECTURE.md](../../ARCHITECTURE.md).

## 1. Goals and non-goals

Provide a small adaptive buffered Reader and gathered-write Writer over generic `io.Reader` / `io.Writer`, backed by `buf.Pool`. Keep the mature semantics of `stream`, its 512B idle read allocation policy and configurable 64KiB default maximum. Do not implement framing, protocol parsing, TLS, transport lifecycles, an internal send queue, asynchronous I/O, or a custom event loop.

The package is independent of `tcp` and `udp`. Applications may wrap a `tcp.Conn`, a TLS connection, or any suitable Reader/Writer.

## 2. Public API freeze gate

Before coding, inventory all exported symbols in `stream` and their callers. Prefer a mechanical move retaining exported method signatures and behaviors. Any new fallible constructor or method required by bounded pools must be explicitly specified and migration-tested; do not silently alter existing semantics. Do not introduce a package named `bufio` alongside `stream` as two maintained implementations.

The name `bufio` is also the name of Go's standard package. In importing applications use an explicit alias (for example `netxbufio "github.com/ninepeach/go-netx/bufio"`) wherever both are imported; do not claim they are interchangeable.

## 3. Reader contract

- `Peek` and `Fill` expose borrowed views valid until the next operation that may modify or recycle the backing buffer, or `Release`. Document precise lifetime in the exported method comments.
- Preserve unread bytes across growth. `Max` limits logical buffered bytes, not necessarily the backing class capacity. Shrink is opportunistic and must never discard unread data.
- Respect `io.Reader` semantics: data with a non-nil error is delivered before the error is reported where required by the existing contract; avoid busy loops on repeated `(0,nil)` and report `io.ErrNoProgress` according to a documented retry bound.
- Bounded-pool acquisition failure must leave the Reader valid and its unread bytes intact. No hidden allocation fallback.
- Release returns owned backing memory exactly once; do not release borrowed external storage.

## 4. Writer contract

- `Add` transfers ownership of a `*buf.Buf` only when accepted; on failure ownership remains with the caller. `AddShared` borrows immutable storage until it has been written or discarded.
- Gather writes using `net.Buffers` where supported without bypassing wrappers or deadlines. Provide a safe ordinary `Write` fallback.
- A partial write advances only the acknowledged byte count. If `Write` returns `n>0` and an error, retain exactly the unsent suffix. If `n==0,err==nil`, stop with a no-progress error rather than spin.
- `Flush` errors must not drop unsent buffers; `Discard` releases owned buffers and clears references to shared buffers. Repeated `Flush` after partial failure cannot duplicate bytes.
- Reader and Writer instances are not concurrent-use-safe unless a method specifically documents otherwise; the caller serializes access.

## 5. Integration with buf budget

Default unbounded mode preserves fast-path behavior. Any integration with a bounded `buf.Pool` must use `Acquire`/`AcquireHeadroom` and surface `ErrPoolExhausted`. Growth, compaction and error recovery must not temporarily violate the physical reserved-byte budget. Budget pressure may prevent growth; it must never corrupt unread/unsent data.

## 6. Performance and acceptance

- Preserve existing stream unit tests and add error-injection tests for EOF with data, short writes, `(0,nil)`, mid-flush failures, writev fallback, wrappers/TLS, buffer ownership and bounded-budget failure.
- Benchmark Reader and Writer on small/large payloads with ns/op, B/op, allocs/op, throughput and peak outstanding memory; explain regressions.
- Run `go test ./...`, `go test -race ./...`, relevant `netxdebug` tests on Linux/macOS; Windows compile only. FreeBSD is not a V1 target.
- Step 2 is complete only after there is **one** maintained implementation, imports and documentation are consistent, and tests pass. Do not change the package name before the Step 1 bounded-pool API is frozen.

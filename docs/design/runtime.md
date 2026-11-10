# Package netx (runtime) — design

Overall design: [ARCHITECTURE.md](../../ARCHITECTURE.md). This document covers
the root package only: starting services, stopping them on a signal, and
the three-phase shutdown every go-netx service follows.

## 1. Scope

| Owns | Does not own |
|---|---|
| The `Service` contract, `Server` (one-shot lifecycle), `Group` | Connections, sockets, buffers |
| Signal handling, drain and abort timing | What a service does while draining |
| Turning hook panics into errors | Logging (only `OnError` is called) |

The root package imports nothing from go-netx. Transports satisfy
`Service` structurally; nothing registers with the runtime.

## 2. API

```go
type Service interface {
    Serve(context.Context) error    // blocks until fully stopped; ctx cancel = abort
    Shutdown(context.Context) error // stop accepting → drain until ctx → abort
}
type ServiceFuncs struct{ ServeFunc, ShutdownFunc func(context.Context) error }
func Group(services ...Service) Service

type Server struct {
    Service                  Service
    OnStart, OnStop          func(context.Context) error
    OnError                  func(error)
    Signals                  []os.Signal   // default SIGINT, SIGTERM
    ShutdownTimeout          time.Duration // default 30s; 0 = unbounded
}
func NewServer(Service) *Server
func (*Server) Loop() error                     // with OS signals
func (*Server) LoopContext(context.Context) error
func (*Server) Stop() error                     // start shutdown, don't wait
func (*Server) Shutdown(context.Context) error  // start shutdown and wait
func (*Server) State() State                    // new→starting→running→stopping→stopped
const StateNew, StateStarting, StateRunning, StateStopping, StateStopped // State has String()
const AbortGrace = time.Second
var ErrAlreadyRunning, ErrNotRunning, ErrAbortTimeout
```

## 3. Design

**Two contexts.** A service runs under a context that is *not* derived from
the stop signal: `context.WithoutCancel(parent)` keeps values but drops
cancellation. The stop signal (parent cancelled, `Stop`, `Shutdown`, first
OS signal) only starts the drain. The service context is cancelled later,
to abort. Cancelling one context for both would turn every graceful stop
into an abort.

**Shutdown sequence.**

1. `OnStart(runCtx)`; on error the service is shut down, `OnStop` runs, and
   the joined error goes to `OnError` and is returned (step 7).
2. `Serve(runCtx)` in a goroutine; state `running`.
3. Wait for the stop signal or for `Serve` to return.
4. State `stopping`; `Service.Shutdown(ctx)` with `ShutdownTimeout`. A second
   OS signal cancels that context at once.
5. Cancel `runCtx` (abort) and wait at most `AbortGrace` for `Serve`;
   otherwise `ErrAbortTimeout`, so a stuck service cannot hang exit.
6. `OnStop` with a fresh `ShutdownTimeout` budget, never the drain's possibly
   expired context.
7. The result joins Serve, Shutdown (except `context.Canceled`) and OnStop
   errors, is passed to `OnError`, and returned. Both exits (a failed
   `OnStart` and the normal one) go through one function, so `OnError`
   sees every final error exactly once (REG-26).

**Group.** `Serve` runs every member; the first member to fail cancels the
others (abort). `Shutdown` drains all members concurrently and joins their
errors. Members returning nil early do not affect the others.

**Signals.** `Loop` uses its own `signal.Notify` channel rather than
`signal.NotifyContext` so it can tell the first signal (drain) from the
second (abort). The kernel merges identical signals that arrive back to
back; two Ctrl-C presses a human makes are far enough apart.

## 4. Concurrency and lifecycle

`Server` is one-shot: a second `LoopContext` returns `ErrAlreadyRunning`.
`Stop` and `Shutdown` are safe from any goroutine and concurrently; outside
`starting`–`stopping` they return `ErrNotRunning`. Hook panics are
recovered by `protect` and returned as errors; a panic in `Serve` is
reported the same way. That includes `OnError` (REG-27): a reporter that
panics while logging the final error does not crash the caller; the panic
is appended to the returned error, after the original error, which stays
visible to `errors.Is`.

## 5. Decisions

- Abort is bounded (`AbortGrace`) instead of waiting forever: a service that
  ignores cancellation must not keep a process from exiting.
- `Service.Shutdown` is trusted to honour its context; it is not wrapped in
  another timeout goroutine.
- `DeadlineExceeded` from a drain that timed out is reported; `Canceled`
  caused by the abort itself is not.

## 6. Testing

Checkpoints RUN-01…RUN-18 and INT-01/02 in [docs/TESTING.md](../TESTING.md).
Leak check after shutdown: RUN-16.

## 7. Open items

None.

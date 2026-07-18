# Task 7 report — internal/transport (net.Conn port over binary channel frames)

## Status: DONE

## Files

- `internal/transport/conn.go` — the seam: `FrameSink`/`FrameSource` interfaces, `ErrClosed`.
- `internal/transport/channel.go` — `Channel`, the concrete `net.Conn` implementation; `NewChannel` constructor; read/write pump goroutines.
- `internal/transport/deadline.go` — `deadline` type (resettable one-shot alarm) and `timeoutError` (a `net.Error`).
- `internal/transport/addr.go` — `Addr`, synthetic `net.Addr`.
- `internal/transport/frame.go` — `DefaultMaxFramePayload`, `splitPayload`.
- Tests: `channel_test.go`, `frame_test.go`, `addr_test.go`.

## Structural decisions

**No separate exported "port" interface.** `conn.go` defines the *seam below* the port (`FrameSink`/`FrameSource`), not a port interface itself — `net.Conn` already is the port's contract, so a parallel `transport.Port` interface would be pure indirection. `Channel` (in `channel.go`) is the concrete type that implements `net.Conn`; `var _ net.Conn = (*Channel)(nil)` pins this.

**Frame-codec reuse (frame.go).** Did *not* re-encode the 9-byte header here. The `FrameSink.Send(payload []byte) error` / `FrameSource.Recv() ([]byte, bool)` seam already operates on de-multiplexed, single-channel payloads — kind and channelId are implicit to whatever implements the seam. The actual `[4B length][1B kind][4B channelId]` encode/decode against real stdin/stdout happens once, in whatever Phase 3b component implements `FrameSink`/`FrameSource` against a real channel — that component is expected to use `internal/ipc`'s `Encoder`/`Decoder` directly. `frame.go` only reuses `ipc.MaxFrameLength` as `DefaultMaxFramePayload` (the one fact transport actually needs from ipc) and provides `splitPayload` for chunking oversized `Write` calls.

**Frame-source/sink interface — exact shape (Phase 3b depends on this):**
```go
type FrameSink interface {
    Send(payload []byte) error
}
type FrameSource interface {
    Recv() (payload []byte, ok bool)
}
```
Both blocking, as directed. `Recv` returning `ok == false` means "closed/exhausted, no error detail" — deliberately no separate error return; `Channel.Read` surfaces that as `io.EOF`. `NewChannel(sink FrameSink, source FrameSource, local, remote net.Addr, maxPayload int) *Channel`.

**Concurrency model.** `Channel` runs a background "read pump" (pulls via `source.Recv()`, forwards to an internal channel) and "write pump" (dequeues writer requests, calls `sink.Send`). This decouples the blocking, non-cancelable `Send`/`Recv` calls from deadline/Close handling: `Read`/`Write` select between pump progress, deadline expiry, and `Close`, rather than trying to interrupt `Send`/`Recv` directly — that seam has no cancellation, by design (Phase 3b's real implementation is expected to make its own `Send`/`Recv` return once the underlying channel is torn down). One known consequence, documented in `channel.go`: if a fake/real `Send` blocks forever with nobody ever unblocking it (e.g. no reader, no close-awareness), the write-pump goroutine can leak past `Close()` — `Close()` still unblocks the *caller's* `Write`/`Read` call itself (via the direct `<-c.closed` case in their selects), it just can't force an in-flight `sink.Send`/`source.Recv()` call to return. Exercised deliberately in `TestChannel_CloseUnblocksParkedWrite` / `TestChannel_WriteDeadlineExpires`.

**Deadline semantics (deadline.go).** Reimplements the same pattern `net.Pipe` uses internally (not exported by stdlib): a `cancel` channel that's *reused* across `set()` calls as long as it hasn't fired yet (so a call already parked on `wait()`'s previously-returned channel is still affected by a new `set()`), replaced with a fresh one only once it has expired. This satisfies both required properties: currently-blocked calls see a newly-set deadline, and once expired, `wait()` keeps returning an already-closed channel (so subsequent calls fail immediately) until a future deadline is set again.

**Read/Write mechanics.** `Read` buffers a payload's unconsumed remainder (`readBuf`) across calls, so a caller requesting fewer bytes than one payload's length gets a correct short read. `Write` splits via `splitPayload(b, maxPayload)` and sends each chunk through the write pump in order, accumulating `total` bytes only for chunks fully handed off — matching `net.Conn.Write`'s partial-write contract when an error/deadline/close interrupts mid-stream.

## Tests / results

`go build ./...` — clean.
`go test ./... -race -cover`:
```
ok  xqs-plugin-vnc/internal/transport  coverage: 84.7% of statements
```
(all other packages unaffected, still green.)

Covers: round-trip write/read; read smaller than one payload (buffering across calls); write larger than `maxPayload` (verified split into ≥3 chunks, each ≤ max); `Close` idempotence; `Close` unblocking a parked `Read` and a parked `Write`; read-deadline expiry unblocking a parked `Read` with a `net.Error`/`Timeout()==true`, and the *next* call after expiry failing immediately (<200ms) rather than blocking; write-deadline expiry unblocking a parked `Write` similarly; a deadline already in the past failing immediately; `splitPayload` edge cases (empty, exact multiple, remainder, smaller-than-max, zero-max fallback); `Addr.Network()`/`String()`.

## Assumptions

- `FrameSource.Recv`'s "no error detail on close" was taken literally per the task's directed shape; if Phase 3b needs to distinguish "clean close" from "channel-bus error," that can be layered on by having its concrete implementation stash the reason and expose it separately — `Channel` doesn't need to know.
- `Close()` always returns `nil` (idempotent, no "already closed" error) since nothing in the port's contract requires otherwise and it simplifies defer-heavy call sites.
- Did not build `open.go`/`credit.go` — out of scope per the task boundary (Phase 3b).

## Status: DONE

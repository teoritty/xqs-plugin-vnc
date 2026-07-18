# Task 13 — Edge-case audit (design doc §7)

**Status: DONE**

## Row-by-row audit

| Row | Verdict | Notes |
|---|---|---|
| `session.connect` sync + 5s timeout | already-handled | `internal/session/connect.go`: responds `{"accepted":true}` synchronously, all work in `go s.orchestrate(...)`. |
| Crash-recovery re-`registerEmbed` | already-handled | Fresh `Session` per `session.connect`; `registerEmbed` re-callable (`TestRegisterEmbed_ReCallable`). No process-level cache. |
| `channel.close` idempotent no-op | already-handled | `transport.Channel.Close` uses `sync.Once`; `Teardown` tolerates already-closed channels as best-effort. |
| Закрытие сессии (host-initiated, no final write) | already-handled | `internal/session/teardown.go`'s `Teardown`/`ReportRelayEnded`: all channel-close calls are `_ = ...` best-effort, no blocking flush assumed, idempotent via `torndown` flag. |
| `embed.suspend` (уход фокуса) | already-handled | `ui/boot.js` never closes the WebSocket on `embed.suspend` (records `suspended` flag only, reserved hook). This satisfies the doc's hard requirement ("не рвать WebSocket"); Go-plugin side receives no direct RPC for this (only `session.embedActivity`, tracked in `internal/session/viewport.go`, already recorded). No teardown-on-suspend bug found. |
| **`tunnelBackpressure` / `tunnelResume`** | **gap found and fixed** | Was a real, total gap: `session.tunnelBackpressure`/`session.tunnelResume` were not registered as notification methods anywhere in `internal/session`, and `internal/relay/pump.go`'s server→browser read loop had no pause mechanism at all. Fixed — see below. |
| Кредит `embed-stream` близок к нулю | already-handled | `internal/relay/coupling.go`'s `CouplingPolicy.Grant` implements the revised (F-4-closed) policy: progressively withholds `tcp-relay` credit as `embed-stream` send-credit shrinks, reaching 0 grant at 0 remaining — matches "снижает задержку реакции", not the outdated "must never hit zero" framing. |
| Размер фрейма 64 KiB | already-handled | `transport.Channel`'s `splitPayload` / embed-stream max frame constant enforce 64 KiB chunking (per earlier tasks / F-8). Not touched. |
| Сервер закрыл TCP | already-handled | `pumpServerToBrowser` returns a non-nil, wrapped error on `io.EOF` from `tcpCh.Read`; `runPump` closes both channels unconditionally once either direction ends; `ReportRelayEnded` routes to `updateState(error)` then `Teardown` — no retry anywhere. |
| Версия браузера < 3.8 | already-handled | `internal/rfb/frontshake.go` / `version.go` fail-fast (prior task). |
| Аутентификация отклонена / reason string, no leaked network details | already-handled | `rfb.ErrSecurityRejected.Error()` surfaces only the server's reason string; `internal/relay/pump.go`'s `doHandshakes` wraps it as `"relay: VNC server handshake: %w"` with no raw `net.OpError`/IP concatenation — the plugin never touches a raw socket itself (host does the dial), so there's no network detail available to leak in the first place. |
| `SecurityResult != 0` | already-handled | Same `ErrSecurityRejected` path; no retry (`ReportRelayEnded` → `StateError`, `Teardown`, done). |
| Сервер предлагает только неподдерживаемый тип | already-handled | `rfb.ErrNoSupportedSecurityType.Error()` lists both `Offered` and `Supported` types explicitly. |
| Пустой список security types (n=0) | already-handled | `rfb.Handshake`: `list.Empty` → `ReadReasonString` → `ErrSecurityRejected{Reason: reason}`, same clean path as auth-rejected. |
| Версия сервера < 3.8 | already-handled | `ReadVersion` enforces the floor (prior task). |
| Паника в горутине → `updateState(error)` | already-handled | `orchestrate`'s `defer recover()` (prior task), plus `clearPassword()` on that path. |

## Gap fixed: `tunnelBackpressure`/`tunnelResume`

Per `docs/plugin-api.md` (`session.tunnelBackpressure` / `session.tunnelResume`, params `{"sessionId":"..."}`, JSON-RPC notifications, host → plugin) and API-FINDINGS.md F-6 (now delivered on the channel-bus embed-stream path too via `ChannelCloseNotifier`/`AttachChannelCloseNotifier`, same notification shape as the legacy path) — nothing in the plugin registered or acted on either notification, and `pumpServerToBrowser`'s read loop had no pause hook.

**Files added/changed:**
- `internal/session/backpressure.go` (new): `handleTunnelBackpressure`/`handleTunnelResume` notification handlers; `Session.setBackpressure`/`Session.WaitForReadClearance(stop <-chan struct{}) bool` — a close-and-replace gate channel guarded by `s.mu`, matching `viewport.go`'s existing state-recording convention. `WaitForReadClearance` returns `false` (never blocks forever) the instant `stop` fires, so a channel torn down mid-backpressure (session.disconnect, server-closed TCP) can't leak the pump goroutine even if `tunnelResume` never arrives.
- `internal/session/lifecycle.go`: registered `MethodSessionTunnelBackpressure`/`MethodSessionTunnelResume` consts and dispatch cases; added `backpressureGate chan struct{}` field to `Session`.
- `internal/transport/channel.go`: added `Channel.Done() <-chan struct{}` (exposes the existing internal `closed` channel) — the cancellation signal the gate's `stop` parameter needs, tied to the channel's real lifecycle rather than a separate context.
- `internal/relay/pump.go`: added `BackpressureGate` interface; `pumpServerToBrowser` now takes a `gate BackpressureGate` param and calls `gate.WaitForReadClearance(tcpCh.Done())` before every read when a gate is set; added `RunWithGate` (keeps `Run`'s existing signature/behavior — `nil` gate — untouched for existing callers/tests); `Pump.StartRelay` now passes `s` (the `*session.Session`, which implements `BackpressureGate`) into `runPump`; compile-time assertion `var _ BackpressureGate = (*session.Session)(nil)`.

**Tests added (TDD — written to prove the gap first):**
- `internal/session/session_test.go`: `TestHandleTunnelBackpressure_BlocksUntilResume`, `TestWaitForReadClearance_StopUnblocksWithoutResume`.
- `internal/relay/pump_test.go`: `TestPumpServerToBrowser_GateBlocksReadUntilCleared` (fake gate + fake channel pair; proves bytes written by the fake VNC server do not reach the browser side while the gate is withheld, and flow through immediately once cleared).

## Deferred / not applicable

None — no row required a fix beyond the one above. `session.tunnelOpen`/`tunnelUrl` discovery (F-7) remains an open, non-blocking documentation question per the design doc itself, not an edge-case-table row.

## Test results

```
go build ./...   — clean
go vet ./...     — clean
go test ./... -race -cover:
  cmd/xqs-vnc          64.4%
  internal/ipc         88.0%
  internal/lifecycle   82.9%
  internal/relay       79.6%   (up from before this task)
  internal/rfb         92.6%
  internal/session     74.4%   (up from before this task)
  internal/transport   83.5%
```

## Post-final-review fix: router registration gap

The final whole-branch review flagged an Important finding on top of this
task's work: the gate machinery built here (`internal/session/backpressure.go`,
`internal/relay/pump.go`'s gate-polling read loop) was fully implemented and
covered by unit tests, but **unreachable in the real running process**.

**What was wrong.** `cmd/xqs-vnc/wiring.go`'s `sessionMethods` map — the
lookup table `router.routeNotification`/`routeRequest` consult to decide
whether a method belongs to `session.Handler` — listed only `session.connect`,
`session.disconnect`, `session.embedViewport`, `session.embedActivity`.
`session.MethodSessionTunnelBackpressure` and `session.MethodSessionTunnelResume`
(`internal/session/lifecycle.go` lines 34-35) were never added to that map.
Since `routeNotification`'s default case silently drops any method not
present in `sessionMethods` (by design, matching "a notification has no
error channel back to the host"), a real `session.tunnelBackpressure`/
`session.tunnelResume` notification arriving from the host over the wire
was dropped before ever reaching `session.Handler.HandleRPC`, so
`handleTunnelBackpressure`/`handleTunnelResume` never ran and the relay's
gate could never actually be armed or cleared in production. The existing
unit test (`internal/session/session_test.go` ~line 400) called
`handleTunnelBackpressure`/`handleTunnelResume` directly, bypassing the
router entirely, which is why this gap stayed green through the original
task and the first review pass.

**The fix.** Added both method constants to `sessionMethods` in
`cmd/xqs-vnc/wiring.go`:

```go
var sessionMethods = map[string]bool{
	session.MethodSessionConnect:            true,
	session.MethodSessionDisconnect:         true,
	session.MethodSessionEmbedViewport:      true,
	session.MethodSessionEmbedActivity:      true,
	session.MethodSessionTunnelBackpressure: true,
	session.MethodSessionTunnelResume:       true,
}
```

**New regression test.** `cmd/xqs-vnc/backpressure_router_test.go`'s
`TestRouter_TunnelBackpressureAndResumeReachSession` drives the real
process end to end (`run()`, the real router, the real `session.Handler`,
the real `relay.Pump`) — same harness as
`TestFullProcess_InitializeActivateConnectReachesReady` in
`integration_test.go` (extended with `fakeHostProcess.tcpStream`/
`embedStream` tracking and a `notify()` helper for sending framed
notifications, since `call()` only does request/response). It sends a
framed `session.tunnelBackpressure` notification from the fake host
immediately after `session.connect` is accepted (before the relay pump
goroutine can possibly start, so there's no race against an in-flight
blocking `Read`), then writes bytes as the fake VNC server and asserts
they do **not** reach the fake browser side within 300ms — proving the
notification reached the session's gate through `routeNotification`, not
by calling the handler directly. It then sends `session.tunnelResume` and
asserts the same bytes flow through within 5s, proving the resume also
reaches the gate via the router.

**Confirmed the test actually catches the bug**: reverted the one-line
`wiring.go` fix (dropped the two new map entries back out), reran only
this test — it failed as expected:
```
backpressure_router_test.go:143: browser side received bytes while
backpressured; router did not deliver session.tunnelBackpressure to the
session gate
```
Reapplied the fix; the test passes again.

**Full-repo verification after the fix** (`go build ./...` then
`go test ./... -race -cover`): all packages pass, no regressions —
`cmd/xqs-vnc` 72.9%, `cmd/xqs-vnc-pack` 52.2%, `internal/ipc` 88.0%,
`internal/lifecycle` 82.9%, `internal/relay` 79.6%, `internal/rfb` 92.6%,
`internal/session` 74.4%, `internal/transport` 83.5%.
All packages pass, no coverage regressions.

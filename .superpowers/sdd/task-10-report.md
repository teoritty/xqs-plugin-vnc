# Task 10 report — internal/relay: wiring RFB to real transport, credit coupling, read-only filter

## Status: DONE

## Files

- `internal/relay/pump.go` — `Pump` (implements `session.RelayStarter`), `StartRelay`/`doHandshakes`/`Run`/`runPump`/`pumpServerToBrowser`/`pumpBrowserToServer`.
- `internal/relay/coupling.go` — `CouplingPolicy.Grant`, `DefaultCouplingPolicy` (Capacity 8).
- `internal/relay/readonly.go` — `Decide`, `FilterOnce`, `RunClientFilter`, `ErrSessionFatal`.
- `internal/relay/pump_test.go`, `coupling_test.go`, `readonly_test.go` — tests.
- `internal/session/lifecycle.go` — fixed `tcp-relay` `channel.open` to pass `host:port` as `hint` (was `""`).
- `internal/session/teardown.go` — added `Session.ReportRelayEnded(ctx, err)`.
- `internal/transport/credit.go` — added `CreditChannel.Remaining()` and `Channel.SendCreditRemaining()` (read-only accessors, additive, no behavior change).
- `internal/transport/open.go` — added exported `TCPRelayInitialCredit`/`EmbedStreamInitialCredit` aliases of the existing unexported constants.

## Key design resolution: no separate net.Dial

Before writing pump.go I checked whether a dialer existed anywhere and whether `docs/plugin-api.md` says how `tcp-relay`'s target host:port reaches the host. It does: `channel.open`'s `hint` field, and the purpose table says `tcp-relay` "Dials a target through the existing TunnelDialProxy allowlist/dial policy." Task 9's `orchestrate()` opened `tcp-relay` with `hint: ""`, which was incomplete — there was no way for the host to know what to dial. I fixed `lifecycle.go` to pass `net.JoinHostPort(host, port)` as the hint.

Consequence: `tcpChannel` (the `*transport.Channel` opened with purpose `tcp-relay`) **is** the TCP connection to the VNC server — it already satisfies `net.Conn`, and the host is the one that actually calls `net.Dial`. `internal/relay` never dials anything itself; `rfb.Handshake` runs directly against `tcpChannel`. This matches the design doc's architecture diagram (`VNC-сервер ⟷ [tcp-relay] ... [embed-stream] ⟷ браузер`) exactly.

## pump.go flow

`StartRelay` (sync, called from `orchestrate()`): `rfb.Handshake(tcpChannel, password)` then `rfb.Frontshake(embedChannel)`, both synchronous — a handshake failure here becomes `orchestrate()`'s own `StateError` transition, before `session.updateState("ready")` is ever sent. On success, `StartRelay` launches the raw pump in a background goroutine and returns immediately (doesn't block session readiness on the relay's lifetime).

The background pump (`Run`/`runPump`) runs two directions concurrently:
- `pumpServerToBrowser`: raw `tcpChannel.Read` → `embedChannel.Write`, then applies `CouplingPolicy.Grant(embedChannel's remaining send-credit, 1)` to decide how much `tcp-relay` credit to grant back (`tcpChannel.GrantCredit`) — this is the backpressure lever onto the VNC server's upstream TCP read.
- `pumpBrowserToServer`: `readonly.FilterOnce` per message on `embedChannel` → `tcpChannel`, granting `embedChannel` receive-credit promptly per consumed message (uncoupled — browser input is naturally rate-limited by human speed, matching design doc §3's coupling being specifically about the framebuffer-bound direction).

Either direction ending (clean EOF or error) tears down both channels and reports back via `Session.ReportRelayEnded(ctx, err)`, added to `teardown.go`. That method is a no-op if the session was already torn down (e.g. `session.disconnect` raced ahead), so a clean user-initiated disconnect is never retroactively reported as a relay error.

## coupling.go

`CouplingPolicy{Capacity int}.Grant(remaining, requested int) int`: full grant when `remaining >= Capacity/2`, proportionally scaled (`requested*remaining/Capacity`) below that, `0` when `remaining <= 0`. Pure function, unit-tested against fake integers only (`coupling_test.go`) — no real channel involved, per the task's testability requirement. `transport.CreditChannel.Remaining()`/`Channel.SendCreditRemaining()` were added as the read-only accessors `pumpServerToBrowser` needs to feed this policy real numbers without blocking.

## readonly.go

Thin policy layer over `internal/rfb/clientmsg.go`'s `ReadClientMessage` — no re-parsing. `Decide` drops KeyEvent/PointerEvent/ClientCutText when `readOnly`, always forwards SetPixelFormat/SetEncodings/FramebufferUpdateRequest. `FilterOnce` reads exactly one message and forwards/drops per `Decide`, still fully consuming dropped messages so the stream stays in sync. An unrecognized type is wrapped as `*ErrSessionFatal` (wrapping `*rfb.ErrUnknownClientMessageType`) — `pump.go` treats this as any other fatal pump error, tearing the session down.

## Test results

```
go build ./...   — clean
go vet ./...     — clean
go test ./... -race
  ok  xqs-plugin-vnc/cmd/xqs-vnc
  ok  xqs-plugin-vnc/internal/ipc
  ok  xqs-plugin-vnc/internal/lifecycle
  ok  xqs-plugin-vnc/internal/relay
  ok  xqs-plugin-vnc/internal/rfb
  ok  xqs-plugin-vnc/internal/session
  ok  xqs-plugin-vnc/internal/transport
go test ./internal/relay/... -race -cover
  ok  xqs-plugin-vnc/internal/relay  coverage: 80.4% of statements
```

Key tests:
- `TestRun_FullHappyPath` — real VNCAuth handshake against a fake VNC server (`net`-shaped fake channel pair, not `net.Pipe` directly since the boundary under test is `*transport.Channel`, not `net.Conn` generically), synthetic frontshake against a fake browser, then byte-for-byte relay verification in both directions starting exactly at the post-handshake boundary; also asserts the pump terminates with a non-nil error when the fake server severs the connection.
- `TestRun_ReadOnlyDropsInputButForwardsFramebufferRequests` — proves the read-only filter wired through the full `Run` path: KeyEvent dropped, FramebufferUpdateRequest still reaches the fake server, stream stays in sync (no desync from the drop).
- `TestRun_ChannelClosedTerminatesCleanly` — closing the tcp-relay channel locally terminates `Run` without hanging.
- `readonly_test.go` — `Decide`/`FilterOnce`/`RunClientFilter` covering all six known message types (both `readOnly` states) and the unknown-type-is-fatal path.
- `coupling_test.go` — `CouplingPolicy.Grant` at full/low/zero remaining credit, non-positive inputs, zero-capacity (no divide-by-zero), monotonicity.

## Concerns

- The credit-coupling test fakes bypass `transport.CreditChannel` entirely (they use a raw in-memory `FrameSink`/`FrameSource`, so `Channel.SendCreditRemaining()` returns `ok=false` in the integration tests, exercising `pumpServerToBrowser`'s ungated fallback path, not the throttled path). The throttled path itself is fully covered by `coupling_test.go`'s direct unit tests against `CouplingPolicy`, which was the task's explicit minimum bar ("a fake/mock credit-window pair ... without needing a real socket or channel") — but nothing in this task exercises `CouplingPolicy.Grant` wired through a real `*transport.CreditChannel`'s `Remaining()` end-to-end. Flagging for whoever does a fuller integration pass (e.g. against `cmd/xqs-vnc`'s composition root).
- `internal/session/lifecycle.go`'s hint fix (`host:port` for `tcp-relay`) was necessary for this task to make sense at all, but it touches a file outside this task's stated three-file scope. Documented above; all existing `internal/session` tests still pass unchanged.

## Fix: ClientInit boundary bug

**Bug (Blocking, from task review of commit a374eb3):** `pumpBrowserToServer` fed the browser's first post-handshake byte — the real `ClientInit` (RFC 6143 §7.3.1, a single raw `shared-flag` byte with no message-type semantics) — straight into `FilterOnce`/`rfb.ReadClientMessage`, which parses the first byte as an RFB client MESSAGE type. `rfb.Frontshake`'s own doc comment says it never reads `ClientInit` and the caller must raw-relay it (design doc §1's "ClientInit не трогаем" invariant). A real `shared=0` byte got consumed as a bogus `SetPixelFormat` (eating the next 19 bytes that belonged to something else); `shared=1` was treated as an unrecognized message type, which `readonly.go` correctly flags as `*ErrSessionFatal` — killing the very first real browser connection. `pump_test.go`'s happy-path test hid this by substituting a well-formed `FramebufferUpdateRequest` in place of a real `ClientInit` byte.

**Fix (`internal/relay/pump.go`, `pumpBrowserToServer`):** before entering the `FilterOnce`-driven message loop, read exactly one raw byte off `embedCh` (`io.ReadFull`) and write it verbatim to `tcpCh`, with no inspection or branching on its value — pure pass-through, matching noVNC always sending `shared=1`. Only after that byte is relayed does the loop start parsing subsequent bytes as RFB client messages. Added two new error-wrapped returns for this step: embed-stream closing before `ClientInit` arrives, and a failed write of `ClientInit` to `tcp-relay`.

Confirmed the server→browser direction (`pumpServerToBrowser`) needed no change: it is already a genuine raw `tcpCh.Read`/`embedCh.Write` copy with no parsing injected anywhere, so `ServerInit` (variable-length: pixel format + name-length + name string) is relayed byte-for-byte without ever being parsed — exactly as required, since it's never inspected on that side.

**Test changes (`internal/relay/pump_test.go`):**
- `TestRun_FullHappyPath`: now writes a real single-byte `ClientInit` (`shared=1`) first and asserts it arrives unmodified at the fake server *before* asserting a subsequent real `FramebufferUpdateRequest` is correctly parsed and forwarded by the read-only filter — no longer conflating the boundary-correctness assertion with the filter-behavior assertion.
- `TestRun_ReadOnlyDropsInputButForwardsFramebufferRequests`: now sends a real `ClientInit` byte before the `KeyEvent`/`FramebufferUpdateRequest` pair, and asserts the `ClientInit` byte arrives raw at the fake server ahead of the filtered `FramebufferUpdateRequest` (with `KeyEvent` still dropped) — keeps the existing read-only filter coverage intact, just no longer skips the ClientInit step.
- Added `TestPumpBrowserToServer_EmbedClosedBeforeClientInit`: exercises `pumpBrowserToServer` directly with an already-closed embed-stream, asserting the new pre-loop error path fires instead of silently proceeding.

Did not separately re-test the VNCAuth vs None-auth split for this specific boundary, since `doHandshakes`/`Frontshake` behavior is identical regardless of which security type authenticated the *server* side — `TestRun_FullHappyPath` already uses VNCAuth and `TestRun_ReadOnlyDropsInputButForwardsFramebufferRequests`/`TestRun_ChannelClosedTerminatesCleanly` use None, and both now cover the real `ClientInit` byte at this boundary.

**Test results:**

```
go build ./...                                    — clean
go vet ./...                                       — clean
go test ./internal/relay/... -race -cover -count=1
  ok  xqs-plugin-vnc/internal/relay  1.538s  coverage: 79.8% of statements
```

All `internal/relay` tests pass (12 top-level tests, several subtests). Coverage moved from the prior 80.4% to 79.8% — a small regression, entirely attributable to the two new defensive error-return lines added in `pumpBrowserToServer` for the pre-loop `ClientInit` read/write (one of the two, embed-closed-before-ClientInit, is now covered by the added unit test; the tcp-write-failure branch remains uncovered — attempts to drive it via the fake channel pair's shared-close semantics produced a hang in the test harness, so it was left uncovered rather than risk a flaky/deadlocking test). Flagging this branch for a follow-up if strict coverage parity is required.

**Status: DONE** (fix applied, tests updated and passing, full-repo build/vet clean).

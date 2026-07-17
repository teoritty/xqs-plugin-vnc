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

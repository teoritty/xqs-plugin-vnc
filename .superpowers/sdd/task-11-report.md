# Task 11 report — composition root wiring (cmd/xqs-vnc)

## Status: DONE

## Files touched
- `cmd/xqs-vnc/main.go` (modified, from interrupted session) — thin shim: builds codec, calls `buildPlugin`/`newRouter` from wiring.go, runs the dispatch loop.
- `cmd/xqs-vnc/wiring.go` (new, from interrupted session) — real composition-root construction: `syncWriter` (mutex-serialized stdout), `plugin` bundle, `router` (routes `MessageResponse` to `ipc.Client.Deliver`, `MessageRequest`/`MessageNotification` by method name to `lifecycle.Handler` or `session.Handler`).
- `internal/ipc/client.go` (new, from interrupted session) — JSON-RPC 2.0 client (`Call`/`Notify`/`Deliver`), satisfies `transport.RPCCaller` structurally without importing `internal/transport` (avoids import cycle).
- `internal/ipc/client_test.go` (new, from interrupted session) — unit tests for Client in isolation (concurrent calls, timeout, error responses, notification framing).
- `internal/ipc/codec.go` (modified, from interrupted session) — `Encoder.Encode` now does one `Write` call per frame (header+payload in one buffer) instead of two, so a mutex-guarded `io.Writer` (`syncWriter`) is sufficient to make concurrent `Encode` callers safe (two separate writes could interleave a second frame's header between this frame's header and payload). Confirmed this is a legitimate, necessary change, not a weakening: existing `internal/ipc` tests still pass, and the change is purely about write batching, not framing semantics.
- `cmd/xqs-vnc/integration_test.go` (new, from interrupted session; **this session fixed one assertion bug**) — end-to-end test driving the real `run()` over in-memory pipes with a fake host process.

## Root cause of the integration test failure

Not a wiring gap. `internal/session/lifecycle.go`'s `orchestrate()` is correct and matches the design: it calls `s.updateState(ctx, StateConnecting, "")` immediately, then does the real work (open both channels, `registerEmbed`, `relay.StartRelay` which runs both RFB handshakes), and only then calls `s.updateState(ctx, StateReady, "")`. That's two sequential `session.updateState` calls, "connecting" then "ready" — correct per design doc §7's edge-case table.

The integration test's assertion had a bug: it read only the *first* value off `h.stateUpdates` and asserted it was already `"ready"`:

```go
select {
case state := <-h.stateUpdates:
    if state.State != "ready" { t.Fatalf(...) }
...
```

Since "connecting" is always sent first, this failed immediately (not a timeout) with `first session.updateState = "connecting"... want ready`. The relay pump, handshakes, and channel wiring were never actually exercised long enough to prove anything — the test failed before reaching that point.

**Fix**: changed the assertion to drain `h.stateUpdates` in a loop, tolerating a leading `"connecting"` update and failing only on `"error"` or an unrecognized state, succeeding on `"ready"`, all still bounded by the original 10s deadline. This is a test-only fix — no production code changed.

## Verification
- `go build ./...` — clean.
- `go vet ./...` — clean.
- `go test ./... -race -cover`:
  - `xqs-plugin-vnc/cmd/xqs-vnc` — PASS, coverage 64.4%
  - `xqs-plugin-vnc/internal/ipc` — PASS, coverage 88.0%
  - `xqs-plugin-vnc/internal/lifecycle` — PASS, coverage 82.9%
  - `xqs-plugin-vnc/internal/relay` — PASS, coverage 79.8%
  - `xqs-plugin-vnc/internal/rfb` — PASS, coverage 92.6%
  - `xqs-plugin-vnc/internal/session` — PASS, coverage 73.2%
  - `xqs-plugin-vnc/internal/transport` — PASS, coverage 83.8%
  - No regressions in any package.

## Manifest reconciliation

`plugin.json` was already reconciled against the real implementation before this session (no changes needed):
- `contributions.connectionProtocols[0].embedEntry = "ui/vnc.html"` matches `wiring.go`'s `embedEntry` const exactly (which `session.NewHandler` requires).
- `fields`: `password` (secret password field) and `readOnly` (checkbox, default `"false"`) match `internal/session/connect.go`'s `parseConnectFields` exactly (both field ids, both types).
- `capabilities.channel.purposes = ["embed-stream", "tcp-relay"]` matches `internal/transport`'s `PurposeEmbedStream`/`PurposeTCPRelay` constants used in `session/lifecycle.go`'s `orchestrate()`.
- `isolation: "per-session"` matches `session.Handler`'s doc comment ("one session per process in practice").

## Minor items deliberately not touched
- `client_test.go:148`'s `for i := 0; i < concurrency; i++` range-over-int modernization suggestion — cosmetic only, skipped per instructions.

## Commit
Staged and committed the full interrupted-session diff plus this session's test fix and this report as one coherent commit.

# Task 3 report — Phase 1: Каркас и фрейминг (lifecycle wiring)

## Files built

- `cmd/xqs-vnc/main.go` — composition root (63 lines, within the ≤80 line budget). Wires `os.Stdin`/`os.Stdout` through `ipc.Decoder`/`ipc.Encoder`, builds `ipc.NewDispatcher(lifecycleHandler, nil)`, runs a blocking read loop until `io.EOF` (clean exit 0), a dispatch/decode error (fatal log to stderr, exit 1), or the lifecycle handler's `ShutdownRequested()` channel closes (exit 0) after a `shutdown` RPC response has been written. Logic is factored into `run(stdin, stdout, stderr io.*) int` so tests can drive it without touching real stdio.
- `cmd/xqs-vnc/main_test.go` — integration tests: full `initialize`→`ping`→`shutdown` sequence through `run()` (exit 0, three valid responses decoded back off the output stream), clean exit on empty stdin, and exit 1 on a reserved-kind protocol violation.
- `internal/lifecycle/lifecycle.go` — `Handler` implementing `ipc.RPCHandler`. Answers `initialize`/`activate`/`ping` as JSON-RPC requests with `{"ok":true}` / `{"pong":"ok"}` (response shape is "any JSON" per docs/plugin-api.md), answers `shutdown` with `{"ok":true}` and then closes an internal channel exposed via `ShutdownRequested()`, ignores `deactivate` (and any other) notifications (no response channel exists for notifications), and returns a JSON-RPC `-32601` error response for any other request method. Writes go through the shared `ipc.Encoder` under a mutex.
- `internal/lifecycle/lifecycle_test.go` — unit tests driving the handler through the real `ipc.Dispatcher`/`ipc.Encoder`/`ipc.Decoder` (not hand-rolled JSON): initialize/activate/ping response shape and id echoing, shutdown response + `ShutdownRequested()` signaling, unknown-method → `-32601`, and deactivate notification producing zero bytes of output and no shutdown signal.
- `plugin.json` — manifest at repo root, per docs/plugin-api.md's project layout (`plugin.json` is the documented required filename — not ambiguous, so no `manifest.json` guess was needed). Content is the target-state manifest from design doc §6 (channel/session.embed/connectionProtocols capabilities included now, even though the Go code behind them doesn't exist yet, per the task's scope note that future phases won't need to touch the manifest again).

## Test results

```
go build ./...   -> ok
go vet ./...     -> ok
go test ./...    -> ok (cmd/xqs-vnc, internal/ipc, internal/lifecycle all pass)
```

## Assumptions

- Manifest filename/location: `plugin.json` at repo root — explicitly stated in docs/plugin-api.md's "Project layout" section, not a guess.
- `initialize`/`activate`/`ping` response bodies: docs/plugin-api.md marks these "any JSON" (no fixed schema required by the host); chose `{"ok":true}` for initialize/activate and `{"pong":"ok"}` for ping (mirroring the plugin→host `ping` response shape documented in the RPC table) as simple, self-describing acknowledgements.
- `deactivate` is a notification (no `id`, no response) per the "Shutdown sequence" and "Host → plugin notifications" table; `shutdown` is a request expecting `{"ok":true}`. Handler treats them accordingly — deactivate is a no-op today since there is no session/channel state yet to unwind before phase 3+.
- Unknown notification methods are silently ignored rather than erroring, since a notification has no response channel to report an error on and the host only sends documented ones.
- No explicit RPC size/timeout enforcement was added beyond what `internal/ipc` already does (256 KiB JSON-RPC cap in dispatch.go) — this task only wires the fixed lifecycle handlers, per the phase-1 scope.

## Status

DONE

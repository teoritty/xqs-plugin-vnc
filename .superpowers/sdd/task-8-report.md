# Task 8 report — internal/transport: channel.open wiring + bidirectional credit windows

## Status: DONE

## Files

- `internal/transport/open.go` — `OpenChannel`/`CloseChannel`, `RPCCaller` seam, `channel.open`/`channel.close` param/result types, per-purpose constants.
- `internal/transport/credit.go` — `CreditChannel` (real `FrameSink`/`FrameSource` backed by credit accounting), `ErrNoCredit`, `FrameWriter`/`SharedEncoder`, `(*Channel).GrantCredit`, `Registry` (implements `ipc.ChannelDataHandler`).
- Tests: `internal/transport/credit_test.go`, `internal/transport/open_test.go`.

## Wire-format decisions and sourcing

**`channel.open`** — `docs/plugin-api.md` line 329: request `{"purpose":"...","parentSessionId":"...","hint":"..."}`, response `{"channelId":<uint32>}`. Implemented verbatim as `channelOpenParams`/`channelOpenResult`. 10 s timeout (line 332) is the caller's (`RPCCaller.Call`'s implementer's) responsibility — `open.go` doesn't hardcode a timeout, it just passes through `ctx`.

**`channel.close`** — line 330: `{"channelId":<uint32>,"reason":"...","message":"..."}`, "notification (no response)". This is why `RPCCaller` has a separate `Notify` method distinct from `Call` — `channel.close` is fire-and-forget, not request/response. `CloseChannel` always closes the local `Channel`/`CreditChannel` first, then best-effort notifies; a notify failure doesn't stop local close (line 333's 0 s grace period implies there's nothing to wait for anyway).

**`kind=0x03` credit frame payload** — line 319: "fixed 8-byte payload: `[4B channelId][4B credit]`, no subtype byte". Implemented literally: the payload carries channelId redundantly alongside the 9-byte frame header's own channelId field (which is what `internal/ipc`'s `Dispatcher` actually routes on). `Registry.HandleChannelFrame` validates the two agree and returns a `*ipc.ProtocolViolationError` on mismatch or on a payload not exactly 8 bytes — this seemed like the one place the task's "stop and ask if ambiguous" note might apply, but the doc line is unambiguous (just redundant), so I implemented it as written rather than guessing an alternate shape.

**Per-purpose initial credit** — line 352: "4 frames for `exec`/`tcp-relay`/`udp-relay`, 8 frames for `embed-stream`". This plugin only ever opens `tcp-relay`/`embed-stream` itself (`exec`/`udp-relay` are valid host-side purposes but not ones this plugin's `open.go` constructs), so `purposeParams` only recognizes those two.

**Per-purpose max frame payload** — `API-FINDINGS.md` F-8 (line 232): "явный отдельный потолок `MaxTunnelFrameSize = 64 KiB` для purpose `embed-stream`... `MaxBinaryFrameBytes` (1 MiB) остаётся общим потолком для остальных purpose (`tcp-relay`, `exec`, `udp-relay`)". Design doc §3 line 183 confirms: "потолок фрейма отдельный лимит и разный по purpose: 1 MiB для tcp-relay, 64 KiB для embed-stream". Wired as `tcpRelayMaxFramePayload = 1024*1024` / `embedStreamMaxFramePayload = 64*1024`, asserted with exact-value checks (not just "≤") in `TestOpenChannel_TCPRelay_WiresCorrectCreditAndMaxPayload` / `TestOpenChannel_EmbedStream_WiresCorrectCreditAndMaxPayload`.

## Sentinel-error convention (resolves Task 7's documented gap)

Task 7's report flagged: `FrameSink.Send`/`FrameSource.Recv` return a single opaque error/bool, unable to distinguish "blocked, no credit" from "closed" from "generic error." Resolved as:

- **`ErrClosed`** (reused from `conn.go`, not redefined) — returned by `Send`, `TrySend`, `GrantCredit` once the channel is closed. `Recv` reports closure via `ok == false` per the existing `FrameSource` contract (unchanged, as intended — `Channel.Read` already turns that into `io.EOF`).
- **`ErrNoCredit`** (new, `credit.go`) — the sentinel for "window exhausted, channel still open, would block." The blocking `Send` (satisfying `FrameSink`) never returns it — per the design doc, exhaustion must block, not fail, since sending past credit is a fail-fast protocol violation on the host side. It's surfaced by `TrySend`, a non-blocking variant not required by the `FrameSink` seam but added for callers (e.g. future coupling logic) that need to probe without parking a goroutine.
- **generic wrapped errors** (`fmt.Errorf("transport: send on channel %d: %w", ...)`) — a real `Encode` failure talking to the host, distinct from both of the above. `Send`/`TrySend` refund the consumed credit slot on this path so a live channel isn't left silently short a slot.

## Test results

```
go build ./...          — clean
go vet ./...             — clean
go test ./... -race      — all packages pass
go test ./internal/transport/... -race -cover
ok  xqs-plugin-vnc/internal/transport  coverage: 86.5% of statements
```

Concurrency-critical coverage:
- `TestCreditChannel_SendBlocksAtZeroCreditAndUnblocksOnGrant` — Send parks at 0 credit, unblocks on `receiveCreditGrant`.
- `TestCreditChannel_SendUnblocksWithErrClosedOnClose` — Send parked on exhaustion unblocks with `ErrClosed`, not a hang, when `Close` is called.
- `TestCreditChannel_TrySendReturnsErrNoCredit` / `TestCreditChannel_TrySendReturnsErrClosed` — non-blocking sentinel distinctions.
- `TestCreditChannel_SendRefundsCreditOnEncodeFailure` — generic wire error path, distinct from both sentinels, refunds credit.
- `TestCreditChannel_GrantCreditNotAutomaticOnRecv` — proves credit is NOT granted just because `Recv` was called.
- `TestCreditChannel_GrantCreditEmitsExplicitFrame` — `GrantCredit` is the only path that emits `kind=0x03`.
- `TestCreditChannel_ConcurrentSendAndGrant` — 50 concurrent `Send` + 50 concurrent `receiveCreditGrant`, run under `-race`.
- `TestRegistry_*` — routing by channelId, malformed/mismatched credit-frame rejection as `*ipc.ProtocolViolationError`, unknown-channelId rejection, and closed-channel frames as no-ops (not errors), per the 0 s `channel.close` grace period.
- `TestOpenChannel_TCPRelay_WiresCorrectCreditAndMaxPayload` / `TestOpenChannel_EmbedStream_WiresCorrectCreditAndMaxPayload` — exact-value assertions on the 4 vs 8 frame credit and 1 MiB vs 64 KiB payload ceiling, against a fake `RPCCaller`.
- `TestOpenChannel_RegistersWithRegistry` — proves `OpenChannel` actually wires the `CreditChannel` it builds into the supplied `Registry`, not a disconnected instance.
- `TestCloseChannel_SendsNotificationAndClosesLocally` / `TestCloseChannel_StillClosesLocallyIfNotifyFails` — idempotence and best-effort-notify-then-close-anyway.

## Assumptions

- **`RPCCaller` is a new interface, not an existing one** — no request/response JSON-RPC client exists anywhere in this repo yet (`internal/ipc/rpc.go` only has envelope marshal/unmarshal, no pending-call tracking or `Call`/`Notify` machinery). `open.go` defines the seam it needs (`Call` for request/response, `Notify` for fire-and-forget, matching `channel.open` vs `channel.close`'s documented shapes exactly) and is tested against a fake. Wiring a real implementation against `internal/ipc.Encoder`/`Decoder` + pending-id tracking is out of scope here and presumably a composition-root-level task.
- **`Registry` never removes entries on close** — channel ids are documented as never reused for the lifetime of a plugin process connection (`docs/plugin-api.md` line 310), so a closed channel's entry stays in the map; late frames route to it and become no-ops via `deliverData`/`receiveCreditGrant`'s post-close behavior, rather than erroring as "unknown channelId." A channelId that was never registered at all still returns `*ipc.ProtocolViolationError`, matching host-side fail-fast discipline for genuinely unknown channelIds.
- **`(*Channel).GrantCredit`** is added as a method on `channel.go`'s `Channel` type (defined in `credit.go`, same package — Task 7's file itself untouched) per the task prompt's suggested API shape, delegating to the underlying `*CreditChannel` via a type assertion on the already-existing unexported `source` field. Returns an error for non-`CreditChannel` sources (e.g. the `net.Pipe`-backed fakes Task 7's own tests use), which is intentional — `GrantCredit` is meaningless without real credit accounting underneath.
- **Did not implement the tcp-relay-prompt-grant or embed-stream coupling *policies*** — per the task's explicit instruction, `GrantCredit` is a caller-invoked mechanism only; deciding *when*/*how much* to call it for each purpose is Phase 3d's `relay/coupling.go` (and whatever tcp-relay glue exists) to build.
- **`SharedEncoder`** exists so multiple `CreditChannel`s (each its own read/write-pump goroutine set, per Task 7's `Channel`) can multiplex onto the one real stdout `*ipc.Encoder` safely — `ipc.Encoder.Encode`'s own doc says it's not safe for concurrent callers.

## Status: DONE

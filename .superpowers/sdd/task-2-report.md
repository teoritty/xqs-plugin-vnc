# Task 2 report: JSON-RPC envelope, dispatch routing, redaction

## Files built
- `internal/ipc/rpc.go` — JSON-RPC 2.0 `Request`, `Response`, `Notification`, `RPCError` types plus `EncodeRequest`/`EncodeNotification`/`EncodeResponse` and `DecodeMessage` (sniffs an unknown payload into one of the three concrete types via an `envelope` struct: presence of `result`/`error` → Response, missing `id` → Notification, else → Request).
- `internal/ipc/dispatch.go` — `Dispatcher` routing decoded `ipc.Frame`s: `channelId == 0` → `RPCHandler` (JSON-RPC path, decoded via `rpc.go`), non-zero `channelId` → `ChannelDataHandler` (pass-through seam, no channel-bus logic). Enforces `MaxJSONRPCFrameLength = 256 * 1024` for `kind=KindJSONRPC` frames specifically, reusing `*ipc.ProtocolViolationError` from codec.go rather than a new error type. A non-JSON-RPC kind arriving on channel 0 is also rejected as a protocol violation.
- `internal/ipc/redact.go` — `RedactValue`/`RedactJSON` deep-copy-redact `map[string]any`/`[]any`/`json.RawMessage` params, replacing values of keys matching `sensitiveKeys` (`password`, `secret`, `token`, `key`, `passphrase`, `credential`, `credentials`, case-insensitive exact match) with `RedactedPlaceholder = "[REDACTED]"`. Input is never mutated (fresh maps/slices built bottom-up).
- Tests: `internal/ipc/rpc_test.go`, `internal/ipc/dispatch_test.go`, `internal/ipc/redact_test.go`.

## Test results
`go test ./internal/ipc/... -v` — all 32 tests pass (9 pre-existing codec tests + 8 rpc.go tests + 9 dispatch.go tests + 6 redact.go tests).
`go build ./...` and `go vet ./internal/ipc/...` — clean.

## Assumptions
- Sensitive-key matching in redact.go is exact-name (case-insensitive), not substring — e.g. `apiKey` is NOT redacted since it isn't an exact match for `key`. Chose exact-match over substring to avoid over-redacting legitimate fields like `keyboardLayout`; the list in `sensitiveKeys` is easy to extend if the real manifest surfaces more field names later.
- `docs/security-model.md` line 86 ("Sensitive field keys (`password`, `secret`, `token`, `key`, …) are stripped at the IPC boundary") was the source for the sensitive-key list; added `passphrase`/`credential(s)` since ADR-002 explicitly calls out `passphrase` handling nearby.
- `DecodeMessage` treats a JSON `"id":null` as present (Request with nil ID), matching JSON-RPC 2.0 semantics where a Notification is defined by the *absence* of the id member, not a null value.
- `RPCError.Data` is `json.RawMessage` per the JSON-RPC 2.0 error object's optional free-form `data` member.
- dispatch.go's `ChannelDataHandler` is intentionally a no-op routing seam only; no channel-bus semantics (purposes, credit, backpressure) were implemented, per task scope.

## Status
DONE — all three files built with tests written first (TDD), full suite passes, build/vet clean.

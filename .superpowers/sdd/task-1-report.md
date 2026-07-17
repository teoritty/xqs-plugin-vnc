# Task 1 report: internal/ipc/codec.go

## What was built

- `internal/ipc/codec.go` — the 9-byte frame codec:
  - `Kind` (byte) with `KindJSONRPC = 0x01`, `KindChannelData = 0x02`, `KindCredit = 0x03`.
  - `Frame{Kind, ChannelID, Payload}` with `IsControlPlane()` helper (channelId == 0).
  - `Encoder` (`NewEncoder(io.Writer)` / `Encode(kind, channelID, payload) error`) — writes one frame per call, big-endian length + channelId, refuses reserved/unknown kinds and oversize payloads before writing anything.
  - `Decoder` (`NewDecoder(io.Reader)` / `Decode() (Frame, error)`) — reads one frame per call. Strict: any reserved/unknown kind (0x00, 0x04-0xFF) or oversize declared length returns `*ProtocolViolationError` without reading the payload; no resync attempted.
  - `ProtocolViolationError{Reason, Kind, Length}` wrapping sentinel `ErrProtocolViolation` (checkable via `errors.Is`), so a later caller (the blocking read loop) can log clearly before the process dies per the host's kill-on-violation discipline.
  - `MaxFrameLength = 1 MiB` — the ceiling enforced by both Encode and Decode.
  - Clean EOF (stream ends exactly at a frame boundary) returns unwrapped `io.EOF`; a stream that dies mid-header or mid-payload returns `io.ErrUnexpectedEOF`, so callers can distinguish "host closed stdin cleanly" from "stream corrupted."
- `internal/ipc/codec_test.go` — table-driven round-trip test (5 cases: JSON-RPC control plane, channel data, credit update, empty payload, channelId=0 with a non-JSON-RPC kind) plus dedicated tests for: oversize length rejection, all 12 reserved kinds (0x04-0x0F) rejection, channelId=0 routing/`IsControlPlane()`, truncated header, truncated payload, encoder-side oversize/reserved-kind rejection, and multi-frame sequential decode ending in clean `io.EOF`.

## Test results

```
go test ./internal/ipc/... -v      → 9/9 tests pass (all subtests pass)
go test ./internal/ipc/... -cover  → coverage: 88.4% of statements (target >85%, met)
go build ./...                     → succeeds, no errors
```

## Assumptions

- **Max frame length.** `docs/plugin-api.md` documents two different ceilings: 256 KiB for JSON-RPC frames (`kind=0x01`) and 1 MiB for binary channel data (`kind=0x02`, "Max binary channel frame"). The embed-stream *purpose* is further capped at 64 KiB, but that's a channel-bus/session-layer policy (host-side, per-purpose), not a generic frame-codec concern — `internal/ipc` deliberately doesn't know about purposes. Since this file is kind-agnostic by design (per the package boundary in the design doc), I enforce the single largest documented ceiling, 1 MiB, as `MaxFrameLength` — a defensive ceiling against a corrupted length field driving unbounded allocation. Kind-specific tightening (e.g. rejecting a JSON-RPC frame over 256 KiB) is left to a higher layer (`dispatch.go`, a later task) that knows which kind it's handling. This is called out in a doc comment on `MaxFrameLength` in the source.
- Frame `length` field is payload-only (excludes the 9-byte header) — directly stated in `docs/plugin-api.md` ("`length` is the payload length only (excludes the 9 header bytes)"), no ambiguity there.
- No RFB-specific or session-specific imports were introduced; `internal/ipc/codec.go` imports only `encoding/binary`, `errors`, `fmt`, `io` (stdlib).

## Status: DONE

Committed as a standalone commit. No blockers, no open questions — the two source docs fully specified the wire format needed for this file.

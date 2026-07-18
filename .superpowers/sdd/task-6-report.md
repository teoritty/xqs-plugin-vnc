# Task 6 report — handshake.go, frontshake.go, clientmsg.go

## Status: DONE

## Files

- `internal/rfb/handshake.go` — real client-side handshake against a VNC server (`Handshake(conn io.ReadWriter, password []byte) (*HandshakeResult, error)`). Orchestrates version.go/sectypes.go/result.go/auth_vnc.go exactly per plan §1: version exchange (client always claims 3.8), security-type list (handles n=0 rejection via `ErrSecurityRejected`), `SelectSecurityType`, VNCAuth challenge/response if selected, `ReadSecurityResult` (surfaces reason on failure). Stops before ClientInit/ServerInit.
- `internal/rfb/frontshake.go` — synthetic server-side handshake toward the browser (`Frontshake(rw io.ReadWriter) (*FrontshakeResult, error)`). Writes "RFB 003.008\n", requires the browser's reply to be *exactly* 3.8 (not just >=3.8 — stricter than the real-server leg, per the task brief's explicit "not exactly 3.8" wording and the iframe trust-boundary note in plan §1), offers only `None`, requires the browser to select `None` (anything else is a protocol-violation error), writes `SecurityResultOK`, and returns immediately — never reads/writes another byte.
- `internal/rfb/clientmsg.go` — `ReadClientMessage(r io.Reader) (*ClientMessage, error)` parses one client→server RFB message per the plan §5 table (SetPixelFormat/FramebufferUpdateRequest/KeyEvent/PointerEvent fixed-size; SetEncodings/ClientCutText variable-size, length read from the message itself). Unknown type returns `*ErrUnknownClientMessageType`, a distinct fatal type (added to `errors.go`) — never silently skipped, since the parser can't know an unknown message's length and would desync the stream.
- `internal/rfb/errors.go` — added `ErrUnknownClientMessageType`.
- Tests: `handshake_test.go`, `frontshake_test.go`, `clientmsg_test.go`.

## Frontshake handoff-point decision

`Frontshake` returns `(*FrontshakeResult, error)` where `FrontshakeResult` is currently an empty marker struct, not a bare `error`. Reasoning:

- A non-nil result on success is the caller's signal that synthesis is complete and it may now start an **unmodified, bidirectional raw relay on the same `io.ReadWriter`** passed in — beginning with the browser's `ClientInit` byte, which `Frontshake` never touches.
- The struct is empty today because there's nothing meaningful to report yet (version and security type are both fixed/synthetic in v1), but keeping it a named struct rather than `error`-only means a later phase (e.g. if `ClientInit`'s shared-flag ever needs interception, per plan §1's explicit carve-out for that) can add fields without changing every call site's signature from `err := Frontshake(...)` to `_, err := Frontshake(...)`.
- Verified experimentally (`TestFrontshake_DoesNotConsumeBytesAfterSecurityResult`) that `Frontshake` reads zero bytes beyond the security-type selection byte: all reads in the file use `io.ReadFull` directly against the passed-in `io.ReadWriter`, never a `bufio.Reader`, so there is no hidden internal buffer that could swallow subsequent `ClientInit` bytes.

## Assumptions

- `Frontshake` requires the browser's version reply to be *exactly* RFB 3.8, not merely `>= 3.8` (stricter than `ReadVersion`'s general floor-enforcement used elsewhere). This matches the task brief's literal wording ("fail-fast if the browser's reply is not exactly 3.8"); plan §1's own text only says "< 3.8 must fail", so if a future noVNC ever legitimately sends e.g. 3.9 this would need revisiting — flagging as a judgment call, not a re-derivation of the plan.
- `ClientMessage.Body` holds the full message body including any embedded length/padding fields (not just the "payload" past those fields), so a caller can reconstruct the exact original wire bytes via `append([]byte{byte(m.Type)}, m.Body...)` for a read-only filter that forwards non-dropped messages verbatim.
- Added a 16 MiB sanity cap on `ClientCutText`'s declared length (mirrors the existing 1 MiB cap on reason strings in `result.go`) to guard against a hostile/corrupt peer forcing an unbounded allocation; not specified in the plan but consistent with its existing defensive posture.

## Test results

```
go build ./...   -> OK
go vet ./...     -> OK
go test ./internal/rfb/... -cover -> ok, coverage: 92.6% of statements
```

All handshake.go scenarios covered: VNCAuth success, None success, n=0 rejection, unsupported-type-only rejection, SecurityResult failure with reason, truncation at version/sectypes/challenge/result boundaries.

All frontshake.go scenarios covered: full success against a noVNC-like fake browser, browser version <3.8 fail-fast, browser selecting non-None as protocol violation, and an explicit no-over-read test proving bytes written by the fake browser after SecurityResult are never consumed by Frontshake.

All clientmsg.go scenarios covered: each fixed-size type, SetEncodings/ClientCutText at several `n` values (including 0 and large), oversize ClientCutText rejection, unknown type, and truncation at every type's boundary (type byte, fixed body, SetEncodings header/list, ClientCutText header/body).

Package imports only stdlib (`bufio`, `bytes`, `crypto/des`, `encoding/binary`, `errors`, `fmt`, `io`, `net`, `testing`, `time`) — no third-party or `net`-in-production-code dependency beyond `internal/rfb`'s own test files using `net.Pipe`.

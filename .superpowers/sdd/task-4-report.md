# Task 4 report — Phase 2: internal/rfb (version, security types, result, errors)

## Files built

- `internal/rfb/version.go` — `Version{Major,Minor}`, `String()`/`ParseVersion` for the exact 12-byte `"RFB 003.008\n"` wire line, `Less`/`AtLeast` comparators, `ReadVersion`/`WriteVersion`/`ReadVersionLine`. `V38` is the only version this plugin's handshake code accepts; `ParseVersion` distinguishes exactly 3.8 from anything lower via strict digit-by-digit parsing (no `fmt.Sscanf` leniency — an earlier `%3s%03d...` attempt silently accepted malformed input like `"00A"`, replaced with a manual `parseDigits3`).
- `internal/rfb/sectypes.go` — `SecurityType` constants (`None=1`, `VNCAuth=2`, `VeNCrypt=19`), `SecurityTypeList{Types, Empty}` with `ReadSecurityTypeList`/`WriteSecurityTypeList` parsing the `[n][type...]` wire format; `n=0` sets `Empty=true` and deliberately does NOT consume the trailing reason string (that's `ReadReasonString`'s job, kept separate for testability). `SelectSecurityType(offered, supported)` prefers `VNCAuth` over `None` when both are offered and supported, falls back to any supported offered type, and returns `*ErrNoSupportedSecurityType` (carrying both lists) otherwise. `SupportedSecurityTypes = {None, VNCAuth}` — `VeNCrypt` is a known constant but never selectable in v1.
- `internal/rfb/result.go` — `SecurityResult` (u32, `OK()`), `ReadSecurityResult(r, version)` reads the 4-byte result and, only for a failure on version ≥3.8, reads the length-prefixed reason string (4-byte BE length + UTF-8, with a 1 MiB sanity cap against a corrupt/hostile length field); pre-3.8 failures return no reason since none is on the wire. `ReadReasonString`/`WriteReasonString` factored out and shared with the `n=0` security-type-list case. `WriteSecurityResultOK` is the browser-synthesis writer (result=0, no reason bytes) per plan §1; `WriteSecurityResultFailed` exists for wire-format completeness/testability even though v1's synthesis path never calls it.
- `internal/rfb/errors.go` — `ErrUnsupportedVersion{Raw, Reason}`, `ErrNoSupportedSecurityType{Offered, Supported}`, `ErrSecurityRejected{Reason}`, `ErrTruncated{Field, Err}` (wraps `io.EOF`/`io.ErrUnexpectedEOF` so callers can `errors.Is` to tell clean early close from mid-field truncation, while `Field` names which protocol boundary was cut short). `wrapTruncated` is the shared helper every reader in this package uses at each read boundary.

## Test results

```
go build ./...   -> ok
go vet ./...     -> ok
go test ./...    -> ok (cmd/xqs-vnc, internal/ipc, internal/lifecycle, internal/rfb all pass)
go test ./internal/rfb/... -cover -> ok, coverage: 97.7% of statements (bar: >85%)
```

Table-driven tests cover: version format/parse round-trip and the strict 3.8-vs-lower distinction (`Less`/`AtLeast`), malformed version lines (wrong length, wrong prefix, non-digit fields), truncated version reads distinguishing `io.EOF` (clean close before any bytes) from `io.ErrUnexpectedEOF` (mid-field cut); security type list round-trip, `n=0` empty-list-plus-reason case, truncated reads at both the count byte and the type-entries; `SelectSecurityType` preferring VNCAuth, falling back to None, erroring on VeNCrypt-only offers (unsupported-only case) and on an empty offered list; `SecurityResult` success (no reason, no trailing bytes), failure-with-reason on 3.8, failure-without-reason pre-3.8, truncation at the result field, at the reason length prefix, and mid-reason-body; a reason-length sanity-limit guard; and `Error()`/`Unwrap()` message/wrapping checks for all four typed errors plus write-error propagation paths (failing writer, partial-write-then-fail writer) to exercise the writer-side error branches.

## Assumptions

- RFC 6143 §7.1.2/§7.1.3 wire formats used directly where the plan text didn't spell out byte layout: security-type count is a single unsigned byte (not u32); the `n=0` reason string and the `SecurityResult=1` reason string share the identical 4-byte-BE-length + UTF-8 shape, so both call the same `ReadReasonString`/`WriteReasonString`.
- Added a 1 MiB sanity cap on reason-string length in `ReadReasonString` (not spec-mandated) purely as a guard against a corrupt/hostile length field forcing an unbounded allocation before any data has been validated — this is a defensive addition, not a protocol requirement, and returns a plain `fmt.Errorf` rather than a new typed error since it's not a "truncated stream" condition.
- `SelectSecurityType`'s preference order (VNCAuth over None when both offered+supported) wasn't specified in the plan; chosen because VNCAuth actually authenticates the connection and is the more conservative default when a server offers a choice.
- `internal/rfb` package doc comment states the io.Reader/io.Writer-only, stdlib-only boundary explicitly per the plan's hard constraint — verified no non-stdlib imports exist across all four files (`bufio`, `bytes`, `encoding/binary`, `errors`, `fmt`, `io` only).

## Status

DONE

## Fix: version floor enforcement

**Finding (Critical):** `ParseVersion`/`ReadVersion` only validated wire-format shape and would successfully return a nil-error `Version{3,7}` etc. Nothing in the package enforced the design plan's (§1, §7) fail-fast requirement that any version below RFB 3.8 be rejected, even though `Version.Less`/`AtLeast` existed and were tested in isolation.

**Change:**
- Added `RequireSupportedVersion(v Version) error` in `internal/rfb/version.go`: returns `*ErrUnsupportedVersion{Raw: v.String(), Reason: "version below RFB 3.8 minimum"}` when `v.Less(V38)`, else nil. Reuses the existing `ErrUnsupportedVersion` type from `errors.go` — no new error type.
- Wired it into `ReadVersion` and `ReadVersionLine` (the package's two "read a peer's version off the wire" entry points): both now call `ParseVersion` and then `RequireSupportedVersion` before returning, so a caller cannot get a "successfully parsed" sub-3.8 version with a nil error from either read path. `ParseVersion` itself stays pure/shape-only, consistent with the parse-vs-business-rule separation already used elsewhere in the package (e.g. `ReadSecurityTypeList` parses shape only; `SelectSecurityType` is the separate business-rule step that can reject via `ErrNoSupportedSecurityType`). Read-path callers get validation for free since version enforcement is not deferred to a separate optional call the way security-type selection is.

**Tests added** (`internal/rfb/version_test.go`):
- `TestRequireSupportedVersion` — rejects `{3,3}`/`{3,7}` as `*ErrUnsupportedVersion` (via `errors.As`), accepts `{3,8}`/`{3,9}`/`{4,0}`.
- `TestReadVersionRejectsSubMinimum` / `TestReadVersionLineRejectsSubMinimum` — round-trip a written sub-3.8 version through `ReadVersion`/`ReadVersionLine` and confirm both surface `*ErrUnsupportedVersion`.
- `TestReadVersionMalformedContent` / `TestReadVersionLineMalformedContent` — added to cover the newly-introduced `ParseVersion` error-passthrough branch in both read paths (needed to keep coverage from regressing after the new code paths were added).

No other files in the repo call `rfb.ReadVersion`/`rfb.ParseVersion`/`rfb.ReadVersionLine` yet, so no call-site updates were needed.

**Test results:**
```
go build ./...                          -> ok
go test ./internal/rfb/... -cover       -> ok, all tests pass, coverage: 97.9% of statements
                                            (prior: 97.7%, bar: >= prior)
```

**Status:** DONE

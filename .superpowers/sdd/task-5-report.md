# Task 5 report: VNC Auth DES challenge-response

## What was built

`internal/rfb/auth_vnc.go`:
- `reverseByteBits(b byte) byte` — reverses bit order within a byte.
- `deriveDESKey(password []byte) [8]byte` — truncates/zero-pads password to
  8 bytes, then reverses the bits of each byte (RFC 6143 §7.2.2 quirk).
- `VNCAuthResponse(password, challenge []byte) ([]byte, error)` — derives
  the DES key and encrypts the 16-byte challenge as two independent 8-byte
  ECB blocks under that key (no chaining), returning the 16-byte response.
  Rejects challenges that aren't exactly 16 bytes. Uses stdlib `crypto/des`
  for the block cipher only; the VNC-specific key derivation and
  two-block-ECB framing are hand-rolled per the spec. API is `[]byte`-based
  throughout (no forced `string` conversion), leaving password zeroing to
  the caller as planned for a later phase.

`internal/rfb/auth_vnc_test.go`:
- `TestVNCAuthResponse_KnownVector` — the load-bearing test.
- `TestDeriveDESKey_BitReversal`, `TestDeriveDESKey_PadAndTruncate` — key
  derivation checked in isolation, including a hand-verifiable
  single-bit case (0x01 ⇄ 0x80).
- `TestVNCAuthResponse_SelfConsistency`, `TestVNCAuthResponse_RejectsWrongChallengeLength`.

## Test vector provenance (important part)

Password `"password"` (exactly 8 bytes, no pad/truncate needed) and
challenge `000102030405060708090a0b0c0d0e0f` (16 bytes, ascending byte
values for easy inspection).

Derived **independently of this package and of Go's `crypto/des`**:

1. Bit-reversal key derivation computed with a standalone Python snippet
   (not this codebase, not Go):
   `password.hex() = 70617373776f7264` → reversed-per-byte key
   `0e86ceceeef64e26`.
2. DES-ECB encryption of the two 8-byte challenge halves computed with
   OpenSSL 3.5.5's **legacy** provider (`openssl enc -des-ecb -K
   0e86ceceeef64e26 -nopad -provider legacy -provider default`), a C
   implementation entirely separate from Go's stdlib:
   - block1 `0001020304050607` → `b866924125c8eebb`
   - block2 `08090a0b0c0d0e0f` → `9debc1db61c538e2`
   - expected response: `b866924125c8eebb9debc1db61c538e2`

This matches `VNCAuthResponse`'s output exactly. Full derivation commands
are recorded as a comment in `auth_vnc_test.go` so it's reproducible and
auditable, not just asserted.

**Confidence: high.** The vector was cross-checked against a completely
independent tool (Python for the bit-reversal step, OpenSSL's legacy DES
provider for the block cipher), not derived by reading back this
implementation's own output — this is genuinely a "vector from source 2"
situation (per the task's option 1/2 distinction), not a self-referential
test. The bit-reversal direction (reverse each byte's bits before DES key
scheduling, not before-vs-after some other transform) matches RFC 6143
§7.2.2's description and is additionally sanity-checked by the
hand-verifiable 0x01⇄0x80 single-bit case in `TestDeriveDESKey_BitReversal`.

## Test results

```
go build ./...                             # ok
go test ./internal/rfb/... -cover          # ok, coverage: 97.6% of statements
```

All new tests pass, including the grounded known-vector test.

## Status: DONE

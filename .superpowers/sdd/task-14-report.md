# Task 14 — Release packaging (Phase 6)

## What was built

`cmd/xqs-vnc-pack/main.go`: a Go packaging tool (`go run ./cmd/xqs-vnc-pack`) that:

1. Cross-compiles `cmd/xqs-vnc` for `windows/amd64`, `linux/amd64`, `darwin/amd64`,
   `darwin/arm64` via `go build` with `GOOS`/`GOARCH`/`CGO_ENABLED=0` set explicitly
   (`-targets` flag overrides the list).
2. Assembles one `.xqsp` ZIP bundle per platform under `dist/`, containing the
   platform binary, `plugin.json`, and everything under `ui/` (including the vendored
   noVNC tree).
3. Writes a `SHA256SUMS` file into each bundle.
4. Generates (or reuses) an Ed25519 keypair and signs each bundle's manifest,
   embedding the base64 signature into `plugin.json`'s `signature` field.
5. Re-opens each produced `.xqsp`, recomputes `SHA256SUMS` from the archived files,
   and verifies the Ed25519 signature with `ed25519.Verify` — proving the round trip
   actually works, not just that files got written.

## `.xqsp` format interpretation and doc grounding

`docs/plugin-manifest.md` is explicit on most of this:

- **Archive type**: ".xqsp files are ZIP archives" (Bundle format section) — implemented directly.
- **SHA256SUMS scope**: "SHA-256 hashes of all files except the checksums file itself" — implemented as every bundled file (manifest, binary, all `ui/**`) except `SHA256SUMS` itself.
- **Signature attachment**: embedded in `plugin.json`'s `signature` field (base64 Ed25519), not a detached `.sig` — per the "Signature" section's explicit envelope: `{"manifest": <plugin.json without "signature">, "checksumsSha256": <hex sha256 of SHA256SUMS bytes, CRLF normalized to LF>}`, canonical JSON (map keys sorted). Implemented exactly as specified, including the CRLF→LF normalization.

One point required an interpretation call, documented in the tool's package comment and here:

- **Per-platform vs. multi-platform bundles.** `engine.entry` is a single filename in the
  manifest schema, and the doc states "Binary must exist and match host GOOS at
  discovery/install." There's no documented convention for a single manifest listing
  multiple per-OS binaries. I interpreted this as: **one `.xqsp` per target platform**,
  each with `engine.entry` rewritten to the platform-appropriate binary name
  (`xqs-vnc.exe` on Windows, `xqs-vnc` elsewhere) and containing only that platform's
  binary. Output: `dist/xqs-vnc-<goos>-<goarch>.xqsp`. This is a reasonable, low-risk
  reading but the doc doesn't rule out a fatter single-bundle-multi-binary scheme; if
  the real host expects one universal bundle, only the packaging tool's assembly step
  needs to change, not the signing/checksum logic.

- **Chicken-and-egg in SHA256SUMS vs. the signed `plugin.json`.** The doc says "write
  SHA256SUMS first ... then sign the manifest envelope," but the bundled `plugin.json`
  ends up containing the signature *after* SHA256SUMS is computed — so a naive
  "hash the bundled file as-is" verifier would see `plugin.json`'s SHA256SUMS entry
  not match the actual (signed) file bytes. I resolved this by having the
  `plugin.json` entry in `SHA256SUMS` hash the **unsigned, canonical** manifest bytes
  (signature field stripped), not the final signed file. This is consistent and
  round-trips: a verifier that recomputes the envelope (which it must do anyway to
  check the signature) has the unsigned manifest in hand and can hash that form for
  comparison against `SHA256SUMS`. This is called out clearly as an interpretation in
  the tool's doc comment; the doc itself doesn't address this edge case.

## Signing key handling

- No production key exists or is implied anywhere in the repo/docs. `xqs-vnc-pack`
  generates a throwaway Ed25519 keypair on first run: private seed at
  `dist/dev-signing-key` (base64, 32 raw bytes, `0600`), public key at
  `dist/dev-signing-key.pub`. Console output prints prominently:
  `*** this is a throwaway development key, not a production release signing key ***`.
- `dist/` (and therefore the generated key) is entirely gitignored — added
  `/dist/` to `.gitignore` with a comment explaining why. Nothing resembling a
  production secret is committed.
- `-key <path>` lets a caller point at a different (e.g. CI-provided) key file using
  the same base64-seed format, without changing the tool.

## Verification results (this environment)

```
go build ./...   → OK
go test ./... -race -cover → all packages pass (unchanged from before this task)

go run ./cmd/xqs-vnc-pack:
  windows/amd64: binary 3,877,888 bytes, bundle written, 63 SHA256SUMS entries, verify: OK
  linux/amd64:   binary 3,805,124 bytes, bundle written, 63 SHA256SUMS entries, verify: OK
  darwin/amd64:  binary 3,971,552 bytes, bundle written, 63 SHA256SUMS entries, verify: OK
  darwin/arm64:  binary 3,785,986 bytes, bundle written, 63 SHA256SUMS entries, verify: OK
```

`verify: OK` for each platform means the tool independently re-opened the produced
`.xqsp`, recomputed `SHA256SUMS` from the archived file bytes (byte-for-byte match
against the shipped `SHA256SUMS`), rebuilt the signing envelope, and called
`ed25519.Verify(pub, envelopeBytes, sig)` successfully — a real round trip, not just
"files exist."

Manually inspected one bundle's `SHA256SUMS` (64-hex-char entries, `plugin.json`
first) and `plugin.json` (contains the rewritten `engine.entry` and, at the end,
the base64 `signature` field) to confirm the on-disk shape matches the design.

## Status

DONE — cross-compilation, `.xqsp` bundling, `SHA256SUMS`, and Ed25519 signing all
implemented and verified end-to-end for 4 platforms; full repo build/test stayed
green. One documented interpretation (per-platform bundles; unsigned-manifest hash
for the `plugin.json` SHA256SUMS entry) where the doc was silent — flagged above,
not blocking.

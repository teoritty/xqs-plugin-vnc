package rfb

import (
	"fmt"
	"io"
)

// FrontshakeResult is an intentionally empty marker returned by Frontshake
// on success.
//
// Handoff-point design decision: Frontshake's contract is "write and read
// only the bytes that belong to version/security/SecurityResult, then
// return — never touch a single byte beyond that". A caller that gets a
// non-nil *FrontshakeResult knows synthesis is complete and may now start
// an unmodified, bidirectional raw relay on the *same* io.ReadWriter it
// passed in (starting with the browser's ClientInit byte, which Frontshake
// never reads). The type carries no fields today because there is nothing
// yet to report — no negotiated version choice (browser must send exactly
// 3.8) and no negotiated security type (always synthetic None). It exists
// (rather than Frontshake returning plain error) so a later phase can add
// fields — e.g. bytes-remaining-in-any-internal-buffer, if a future
// implementation ever introduces one — without changing every call site
// from "err := Frontshake(...)" to "_, err := Frontshake(...)". Every read
// in this file uses io.ReadFull directly against rw (no bufio.Reader), so
// there is no hidden internal buffer today: not one byte past the
// selection byte is consumed.
type FrontshakeResult struct{}

// Frontshake synthesizes the browser-facing RFB handshake prefix described
// in the design plan §1: the plugin acts as a fake, already-authenticated
// server toward the browser (noVNC). rw is typically the embed-stream
// channel wrapped as an io.ReadWriter; tests use net.Pipe() or simple
// in-process pipes.
//
// Steps, in order:
//  1. Write "RFB 003.008\n" to the browser.
//  2. Read the browser's version reply. Per the design plan's trust
//     boundary note (§1: "iframe — недоверенная граница"), the reply must
//     be exactly RFB 3.8 — not merely >= 3.8. Anything else is a fail-fast
//     error, even if it would otherwise be a well-formed newer version.
//  3. Write a security-type list containing only None (type 1).
//  4. Read the browser's selection; anything other than None is a
//     protocol violation (the browser isn't offered a real choice here —
//     this is synthesis, not negotiation).
//  5. Write SecurityResult = 0 (success, no reason string).
//
// Frontshake stops the instant SecurityResult is written. It never reads
// or writes ClientInit/ServerInit — that is the caller's job once the real
// end-to-end relay begins.
func Frontshake(rw io.ReadWriter) (*FrontshakeResult, error) {
	if err := WriteVersion(rw, V38); err != nil {
		return nil, err
	}

	clientVersion, err := ReadVersion(rw)
	if err != nil {
		return nil, err
	}
	if clientVersion != V38 {
		return nil, &ErrUnsupportedVersion{
			Raw:    clientVersion.String(),
			Reason: "browser must reply with exactly RFB 3.8 (untrusted iframe boundary)",
		}
	}

	if err := WriteSecurityTypeList(rw, []SecurityType{SecTypeNone}); err != nil {
		return nil, err
	}

	var selBuf [1]byte
	if _, err := io.ReadFull(rw, selBuf[:]); err != nil {
		return nil, wrapTruncated("browser security type selection", err)
	}
	selected := SecurityType(selBuf[0])
	if selected != SecTypeNone {
		return nil, fmt.Errorf("rfb: browser selected security type %s but only None was offered (synthetic handshake)", selected)
	}

	if err := WriteSecurityResultOK(rw); err != nil {
		return nil, err
	}

	return &FrontshakeResult{}, nil
}

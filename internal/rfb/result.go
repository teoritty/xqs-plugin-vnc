package rfb

import (
	"encoding/binary"
	"fmt"
	"io"
)

// SecurityResult is the RFB SecurityResult value (RFC 6143 §7.1.3): a
// big-endian u32, 0 = OK, anything else = failed (1 is the only value the
// spec defines, but we preserve whatever the peer sent).
type SecurityResult uint32

const (
	SecurityResultOK     SecurityResult = 0
	SecurityResultFailed SecurityResult = 1
)

// OK reports whether the result indicates success.
func (r SecurityResult) OK() bool {
	return r == SecurityResultOK
}

// ReadSecurityResult reads the 4-byte SecurityResult from r. If the result
// is a failure and version is >= 3.8, it also reads the length-prefixed
// reason string that RFC 6143 §7.1.3 requires to follow a failed result
// under 3.8, and returns it. For a success result, or for a failure under
// a pre-3.8 version (no reason string on the wire), the returned reason is
// empty.
func ReadSecurityResult(r io.Reader, version Version) (SecurityResult, string, error) {
	var buf [4]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, "", wrapTruncated("security result", err)
	}
	result := SecurityResult(binary.BigEndian.Uint32(buf[:]))
	if result.OK() {
		return result, "", nil
	}
	if !version.AtLeast(V38) {
		// Pre-3.8 servers send no reason string after a failed result.
		return result, "", nil
	}
	reason, err := ReadReasonString(r)
	if err != nil {
		return result, "", err
	}
	return result, reason, nil
}

// ReadReasonString reads an RFC 6143 length-prefixed reason string: a
// 4-byte big-endian length followed by that many bytes of UTF-8 text. Used
// both for a failed SecurityResult and for an empty (n=0) security type
// list, which share this exact wire shape.
func ReadReasonString(r io.Reader) (string, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return "", wrapTruncated("reason string length", err)
	}
	length := binary.BigEndian.Uint32(lenBuf[:])
	// Guard against a malicious/corrupt peer claiming an absurd length and
	// forcing an unbounded allocation.
	const maxReasonLen = 1 << 20 // 1 MiB, generous upper bound for a UI string
	if length > maxReasonLen {
		return "", fmt.Errorf("rfb: reason string length %d exceeds sanity limit %d", length, maxReasonLen)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", wrapTruncated("reason string body", err)
	}
	return string(buf), nil
}

// WriteSecurityResult writes the 4-byte SecurityResult to w, with no
// reason string. Used on the real client<->server leg only when writing a
// success result is meaningful (servers write results, not clients); kept
// here as the single low-level writer both directions build on.
func WriteSecurityResult(w io.Writer, result SecurityResult) error {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(result))
	if _, err := w.Write(buf[:]); err != nil {
		return fmt.Errorf("rfb: write security result: %w", err)
	}
	return nil
}

// WriteSecurityResultOK writes a synthesized SecurityResult=0 (success)
// with no trailing reason bytes. This is the browser-facing synthesis
// direction described in the design plan §1: v1 always tells the browser
// side "you're in" — the real authentication already happened on the
// plugin<->VNC-server leg, and the browser<->plugin leg is protected by
// the host's own tunnel security instead.
func WriteSecurityResultOK(w io.Writer) error {
	return WriteSecurityResult(w, SecurityResultOK)
}

// WriteSecurityResultFailed writes a SecurityResult=1 (failed) followed by
// the length-prefixed reason string, per RFC 6143 §7.1.3. Provided for
// completeness/testability of the wire format; v1's synthesis direction
// never calls this (see WriteSecurityResultOK).
func WriteSecurityResultFailed(w io.Writer, reason string) error {
	if err := WriteSecurityResult(w, SecurityResultFailed); err != nil {
		return err
	}
	return WriteReasonString(w, reason)
}

// WriteReasonString writes an RFC 6143 length-prefixed reason string: a
// 4-byte big-endian length followed by the UTF-8 bytes of reason.
func WriteReasonString(w io.Writer, reason string) error {
	body := []byte(reason)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(body)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return fmt.Errorf("rfb: write reason string length: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("rfb: write reason string body: %w", err)
	}
	return nil
}

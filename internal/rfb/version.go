// Package rfb implements the wire-level pieces of the Remote Framebuffer
// (RFB / VNC) protocol described in RFC 6143. It is pure protocol code: it
// operates only on io.Reader/io.Writer and the Go standard library, with no
// knowledge of net.Conn, the plugin's channel bus, or JSON-RPC. That is what
// keeps it testable on in-memory pipes and reusable in both directions the
// plugin needs: as a real client talking to a VNC server, and as a
// synthetic server talking to the browser-side noVNC client.
package rfb

import (
	"bufio"
	"fmt"
	"io"
)

// Version is a parsed RFB protocol version, e.g. "RFB 003.008\n" -> {3, 8}.
type Version struct {
	Major int
	Minor int
}

// V38 is the only server/client version this plugin's handshake code
// speaks. Per the design plan (§1, §7) anything below 3.8 must fail fast,
// not be silently treated as "close enough".
var V38 = Version{Major: 3, Minor: 8}

// versionLineLen is the fixed length of an RFB version line, including the
// trailing newline: "RFB 003.008\n" is exactly 12 bytes.
const versionLineLen = 12

// String formats the version the way the wire protocol expects:
// "RFB 003.008\n" (3-digit zero-padded major and minor, trailing LF).
func (v Version) String() string {
	return fmt.Sprintf("RFB %03d.%03d\n", v.Major, v.Minor)
}

// Less reports whether v is strictly older than other.
func (v Version) Less(other Version) bool {
	if v.Major != other.Major {
		return v.Major < other.Major
	}
	return v.Minor < other.Minor
}

// AtLeast reports whether v is equal to or newer than other.
func (v Version) AtLeast(other Version) bool {
	return !v.Less(other)
}

// WriteVersion writes the wire-format version line to w.
func WriteVersion(w io.Writer, v Version) error {
	_, err := io.WriteString(w, v.String())
	if err != nil {
		return fmt.Errorf("rfb: write version: %w", err)
	}
	return nil
}

// RequireSupportedVersion enforces the fail-fast floor from the design
// plan (§1, §7): any version strictly below RFB 3.8 must be rejected, even
// though it parses to a well-formed Version. This is a defense against a
// malformed or hostile peer at the untrusted border, not merely a shape
// check — a real noVNC client or real VNC server always sends 3.8.
func RequireSupportedVersion(v Version) error {
	if v.Less(V38) {
		return &ErrUnsupportedVersion{Raw: v.String(), Reason: "version below RFB 3.8 minimum"}
	}
	return nil
}

// ReadVersion reads exactly one RFB version line ("RFB 0XX.0YY\n", 12
// bytes) from r, parses it, and enforces the >=3.8 floor. A short read is
// reported as a typed truncation error rather than a raw io error, so
// callers can distinguish a clean early close from malformed data. A
// well-formed but sub-3.8 version is reported via *ErrUnsupportedVersion
// (see RequireSupportedVersion) rather than returned successfully.
func ReadVersion(r io.Reader) (Version, error) {
	buf := make([]byte, versionLineLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		return Version{}, wrapTruncated("version line", err)
	}
	v, err := ParseVersion(buf)
	if err != nil {
		return Version{}, err
	}
	if err := RequireSupportedVersion(v); err != nil {
		return Version{}, err
	}
	return v, nil
}

// ParseVersion parses a raw 12-byte RFB version line.
func ParseVersion(line []byte) (Version, error) {
	if len(line) != versionLineLen {
		return Version{}, &ErrUnsupportedVersion{Raw: string(line), Reason: "wrong length"}
	}
	s := string(line)
	if s[:4] != "RFB " || s[7] != '.' || s[11] != '\n' {
		return Version{}, &ErrUnsupportedVersion{Raw: s, Reason: "malformed version line"}
	}
	major, err := parseDigits3(s[4:7])
	if err != nil {
		return Version{}, &ErrUnsupportedVersion{Raw: s, Reason: "malformed major version"}
	}
	minor, err := parseDigits3(s[8:11])
	if err != nil {
		return Version{}, &ErrUnsupportedVersion{Raw: s, Reason: "malformed minor version"}
	}
	return Version{Major: major, Minor: minor}, nil
}

// parseDigits3 parses a 3-character substring as a base-10 integer,
// requiring every character to be an ASCII digit.
func parseDigits3(s string) (int, error) {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a digit: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// ReadVersionLine is a convenience for readers that want to use a
// bufio.Reader (e.g. to peek further into the stream afterwards) instead of
// a fixed-size io.ReadFull. Behaves identically to ReadVersion otherwise.
func ReadVersionLine(r *bufio.Reader) (Version, error) {
	buf := make([]byte, versionLineLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		return Version{}, wrapTruncated("version line", err)
	}
	v, err := ParseVersion(buf)
	if err != nil {
		return Version{}, err
	}
	if err := RequireSupportedVersion(v); err != nil {
		return Version{}, err
	}
	return v, nil
}

package rfb

import (
	"errors"
	"fmt"
	"io"
)

// ErrUnsupportedVersion is returned when a peer's RFB version line cannot
// be parsed, or parses to a version this plugin does not speak (v1: only
// exactly 3.8 is accepted per the design plan's fail-fast requirement).
type ErrUnsupportedVersion struct {
	// Raw is the offending version line/string as received on the wire.
	Raw string
	// Reason describes why the version was rejected.
	Reason string
}

func (e *ErrUnsupportedVersion) Error() string {
	return fmt.Sprintf("rfb: unsupported version %q: %s", e.Raw, e.Reason)
}

// ErrNoSupportedSecurityType is returned when a server's offered security
// type list contains nothing this plugin currently supports.
type ErrNoSupportedSecurityType struct {
	// Offered is the list of security types the server offered.
	Offered []SecurityType
	// Supported is the list of security types this plugin supports.
	Supported []SecurityType
}

func (e *ErrNoSupportedSecurityType) Error() string {
	return fmt.Sprintf("rfb: no supported security type: server offered %v, plugin supports %v", e.Offered, e.Supported)
}

// ErrSecurityRejected is returned when the server (or, in the wire format
// sense, whichever peer is playing "server" role) rejects the security
// handshake — either via SecurityResult=1 (failed) or an empty (n=0)
// security type list, both of which carry a reason string under RFB 3.8.
type ErrSecurityRejected struct {
	// Reason is the human-readable reason string sent by the peer, if any.
	// May be empty if the peer sent no reason (e.g. pre-3.8 semantics).
	Reason string
}

func (e *ErrSecurityRejected) Error() string {
	if e.Reason == "" {
		return "rfb: security handshake rejected by server"
	}
	return fmt.Sprintf("rfb: security handshake rejected by server: %s", e.Reason)
}

// ErrUnknownClientMessageType is returned by ReadClientMessage when a
// client->server message's type byte is not one of the known types (§5 of
// the design plan's table). This is deliberately fatal, not skippable: the
// message body's length is type-dependent (some are variable-length), so
// an unknown type means the parser cannot know how many bytes to consume
// to stay in sync with the stream. A caller (e.g. the read-only filter)
// must treat this as an unrecoverable desync and close the session rather
// than attempt to resynchronize or pass the byte through.
type ErrUnknownClientMessageType struct {
	// Type is the unrecognized message type byte as received on the wire.
	Type ClientMessageType
}

func (e *ErrUnknownClientMessageType) Error() string {
	return fmt.Sprintf("rfb: unknown client message type %d (fatal: cannot determine message length, stream desynchronized)", uint8(e.Type))
}

// ErrTruncated wraps a short read at a specific protocol field boundary so
// callers can tell "clean early close" (io.EOF, e.g. peer closed cleanly
// before sending anything for this field) apart from "malformed data mid
// field" (io.ErrUnexpectedEOF, e.g. peer sent some but not all of a fixed
// size field before closing).
type ErrTruncated struct {
	// Field names the protocol field being read when the read was cut
	// short, e.g. "version line", "security type list", "reason string".
	Field string
	// Err is the underlying error: io.EOF or io.ErrUnexpectedEOF (or
	// whatever the reader produced), preserved for errors.Is/As.
	Err error
}

func (e *ErrTruncated) Error() string {
	return fmt.Sprintf("rfb: truncated read at %s: %v", e.Field, e.Err)
}

func (e *ErrTruncated) Unwrap() error {
	return e.Err
}

// wrapTruncated wraps err (expected to be io.EOF, io.ErrUnexpectedEOF, or a
// similar read failure) as an ErrTruncated naming the field being read.
// Non-EOF errors are wrapped as-is so the underlying cause is preserved.
func wrapTruncated(field string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return &ErrTruncated{Field: field, Err: err}
	}
	return &ErrTruncated{Field: field, Err: err}
}

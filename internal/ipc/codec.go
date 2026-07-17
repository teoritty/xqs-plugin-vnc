// Package ipc implements the 9-byte length-prefixed frame codec used for
// all stdin/stdout traffic between the plugin process and the xQuakShell
// host, once the binary channel bus is live. This file (codec.go) is
// intentionally the lowest layer in the plugin: it knows about bytes and
// frame boundaries only, nothing about JSON-RPC, RFB, or sessions.
//
// Wire format (see docs/plugin-api.md "Frame layer" and
// docs/superpowers/specs/2026-07-16-vnc-plugin-design.md §3 "Фрейминг"):
//
//	[4 bytes: payload length][1 byte: kind][4 bytes: channelId][payload]
//
// length and channelId are big-endian uint32. length is the payload
// length only — it excludes the 9 header bytes themselves.
//
// This package must not import anything beyond the standard library.
package ipc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Kind identifies what a frame's payload contains.
type Kind byte

const (
	// KindJSONRPC carries a JSON-RPC 2.0 message on the control plane
	// (channelId 0) or, in principle, any channel — in practice the host
	// only ever uses channelId 0 for this kind.
	KindJSONRPC Kind = 0x01
	// KindChannelData carries raw binary channel bytes: no JSON, no
	// base64.
	KindChannelData Kind = 0x02
	// KindCredit carries a fixed 8-byte flow-control update:
	// [4B channelId][4B credit].
	KindCredit Kind = 0x03
)

// headerLen is the fixed size of the frame header in bytes:
// 4 (length) + 1 (kind) + 4 (channelId).
const headerLen = 9

// MaxFrameLength is the maximum permitted payload length (excluding the
// 9-byte header) that this codec will accept or produce.
//
// Assumption: docs/plugin-api.md documents two distinct ceilings —
// 256 KiB for JSON-RPC frames (kind=0x01) and 1 MiB for binary channel
// data (kind=0x02, "Max binary channel frame"). The embed-stream purpose
// is further restricted to 64 KiB by the host at the channel-bus/session
// layer (see design doc §3, F-8), not at the generic frame-codec layer.
// Since this package is deliberately kind-agnostic and purpose-agnostic
// (it must not know what's inside a frame), we enforce the single
// largest documented ceiling, 1 MiB, as a defensive ceiling against a
// corrupted length field driving unbounded allocation. Kind-specific and
// purpose-specific limits (256 KiB for JSON-RPC, 64 KiB for embed-stream)
// are the responsibility of higher layers (internal/ipc/dispatch.go,
// internal/transport) that know which kind/purpose they're handling.
const MaxFrameLength = 1024 * 1024 // 1 MiB

// ErrProtocolViolation is the sentinel wrapped by ProtocolViolationError.
// Callers can check for it with errors.Is.
var ErrProtocolViolation = errors.New("ipc: protocol violation")

// ProtocolViolationError reports a frame that violates the wire
// contract: an unknown/reserved kind, or an oversize length. Per the
// design doc's "Дисциплина отказа" (failure discipline), the host kills
// the plugin process outright on any such violation — there is no
// recovery path once frame boundaries can't be trusted. This error type
// exists so the caller (the blocking read loop, a later task) can log
// clearly before the process dies, not so it can attempt to resync.
type ProtocolViolationError struct {
	// Reason is a short machine-stable description, e.g. "reserved kind"
	// or "oversize length".
	Reason string
	// Kind is the offending kind byte, if relevant (0 if not applicable).
	Kind Kind
	// Length is the offending declared length, if relevant.
	Length uint32
}

func (e *ProtocolViolationError) Error() string {
	return fmt.Sprintf("ipc: protocol violation: %s (kind=0x%02x length=%d)", e.Reason, byte(e.Kind), e.Length)
}

func (e *ProtocolViolationError) Unwrap() error {
	return ErrProtocolViolation
}

// isReservedOrUnknownKind reports whether k is not one of the three
// defined kinds. Per docs/plugin-api.md, 0x04-0x0F are reserved and
// forbidden in v1; any other byte value is simply unknown.
func isReservedOrUnknownKind(k Kind) bool {
	switch k {
	case KindJSONRPC, KindChannelData, KindCredit:
		return false
	default:
		return true
	}
}

// Frame is a single decoded frame.
type Frame struct {
	Kind      Kind
	ChannelID uint32
	Payload   []byte
}

// IsControlPlane reports whether this frame belongs to channelId 0, the
// JSON-RPC control plane.
func (f Frame) IsControlPlane() bool {
	return f.ChannelID == 0
}

func putUint32(b []byte, v uint32) {
	binary.BigEndian.PutUint32(b, v)
}

// Encoder writes length-prefixed frames to an underlying io.Writer. An
// Encoder is safe for use by a single writer goroutine; callers that
// write from multiple goroutines must serialize calls to Encode
// themselves (frame writes are not atomic across a shared io.Writer
// otherwise).
type Encoder struct {
	w io.Writer
}

// NewEncoder returns an Encoder that writes frames to w.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w}
}

// Encode writes one frame: header followed by payload. It refuses to
// write reserved/unknown kinds or oversize payloads rather than putting
// a malformed frame on the wire — we hold ourselves to the same strict
// contract we require of the host, since sending a violation would get
// our own process killed just as surely as accepting one.
func (e *Encoder) Encode(kind Kind, channelID uint32, payload []byte) error {
	if isReservedOrUnknownKind(kind) {
		return &ProtocolViolationError{Reason: "reserved or unknown kind", Kind: kind}
	}
	if len(payload) > MaxFrameLength {
		return &ProtocolViolationError{Reason: "oversize length", Length: uint32(len(payload))}
	}

	hdr := make([]byte, headerLen)
	putUint32(hdr[0:4], uint32(len(payload)))
	hdr[4] = byte(kind)
	putUint32(hdr[5:9], channelID)

	if _, err := e.w.Write(hdr); err != nil {
		return fmt.Errorf("ipc: write frame header: %w", err)
	}
	if len(payload) > 0 {
		if _, err := e.w.Write(payload); err != nil {
			return fmt.Errorf("ipc: write frame payload: %w", err)
		}
	}
	return nil
}

// Decoder reads length-prefixed frames from an underlying io.Reader. A
// Decoder is safe for use by a single reader goroutine — the host runs
// one blocking stdin read loop, and that is the only supported usage
// pattern; concurrent calls to Decode on the same Decoder would race on
// the underlying reader.
//
// Decoder is strict by design: on any protocol violation it returns a
// *ProtocolViolationError and does not attempt to resynchronize the
// stream. Once a length or kind can't be trusted, no subsequent byte in
// the stream can be trusted either — the caller must treat the
// connection as dead, exactly as the host does on its side.
type Decoder struct {
	r io.Reader
}

// NewDecoder returns a Decoder that reads frames from r.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: r}
}

// Decode reads and returns exactly one frame. It returns io.EOF (unwrapped)
// only when the stream ends cleanly at a frame boundary (zero bytes read
// for a new header). A stream that ends partway through a header or
// payload returns io.ErrUnexpectedEOF, distinguishing a clean shutdown
// from a truncated/corrupted stream. A frame with a reserved kind or an
// oversize declared length returns *ProtocolViolationError without
// attempting to read the (untrustworthy) payload.
func (d *Decoder) Decode() (Frame, error) {
	hdr := make([]byte, headerLen)
	if _, err := io.ReadFull(d.r, hdr); err != nil {
		if err == io.EOF {
			return Frame{}, io.EOF
		}
		// ReadFull returns io.ErrUnexpectedEOF for a partial header; pass
		// through as-is (and wrap anything else, e.g. underlying read
		// errors).
		return Frame{}, err
	}

	length := binary.BigEndian.Uint32(hdr[0:4])
	kind := Kind(hdr[4])
	channelID := binary.BigEndian.Uint32(hdr[5:9])

	if isReservedOrUnknownKind(kind) {
		return Frame{}, &ProtocolViolationError{Reason: "reserved or unknown kind", Kind: kind, Length: length}
	}
	if length > MaxFrameLength {
		return Frame{}, &ProtocolViolationError{Reason: "oversize length", Kind: kind, Length: length}
	}

	var payload []byte
	if length > 0 {
		payload = make([]byte, length)
		if _, err := io.ReadFull(d.r, payload); err != nil {
			if err == io.EOF {
				// Zero bytes available where a full payload was declared:
				// the stream ended mid-frame, not at a boundary.
				return Frame{}, io.ErrUnexpectedEOF
			}
			return Frame{}, err
		}
	}

	return Frame{Kind: kind, ChannelID: channelID, Payload: payload}, nil
}

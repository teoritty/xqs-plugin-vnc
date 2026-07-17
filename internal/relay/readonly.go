// This file (readonly.go) implements the read-only mode filter described
// in docs/superpowers/specs/2026-07-16-vnc-plugin-design.md §5: when a
// session's readOnly field is set, client (browser)->server (VNC) input
// messages are dropped, but the stream must stay byte-synchronized —
// every message still has to be fully parsed off the wire so its length
// is known, even the ones that get dropped.
//
// This is a thin policy layer over internal/rfb/clientmsg.go, which owns
// all message-boundary parsing; this file never re-implements or
// duplicates that parsing.
package relay

import (
	"errors"
	"fmt"
	"io"

	"xqs-plugin-vnc/internal/rfb"
)

// ErrSessionFatal wraps an underlying error to flag it as one that must
// terminate the whole relay session (not merely this one filter step) —
// per the design doc §5, an unrecognized client message type desyncs the
// byte stream irrecoverably, so continuing to read from it is unsafe.
type ErrSessionFatal struct {
	Err error
}

func (e *ErrSessionFatal) Error() string {
	return fmt.Sprintf("relay: session-fatal: %v", e.Err)
}

func (e *ErrSessionFatal) Unwrap() error {
	return e.Err
}

// isInputMessage reports whether msgType is one of the three
// input-producing messages design doc §5 says must be dropped when
// readOnly is set: KeyEvent(4), PointerEvent(5), ClientCutText(6).
func isInputMessage(t rfb.ClientMessageType) bool {
	switch t {
	case rfb.MsgKeyEvent, rfb.MsgPointerEvent, rfb.MsgClientCutText:
		return true
	default:
		return false
	}
}

// FilterDecision is the policy layer's verdict on a single parsed client
// message.
type FilterDecision int

const (
	// DecisionForward: write the message to the server unmodified.
	DecisionForward FilterDecision = iota
	// DecisionDrop: the message was fully consumed off the wire (so the
	// stream stays in sync) but must not reach the server.
	DecisionDrop
)

// Decide applies the design doc §5 read-only policy to msg: the three
// input-producing types (KeyEvent/PointerEvent/ClientCutText) are dropped
// when readOnly is true and forwarded when false; the three
// non-input-producing types (SetPixelFormat/SetEncodings/
// FramebufferUpdateRequest) are always forwarded regardless of readOnly.
// Decide itself never returns an error — msg is already a successfully
// parsed rfb.ClientMessage, so by construction its Type is one
// ReadClientMessage recognized.
func Decide(msg *rfb.ClientMessage, readOnly bool) FilterDecision {
	if isInputMessage(msg.Type) {
		if readOnly {
			return DecisionDrop
		}
		return DecisionForward
	}
	// SetPixelFormat/SetEncodings/FramebufferUpdateRequest — and anything
	// else ReadClientMessage successfully parsed — are forwarded
	// unconditionally; none of them produce input on the VNC server.
	return DecisionForward
}

// FilterOnce reads exactly one client->server RFB message from r via
// rfb.ReadClientMessage and, per Decide's policy, writes it to w
// (forward) or discards it (drop) while still having consumed it from r
// (keeping the stream in sync). Returns the decision made, or an error:
// a plain error for I/O failures reading from r or writing to w, or an
// *ErrSessionFatal wrapping *rfb.ErrUnknownClientMessageType when the
// message type is unrecognized — per design doc §5, an unknown type means
// the parser can no longer know where the next message starts, which is
// unrecoverable and must terminate the session, not just this message.
func FilterOnce(r io.Reader, w io.Writer, readOnly bool) (FilterDecision, error) {
	msg, err := rfb.ReadClientMessage(r)
	if err != nil {
		var unknown *rfb.ErrUnknownClientMessageType
		if errors.As(err, &unknown) {
			return 0, &ErrSessionFatal{Err: err}
		}
		return 0, err
	}

	decision := Decide(msg, readOnly)
	if decision == DecisionForward {
		body := make([]byte, 0, 1+len(msg.Body))
		body = append(body, byte(msg.Type))
		body = append(body, msg.Body...)
		if _, err := w.Write(body); err != nil {
			return decision, fmt.Errorf("relay: forward client message: %w", err)
		}
	}
	return decision, nil
}

// RunClientFilter repeatedly applies FilterOnce to messages arriving on r
// until r is exhausted (io.EOF, reported as a nil error — a clean peer
// close is not a session failure) or a fatal error occurs (I/O failure,
// or *ErrSessionFatal for an unrecognized message type). This is the loop
// pump.go drives on the browser->server direction.
func RunClientFilter(r io.Reader, w io.Writer, readOnly bool) error {
	for {
		_, err := FilterOnce(r, w, readOnly)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

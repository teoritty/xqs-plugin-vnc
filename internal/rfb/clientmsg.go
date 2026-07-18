package rfb

import (
	"encoding/binary"
	"fmt"
	"io"
)

// ClientMessageType is an RFB client-to-server message type identifier
// (RFC 6143 §7.5).
type ClientMessageType byte

// Known client->server message types, per the design plan §5's table. Only
// these six are relevant to v1: they're the complete set a read-only
// filter needs to recognize to keep the stream in sync while selectively
// dropping input-producing messages.
const (
	MsgSetPixelFormat           ClientMessageType = 0
	MsgSetEncodings             ClientMessageType = 2
	MsgFramebufferUpdateRequest ClientMessageType = 3
	MsgKeyEvent                 ClientMessageType = 4
	MsgPointerEvent             ClientMessageType = 5
	MsgClientCutText            ClientMessageType = 6
)

func (t ClientMessageType) String() string {
	switch t {
	case MsgSetPixelFormat:
		return "SetPixelFormat(0)"
	case MsgSetEncodings:
		return "SetEncodings(2)"
	case MsgFramebufferUpdateRequest:
		return "FramebufferUpdateRequest(3)"
	case MsgKeyEvent:
		return "KeyEvent(4)"
	case MsgPointerEvent:
		return "PointerEvent(5)"
	case MsgClientCutText:
		return "ClientCutText(6)"
	default:
		return fmt.Sprintf("Unknown(%d)", uint8(t))
	}
}

// Fixed body lengths (bytes following the 1-byte type field) for the
// fixed-size message types, per RFC 6143 §7.5 and the design plan §5 table
// (total wire size minus the 1-byte type field already consumed).
const (
	setPixelFormatBodyLen           = 20 - 1
	framebufferUpdateRequestBodyLen = 10 - 1
	keyEventBodyLen                 = 8 - 1
	pointerEventBodyLen             = 6 - 1
)

// maxClientCutTextLen bounds ClientCutText's declared length so a
// malicious/corrupt peer claiming an absurd size can't force an unbounded
// allocation. 16 MiB is generous for clipboard text.
const maxClientCutTextLen = 16 << 20

// ClientMessage is one parsed client->server message: its type, and the
// raw bytes that followed the type byte on the wire (the full body,
// including any nested length/padding fields for variable-length types).
// A caller that wants to forward a message unmodified can reconstruct the
// exact original bytes as append([]byte{byte(m.Type)}, m.Body...).
type ClientMessage struct {
	Type ClientMessageType
	Body []byte
}

// ReadClientMessage reads exactly one client->server RFB message from r:
// the 1-byte type field, then the type-dependent body (fixed-size for
// SetPixelFormat/FramebufferUpdateRequest/KeyEvent/PointerEvent,
// variable-size for SetEncodings/ClientCutText, where the length is read
// from within the message itself per RFC 6143 §7.5).
//
// An unrecognized type returns *ErrUnknownClientMessageType, which callers
// must treat as fatal: see that error's doc comment for why silently
// skipping is not an option.
func ReadClientMessage(r io.Reader) (*ClientMessage, error) {
	var typeBuf [1]byte
	if _, err := io.ReadFull(r, typeBuf[:]); err != nil {
		return nil, wrapTruncated("client message type", err)
	}
	t := ClientMessageType(typeBuf[0])

	switch t {
	case MsgSetPixelFormat:
		body := make([]byte, setPixelFormatBodyLen)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, wrapTruncated("SetPixelFormat body", err)
		}
		return &ClientMessage{Type: t, Body: body}, nil

	case MsgFramebufferUpdateRequest:
		body := make([]byte, framebufferUpdateRequestBodyLen)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, wrapTruncated("FramebufferUpdateRequest body", err)
		}
		return &ClientMessage{Type: t, Body: body}, nil

	case MsgKeyEvent:
		body := make([]byte, keyEventBodyLen)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, wrapTruncated("KeyEvent body", err)
		}
		return &ClientMessage{Type: t, Body: body}, nil

	case MsgPointerEvent:
		body := make([]byte, pointerEventBodyLen)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, wrapTruncated("PointerEvent body", err)
		}
		return &ClientMessage{Type: t, Body: body}, nil

	case MsgSetEncodings:
		// 1 padding byte + u16 number-of-encodings, then n * 4 bytes of
		// encoding type IDs.
		header := make([]byte, 3)
		if _, err := io.ReadFull(r, header); err != nil {
			return nil, wrapTruncated("SetEncodings header", err)
		}
		n := binary.BigEndian.Uint16(header[1:3])
		encodings := make([]byte, int(n)*4)
		if _, err := io.ReadFull(r, encodings); err != nil {
			return nil, wrapTruncated("SetEncodings encoding list", err)
		}
		body := make([]byte, 0, len(header)+len(encodings))
		body = append(body, header...)
		body = append(body, encodings...)
		return &ClientMessage{Type: t, Body: body}, nil

	case MsgClientCutText:
		// 3 padding bytes + u32 length, then length bytes of text.
		header := make([]byte, 7)
		if _, err := io.ReadFull(r, header); err != nil {
			return nil, wrapTruncated("ClientCutText header", err)
		}
		length := binary.BigEndian.Uint32(header[3:7])
		if length > maxClientCutTextLen {
			return nil, fmt.Errorf("rfb: ClientCutText length %d exceeds sanity limit %d", length, maxClientCutTextLen)
		}
		text := make([]byte, length)
		if _, err := io.ReadFull(r, text); err != nil {
			return nil, wrapTruncated("ClientCutText body", err)
		}
		body := make([]byte, 0, len(header)+len(text))
		body = append(body, header...)
		body = append(body, text...)
		return &ClientMessage{Type: t, Body: body}, nil

	default:
		return nil, &ErrUnknownClientMessageType{Type: t}
	}
}

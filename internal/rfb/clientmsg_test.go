package rfb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestReadClientMessage_FixedSizeTypes(t *testing.T) {
	tests := []struct {
		name string
		typ  ClientMessageType
		body []byte
	}{
		{"SetPixelFormat", MsgSetPixelFormat, make([]byte, setPixelFormatBodyLen)},
		{"FramebufferUpdateRequest", MsgFramebufferUpdateRequest, make([]byte, framebufferUpdateRequestBodyLen)},
		{"KeyEvent", MsgKeyEvent, make([]byte, keyEventBodyLen)},
		{"PointerEvent", MsgPointerEvent, make([]byte, pointerEventBodyLen)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for i := range tc.body {
				tc.body[i] = byte(i + 1)
			}
			var buf bytes.Buffer
			buf.WriteByte(byte(tc.typ))
			buf.Write(tc.body)

			msg, err := ReadClientMessage(&buf)
			if err != nil {
				t.Fatalf("ReadClientMessage: %v", err)
			}
			if msg.Type != tc.typ {
				t.Errorf("Type = %v, want %v", msg.Type, tc.typ)
			}
			if !bytes.Equal(msg.Body, tc.body) {
				t.Errorf("Body = %v, want %v", msg.Body, tc.body)
			}
		})
	}
}

func TestReadClientMessage_SetEncodingsVariousN(t *testing.T) {
	for _, n := range []uint16{0, 1, 5, 300} {
		t.Run("", func(t *testing.T) {
			var buf bytes.Buffer
			buf.WriteByte(byte(MsgSetEncodings))
			buf.WriteByte(0) // padding
			var nBuf [2]byte
			binary.BigEndian.PutUint16(nBuf[:], n)
			buf.Write(nBuf[:])
			encodings := make([]byte, int(n)*4)
			for i := range encodings {
				encodings[i] = byte(i)
			}
			buf.Write(encodings)

			msg, err := ReadClientMessage(&buf)
			if err != nil {
				t.Fatalf("ReadClientMessage n=%d: %v", n, err)
			}
			if msg.Type != MsgSetEncodings {
				t.Errorf("Type = %v, want SetEncodings", msg.Type)
			}
			wantLen := 3 + int(n)*4
			if len(msg.Body) != wantLen {
				t.Errorf("Body len = %d, want %d", len(msg.Body), wantLen)
			}
			if got := binary.BigEndian.Uint16(msg.Body[1:3]); got != n {
				t.Errorf("embedded n = %d, want %d", got, n)
			}
		})
	}
}

func TestReadClientMessage_ClientCutTextVariousN(t *testing.T) {
	for _, n := range []uint32{0, 1, 10, 5000} {
		t.Run("", func(t *testing.T) {
			var buf bytes.Buffer
			buf.WriteByte(byte(MsgClientCutText))
			buf.Write([]byte{0, 0, 0}) // padding
			var lenBuf [4]byte
			binary.BigEndian.PutUint32(lenBuf[:], n)
			buf.Write(lenBuf[:])
			text := make([]byte, n)
			for i := range text {
				text[i] = byte(i)
			}
			buf.Write(text)

			msg, err := ReadClientMessage(&buf)
			if err != nil {
				t.Fatalf("ReadClientMessage n=%d: %v", n, err)
			}
			wantLen := 7 + int(n)
			if len(msg.Body) != wantLen {
				t.Errorf("Body len = %d, want %d", len(msg.Body), wantLen)
			}
		})
	}
}

func TestReadClientMessage_ClientCutTextOversizeRejected(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(byte(MsgClientCutText))
	buf.Write([]byte{0, 0, 0})
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], maxClientCutTextLen+1)
	buf.Write(lenBuf[:])

	_, err := ReadClientMessage(&buf)
	if err == nil {
		t.Fatal("expected error for oversize ClientCutText length")
	}
}

func TestReadClientMessage_UnknownType(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(200) // not a known type
	_, err := ReadClientMessage(&buf)
	var unknown *ErrUnknownClientMessageType
	if !errors.As(err, &unknown) {
		t.Fatalf("err = %v, want *ErrUnknownClientMessageType", err)
	}
	if unknown.Type != ClientMessageType(200) {
		t.Errorf("Type = %v, want 200", unknown.Type)
	}
}

func TestReadClientMessage_TruncatedAtType(t *testing.T) {
	var buf bytes.Buffer
	_, err := ReadClientMessage(&buf)
	var trunc *ErrTruncated
	if !errors.As(err, &trunc) {
		t.Fatalf("err = %v, want *ErrTruncated", err)
	}
}

func TestReadClientMessage_TruncatedAtFixedBody(t *testing.T) {
	cases := []ClientMessageType{MsgSetPixelFormat, MsgFramebufferUpdateRequest, MsgKeyEvent, MsgPointerEvent}
	for _, typ := range cases {
		var buf bytes.Buffer
		buf.WriteByte(byte(typ))
		buf.Write([]byte{1, 2}) // short body
		_, err := ReadClientMessage(&buf)
		var trunc *ErrTruncated
		if !errors.As(err, &trunc) {
			t.Errorf("type %v: err = %v, want *ErrTruncated", typ, err)
		}
	}
}

func TestReadClientMessage_TruncatedAtSetEncodingsHeader(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(byte(MsgSetEncodings))
	buf.WriteByte(0) // only 1 of 3 header bytes
	_, err := ReadClientMessage(&buf)
	var trunc *ErrTruncated
	if !errors.As(err, &trunc) {
		t.Fatalf("err = %v, want *ErrTruncated", err)
	}
}

func TestReadClientMessage_TruncatedAtSetEncodingsList(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(byte(MsgSetEncodings))
	buf.WriteByte(0)
	var nBuf [2]byte
	binary.BigEndian.PutUint16(nBuf[:], 3)
	buf.Write(nBuf[:])
	buf.Write([]byte{1, 2, 3}) // short; want 12 bytes
	_, err := ReadClientMessage(&buf)
	var trunc *ErrTruncated
	if !errors.As(err, &trunc) {
		t.Fatalf("err = %v, want *ErrTruncated", err)
	}
}

func TestReadClientMessage_TruncatedAtClientCutTextHeader(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(byte(MsgClientCutText))
	buf.Write([]byte{0, 0}) // only 2 of 7 header bytes
	_, err := ReadClientMessage(&buf)
	var trunc *ErrTruncated
	if !errors.As(err, &trunc) {
		t.Fatalf("err = %v, want *ErrTruncated", err)
	}
}

func TestReadClientMessage_TruncatedAtClientCutTextBody(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(byte(MsgClientCutText))
	buf.Write([]byte{0, 0, 0})
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], 10)
	buf.Write(lenBuf[:])
	buf.Write([]byte{1, 2, 3}) // short; want 10 bytes
	_, err := ReadClientMessage(&buf)
	var trunc *ErrTruncated
	if !errors.As(err, &trunc) {
		t.Fatalf("err = %v, want *ErrTruncated", err)
	}
}

func TestClientMessageType_String(t *testing.T) {
	if MsgKeyEvent.String() == "" {
		t.Fatal("String() returned empty for known type")
	}
	if got := ClientMessageType(250).String(); got != "Unknown(250)" {
		t.Errorf("String() = %q, want Unknown(250)", got)
	}
}

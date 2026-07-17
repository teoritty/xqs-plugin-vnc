package ipc

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	cases := []struct {
		name      string
		kind      Kind
		channelID uint32
		payload   []byte
	}{
		{"jsonrpc control plane", KindJSONRPC, 0, []byte(`{"jsonrpc":"2.0","method":"ping"}`)},
		{"channel data", KindChannelData, 7, []byte{0x01, 0x02, 0x03}},
		{"credit update", KindCredit, 3, make([]byte, 8)},
		{"empty payload", KindJSONRPC, 0, nil},
		{"channelId zero with data kind", KindChannelData, 0, []byte("x")},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			enc := NewEncoder(&buf)
			if err := enc.Encode(c.kind, c.channelID, c.payload); err != nil {
				t.Fatalf("Encode: %v", err)
			}

			dec := NewDecoder(&buf)
			f, err := dec.Decode()
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if f.Kind != c.kind {
				t.Errorf("kind = %v, want %v", f.Kind, c.kind)
			}
			if f.ChannelID != c.channelID {
				t.Errorf("channelID = %v, want %v", f.ChannelID, c.channelID)
			}
			if !bytes.Equal(f.Payload, c.payload) {
				t.Errorf("payload = %v, want %v", f.Payload, c.payload)
			}
		})
	}
}

func TestDecodeRejectsOversizeLength(t *testing.T) {
	var buf bytes.Buffer
	// Write a header claiming a payload larger than MaxFrameLength, no payload bytes.
	hdr := make([]byte, headerLen)
	putUint32(hdr[0:4], MaxFrameLength+1)
	hdr[4] = byte(KindJSONRPC)
	putUint32(hdr[5:9], 0)
	buf.Write(hdr)

	dec := NewDecoder(&buf)
	_, err := dec.Decode()
	if err == nil {
		t.Fatal("expected error for oversize length, got nil")
	}
	var pv *ProtocolViolationError
	if !errors.As(err, &pv) {
		t.Fatalf("expected ProtocolViolationError, got %T: %v", err, err)
	}
}

func TestDecodeRejectsReservedKind(t *testing.T) {
	for k := 0x04; k <= 0x0F; k++ {
		var buf bytes.Buffer
		hdr := make([]byte, headerLen)
		putUint32(hdr[0:4], 0)
		hdr[4] = byte(k)
		putUint32(hdr[5:9], 0)
		buf.Write(hdr)

		dec := NewDecoder(&buf)
		_, err := dec.Decode()
		if err == nil {
			t.Fatalf("kind 0x%02x: expected error, got nil", k)
		}
		var pv *ProtocolViolationError
		if !errors.As(err, &pv) {
			t.Fatalf("kind 0x%02x: expected ProtocolViolationError, got %T: %v", k, err, err)
		}
	}
}

func TestDecodeChannelIDZeroIsControlPlane(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	if err := enc.Encode(KindJSONRPC, 0, []byte("{}")); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	dec := NewDecoder(&buf)
	f, err := dec.Decode()
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if f.ChannelID != 0 {
		t.Errorf("channelID = %v, want 0", f.ChannelID)
	}
	if !f.IsControlPlane() {
		t.Errorf("expected IsControlPlane() true for channelId=0")
	}
}

func TestDecodeTruncatedHeaderReturnsIOError(t *testing.T) {
	buf := bytes.NewBuffer([]byte{0x00, 0x00, 0x00}) // only 3 of 9 header bytes
	dec := NewDecoder(buf)
	_, err := dec.Decode()
	if err == nil {
		t.Fatal("expected error for truncated header, got nil")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF or io.ErrUnexpectedEOF, got %v", err)
	}
}

func TestDecodeTruncatedPayloadReturnsIOError(t *testing.T) {
	var buf bytes.Buffer
	hdr := make([]byte, headerLen)
	putUint32(hdr[0:4], 10) // claims 10 bytes of payload
	hdr[4] = byte(KindJSONRPC)
	putUint32(hdr[5:9], 0)
	buf.Write(hdr)
	buf.Write([]byte{0x01, 0x02, 0x03}) // only 3 bytes present

	dec := NewDecoder(&buf)
	_, err := dec.Decode()
	if err == nil {
		t.Fatal("expected error for truncated payload, got nil")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF or io.ErrUnexpectedEOF, got %v", err)
	}
}

func TestEncodeRejectsOversizePayload(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	oversized := make([]byte, MaxFrameLength+1)
	err := enc.Encode(KindChannelData, 1, oversized)
	if err == nil {
		t.Fatal("expected error encoding oversize payload, got nil")
	}
}

func TestEncodeRejectsReservedKind(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	err := enc.Encode(Kind(0x05), 1, nil)
	if err == nil {
		t.Fatal("expected error encoding reserved kind, got nil")
	}
}

func TestMultipleFramesSequentialDecode(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	payloads := [][]byte{[]byte("one"), []byte("two"), []byte("three")}
	for i, p := range payloads {
		if err := enc.Encode(KindJSONRPC, uint32(i), p); err != nil {
			t.Fatalf("Encode %d: %v", i, err)
		}
	}

	dec := NewDecoder(&buf)
	for i, want := range payloads {
		f, err := dec.Decode()
		if err != nil {
			t.Fatalf("Decode %d: %v", i, err)
		}
		if !bytes.Equal(f.Payload, want) {
			t.Errorf("frame %d payload = %v, want %v", i, f.Payload, want)
		}
	}
	if _, err := dec.Decode(); err != io.EOF {
		t.Fatalf("expected io.EOF at end of stream, got %v", err)
	}
}

package relay

import (
	"bytes"
	"errors"
	"testing"

	"xqs-plugin-vnc/internal/rfb"
)

func TestFilterOnce_InputMessagesDroppedWhenReadOnly(t *testing.T) {
	tests := []struct {
		name string
		wire []byte
	}{
		{"KeyEvent", []byte{4, 1, 0, 0, 0, 0, 0, 0x41}},
		{"PointerEvent", []byte{5, 0, 0, 10, 0, 20}},
		{"ClientCutText", []byte{6, 0, 0, 0, 0, 0, 0, 3, 'h', 'i', '!'}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := bytes.NewReader(tc.wire)
			var w bytes.Buffer
			decision, err := FilterOnce(r, &w, true)
			if err != nil {
				t.Fatalf("FilterOnce: %v", err)
			}
			if decision != DecisionDrop {
				t.Errorf("decision = %v, want DecisionDrop", decision)
			}
			if w.Len() != 0 {
				t.Errorf("w.Len() = %d, want 0 (dropped message must not be forwarded)", w.Len())
			}
			if r.Len() != 0 {
				t.Errorf("r.Len() = %d, want 0 (message must be fully consumed even when dropped)", r.Len())
			}
		})
	}
}

func TestFilterOnce_InputMessagesForwardedWhenNotReadOnly(t *testing.T) {
	tests := []struct {
		name string
		wire []byte
	}{
		{"KeyEvent", []byte{4, 1, 0, 0, 0, 0, 0, 0x41}},
		{"PointerEvent", []byte{5, 0, 0, 10, 0, 20}},
		{"ClientCutText", []byte{6, 0, 0, 0, 0, 0, 0, 3, 'h', 'i', '!'}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := bytes.NewReader(tc.wire)
			var w bytes.Buffer
			decision, err := FilterOnce(r, &w, false)
			if err != nil {
				t.Fatalf("FilterOnce: %v", err)
			}
			if decision != DecisionForward {
				t.Errorf("decision = %v, want DecisionForward", decision)
			}
			if !bytes.Equal(w.Bytes(), tc.wire) {
				t.Errorf("forwarded bytes = %x, want %x (byte-for-byte)", w.Bytes(), tc.wire)
			}
		})
	}
}

func TestFilterOnce_AlwaysForwardedRegardlessOfReadOnly(t *testing.T) {
	tests := []struct {
		name string
		wire []byte
	}{
		{"SetPixelFormat", append([]byte{0, 0, 0, 0}, make([]byte, 16)...)},
		{"SetEncodings", []byte{2, 0, 0, 2, 0, 0, 0, 1, 0, 0, 0, 2}},
		{"FramebufferUpdateRequest", []byte{3, 0, 0, 0, 0, 0, 0, 10, 0, 10}},
	}
	for _, tc := range tests {
		for _, ro := range []bool{true, false} {
			r := bytes.NewReader(tc.wire)
			var w bytes.Buffer
			decision, err := FilterOnce(r, &w, ro)
			if err != nil {
				t.Fatalf("%s readOnly=%v: FilterOnce: %v", tc.name, ro, err)
			}
			if decision != DecisionForward {
				t.Errorf("%s readOnly=%v: decision = %v, want DecisionForward", tc.name, ro, decision)
			}
			if !bytes.Equal(w.Bytes(), tc.wire) {
				t.Errorf("%s readOnly=%v: forwarded = %x, want %x", tc.name, ro, w.Bytes(), tc.wire)
			}
		}
	}
}

func TestFilterOnce_UnknownTypeIsSessionFatal(t *testing.T) {
	r := bytes.NewReader([]byte{200, 1, 2, 3})
	var w bytes.Buffer
	_, err := FilterOnce(r, &w, false)
	if err == nil {
		t.Fatal("FilterOnce: want error for unknown message type, got nil")
	}
	var fatal *ErrSessionFatal
	if !errors.As(err, &fatal) {
		t.Fatalf("err = %v (%T), want *ErrSessionFatal", err, err)
	}
	var unknown *rfb.ErrUnknownClientMessageType
	if !errors.As(err, &unknown) {
		t.Errorf("err does not wrap *rfb.ErrUnknownClientMessageType: %v", err)
	}
	if w.Len() != 0 {
		t.Errorf("w.Len() = %d, want 0", w.Len())
	}
}

func TestRunClientFilter_MultipleMessagesAndCleanEOF(t *testing.T) {
	// KeyEvent, then PointerEvent, then clean EOF.
	wire := append(
		[]byte{4, 1, 0, 0, 0, 0, 0, 0x41},
		[]byte{5, 0, 0, 10, 0, 20}...,
	)
	r := bytes.NewReader(wire)
	var w bytes.Buffer
	if err := RunClientFilter(r, &w, false); err != nil {
		t.Fatalf("RunClientFilter: %v", err)
	}
	if !bytes.Equal(w.Bytes(), wire) {
		t.Errorf("forwarded = %x, want %x", w.Bytes(), wire)
	}
}

func TestRunClientFilter_StopsAtUnknownType(t *testing.T) {
	wire := []byte{4, 1, 0, 0, 0, 0, 0, 0x41, 99}
	r := bytes.NewReader(wire)
	var w bytes.Buffer
	err := RunClientFilter(r, &w, false)
	var fatal *ErrSessionFatal
	if !errors.As(err, &fatal) {
		t.Fatalf("err = %v, want *ErrSessionFatal", err)
	}
}

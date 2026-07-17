package ipc

import (
	"context"
	"errors"
	"testing"
)

func TestDispatchRoutesControlPlaneToRPCHandler(t *testing.T) {
	var gotKind MessageKind
	var called bool
	d := NewDispatcher(RPCHandlerFunc(func(ctx context.Context, kind MessageKind, msg any) error {
		called = true
		gotKind = kind
		return nil
	}), nil)

	f := Frame{Kind: KindJSONRPC, ChannelID: 0, Payload: []byte(`{"jsonrpc":"2.0","method":"ping"}`)}
	if err := d.Dispatch(context.Background(), f); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !called {
		t.Fatal("RPC handler was not called")
	}
	if gotKind != MessageNotification {
		t.Errorf("kind = %v, want MessageNotification", gotKind)
	}
}

func TestDispatchRoutesNonZeroChannelToChannelHandler(t *testing.T) {
	var called bool
	var gotChannelID uint32
	d := NewDispatcher(nil, ChannelDataHandlerFunc(func(ctx context.Context, f Frame) error {
		called = true
		gotChannelID = f.ChannelID
		return nil
	}))

	f := Frame{Kind: KindChannelData, ChannelID: 7, Payload: []byte{1, 2, 3}}
	if err := d.Dispatch(context.Background(), f); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !called {
		t.Fatal("channel handler was not called")
	}
	if gotChannelID != 7 {
		t.Errorf("channelID = %d, want 7", gotChannelID)
	}
}

func TestDispatchRoutesCreditFrameToChannelHandler(t *testing.T) {
	var called bool
	d := NewDispatcher(nil, ChannelDataHandlerFunc(func(ctx context.Context, f Frame) error {
		called = true
		return nil
	}))
	f := Frame{Kind: KindCredit, ChannelID: 3, Payload: make([]byte, 8)}
	if err := d.Dispatch(context.Background(), f); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !called {
		t.Fatal("channel handler was not called for credit frame")
	}
}

func TestDispatchRejectsNonJSONRPCKindOnControlPlane(t *testing.T) {
	d := NewDispatcher(RPCHandlerFunc(func(ctx context.Context, kind MessageKind, msg any) error {
		return nil
	}), nil)
	f := Frame{Kind: KindChannelData, ChannelID: 0, Payload: []byte("x")}
	err := d.Dispatch(context.Background(), f)
	if err == nil {
		t.Fatal("expected error for non-JSON-RPC kind on channel 0, got nil")
	}
	var pv *ProtocolViolationError
	if !errors.As(err, &pv) {
		t.Fatalf("expected ProtocolViolationError, got %T: %v", err, err)
	}
}

func TestDispatchRejectsOversizeJSONRPCFrame(t *testing.T) {
	d := NewDispatcher(RPCHandlerFunc(func(ctx context.Context, kind MessageKind, msg any) error {
		return nil
	}), nil)
	oversized := make([]byte, MaxJSONRPCFrameLength+1)
	f := Frame{Kind: KindJSONRPC, ChannelID: 0, Payload: oversized}
	err := d.Dispatch(context.Background(), f)
	if err == nil {
		t.Fatal("expected error for oversize JSON-RPC frame, got nil")
	}
	var pv *ProtocolViolationError
	if !errors.As(err, &pv) {
		t.Fatalf("expected ProtocolViolationError, got %T: %v", err, err)
	}
}

// TestDispatchAllowsLargeChannelDataFrame verifies that a kind=0x02
// (channel data) frame well above the 256 KiB JSON-RPC ceiling but still
// under codec.go's 1 MiB MaxFrameLength is NOT rejected by this layer —
// the 256 KiB cap is JSON-RPC-specific, not a generic frame cap.
func TestDispatchAllowsLargeChannelDataFrame(t *testing.T) {
	var called bool
	d := NewDispatcher(nil, ChannelDataHandlerFunc(func(ctx context.Context, f Frame) error {
		called = true
		return nil
	}))
	large := make([]byte, MaxJSONRPCFrameLength+1) // bigger than 256 KiB, well under 1 MiB
	f := Frame{Kind: KindChannelData, ChannelID: 1, Payload: large}
	if err := d.Dispatch(context.Background(), f); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !called {
		t.Fatal("channel handler was not called for large channel-data frame")
	}
}

func TestDispatchReturnsErrorForNilRPCHandler(t *testing.T) {
	d := NewDispatcher(nil, nil)
	f := Frame{Kind: KindJSONRPC, ChannelID: 0, Payload: []byte(`{"jsonrpc":"2.0","method":"ping"}`)}
	if err := d.Dispatch(context.Background(), f); err == nil {
		t.Fatal("expected error for nil RPC handler, got nil")
	}
}

func TestDispatchReturnsErrorForNilChannelHandler(t *testing.T) {
	d := NewDispatcher(nil, nil)
	f := Frame{Kind: KindChannelData, ChannelID: 1, Payload: []byte("x")}
	if err := d.Dispatch(context.Background(), f); err == nil {
		t.Fatal("expected error for nil channel handler, got nil")
	}
}

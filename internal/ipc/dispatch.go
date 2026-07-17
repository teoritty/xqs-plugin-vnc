// This file (dispatch.go) routes decoded frames by channelId: channelId
// == 0 is the JSON-RPC control plane (parsed with rpc.go and handed to a
// caller-supplied RPCHandler); any other channelId belongs to the binary
// channel bus, whose actual logic doesn't exist yet — ChannelDataHandler
// here is only the routing seam a later task implements against.
//
// This file also enforces the JSON-RPC-specific 256 KiB frame ceiling
// from docs/plugin-api.md ("Max NDJSON frame size: 256 KiB" /
// "0x01 JSON-RPC ... 256 KiB cap, unchanged"). codec.go's MaxFrameLength
// is deliberately the kind-agnostic 1 MiB defensive ceiling; the tighter,
// kind=0x01-specific ceiling belongs at this layer, which is the first
// place that knows a frame is a JSON-RPC frame specifically.
package ipc

import "context"

// MaxJSONRPCFrameLength is the maximum payload length permitted for a
// kind=KindJSONRPC frame, per docs/plugin-api.md. A frame at or below
// this size is fine even if it approaches codec.MaxFrameLength; a
// kind=KindJSONRPC frame above it is a protocol violation even though
// codec.go alone would have accepted it (it only rejects above 1 MiB).
const MaxJSONRPCFrameLength = 256 * 1024 // 256 KiB

// RPCHandler processes a single decoded JSON-RPC message from the
// control plane (channelId 0). kind indicates which concrete type msg
// is (*Request, *Notification, or *Response); implementations type-switch
// on it. Actual RPC method wiring (initialize, session.connect, ping,
// etc.) is a later task's responsibility — this interface is only the
// seam that later code implements.
type RPCHandler interface {
	HandleRPC(ctx context.Context, kind MessageKind, msg any) error
}

// RPCHandlerFunc adapts a plain function to RPCHandler.
type RPCHandlerFunc func(ctx context.Context, kind MessageKind, msg any) error

// HandleRPC implements RPCHandler.
func (f RPCHandlerFunc) HandleRPC(ctx context.Context, kind MessageKind, msg any) error {
	return f(ctx, kind, msg)
}

// ChannelDataHandler processes a single decoded frame belonging to the
// binary channel bus (channelId != 0): either KindChannelData payload
// bytes or a KindCredit flow-control update. It is a pure routing seam —
// no channel-bus semantics (backpressure, purposes, session binding)
// live in this package; those belong to code that doesn't exist yet.
type ChannelDataHandler interface {
	HandleChannelFrame(ctx context.Context, f Frame) error
}

// ChannelDataHandlerFunc adapts a plain function to ChannelDataHandler.
type ChannelDataHandlerFunc func(ctx context.Context, f Frame) error

// HandleChannelFrame implements ChannelDataHandler.
func (f ChannelDataHandlerFunc) HandleChannelFrame(ctx context.Context, fr Frame) error {
	return f(ctx, fr)
}

// Dispatcher routes decoded frames to the appropriate handler based on
// channelId, per docs/superpowers/specs/2026-07-16-vnc-plugin-design.md
// §2's description of dispatch.go: "Маршрутизация JSON-RPC ↔ бинарных
// каналов" (routing between JSON-RPC and binary channels).
type Dispatcher struct {
	RPC     RPCHandler
	Channel ChannelDataHandler
}

// NewDispatcher returns a Dispatcher that routes control-plane frames to
// rpc and channel-bus frames to channel. Either may be nil; Dispatch
// returns an error if it needs to route to a nil handler.
func NewDispatcher(rpc RPCHandler, channel ChannelDataHandler) *Dispatcher {
	return &Dispatcher{RPC: rpc, Channel: channel}
}

// Dispatch routes a single decoded Frame. Frames with ChannelID == 0 are
// treated as JSON-RPC control-plane traffic: f.Kind must be KindJSONRPC
// (anything else on channel 0 is a protocol violation, since codec.go
// only ever produces the three defined kinds and the control plane only
// carries JSON-RPC), the payload is size-checked against
// MaxJSONRPCFrameLength, decoded via DecodeMessage, and handed to
// d.RPC. Frames with ChannelID != 0 are handed to d.Channel verbatim,
// regardless of kind — the channel bus decides what to do with
// KindChannelData vs. KindCredit.
func (d *Dispatcher) Dispatch(ctx context.Context, f Frame) error {
	if f.ChannelID == 0 {
		if f.Kind != KindJSONRPC {
			return &ProtocolViolationError{
				Reason: "non-JSON-RPC kind on control plane",
				Kind:   f.Kind,
				Length: uint32(len(f.Payload)),
			}
		}
		if len(f.Payload) > MaxJSONRPCFrameLength {
			return &ProtocolViolationError{
				Reason: "oversize JSON-RPC frame",
				Kind:   f.Kind,
				Length: uint32(len(f.Payload)),
			}
		}
		if d.RPC == nil {
			return errNoRPCHandler
		}
		kind, msg, err := DecodeMessage(f.Payload)
		if err != nil {
			return err
		}
		return d.RPC.HandleRPC(ctx, kind, msg)
	}

	if d.Channel == nil {
		return errNoChannelHandler
	}
	return d.Channel.HandleChannelFrame(ctx, f)
}

var errNoRPCHandler = &dispatchConfigError{"no RPCHandler configured"}
var errNoChannelHandler = &dispatchConfigError{"no ChannelDataHandler configured"}

// dispatchConfigError reports a Dispatcher used without a required
// handler wired up; this is a caller programming error, not a protocol
// violation from the wire.
type dispatchConfigError struct {
	msg string
}

func (e *dispatchConfigError) Error() string { return "ipc: " + e.msg }

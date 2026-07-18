// This file (open.go) wires channel.open/channel.close (the JSON-RPC
// negotiation, per docs/plugin-api.md "channel.open / channel.close") to
// credit.go's real credit-windowed CreditChannel and channel.go's Channel
// (the net.Conn from Task 7), with the correct per-purpose initial credit
// and max frame payload built in per docs/plugin-api.md "Flow control
// (credit)" and API-FINDINGS.md F-8.
package transport

import (
	"context"
	"fmt"
)

// Purpose values recognized by channel.open, per docs/plugin-api.md
// "Purpose".
const (
	PurposeTCPRelay    = "tcp-relay"
	PurposeEmbedStream = "embed-stream"
)

// Per-purpose initial credit (docs/plugin-api.md "Flow control (credit)":
// "On channel.open, the receiver grants an initial credit: 4 frames for
// exec/tcp-relay/udp-relay, 8 frames for embed-stream") and max frame
// payload. tcp-relay uses the general 1 MiB channel-bus ceiling
// (ipc.MaxFrameLength / DefaultMaxFramePayload); embed-stream does NOT —
// API-FINDINGS.md F-8 documents that the host actually enforces a
// separate, smaller 64 KiB ceiling for embed-stream specifically
// (MaxTunnelFrameSize in the host's channel.deliver), and a plugin that
// splits at 1 MiB for embed-stream gets killed on the first oversize
// frame. Both numbers are asserted explicitly in open_test.go.
const (
	tcpRelayInitialCredit    = 4
	embedStreamInitialCredit = 8

	tcpRelayMaxFramePayload    = 1024 * 1024 // 1 MiB
	embedStreamMaxFramePayload = 64 * 1024   // 64 KiB, per API-FINDINGS.md F-8
)

// Exported aliases of the per-purpose initial credit constants above, for
// callers outside this package that need the same numbers without
// duplicating them — notably internal/relay/coupling.go, which needs
// embed-stream's full credit-window capacity to compute how depleted the
// window currently is (see CreditChannel.Remaining / Channel.SendCreditRemaining).
const (
	TCPRelayInitialCredit    = tcpRelayInitialCredit
	EmbedStreamInitialCredit = embedStreamInitialCredit
)

// purposeParams returns the initial sender-side credit and max frame
// payload for purpose, or an error if purpose isn't one this plugin knows
// how to open (exec/udp-relay are valid host-side purposes per
// docs/plugin-api.md, but this plugin only ever opens tcp-relay or
// embed-stream channels itself).
func purposeParams(purpose string) (initialCredit, maxPayload int, err error) {
	switch purpose {
	case PurposeTCPRelay:
		return tcpRelayInitialCredit, tcpRelayMaxFramePayload, nil
	case PurposeEmbedStream:
		return embedStreamInitialCredit, embedStreamMaxFramePayload, nil
	default:
		return 0, 0, fmt.Errorf("transport: unknown channel purpose %q", purpose)
	}
}

// RPCCaller is the seam OpenChannel/CloseChannel need to talk to the host
// over the JSON-RPC control plane. No concrete implementation exists yet
// in this repo (the process-wide request/response loop — tracking pending
// call ids against internal/ipc.Encoder/Decoder — is a later task's
// responsibility, e.g. a top-level client wired at the composition root);
// this file only defines the interface it needs and is tested against a
// fake (see open_test.go).
//
// The split between Call and Notify matches docs/plugin-api.md's
// channel.open/channel.close table exactly: channel.open is a synchronous
// request expecting {"channelId":...} back (10 s timeout, not the
// standard 5 s RPC timeout); channel.close is documented as "notification
// (no response)" — sent, not awaited.
type RPCCaller interface {
	// Call sends method with params as a JSON-RPC request and decodes the
	// response's result into result (a pointer), or returns the response's
	// error / a transport failure.
	Call(ctx context.Context, method string, params any, result any) error
	// Notify sends method with params as a JSON-RPC notification: no
	// response is expected or awaited.
	Notify(ctx context.Context, method string, params any) error
}

// channelOpenParams is the request shape for channel.open, per
// docs/plugin-api.md: `{"purpose":"...","parentSessionId":"...","hint":"..."}`.
type channelOpenParams struct {
	Purpose         string `json:"purpose"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
	Hint            string `json:"hint,omitempty"`
}

// channelOpenResult is the response shape for channel.open, per
// docs/plugin-api.md: `{"channelId":<uint32>}`.
type channelOpenResult struct {
	ChannelID uint32 `json:"channelId"`
}

// channelCloseParams is the notification shape for channel.close, per
// docs/plugin-api.md: `{"channelId":<uint32>,"reason":"...","message":"..."}`.
type channelCloseParams struct {
	ChannelID uint32 `json:"channelId"`
	Reason    string `json:"reason,omitempty"`
	Message   string `json:"message,omitempty"`
}

// OpenChannel performs channel.open against caller for purpose
// (PurposeTCPRelay or PurposeEmbedStream), then builds a net.Conn-shaped
// Channel over a real credit-windowed CreditChannel using the channelId
// the host allocated and the correct per-purpose initial credit / max
// frame payload. If reg is non-nil, the underlying CreditChannel is
// registered with it so subsequent kind=0x02/kind=0x03 frames for this
// channelId (routed by internal/ipc's Dispatcher via reg.HandleChannelFrame)
// reach it; reg is typically a single process-wide Registry shared by every
// open channel, wired at the composition root alongside the Decoder loop.
func OpenChannel(ctx context.Context, caller RPCCaller, enc FrameWriter, reg *Registry, purpose, parentSessionID, hint string) (*Channel, error) {
	initialCredit, maxPayload, err := purposeParams(purpose)
	if err != nil {
		return nil, err
	}

	var res channelOpenResult
	if err := caller.Call(ctx, "channel.open", channelOpenParams{
		Purpose:         purpose,
		ParentSessionID: parentSessionID,
		Hint:            hint,
	}, &res); err != nil {
		return nil, fmt.Errorf("transport: channel.open(%s): %w", purpose, err)
	}

	cc := NewCreditChannel(res.ChannelID, enc, initialCredit, maxPayload)
	if reg != nil {
		reg.Register(cc)
	}

	addr := Addr{ChannelID: res.ChannelID, Purpose: purpose}
	return NewChannel(cc, cc, addr, addr, maxPayload), nil
}

// CloseChannel closes ch locally (idempotent, safe to call more than once
// or concurrently with an in-flight Read/Write/Send/Recv — see channel.go
// and credit.go) and, if caller is non-nil, sends channel.close to the
// host as a best-effort notification. reason/message are optional
// (docs/plugin-api.md marks both "?"); pass "" for either to omit.
//
// CloseChannel always closes ch locally even if the channel.close
// notification fails to send — a failure there almost always means the
// stdio pipe to the host is already gone, in which case there is nothing
// further to notify anyway.
func CloseChannel(ctx context.Context, caller RPCCaller, ch *Channel, reason, message string) error {
	cc, ok := ch.source.(*CreditChannel)
	if !ok {
		return ch.Close()
	}

	cc.Close()
	closeErr := ch.Close()

	if caller != nil {
		notifyErr := caller.Notify(ctx, "channel.close", channelCloseParams{
			ChannelID: cc.id,
			Reason:    reason,
			Message:   message,
		})
		if closeErr == nil {
			closeErr = notifyErr
		}
	}
	return closeErr
}

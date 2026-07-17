// This file (credit.go) is the real FrameSink/FrameSource implementation
// for an open channel-bus channel (see conn.go for the seam Task 7
// defined), backed by actual credit-window accounting per
// docs/plugin-api.md "Flow control (credit)" and
// docs/superpowers/specs/2026-07-16-vnc-plugin-design.md §3's "Кредиты — и
// почему две трубы ведут себя по-разному" / "credit.go — оба направления".
//
// Two directions, two different mechanisms:
//
//   - Sender side (this plugin sending kind=0x02 data to the host): a
//     counted credit window. CreditChannel.Send (satisfying FrameSink)
//     blocks while the window is exhausted and wakes either when a
//     kind=0x03 grant frame arrives from the host or the channel closes —
//     per the design doc, sending past granted credit is a protocol
//     violation that gets the whole process killed, so this must block,
//     never error, on exhaustion.
//   - Receiver side (this plugin receiving kind=0x02 data from the host):
//     CreditChannel.Recv (satisfying FrameSource) simply delivers payloads
//     as they arrive; granting more credit back to the host is a
//     *separate*, explicitly-invoked operation (GrantCredit) — not
//     automatic on every Recv call. Phase 3d's relay/coupling.go drives
//     the policy of *when* to call it for embed-stream (deliberately
//     withholding grants is the plugin's only backpressure lever on a VNC
//     server that doesn't understand real TCP backpressure); tcp-relay's
//     policy (grant promptly after each consumed frame) is likewise a
//     caller concern, not something this file hard-codes.
package transport

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"xqs-plugin-vnc/internal/ipc"
)

// ErrNoCredit is the sentinel this file introduces to close the gap Task
// 7's report flagged: FrameSink.Send/FrameSource.Recv return a single
// opaque error/bool, with no way to distinguish "blocked, no credit right
// now" from "channel closed" from "a generic wire/I-O error". The blocking
// FrameSink.Send method itself never returns ErrNoCredit — per the design
// doc it must block on exhaustion, not fail — but TrySend (a non-blocking
// variant exposed for callers, e.g. future coupling logic, that need to
// probe without parking a goroutine) returns it, and it is documented here
// as the vocabulary the closed/generic-error split is defined against:
//
//   - ErrNoCredit: window exhausted, channel still open, would block.
//   - ErrClosed (conn.go): channel closed, out from under caller or via
//     Close(); no further Send/Recv will ever succeed.
//   - anything else wrapped in a plain error: a real encode/wire failure
//     talking to the host, distinct from both of the above.
var ErrNoCredit = errors.New("transport: no credit available")

// FrameWriter is the seam credit.go needs to put a frame on the wire.
// *ipc.Encoder satisfies it directly; SharedEncoder wraps one with the
// mutex multiple CreditChannels multiplexed onto one Encoder need (see
// ipc.Encoder's own doc: "safe for use by a single writer goroutine;
// callers that write from multiple goroutines must serialize calls to
// Encode themselves").
type FrameWriter interface {
	Encode(kind ipc.Kind, channelID uint32, payload []byte) error
}

// SharedEncoder adapts an *ipc.Encoder for use by multiple CreditChannels
// (each its own goroutine-set) multiplexed onto the same underlying
// stdout writer, serializing Encode calls across all of them.
type SharedEncoder struct {
	mu  sync.Mutex
	enc *ipc.Encoder
}

// NewSharedEncoder wraps enc for concurrent use by multiple CreditChannels.
func NewSharedEncoder(enc *ipc.Encoder) *SharedEncoder {
	return &SharedEncoder{enc: enc}
}

// Encode implements FrameWriter.
func (s *SharedEncoder) Encode(kind ipc.Kind, channelID uint32, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enc.Encode(kind, channelID, payload)
}

// creditFramePayloadLen is the fixed size of a kind=0x03 credit frame's
// payload per docs/plugin-api.md: "fixed 8-byte payload: [4B channelId][4B
// credit], no subtype byte". The channelId is carried redundantly here (the
// 9-byte frame header already carries channelId, per codec.go, and that is
// what internal/ipc's Dispatcher actually routes on) — the payload
// duplicates it per the documented wire shape, so CreditChannel validates
// the two agree rather than silently trusting one or the other.
const creditFramePayloadLen = 8

// CreditChannel is the concrete FrameSink/FrameSource for one open
// channel-bus channel. It is safe for concurrent use: Send may be called
// from one goroutine while GrantCredit, receiveCreditGrant (invoked by a
// Registry as kind=0x03 frames arrive), and deliverData (invoked by a
// Registry as kind=0x02 frames arrive) run from others.
type CreditChannel struct {
	id         uint32
	enc        FrameWriter
	maxPayload int

	mu       sync.Mutex
	cond     *sync.Cond
	sendCred int
	isClosed bool

	recvCh chan []byte
	done   chan struct{}

	closeOnce sync.Once
}

// NewCreditChannel builds a CreditChannel for channel id, writing frames
// via enc, with initialCredit unacknowledged-frame slots on the sender
// side (per docs/plugin-api.md: 4 for exec/tcp-relay/udp-relay, 8 for
// embed-stream — see open.go for where those numbers are wired in per
// purpose) and refusing to Send a payload larger than maxPayload bytes
// (1 MiB for tcp-relay, 64 KiB for embed-stream per API-FINDINGS.md F-8).
func NewCreditChannel(id uint32, enc FrameWriter, initialCredit, maxPayload int) *CreditChannel {
	c := &CreditChannel{
		id:         id,
		enc:        enc,
		maxPayload: maxPayload,
		sendCred:   initialCredit,
		recvCh:     make(chan []byte, 64),
		done:       make(chan struct{}),
	}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// Send implements FrameSink. It blocks while the sender-side credit window
// is exhausted (sendCred == 0), waking either when a kind=0x03 grant frame
// from the host replenishes it (via receiveCreditGrant) or the channel is
// closed — it never returns ErrNoCredit itself, per the design doc's
// requirement that exhaustion block rather than fail (sending past credit
// is a fail-fast protocol violation on the host side, so a caller must
// never be tempted to skip the frame and move on).
func (c *CreditChannel) Send(payload []byte) error {
	if len(payload) > c.maxPayload {
		return fmt.Errorf("transport: payload %d bytes exceeds max %d for channel %d", len(payload), c.maxPayload, c.id)
	}

	c.mu.Lock()
	for !c.isClosed && c.sendCred <= 0 {
		c.cond.Wait()
	}
	if c.isClosed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.sendCred--
	c.mu.Unlock()

	if err := c.enc.Encode(ipc.KindChannelData, c.id, payload); err != nil {
		// The send never reached the wire; refund the credit slot rather
		// than leaking it, even though in practice an Encode failure here
		// means the underlying stdio pipe is already dead and the process
		// is on its way down per the wire's fail-fast discipline.
		c.mu.Lock()
		c.sendCred++
		c.cond.Signal()
		c.mu.Unlock()
		return fmt.Errorf("transport: send on channel %d: %w", c.id, err)
	}
	return nil
}

// TrySend is a non-blocking variant of Send: it returns ErrNoCredit
// immediately, instead of blocking, if the sender-side window is currently
// exhausted. Not required by the FrameSink seam (Channel.Write always
// wants blocking Send), but exposed for callers that need to probe credit
// state without parking a goroutine.
func (c *CreditChannel) TrySend(payload []byte) error {
	if len(payload) > c.maxPayload {
		return fmt.Errorf("transport: payload %d bytes exceeds max %d for channel %d", len(payload), c.maxPayload, c.id)
	}

	c.mu.Lock()
	if c.isClosed {
		c.mu.Unlock()
		return ErrClosed
	}
	if c.sendCred <= 0 {
		c.mu.Unlock()
		return ErrNoCredit
	}
	c.sendCred--
	c.mu.Unlock()

	if err := c.enc.Encode(ipc.KindChannelData, c.id, payload); err != nil {
		c.mu.Lock()
		c.sendCred++
		c.cond.Signal()
		c.mu.Unlock()
		return fmt.Errorf("transport: send on channel %d: %w", c.id, err)
	}
	return nil
}

// receiveCreditGrant is invoked (by a Registry) when a kind=0x03 credit
// frame from the host arrives for this channel: it replenishes the
// sender-side window by n slots and wakes any Send blocked on exhaustion.
func (c *CreditChannel) receiveCreditGrant(n int) {
	if n <= 0 {
		return
	}
	c.mu.Lock()
	c.sendCred += n
	c.cond.Broadcast()
	c.mu.Unlock()
}

// GrantCredit is the receiver-side mechanism: it emits a kind=0x03 credit
// frame telling the host it may send n more frames on this channel. It is
// never called automatically by Recv — the caller (tcp-relay glue, or
// Phase 3d's relay/coupling.go for embed-stream) controls exactly when and
// how much credit to grant, which is the whole point of the seam: for
// embed-stream, withholding a grant is the plugin's only backpressure
// lever against a VNC server that has no idea the browser side is slow.
func (c *CreditChannel) GrantCredit(n int) error {
	if n <= 0 {
		return nil
	}
	c.mu.Lock()
	closed := c.isClosed
	c.mu.Unlock()
	if closed {
		return ErrClosed
	}

	payload := make([]byte, creditFramePayloadLen)
	binary.BigEndian.PutUint32(payload[0:4], c.id)
	binary.BigEndian.PutUint32(payload[4:8], uint32(n))
	if err := c.enc.Encode(ipc.KindCredit, c.id, payload); err != nil {
		return fmt.Errorf("transport: grant credit on channel %d: %w", c.id, err)
	}
	return nil
}

// Remaining reports c's current sender-side credit window: how many more
// frames c may Send before blocking. Safe for concurrent use. This is a
// read-only snapshot for backpressure/coupling policies (see
// internal/relay/coupling.go) that want to react to a shrinking outbound
// window without parking a goroutine in Send/TrySend — it does not itself
// consume or reserve anything.
func (c *CreditChannel) Remaining() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sendCred
}

// Recv implements FrameSource. It blocks until a payload is available or
// the channel closes, in which case it returns ok == false — matching
// conn.go's FrameSource contract exactly.
func (c *CreditChannel) Recv() ([]byte, bool) {
	select {
	case payload := <-c.recvCh:
		return payload, true
	case <-c.done:
		select {
		case payload := <-c.recvCh:
			return payload, true
		default:
			return nil, false
		}
	}
}

// deliverData is invoked (by a Registry) when a kind=0x02 data frame from
// the host arrives for this channel. It is a no-op once the channel is
// closed, per docs/plugin-api.md's "channel.close has a 0 s grace period
// ... both sides must treat the channel as closed and drop any further
// frames for that channelId as no-ops, not errors."
func (c *CreditChannel) deliverData(payload []byte) {
	select {
	case c.recvCh <- payload:
	case <-c.done:
	}
}

// Close closes the channel: it is idempotent, unblocks any Send parked on
// exhausted credit (with ErrClosed), and makes Recv return ok == false
// (after draining anything already buffered).
func (c *CreditChannel) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.isClosed = true
		c.mu.Unlock()
		close(c.done)
		c.cond.Broadcast()
	})
	return nil
}

var (
	_ FrameSink   = (*CreditChannel)(nil)
	_ FrameSource = (*CreditChannel)(nil)
)

// GrantCredit is exposed on Channel itself (the net.Conn Task 7 built)
// so callers driving credit policy (tcp-relay's promptly-after-consume
// grant, or Phase 3d's coupling for embed-stream) don't need to reach past
// the net.Conn abstraction into the CreditChannel underneath — matching
// the design doc's "credit.go — оба направления" split, where the
// receiver-side grant is explicitly caller-invoked, never automatic.
func (c *Channel) GrantCredit(n int) error {
	cc, ok := c.source.(*CreditChannel)
	if !ok {
		return fmt.Errorf("transport: GrantCredit requires a credit-windowed channel, got %T", c.source)
	}
	return cc.GrantCredit(n)
}

// SendCreditRemaining reports c's current sender-side credit window (see
// CreditChannel.Remaining), and whether c is backed by a credit-windowed
// source at all — false for a Channel built over some other FrameSource
// (e.g. a test fake), matching GrantCredit's own type-assertion pattern.
func (c *Channel) SendCreditRemaining() (int, bool) {
	cc, ok := c.source.(*CreditChannel)
	if !ok {
		return 0, false
	}
	return cc.Remaining(), true
}

// Registry routes decoded channel-bus frames (channelId != 0) to the
// CreditChannel they belong to, implementing ipc.ChannelDataHandler — the
// routing seam internal/ipc/dispatch.go defines. Channel ids are never
// reused within a plugin process's lifetime (per docs/plugin-api.md: "the
// host is the sole allocator, from a monotonic counter that is never
// reused"), so Registry never removes an entry on close: a frame arriving
// for a channel this plugin has already closed locally still finds it in
// the map and is routed to deliverData/receiveCreditGrant, which are
// no-ops post-close — exactly the "drop as no-op, not error" behavior the
// 0 s channel.close grace period requires. A channelId that was *never*
// registered at all (never opened) is a genuine protocol violation and is
// reported as one, matching the host's own fail-fast discipline for
// unknown channelIds.
type Registry struct {
	mu       sync.Mutex
	channels map[uint32]*CreditChannel
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{channels: make(map[uint32]*CreditChannel)}
}

// Register adds c to the registry, keyed by its channel id.
func (r *Registry) Register(c *CreditChannel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channels[c.id] = c
}

// HandleChannelFrame implements ipc.ChannelDataHandler.
func (r *Registry) HandleChannelFrame(_ context.Context, f ipc.Frame) error {
	r.mu.Lock()
	c, ok := r.channels[f.ChannelID]
	r.mu.Unlock()
	if !ok {
		return &ipc.ProtocolViolationError{
			Reason: "channelId not open",
			Kind:   f.Kind,
			Length: uint32(len(f.Payload)),
		}
	}

	switch f.Kind {
	case ipc.KindChannelData:
		c.deliverData(f.Payload)
		return nil
	case ipc.KindCredit:
		if len(f.Payload) != creditFramePayloadLen {
			return &ipc.ProtocolViolationError{
				Reason: "malformed credit frame payload",
				Kind:   f.Kind,
				Length: uint32(len(f.Payload)),
			}
		}
		payloadChannelID := binary.BigEndian.Uint32(f.Payload[0:4])
		if payloadChannelID != f.ChannelID {
			return &ipc.ProtocolViolationError{
				Reason: "credit frame payload channelId mismatch with header",
				Kind:   f.Kind,
				Length: uint32(len(f.Payload)),
			}
		}
		n := binary.BigEndian.Uint32(f.Payload[4:8])
		c.receiveCreditGrant(int(n))
		return nil
	default:
		return &ipc.ProtocolViolationError{
			Reason: "unexpected kind on channel-bus",
			Kind:   f.Kind,
			Length: uint32(len(f.Payload)),
		}
	}
}

var _ ipc.ChannelDataHandler = (*Registry)(nil)

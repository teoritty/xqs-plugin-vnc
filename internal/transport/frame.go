// Package transport turns an already-open, credit-windowed binary channel
// (channelId != 0, kind=0x02 "channel data" per the design doc's §3
// "Фрейминг") into a full net.Conn, so internal/rfb can run unmodified
// against either a net.Pipe() in tests or a real channel-bus connection in
// production, and so a later phase can wrap it in tls.Client(conn, cfg) for
// VeNCrypt without reshaping anything below.
//
// This file, frame.go, only concerns itself with the max-payload-per-frame
// ceiling and splitting an application-level Write() into frame-sized
// chunks.
//
// Reuse decision: this package does NOT re-encode the 9-byte
// [length][kind][channelId] header that internal/ipc/codec.go already
// implements. The seam this package defines (FrameSink.Send /
// FrameSource.Recv, see conn.go) operates on already-demultiplexed
// payloads — a single channel's bytes, with kind and channelId already
// known/fixed by construction. The actual encode/decode of those 9 bytes
// against the real stdin/stdout stream happens once, in whatever component
// implements FrameSink/FrameSource for a real channel (Phase 3b's
// channel.open/credit work): that component reads/writes ipc.Frame values
// via ipc.Encoder/ipc.Decoder directly. Duplicating the header codec here
// would mean two places need to agree on frame layout; reusing
// ipc.MaxFrameLength as this package's ceiling (below) is the one piece of
// ipc knowledge transport actually needs.
package transport

import "xqs-plugin-vnc/internal/ipc"

// DefaultMaxFramePayload is the largest payload this package will ever
// pack into a single logical unit of channel data, absent an explicit
// override. It mirrors internal/ipc.MaxFrameLength, the single largest
// documented wire ceiling (see ipc/codec.go's comment on MaxFrameLength).
// Callers wiring up a real purpose (tcp-relay: 1 MiB, embed-stream: 64 KiB
// per docs/plugin-api.md and API-FINDINGS.md F-8) are expected to pass
// their own, smaller maxPayload into NewChannel — this package is
// purpose-agnostic and does not know which one applies.
const DefaultMaxFramePayload = ipc.MaxFrameLength

// splitPayload divides b into a sequence of chunks each at most max bytes
// long, preserving order. It never returns a zero-length chunk, and
// returns nil for an empty input. If max <= 0, DefaultMaxFramePayload is
// used instead.
//
// The returned slices alias b's backing array; callers must not mutate b
// until the chunks have been consumed (Write's caller-owned buffer d is
// safe here since net.Conn.Write's contract lets us assume the caller
// won't mutate it concurrently, but see channel.go for how the chunks are
// actually handed off).
func splitPayload(b []byte, max int) [][]byte {
	if max <= 0 {
		max = DefaultMaxFramePayload
	}
	if len(b) == 0 {
		return nil
	}
	chunks := make([][]byte, 0, (len(b)+max-1)/max)
	for len(b) > 0 {
		n := max
		if n > len(b) {
			n = len(b)
		}
		chunks = append(chunks, b[:n:n])
		b = b[n:]
	}
	return chunks
}

// This file (pump.go) is the composition point design doc §2 flags as
// the one exception to "cmd/xqs-vnc/main.go — единственное место, где эти
// пакеты встречаются": internal/relay is where internal/rfb and
// internal/transport genuinely meet, wired together through the
// internal/session.RelayStarter extension point Task 9 left open.
//
// Pump implements session.RelayStarter. Its StartRelay:
//
//  1. Runs the real RFB handshake (internal/rfb.Handshake) against
//     tcpChannel — a *transport.Channel that IS the TCP connection to the
//     VNC server (opened with purpose "tcp-relay"; the host does the
//     actual net.Dial against its TunnelDialProxy allowlist using the
//     host:port hint internal/session/lifecycle.go now passes to
//     channel.open — there is no separate net.Dial anywhere in this
//     plugin, since the plugin process never gets a raw socket per
//     docs/plugin-api.md's purpose table).
//  2. Runs the synthetic browser-facing handshake
//     (internal/rfb.Frontshake) against embedChannel.
//  3. Once both complete, starts the raw bidirectional pump described in
//     design doc §1: from here on, every byte is relayed unmodified,
//     starting exactly at ServerInit (server->browser) / ClientInit
//     (browser->server) — rfb.Handshake and rfb.Frontshake both
//     deliberately stop right before those messages, by construction.
//
// StartRelay itself returns as soon as both handshakes succeed and the
// pump goroutines are launched — it does not block for the relay's
// lifetime, matching orchestrate()'s expectation that StartRelay
// completes so it can move on to session.updateState("ready"). Any
// failure after that point (server closes its TCP connection, an
// embed-stream write fails because the browser tab is gone, an
// unrecognized client message desyncs the stream) is reported back to
// the session asynchronously via Session.ReportRelayEnded.
package relay

import (
	"context"
	"errors"
	"fmt"
	"io"

	"xqs-plugin-vnc/internal/rfb"
	"xqs-plugin-vnc/internal/session"
	"xqs-plugin-vnc/internal/transport"
)

// serverToBrowserBufSize bounds how much VNC-server data Pump reads from
// tcpChannel per iteration before relaying it onward. It is deliberately
// smaller than embed-stream's 64 KiB frame ceiling (transport.
// EmbedStreamMaxFramePayload is unexported, but Channel.Write already
// splits any larger write into multiple frames of at most that size
// itself — see channel.go's splitPayload — so this buffer size is only a
// read-granularity/latency tuning knob, not a correctness requirement).
const serverToBrowserBufSize = 32 * 1024

// Pump is a session.RelayStarter that wires a session's already-open
// tcp-relay and embed-stream channels into a real, authenticated,
// bidirectional RFB relay. The zero value is ready to use; Policy
// defaults to DefaultCouplingPolicy if left zero-valued (Capacity <= 0).
type Pump struct {
	// Policy governs how much tcp-relay credit to grant back per
	// relayed server->browser chunk, based on embed-stream's remaining
	// outbound credit. Zero value (Capacity == 0) falls back to
	// DefaultCouplingPolicy at StartRelay time.
	Policy CouplingPolicy
}

var _ session.RelayStarter = Pump{}

// StartRelay implements session.RelayStarter. It delegates the actual
// handshake + pump work to Run (kept free of *session.Session so it is
// directly testable against fake transport.Channel pairs — see
// pump_test.go), then wires Run's eventual result back to the session via
// Session.ReportRelayEnded once it finishes in the background.
func (p Pump) StartRelay(ctx context.Context, s *session.Session, tcpChannel, embedChannel *transport.Channel) error {
	password := s.Password()
	readOnly := s.ReadOnly()
	policy := p.Policy
	if policy.Capacity <= 0 {
		policy = DefaultCouplingPolicy
	}

	if err := doHandshakes(tcpChannel, embedChannel, password); err != nil {
		return err
	}

	go func() {
		err := runPump(tcpChannel, embedChannel, readOnly, policy)
		s.ReportRelayEnded(context.Background(), err)
	}()

	return nil
}

// doHandshakes runs the real VNC handshake against tcpChannel and the
// synthetic browser-facing handshake against embedChannel, in that order.
// Kept separate from Run so StartRelay can surface a handshake failure
// synchronously (as its own returned error, which orchestrate() turns
// into StateError before ever reaching "ready") while Run itself only
// needs to run the two already-authenticated raw pumps.
func doHandshakes(tcpChannel, embedChannel *transport.Channel, password []byte) error {
	if _, err := rfb.Handshake(tcpChannel, password); err != nil {
		return fmt.Errorf("relay: VNC server handshake: %w", err)
	}
	if _, err := rfb.Frontshake(embedChannel); err != nil {
		return fmt.Errorf("relay: browser frontshake: %w", err)
	}
	return nil
}

// Run drives both directions of the raw, already-authenticated relay
// concurrently over tcpCh/embedCh (whose handshakes must already be
// complete — see doHandshakes) until either direction ends, for any
// reason (clean EOF, I/O error, protocol desync), then closes both
// channels (unblocking whichever direction is still running) and returns
// the error that ended it (nil for a clean close). Exported and free of
// any *session.Session dependency specifically so it is testable against
// fake transport.Channel pairs without constructing a Session.
func Run(tcpCh, embedCh *transport.Channel, readOnly bool, policy CouplingPolicy) error {
	return runPump(tcpCh, embedCh, readOnly, policy)
}

func runPump(tcpCh, embedCh *transport.Channel, readOnly bool, policy CouplingPolicy) error {
	errCh := make(chan error, 2)

	go func() { errCh <- pumpServerToBrowser(tcpCh, embedCh, policy) }()
	go func() { errCh <- pumpBrowserToServer(embedCh, tcpCh, readOnly) }()

	// The relay is done the instant either direction ends, for any
	// reason: per design doc §7, there is no partial-relay state to
	// preserve, and the pump does not retry. Closing both channels here
	// unblocks whichever goroutine is still running so it exits too,
	// without this function waiting for it — its result is discarded but
	// errCh is buffered (cap 2) so that goroutine's send never blocks or
	// leaks.
	firstErr := <-errCh
	_ = tcpCh.Close()
	_ = embedCh.Close()
	return firstErr
}

// pumpServerToBrowser is the VNC-server -> browser direction: raw bytes
// read from tcpCh (already past ServerInit's start, per the handshake
// boundary invariant) are written unmodified to embedCh. After each
// successfully relayed chunk, it applies the credit-window coupling
// policy (coupling.go): how much tcp-relay credit to grant back depends
// on how much outbound send-credit remains on embedCh, so a slow browser
// consumer throttles how fast this plugin keeps reading from the VNC
// server, per design doc §3/§7.
//
// Returns nil on a clean tcpCh close (server closed its TCP connection
// gracefully) or a non-nil error for any other failure — both are
// reported identically by the caller (runPump) via
// Session.ReportRelayEnded, since design doc §7 treats "server closed
// TCP" as fail-fast/no-retry the same as any other relay failure.
func pumpServerToBrowser(tcpCh, embedCh *transport.Channel, policy CouplingPolicy) error {
	buf := make([]byte, serverToBrowserBufSize)
	for {
		n, readErr := tcpCh.Read(buf)
		if n > 0 {
			if _, err := embedCh.Write(buf[:n]); err != nil {
				return fmt.Errorf("relay: write to embed-stream: %w", err)
			}

			remaining, ok := embedCh.SendCreditRemaining()
			if !ok {
				// Not backed by a real credit-windowed channel (e.g. a
				// test fake) — nothing to couple against, so grant
				// plainly.
				_ = tcpCh.GrantCredit(1)
			} else if grant := policy.Grant(remaining, 1); grant > 0 {
				_ = tcpCh.GrantCredit(grant)
			}
			// grant == 0: deliberately withhold — this is the coupling's
			// backpressure lever reaching the VNC server (design doc §3).
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return fmt.Errorf("relay: VNC server closed the connection: %w", readErr)
			}
			return fmt.Errorf("relay: read from tcp-relay: %w", readErr)
		}
	}
}

// pumpBrowserToServer is the browser -> VNC-server direction: client
// messages arriving on embedCh (already past ClientInit's start) are
// parsed message-by-message via readonly.go's FilterOnce (itself a thin
// wrapper over internal/rfb/clientmsg.go's parsing) and, per readOnly,
// either forwarded to tcpCh unmodified or dropped while still being fully
// consumed off the wire (keeping the stream in sync). Embed-stream
// receive credit is granted back promptly after each consumed message —
// there is no coupling need on this side: browser input is naturally
// rate-limited by human input speed, and design doc §3's coupling
// requirement is specifically about the opposite, framebuffer-bound
// direction.
//
// Returns nil on a clean embedCh close (browser/host closed the
// embed-stream channel), or a non-nil error for any other failure,
// including *readonly.ErrSessionFatal-wrapped errors for an unrecognized
// client message type — per design doc §5, that desyncs the byte stream
// irrecoverably and must terminate the session, not just this call.
func pumpBrowserToServer(embedCh, tcpCh *transport.Channel, readOnly bool) error {
	// ClientInit (RFC 6143 §7.3.1) is a single raw shared-flag byte with no
	// message-type semantics. rfb.Frontshake's doc comment is explicit that
	// it never reads this byte and the caller must raw-relay it — feeding
	// it into FilterOnce/rfb.ReadClientMessage would misparse it as a
	// bogus client MESSAGE type (0 = SetPixelFormat, consuming 19 bytes
	// that don't belong to it; 1 = unrecognized type, fatal). Per design
	// doc §1's "ClientInit не трогаем" invariant, it is relayed verbatim,
	// unmodified, without inspecting its value — noVNC always sends
	// shared=1 and v1 has no field that needs to branch on it.
	var clientInit [1]byte
	if _, err := io.ReadFull(embedCh, clientInit[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("relay: embed-stream closed before ClientInit: %w", err)
		}
		return fmt.Errorf("relay: read ClientInit: %w", err)
	}
	if _, err := tcpCh.Write(clientInit[:]); err != nil {
		return fmt.Errorf("relay: relay ClientInit to tcp-relay: %w", err)
	}

	for {
		_, err := FilterOnce(embedCh, tcpCh, readOnly)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("relay: embed-stream closed: %w", err)
			}
			return fmt.Errorf("relay: browser->server relay: %w", err)
		}
		_ = embedCh.GrantCredit(1)
	}
}

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
	"os"
	"time"

	"xqs-plugin-vnc/internal/rfb"
	"xqs-plugin-vnc/internal/session"
	"xqs-plugin-vnc/internal/transport"
)

// handshakeTimeout bounds how long doHandshakes waits for the VNC server's
// RFB handshake and the browser's synthetic frontshake. Neither
// rfb.Handshake nor rfb.Frontshake carries any timeout of its own (they
// block on plain io.ReadFull against the channel), so without this a
// server that never sends its version banner (bad host:port, firewalled,
// or a protocol this plugin can't parse) leaves the session stuck at
// "Connecting..." forever with no error ever reaching updateState.
const handshakeTimeout = 15 * time.Second

// debugLog writes a diagnostic line to stderr. The host does not surface
// plugin stderr in its UI, but it does show up in whatever wraps the
// plugin process (e.g. captured in the host's own log file), which is the
// only place to look when a session hangs before any updateState/RPC
// traffic would explain why.
func debugLog(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "xqs-vnc: relay: "+format+"\n", args...)
}

// serverToBrowserBufSize bounds how much VNC-server data Pump reads from
// tcpChannel per iteration before relaying it onward. It is deliberately
// smaller than embed-stream's 64 KiB frame ceiling (transport.
// EmbedStreamMaxFramePayload is unexported, but Channel.Write already
// splits any larger write into multiple frames of at most that size
// itself — see channel.go's splitPayload — so this buffer size is only a
// read-granularity/latency tuning knob, not a correctness requirement).
const serverToBrowserBufSize = 32 * 1024

// BackpressureGate is the extension point through which a host-originated
// session.tunnelBackpressure/session.tunnelResume signal (docs/plugin-api.md;
// design doc §7's edge-case row "tunnelBackpressure: Прекратить читать из
// VNC-сервера. При tunnelResume — возобновить.") reaches the
// server->browser pump's read loop. *session.Session implements this (see
// internal/session/backpressure.go); it is expressed as an interface here,
// not a *session.Session parameter, so pumpServerToBrowser/runPump stay
// testable against fakes with no session package dependency, matching this
// file's existing decoupling rationale.
type BackpressureGate interface {
	// WaitForReadClearance blocks while backpressured, returning true once
	// clear to read again, or false immediately if stop fires first
	// (channel torn down) without ever having to observe a resume.
	WaitForReadClearance(stop <-chan struct{}) bool
}

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
var _ BackpressureGate = (*session.Session)(nil)

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
		err := runPump(tcpChannel, embedChannel, readOnly, policy, s)
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
	deadline := time.Now().Add(handshakeTimeout)
	_ = tcpChannel.SetDeadline(deadline)
	_ = embedChannel.SetDeadline(deadline)
	defer func() {
		_ = tcpChannel.SetDeadline(time.Time{})
		_ = embedChannel.SetDeadline(time.Time{})
	}()

	debugLog("dialing VNC server handshake (timeout=%s)", handshakeTimeout)
	if _, err := rfb.Handshake(tcpChannel, password); err != nil {
		debugLog("VNC server handshake failed: %v", err)
		return fmt.Errorf("relay: VNC server handshake: %w", err)
	}
	debugLog("VNC server handshake OK")

	if _, err := rfb.Frontshake(embedChannel); err != nil {
		debugLog("browser frontshake failed: %v", err)
		return fmt.Errorf("relay: browser frontshake: %w", err)
	}
	debugLog("browser frontshake OK")
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
	return runPump(tcpCh, embedCh, readOnly, policy, nil)
}

// RunWithGate is Run plus a BackpressureGate: gate.WaitForReadClearance is
// consulted before every read from tcpCh in the server->browser direction,
// pausing that direction while the host has signaled
// session.tunnelBackpressure. A nil gate behaves exactly like Run.
func RunWithGate(tcpCh, embedCh *transport.Channel, readOnly bool, policy CouplingPolicy, gate BackpressureGate) error {
	return runPump(tcpCh, embedCh, readOnly, policy, gate)
}

func runPump(tcpCh, embedCh *transport.Channel, readOnly bool, policy CouplingPolicy, gate BackpressureGate) error {
	errCh := make(chan error, 2)

	go func() { errCh <- pumpServerToBrowser(tcpCh, embedCh, policy, gate) }()
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
func pumpServerToBrowser(tcpCh, embedCh *transport.Channel, policy CouplingPolicy, gate BackpressureGate) error {
	buf := make([]byte, serverToBrowserBufSize)
	for {
		if gate != nil {
			// Per design doc §7: "tunnelBackpressure: Прекратить читать из
			// VNC-сервера." stop is tcpCh.Done() so a channel torn down
			// while backpressured (session.disconnect, server closed TCP,
			// crash-recovery) never leaves this goroutine parked forever
			// waiting for a tunnelResume that will now never arrive.
			if !gate.WaitForReadClearance(tcpCh.Done()) {
				return nil
			}
		}
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

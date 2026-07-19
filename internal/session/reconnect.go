package session

import (
	"context"
	"time"

	"xqs-plugin-vnc/internal/transport"
)

// reconnectWindow bounds how long a lost VNC session tries to re-establish itself before giving up
// and surfacing an error. reconnectBackoff is the pause between attempts so a server that is down
// (rather than a momentary network blip) is not hammered.
// vars, not consts, only so tests can shrink them and not wait out the real 30s window.
var (
	reconnectWindow  = 30 * time.Second
	reconnectBackoff = 2 * time.Second
)

// reconnect re-establishes the relay for up to reconnectWindow after the connection to the VNC
// server dropped (network change, server restart), keeping the SAME session so the UI tab and its
// embed panel survive. Each attempt closes the dead channels and re-runs connectAndRelay, whose
// session.registerEmbed rotates the embed token; the frontend reloads the iframe on the new uiUrl,
// so a fresh noVNC reconnects and re-handshakes on its own — no browser-side reconnect code needed.
//
// It is called from ReportRelayEnded on the relay's own (ending) goroutine: on success a fresh relay
// goroutine is now running and will call ReportRelayEnded again on the next drop, so there is no
// recursion. session.disconnect / process teardown set torndown, which stops the loop.
//
// Returns true once the session is ready again; false if teardown intervened or the window elapsed.
func (s *Session) reconnect(ctx context.Context) bool {
	deadline := time.Now().Add(reconnectWindow)
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		s.mu.Lock()
		torndown := s.torndown
		s.mu.Unlock()
		if torndown {
			return false
		}

		traceLog("session %s: reconnect attempt %d", s.id, attempt)
		// StateConnecting keeps the embed panel mounted (SessionView shows it for an embed session
		// regardless of connecting/ready); once registerEmbed rotates the token the iframe reloads
		// and noVNC shows its own "Connecting" overlay, so the user sees the reconnect in progress.
		s.updateState(ctx, StateConnecting, "reconnecting")
		s.closeChannels(ctx, "reconnect")

		if err := s.connectAndRelay(ctx); err != nil {
			traceLog("session %s: reconnect attempt %d failed: %v", s.id, attempt, err)
			select {
			case <-time.After(reconnectBackoff):
			case <-ctx.Done():
				return false
			}
			continue
		}

		traceLog("session %s: reconnected", s.id)
		s.updateState(ctx, StateReady, "")
		return true
	}
	return false
}

// closeChannels best-effort closes and clears the tcp-relay and embed-stream channels before a
// reconnect attempt re-opens fresh ones. Idempotent against nil channels.
func (s *Session) closeChannels(ctx context.Context, reason string) {
	s.mu.Lock()
	tcp := s.tcpChannel
	embed := s.embedChannel
	s.tcpChannel = nil
	s.embedChannel = nil
	s.mu.Unlock()

	if tcp != nil {
		_ = transport.CloseChannel(ctx, s.caller, tcp, reason, "")
	}
	if embed != nil {
		_ = transport.CloseChannel(ctx, s.caller, embed, reason, "")
	}
}

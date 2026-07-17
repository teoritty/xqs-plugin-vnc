// This file (backpressure.go) receives the two host -> plugin tunnel
// flow-control notifications documented in docs/plugin-api.md's "Host ->
// plugin notifications" table:
//
//   - session.tunnelBackpressure: {"sessionId":"..."} — "Pause TCP read
//     (consumer slow / tab inactive)".
//   - session.tunnelResume: {"sessionId":"..."} — "Resume after
//     backpressure".
//
// Per design doc §7's edge-case row: "tunnelBackpressure: Прекратить
// читать из VNC-сервера. При tunnelResume — возобновить." This was a real
// gap (F-6 in API-FINDINGS.md notes the host now delivers this on the
// channel-bus embed-stream path too, via ChannelCloseNotifier/
// AttachChannelCloseNotifier, using the same JSON-RPC notification shape
// as the legacy tunnelFrame path) — nothing in this package previously
// registered or acted on either method.
//
// The gate itself is a simple close-and-replace channel, following the
// same sync.Mutex-guarded-state convention as viewport.go: setBackpressure
// replaces/closes a channel under s.mu; WaitForReadClearance (consumed by
// internal/relay's server->browser pump) blocks on it without holding the
// lock, so a concurrent resume is never missed and the read side never
// deadlocks against a resume that already happened.
package session

import (
	"xqs-plugin-vnc/internal/ipc"
)

type tunnelBackpressureParams struct {
	SessionID string `json:"sessionId"`
}

// handleTunnelBackpressure processes a session.tunnelBackpressure
// notification: it looks up the target Session and marks it as
// backpressured, causing WaitForReadClearance to block until a matching
// session.tunnelResume arrives (or the caller's stop channel fires, e.g.
// because the underlying channel closed). Unknown/missing sessionId or
// malformed params are silently ignored, matching every other notification
// handler's convention here.
func (h *Handler) handleTunnelBackpressure(n *ipc.Notification) {
	var p tunnelBackpressureParams
	if err := unmarshalIgnoreEmpty(n.Params, &p); err != nil {
		return
	}
	s := h.session(p.SessionID)
	if s == nil {
		return
	}
	s.setBackpressure(true)
}

// handleTunnelResume processes a session.tunnelResume notification:
// clears the backpressure gate set by handleTunnelBackpressure, unblocking
// any goroutine currently parked in WaitForReadClearance.
func (h *Handler) handleTunnelResume(n *ipc.Notification) {
	var p tunnelBackpressureParams
	if err := unmarshalIgnoreEmpty(n.Params, &p); err != nil {
		return
	}
	s := h.session(p.SessionID)
	if s == nil {
		return
	}
	s.setBackpressure(false)
}

// setBackpressure toggles s's backpressure gate. active=true (re-)arms a
// fresh, open gate channel if one isn't already set (idempotent — a second
// tunnelBackpressure before a resume is a no-op, not a leaked channel);
// active=false closes and clears the current gate channel, if any
// (idempotent — a stray/duplicate tunnelResume is a no-op).
func (s *Session) setBackpressure(active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if active {
		if s.backpressureGate == nil {
			s.backpressureGate = make(chan struct{})
		}
		return
	}
	if s.backpressureGate != nil {
		close(s.backpressureGate)
		s.backpressureGate = nil
	}
}

// WaitForReadClearance blocks while s is under host-signaled backpressure
// (session.tunnelBackpressure received, no matching session.tunnelResume
// yet), returning true once cleared. It returns false without blocking
// further the instant stop fires (the caller's underlying channel/pump
// tearing down) — this is what keeps a session.disconnect or server-closed
// TCP from leaking a goroutine parked here forever if backpressure was
// never resumed. A nil stop is not supported; callers always have a
// channel-close signal to pass (see internal/relay/pump.go).
func (s *Session) WaitForReadClearance(stop <-chan struct{}) bool {
	for {
		s.mu.Lock()
		gate := s.backpressureGate
		s.mu.Unlock()
		if gate == nil {
			return true
		}
		select {
		case <-gate:
			// Resumed; loop to re-check in case another
			// tunnelBackpressure raced in immediately after.
		case <-stop:
			return false
		}
	}
}

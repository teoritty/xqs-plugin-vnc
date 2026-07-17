// This file (teardown.go) handles session.disconnect and general
// cleanup: zeroing the password buffer (defer on every exit path,
// including the connect goroutine's panic-recover path in lifecycle.go)
// and closing both channel-bus channels.
//
// Per design doc §7 "Закрытие сессии: Ядро синхронно закрывает каналы до
// сноса объекта сессии. Мы не должны рассчитывать, что успеем что-то
// дописать." — the host may already have force-closed the channels by
// the time session.disconnect reaches us, so Teardown treats
// CloseChannel failures as best-effort, not fatal, and does not attempt
// any multi-step graceful handshake. It is idempotent and safe to call
// more than once (e.g. once from session.disconnect, once from process
// shutdown) or concurrently with orchestrate() still running.
package session

import (
	"context"
	"encoding/json"

	"xqs-plugin-vnc/internal/ipc"
	"xqs-plugin-vnc/internal/transport"
)

// handleDisconnect processes a session.disconnect notification, per
// docs/plugin-api.md: {"sessionId":"..."}. It tears down the matching
// Session if one is tracked; an unknown sessionId is ignored (mirrors
// every other notification handler's convention here).
func (h *Handler) handleDisconnect(ctx context.Context, n *ipc.Notification) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := unmarshalIgnoreEmpty(n.Params, &p); err != nil {
		return
	}
	s := h.session(p.SessionID)
	if s == nil {
		return
	}
	s.Teardown(ctx, "session-disconnect")
}

// Teardown zeroes s's password buffer and closes both channel-bus
// channels (if open). It is idempotent: a second call is a no-op. Safe
// to call from any goroutine, including a panic-recover path (it does
// not itself allocate in a way that could panic, and tolerates a nil
// caller/channel).
func (s *Session) Teardown(ctx context.Context, reason string) {
	s.mu.Lock()
	if s.torndown {
		s.mu.Unlock()
		return
	}
	s.torndown = true
	tcpCh := s.tcpChannel
	embedCh := s.embedChannel
	caller := s.caller
	s.mu.Unlock()

	s.clearPassword()

	// Best-effort: the host may have already synchronously closed these
	// channels itself before session.disconnect ever reached us (design
	// doc §7), so a failure here is expected/ignorable, not a bug to
	// surface.
	if tcpCh != nil {
		_ = transport.CloseChannel(ctx, caller, tcpCh, reason, "")
	}
	if embedCh != nil {
		_ = transport.CloseChannel(ctx, caller, embedCh, reason, "")
	}
}

// clearPassword zeroes s.password in place (clear(), not a reassignment
// to nil — a reassignment would leave the original backing array's bytes
// live in memory until GC, defeating the point). Safe to call multiple
// times; a nil/empty password is a no-op.
func (s *Session) clearPassword() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.password)
}

// unmarshalIgnoreEmpty is a small helper so notification handlers don't
// each repeat the same "empty params -> just skip" check.
func unmarshalIgnoreEmpty(params []byte, v any) error {
	if len(params) == 0 {
		return nil
	}
	return json.Unmarshal(params, v)
}

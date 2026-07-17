// This file (embed.go) implements the session.registerEmbed call: this
// plugin CALLS it (plugin -> host RPC, not a handler), per
// docs/plugin-api.md "session.registerEmbed" and the "Minimal embed
// plugin pattern". It's built as a re-callable step (not a one-shot)
// because crash-recovery must call it again with a fresh token: design
// doc §7 "Crash-recovery ... Обязателен повторный registerEmbed — токен
// новый, старый отозван," and docs/plugin-api.md: "Re-registering for
// the same session invalidates the previous token."
package session

import (
	"context"
	"fmt"
)

// registerEmbedParams is the session.registerEmbed request shape, per
// docs/plugin-api.md "session.registerEmbed":
// {"sessionId":"...","uiEntry":"ui/vnc.html","tunnelIds":["main"]}.
type registerEmbedParams struct {
	SessionID string   `json:"sessionId"`
	UIEntry   string   `json:"uiEntry"`
	TunnelIDs []string `json:"tunnelIds,omitempty"`
}

// registerEmbedResult is the session.registerEmbed response shape, per
// docs/plugin-api.md: {"embedToken":"<64-hex>","uiUrl":"...","tunnelUrl":"...","expiresAt":"..."}.
type registerEmbedResult struct {
	EmbedToken string `json:"embedToken"`
	UIUrl      string `json:"uiUrl"`
	TunnelUrl  string `json:"tunnelUrl"`
	ExpiresAt  string `json:"expiresAt"`
}

// registerEmbed calls session.registerEmbed for s, binding the
// embed-stream channel-bus channel already open for this session (design
// doc §10 F-6: "hint = tunnelId (default main); оба направления по
// одному каналу") to the token/URLs the host mints. It stores the latest
// result on s and returns it. Safe to call more than once per Session —
// each call is a fresh registration; the caller (lifecycle.go's
// orchestrate on first connect, or a future crash-recovery re-drive) is
// responsible for deciding when a re-call is needed.
func (s *Session) registerEmbed(ctx context.Context) (registerEmbedResult, error) {
	s.mu.Lock()
	id := s.id
	entry := s.embedEntry
	caller := s.caller
	s.mu.Unlock()

	if caller == nil {
		return registerEmbedResult{}, fmt.Errorf("session: registerEmbed: no RPCCaller configured")
	}

	var res registerEmbedResult
	if err := caller.Call(ctx, "session.registerEmbed", registerEmbedParams{
		SessionID: id,
		UIEntry:   entry,
		TunnelIDs: []string{tunnelIDMain},
	}, &res); err != nil {
		return registerEmbedResult{}, fmt.Errorf("session: session.registerEmbed: %w", err)
	}

	s.mu.Lock()
	s.embedToken = res
	s.mu.Unlock()

	return res, nil
}

// EmbedToken returns the most recent session.registerEmbed result for s,
// or the zero value if registerEmbed hasn't succeeded yet.
func (s *Session) EmbedToken() registerEmbedResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.embedToken
}

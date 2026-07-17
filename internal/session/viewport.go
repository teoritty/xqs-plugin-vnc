// This file (viewport.go) receives the two host -> plugin embed
// notifications documented in docs/plugin-api.md "Host -> plugin
// notifications (embed)":
//
//   - session.embedViewport: "Container resized or tab re-activated"
//     ({"sessionId":"...","widthPx":...,"heightPx":...,"devicePixelRatio":...,"active":...}).
//   - session.embedActivity: "Tab focus changed" ({"sessionId":"...","active":...}).
//
// DONE_WITH_CONCERNS: neither doc section, nor design doc §2's one-liner
// ("embedViewport / embedActivity"), specifies what a plugin is supposed
// to *do* with these beyond the general edge-case guidance in §7 ("Tab
// blur: embed.suspend, ReportEmbedActivity(false), broker backpressure.
// Tab focus: embed.resume + full viewport report" and "embed.suspend ...
// не рвать WebSocket, только приостановить отрисовку"). That behavior
// (pausing/resuming the actual RFB framebuffer pump, coupling to
// tcp-relay credit) belongs to Phase 3d's relay pump, which doesn't
// exist yet in this task (internal/session may not import internal/rfb
// or drive the relay). This file therefore does the minimal
// non-speculative thing: record the latest viewport/activity state on
// the Session (safe for concurrent access) and expose it via
// LatestViewport()/IsActive() as a clean extension point Phase 3d can
// poll or wrap with its own listener, rather than inventing suspend/resume
// semantics a later task would have to reverse-engineer around.
package session

import (
	"encoding/json"

	"xqs-plugin-vnc/internal/ipc"
)

// EmbedViewport is the last-known viewport geometry reported by
// session.embedViewport.
type EmbedViewport struct {
	WidthPx          int     `json:"widthPx"`
	HeightPx         int     `json:"heightPx"`
	DevicePixelRatio float64 `json:"devicePixelRatio"`
	Active           bool    `json:"active"`
}

type embedViewportParams struct {
	SessionID        string  `json:"sessionId"`
	WidthPx          int     `json:"widthPx"`
	HeightPx         int     `json:"heightPx"`
	DevicePixelRatio float64 `json:"devicePixelRatio"`
	Active           bool    `json:"active"`
}

type embedActivityParams struct {
	SessionID string `json:"sessionId"`
	Active    bool   `json:"active"`
}

// handleEmbedViewport processes a session.embedViewport notification: it
// looks up the target Session by sessionId and records the reported
// geometry. Unknown/missing sessionId or malformed params are silently
// ignored — matching internal/lifecycle.Handler's convention that a
// notification has no error channel back to the host.
func (h *Handler) handleEmbedViewport(n *ipc.Notification) {
	var p embedViewportParams
	if err := json.Unmarshal(n.Params, &p); err != nil {
		return
	}
	s := h.session(p.SessionID)
	if s == nil {
		return
	}
	s.mu.Lock()
	s.viewport = EmbedViewport{
		WidthPx:          p.WidthPx,
		HeightPx:         p.HeightPx,
		DevicePixelRatio: p.DevicePixelRatio,
		Active:           p.Active,
	}
	s.mu.Unlock()
}

// handleEmbedActivity processes a session.embedActivity notification: it
// records tab focus state on the target Session.
func (h *Handler) handleEmbedActivity(n *ipc.Notification) {
	var p embedActivityParams
	if err := json.Unmarshal(n.Params, &p); err != nil {
		return
	}
	s := h.session(p.SessionID)
	if s == nil {
		return
	}
	s.mu.Lock()
	s.active = p.Active
	s.mu.Unlock()
}

// LatestViewport returns the most recently reported embed viewport
// geometry for s (zero value if none has been reported yet).
func (s *Session) LatestViewport() EmbedViewport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewport
}

// IsActive returns the most recently reported tab-focus state (true
// unless a session.embedActivity notification has reported false).
func (s *Session) IsActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

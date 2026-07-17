// This file (connect.go) implements the session.connect RPC handler:
// parse fields (host, port, password, readOnly), respond
// {"accepted":true} immediately (well within the host's 5 s synchronous
// timeout), and do the real work (opening channels, registerEmbed) in a
// background goroutine, per docs/plugin-api.md "session.connect contract"
// and design doc §7's edge-case row for it.
package session

import (
	"context"
	"encoding/json"
	"fmt"

	"xqs-plugin-vnc/internal/ipc"
)

// connectParams is the session.connect request shape, per
// docs/plugin-api.md "session.connect contract" (field names match the
// JSON tags the host core actually sends).
type connectParams struct {
	SessionID    string                     `json:"sessionId"`
	ConnectionID string                     `json:"connectionId"`
	Protocol     string                     `json:"protocol"`
	Host         string                     `json:"host"`
	Port         int                        `json:"port"`
	Username     string                     `json:"username"`
	Fields       map[string]json.RawMessage `json:"fields"`
}

// connectResult is the session.connect response shape: "Plugin must
// respond with {"accepted":true}." per docs/plugin-api.md.
type connectResult struct {
	Accepted bool `json:"accepted"`
}

var connectResultOK = mustMarshal(connectResult{Accepted: true})

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("session: mustMarshal: %v", err))
	}
	return b
}

// handleConnect answers session.connect: it parses fields (extracting
// the password as a []byte per design doc §4's "живёт как []byte, не как
// string"), builds a fresh Session (crash-recovery re-sends
// session.connect with a fresh sessionId/fields — this never reuses any
// prior Session), stores it, responds {"accepted":true} synchronously,
// and only then kicks off orchestrate() in a goroutine to do the actual
// channel-opening/registerEmbed work. This ordering is required by
// docs/plugin-api.md: "Plugin returns {"accepted":true} quickly.
// Long-running work ... must run in a goroutine" — dialing/handshaking
// easily exceeds the host's 5 s synchronous timeout.
func (h *Handler) handleConnect(ctx context.Context, r *ipc.Request) error {
	var p connectParams
	if len(r.Params) > 0 {
		if err := json.Unmarshal(r.Params, &p); err != nil {
			return h.respondError(r.ID, errInvalidParams, "invalid session.connect params")
		}
	}

	password, readOnly, err := parseConnectFields(p.Fields)
	if err != nil {
		return h.respondError(r.ID, errInvalidParams, err.Error())
	}

	s := &Session{
		id:          p.SessionID,
		caller:      h.caller,
		frameWriter: h.frameWriter,
		registry:    h.registry,
		embedEntry:  h.embedEntry,
		relay:       h.relay,
		state:       StateConnecting,
		password:    password,
		readOnly:    readOnly,
		host:        p.Host,
		port:        p.Port,
		active:      true,
	}
	h.setSession(p.SessionID, s)

	if err := h.respond(r.ID, connectResultOK); err != nil {
		return err
	}

	go s.orchestrate(context.Background())
	return nil
}

// parseConnectFields extracts the password and readOnly connect fields
// declared in plugin.json's connectionProtocols[0].fields (password:
// secret text field; readOnly: checkbox), per docs/plugin-manifest.md
// "Connection protocol fields" and plugin.json in this repo. Both are
// optional in the wire shape (fields itself may be absent, e.g. during
// an unexpected empty-fields connect) — an absent password yields a nil
// slice (Session.Password() returns nil, meaning "no password"); an
// absent readOnly defaults to false, matching the manifest's declared
// default "false".
//
// The password is decoded straight into a []byte, never held as a Go
// string at any point after this function returns, per design doc §4:
// "живёт как []byte, не как string — строку в Go не занулить."
func parseConnectFields(fields map[string]json.RawMessage) (password []byte, readOnly bool, err error) {
	if raw, ok := fields["password"]; ok && len(raw) > 0 && string(raw) != "null" {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, false, fmt.Errorf("session: invalid password field: %w", err)
		}
		password = []byte(s)
	}

	if raw, ok := fields["readOnly"]; ok && len(raw) > 0 && string(raw) != "null" {
		var b bool
		if err := json.Unmarshal(raw, &b); err == nil {
			readOnly = b
		} else {
			// Manifest declares the checkbox's default as the string
			// "false"; be lenient and accept a string-encoded boolean too
			// rather than failing the whole connect over a formatting
			// quirk in how the host serializes checkbox defaults.
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return password, false, fmt.Errorf("session: invalid readOnly field: %w", err)
			}
			readOnly = s == "true"
		}
	}

	return password, readOnly, nil
}

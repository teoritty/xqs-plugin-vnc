// This file (state.go) implements session.updateState: the plugin
// reports its lifecycle state to the host. Simple state machine, safely
// callable from the connect goroutine even after a panic recover (it
// takes s.mu internally and tolerates a nil/failed caller — see
// updateState below).
//
// Wire shape note: docs/plugin-api.md documents session.updateState as a
// plugin -> host **RPC** (not a notification): the "Plugin -> host (RPC)"
// table (line 167-168) gives it request params
// {"sessionId":"...","state":"ready","error":"optional"} and the
// reference table (line 454-456) gives its response as {"ok":true}. That
// is what's implemented here — via caller.Call, not caller.Notify — even
// though this task's own prompt described it as a notification; the
// doc's explicit response shape is treated as the source of truth over
// that description (flagged in the task report).
package session

import (
	"context"
)

// State is a session's reported lifecycle state, per docs/plugin-api.md
// "session.updateState values".
type State string

const (
	// StateConnecting: "UI stays on connecting screen."
	StateConnecting State = "connecting"
	// StateReady: "UI shows terminal [or embed iframe]; output stream
	// attached."
	StateReady State = "ready"
	// StateError: "UI shows error; optional error string -> errorMessage."
	StateError State = "error"
)

// updateStateParams is the session.updateState request shape, per
// docs/plugin-api.md: {"sessionId":"...","state":"ready","error":"optional"}.
type updateStateParams struct {
	SessionID string `json:"sessionId"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
}

type updateStateResult struct {
	OK bool `json:"ok"`
}

// updateState records st as s's current state and reports it to the
// host via session.updateState. It is safe to call from the connect
// goroutine's panic-recover path: it never panics itself, and if
// s.caller is nil (e.g. a test double, or a Session built before wiring
// completed) it silently skips the RPC after recording state locally,
// rather than panicking again during panic recovery.
func (s *Session) updateState(ctx context.Context, st State, errMsg string) {
	s.mu.Lock()
	s.state = st
	id := s.id
	caller := s.caller
	s.mu.Unlock()

	if caller == nil {
		return
	}

	var res updateStateResult
	_ = caller.Call(ctx, "session.updateState", updateStateParams{
		SessionID: id,
		State:     string(st),
		Error:     errMsg,
	}, &res)
}

// CurrentState returns s's most recently recorded state.
func (s *Session) CurrentState() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

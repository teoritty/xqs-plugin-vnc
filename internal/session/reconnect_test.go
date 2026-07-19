package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"xqs-plugin-vnc/internal/transport"
)

// noopRelay's StartRelay succeeds without spawning a pump, so a test drives ReportRelayEnded itself.
type noopRelay struct{}

func (noopRelay) StartRelay(ctx context.Context, s *Session, tcp, embed *transport.Channel) error {
	return nil
}

// TestReconnect_RecoversWithinWindow proves a lost relay is re-established in the SAME session
// (state returns to ready, no teardown) and that registerEmbed is re-issued so the embed token
// rotates and the frontend reloads a fresh noVNC.
func TestReconnect_RecoversWithinWindow(t *testing.T) {
	caller := &fakeCaller{}
	s := &Session{id: "sess-rc", caller: caller, password: []byte("pw"), relay: noopRelay{}}

	s.orchestrate(context.Background())
	if got := s.CurrentState(); got != StateReady {
		t.Fatalf("setup: state = %q, want ready", got)
	}
	before := caller.callCount("session.registerEmbed")

	s.ReportRelayEnded(context.Background(), errors.New("read: connection reset by peer"))

	if got := s.CurrentState(); got != StateReady {
		t.Fatalf("state = %q, want ready after reconnect", got)
	}
	if after := caller.callCount("session.registerEmbed"); after <= before {
		t.Fatalf("registerEmbed calls = %d, want > %d (token must rotate on reconnect)", after, before)
	}
	s.mu.Lock()
	td := s.torndown
	s.mu.Unlock()
	if td {
		t.Fatal("session torn down after a successful reconnect")
	}
}

// TestReconnect_GivesUpAfterWindow proves that when re-establishing keeps failing, the session goes
// to error ("connection lost") and tears down once the window elapses.
func TestReconnect_GivesUpAfterWindow(t *testing.T) {
	oldW, oldB := reconnectWindow, reconnectBackoff
	reconnectWindow, reconnectBackoff = 40*time.Millisecond, 5*time.Millisecond
	defer func() { reconnectWindow, reconnectBackoff = oldW, oldB }()

	caller := &fakeCaller{callErr: errors.New("dial: connection refused")}
	s := &Session{id: "sess-rc-fail", caller: caller, password: []byte("pw"), relay: noopRelay{}}

	s.ReportRelayEnded(context.Background(), errors.New("read: connection reset by peer"))

	if got := s.CurrentState(); got != StateError {
		t.Fatalf("state = %q, want error after the reconnect window elapsed", got)
	}
	s.mu.Lock()
	td := s.torndown
	s.mu.Unlock()
	if !td {
		t.Fatal("session not torn down after giving up on reconnect")
	}
}

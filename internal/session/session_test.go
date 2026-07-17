package session

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"xqs-plugin-vnc/internal/ipc"
	"xqs-plugin-vnc/internal/transport"
)

// fakeCaller is a configurable fake transport.RPCCaller for session
// package tests: channel.open returns increasing channel ids;
// session.registerEmbed/session.updateState return canned {"ok":true}-ish
// results; an optional openDelay simulates a slow dial/handshake so tests
// can prove the connect RPC handler itself returns well before that
// delay elapses.
type fakeCaller struct {
	mu sync.Mutex

	nextChannelID uint32
	openDelay     time.Duration
	callErr       error

	calls         []string
	registerCalls int
}

func (f *fakeCaller) Call(ctx context.Context, method string, params any, result any) error {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	if method == "session.registerEmbed" {
		f.registerCalls++
	}
	delay := f.openDelay
	err := f.callErr
	f.mu.Unlock()

	if method == "channel.open" && delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if err != nil {
		return err
	}

	switch r := result.(type) {
	case *struct {
		ChannelID uint32 `json:"channelId"`
	}:
		f.mu.Lock()
		f.nextChannelID++
		r.ChannelID = f.nextChannelID
		f.mu.Unlock()
	default:
		// Generic path: round-trip a canned response through JSON so this
		// fake doesn't need a case for every result type (channelOpenResult
		// lives in package transport, unexported).
		var canned []byte
		switch method {
		case "channel.open":
			f.mu.Lock()
			f.nextChannelID++
			id := f.nextChannelID
			f.mu.Unlock()
			canned, _ = json.Marshal(map[string]any{"channelId": id})
		case "session.registerEmbed":
			canned, _ = json.Marshal(map[string]any{
				"embedToken": "token123",
				"uiUrl":      "/embed/s/token123/ui/index.html",
				"tunnelUrl":  "/embed/s/token123/tunnel/main",
				"expiresAt":  "2026-07-03T20:00:00Z",
			})
		case "session.updateState":
			canned, _ = json.Marshal(map[string]any{"ok": true})
		default:
			canned = []byte(`{}`)
		}
		return json.Unmarshal(canned, result)
	}
	return nil
}

func (f *fakeCaller) Notify(ctx context.Context, method string, params any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "notify:"+method)
	return nil
}

func (f *fakeCaller) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == method {
			n++
		}
	}
	return n
}

var _ transport.RPCCaller = (*fakeCaller)(nil)

func newTestHandler(caller *fakeCaller) (*Handler, *bytes.Buffer) {
	var buf bytes.Buffer
	enc := ipc.NewEncoder(&buf)
	reg := transport.NewRegistry()
	h := NewHandler(enc, caller, enc, reg, "ui/vnc.html", nil)
	return h, &buf
}

func connectRequest(id, sessionID string, password string) *ipc.Request {
	params, _ := json.Marshal(map[string]any{
		"sessionId": sessionID,
		"host":      "10.0.0.5",
		"port":      5900,
		"fields": map[string]any{
			"password": password,
			"readOnly": false,
		},
	})
	return &ipc.Request{ID: id, Method: MethodSessionConnect, Params: params}
}

// TestHandleConnect_RespondsImmediately proves the session.connect RPC
// handler returns well under the host's 5 s synchronous timeout even
// when the background goroutine's channel.open work is artificially
// slowed (simulating a slow dial/handshake).
func TestHandleConnect_RespondsImmediately(t *testing.T) {
	caller := &fakeCaller{openDelay: 2 * time.Second}
	h, buf := newTestHandler(caller)

	req := connectRequest("1", "sess-1", "hunter2")

	start := time.Now()
	if err := h.HandleRPC(context.Background(), ipc.MessageRequest, req); err != nil {
		t.Fatalf("HandleRPC: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("handleConnect took %v, want well under 5s (ideally near-instant)", elapsed)
	}

	kind, msg, err := ipc.DecodeMessage(bytes.TrimSpace(buf.Bytes()[9:]))
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if kind != ipc.MessageResponse {
		t.Fatalf("kind = %v, want MessageResponse", kind)
	}
	resp := msg.(*ipc.Response)
	var result connectResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !result.Accepted {
		t.Fatalf("accepted = false, want true")
	}

	// Let the slow background goroutine finish so it doesn't leak past the
	// test (it will eventually reach ready/error on its own).
	time.Sleep(2100 * time.Millisecond)
}

// TestOrchestrate_PanicRecoversToErrorState proves a panic anywhere in
// orchestrate() is recovered and routed to an error-state transition
// instead of crashing the test process (standing in for the plugin
// process).
func TestOrchestrate_PanicRecoversToErrorState(t *testing.T) {
	caller := &fakeCaller{}
	s := &Session{
		id:       "sess-panic",
		caller:   caller,
		password: []byte("secret"),
		relay:    panicRelay{},
	}

	done := make(chan struct{})
	go func() {
		s.orchestrate(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("orchestrate did not return after panic; process would have crashed for real")
	}

	if got := s.CurrentState(); got != StateError {
		t.Fatalf("state = %q, want %q", got, StateError)
	}
}

type panicRelay struct{}

func (panicRelay) StartRelay(ctx context.Context, s *Session, tcpChannel, embedChannel *transport.Channel) error {
	panic("simulated relay panic")
}

// TestPasswordZeroedAfterTeardown proves the password buffer is all-zero
// after Teardown, including after a simulated panic in orchestrate.
func TestPasswordZeroedAfterTeardown(t *testing.T) {
	caller := &fakeCaller{}
	password := []byte("hunter2")
	s := &Session{id: "sess-td", caller: caller, password: password}

	s.Teardown(context.Background(), "test")

	for i, b := range password {
		if b != 0 {
			t.Fatalf("password[%d] = %d, want 0 after Teardown", i, b)
		}
	}

	// Idempotent: a second Teardown call must not panic or double-close.
	s.Teardown(context.Background(), "test")
}

func TestPasswordZeroedAfterPanicRecovery(t *testing.T) {
	caller := &fakeCaller{}
	password := []byte("hunter2")
	s := &Session{id: "sess-panic-pw", caller: caller, password: password, relay: panicRelay{}}

	s.orchestrate(context.Background())

	for i, b := range password {
		if b != 0 {
			t.Fatalf("password[%d] = %d, want 0 after panic recovery", i, b)
		}
	}
}

// TestRegisterEmbed_ReCallable proves session.registerEmbed can be
// called more than once for the same Session (crash-recovery re-drive),
// producing a fresh RPC call each time rather than a cached result.
func TestRegisterEmbed_ReCallable(t *testing.T) {
	caller := &fakeCaller{}
	s := &Session{id: "sess-re", caller: caller, embedEntry: "ui/vnc.html"}

	if _, err := s.registerEmbed(context.Background()); err != nil {
		t.Fatalf("first registerEmbed: %v", err)
	}
	if _, err := s.registerEmbed(context.Background()); err != nil {
		t.Fatalf("second registerEmbed: %v", err)
	}

	if caller.registerCalls != 2 {
		t.Fatalf("registerCalls = %d, want 2", caller.registerCalls)
	}
}

// TestOrchestrate_FullSequence exercises the whole connect -> channels
// -> registerEmbed -> ready sequence against the fake caller, without
// any RFB/relay logic (relay is nil, the documented no-op stub).
func TestOrchestrate_FullSequence(t *testing.T) {
	caller := &fakeCaller{}
	s := &Session{
		id:          "sess-full",
		caller:      caller,
		frameWriter: discardWriter{},
		registry:    transport.NewRegistry(),
		embedEntry:  "ui/vnc.html",
	}

	s.orchestrate(context.Background())

	if got := s.CurrentState(); got != StateReady {
		t.Fatalf("state = %q, want %q", got, StateReady)
	}
	if caller.callCount("channel.open") != 2 {
		t.Fatalf("channel.open calls = %d, want 2", caller.callCount("channel.open"))
	}
	if caller.callCount("session.registerEmbed") != 1 {
		t.Fatalf("registerEmbed calls = %d, want 1", caller.callCount("session.registerEmbed"))
	}
}

type discardWriter struct{}

func (discardWriter) Encode(kind ipc.Kind, channelID uint32, payload []byte) error { return nil }

// TestHandleDisconnect_TearsDownSession proves session.disconnect closes
// the session's channels and zeroes its password.
func TestHandleDisconnect_TearsDownSession(t *testing.T) {
	caller := &fakeCaller{}
	h, _ := newTestHandler(caller)

	req := connectRequest("1", "sess-dc", "topsecret")
	if err := h.HandleRPC(context.Background(), ipc.MessageRequest, req); err != nil {
		t.Fatalf("HandleRPC connect: %v", err)
	}

	// Wait for orchestrate to reach ready (fast fake caller, no delay).
	deadline := time.Now().Add(2 * time.Second)
	var s *Session
	for time.Now().Before(deadline) {
		s = h.session("sess-dc")
		if s != nil && s.CurrentState() == StateReady {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s == nil || s.CurrentState() != StateReady {
		t.Fatalf("session did not reach ready in time (state=%v)", s.CurrentState())
	}

	params, _ := json.Marshal(map[string]any{"sessionId": "sess-dc"})
	n := &ipc.Notification{Method: MethodSessionDisconnect, Params: params}
	if err := h.HandleRPC(context.Background(), ipc.MessageNotification, n); err != nil {
		t.Fatalf("HandleRPC disconnect: %v", err)
	}

	pw := s.Password()
	for i, b := range pw {
		if b != 0 {
			t.Fatalf("password[%d] = %d, want 0 after disconnect", i, b)
		}
	}
}

// TestParseConnectFields covers password/readOnly extraction, including
// the string-encoded readOnly default the manifest declares.
func TestParseConnectFields(t *testing.T) {
	fields := map[string]json.RawMessage{
		"password": json.RawMessage(`"s3cr3t"`),
		"readOnly": json.RawMessage(`true`),
	}
	pw, ro, err := parseConnectFields(fields)
	if err != nil {
		t.Fatalf("parseConnectFields: %v", err)
	}
	if string(pw) != "s3cr3t" {
		t.Fatalf("password = %q, want s3cr3t", pw)
	}
	if !ro {
		t.Fatalf("readOnly = false, want true")
	}

	pw2, ro2, err := parseConnectFields(map[string]json.RawMessage{
		"readOnly": json.RawMessage(`"false"`),
	})
	if err != nil {
		t.Fatalf("parseConnectFields (string bool): %v", err)
	}
	if pw2 != nil {
		t.Fatalf("password = %v, want nil", pw2)
	}
	if ro2 {
		t.Fatalf("readOnly = true, want false")
	}
}

// TestHandleEmbedViewportAndActivity proves the notifications update the
// tracked Session state.
func TestHandleEmbedViewportAndActivity(t *testing.T) {
	caller := &fakeCaller{}
	h, _ := newTestHandler(caller)
	s := &Session{id: "sess-vp", caller: caller, active: true}
	h.setSession("sess-vp", s)

	vpParams, _ := json.Marshal(map[string]any{
		"sessionId":        "sess-vp",
		"widthPx":          1280,
		"heightPx":         720,
		"devicePixelRatio": 1.25,
		"active":           true,
	})
	h.handleEmbedViewport(&ipc.Notification{Method: MethodSessionEmbedViewport, Params: vpParams})

	vp := s.LatestViewport()
	if vp.WidthPx != 1280 || vp.HeightPx != 720 {
		t.Fatalf("unexpected viewport: %+v", vp)
	}

	actParams, _ := json.Marshal(map[string]any{"sessionId": "sess-vp", "active": false})
	h.handleEmbedActivity(&ipc.Notification{Method: MethodSessionEmbedActivity, Params: actParams})

	if s.IsActive() {
		t.Fatalf("IsActive() = true, want false after embedActivity(active:false)")
	}
}

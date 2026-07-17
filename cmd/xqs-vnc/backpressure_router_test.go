package main

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
	"time"
)

// TestRouter_TunnelBackpressureAndResumeReachSession is the router-level
// regression test for the bug this task fixes: wiring.go's sessionMethods
// map (consulted by router.routeNotification) previously listed only
// session.connect/disconnect/embedViewport/embedActivity, so a real
// session.tunnelBackpressure/session.tunnelResume notification arriving
// from the host over the wire was silently dropped by the default case in
// routeNotification before it ever reached session.Handler.HandleRPC —
// even though internal/session's handleTunnelBackpressure/handleTunnelResume
// and internal/relay's gate-polling pump were both fully implemented and
// unit-tested in isolation (see internal/session/session_test.go and
// internal/relay/pump_test.go's TestPumpServerToBrowser_GateBlocksReadUntilCleared).
// Those tests call the handlers/gate directly and so could never catch a
// missing router registration.
//
// This test drives the real process end to end — run(), the real router,
// the real session.Handler, the real relay.Pump — exactly like
// TestFullProcess_InitializeActivateConnectReachesReady, then sends a
// framed session.tunnelBackpressure notification from the fake host and
// checks that bytes the fake VNC server writes afterward do NOT reach the
// fake browser (the relay's server->browser read is gated). It then sends
// session.tunnelResume and checks the same bytes flow through. Before the
// fix (sessionMethods missing the two constants) this test hangs on the
// "resume" step forever (the notification is silently dropped, so
// WaitForReadClearance never unblocks) — see this file's companion
// verification note in .superpowers/sdd/task-13-report.md for how that was
// confirmed by temporarily reverting wiring.go's fix.
func TestRouter_TunnelBackpressureAndResumeReachSession(t *testing.T) {
	hostToPluginR, hostToPluginW := io.Pipe()
	pluginToHostR, pluginToHostW := io.Pipe()
	t.Cleanup(func() {
		hostToPluginR.Close()
		hostToPluginW.Close()
		pluginToHostR.Close()
		pluginToHostW.Close()
	})

	runDone := make(chan int, 1)
	var stderrBuf bytes.Buffer
	go func() {
		runDone <- run(hostToPluginR, pluginToHostW, &stderrBuf)
	}()

	h := newFakeHostProcess(t, pluginToHostR, hostToPluginW)
	go h.loop()

	if _, err := h.call(t, "initialize", nil); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if _, err := h.call(t, "activate", nil); err != nil {
		t.Fatalf("activate: %v", err)
	}

	const sessionID = "sess-bp-1"
	connectParams := map[string]any{
		"sessionId":    sessionID,
		"connectionId": "conn-bp-1",
		"protocol":     "vnc",
		"host":         "10.0.0.5",
		"port":         5900,
		"username":     "",
		"fields": map[string]any{
			"password": "secret",
			"readOnly": false,
		},
	}
	connectResp, err := h.call(t, "session.connect", connectParams)
	if err != nil {
		t.Fatalf("session.connect: %v", err)
	}
	var connectResult struct {
		Accepted bool `json:"accepted"`
	}
	if err := json.Unmarshal(connectResp.Result, &connectResult); err != nil {
		t.Fatalf("decode session.connect result: %v", err)
	}
	if !connectResult.Accepted {
		t.Fatalf("session.connect not accepted: %+v", connectResp)
	}

	// Arm backpressure through the REAL router path as early as possible
	// — immediately after session.connect is accepted, well before the
	// relay pump goroutine (started inside orchestrate(), after both
	// channel opens, registerEmbed, and both RFB handshakes) ever gets to
	// its first gate check. This ordering guarantees the pump observes
	// the gate already armed rather than racing a blocking Read call
	// that started before the gate existed (mirroring
	// internal/relay/pump_test.go's TestPumpServerToBrowser_GateBlocksReadUntilCleared,
	// which starts its fake gate already withheld before the pump goroutine
	// launches, for the same reason).
	h.notify(t, "session.tunnelBackpressure", map[string]any{"sessionId": sessionID})

	deadline := time.After(10 * time.Second)
	reachedReady := false
	for !reachedReady {
		select {
		case state := <-h.stateUpdates:
			switch state.State {
			case "ready":
				reachedReady = true
			case "error":
				t.Fatalf("session.updateState reported error: %q", state.Error)
			case "connecting":
			default:
				t.Fatalf("unexpected session.updateState state = %q (error=%q)", state.State, state.Error)
			}
		case <-deadline:
			t.Fatal("timed out waiting for session.updateState(ready); stderr=" + stderrBuf.String())
		}
	}

	h.mu.Lock()
	tcpStream := h.tcpStream
	embedStream := h.embedStream
	h.mu.Unlock()
	if tcpStream == nil || embedStream == nil {
		t.Fatal("tcp-relay/embed-stream channels were not opened")
	}

	payload := []byte("hello-while-backpressured")
	if _, err := tcpStream.Write(payload); err != nil {
		t.Fatalf("tcpStream.Write: %v", err)
	}

	readDone := make(chan struct{})
	go func() {
		buf := make([]byte, len(payload))
		io.ReadFull(embedStream, buf)
		close(readDone)
	}()

	select {
	case <-readDone:
		t.Fatal("browser side received bytes while backpressured; router did not deliver session.tunnelBackpressure to the session gate")
	case <-time.After(300 * time.Millisecond):
		// Still blocked, as expected: proves the notification reached
		// the session's gate via routeNotification.
	}

	// Clear backpressure through the REAL router path.
	h.notify(t, "session.tunnelResume", map[string]any{"sessionId": sessionID})

	select {
	case <-readDone:
		// Resume reached the gate via routeNotification and unblocked
		// the relay's server->browser read.
	case <-time.After(5 * time.Second):
		t.Fatal("browser side never received bytes after session.tunnelResume; router did not deliver the resume to the session gate")
	}

	if _, err := h.call(t, "shutdown", nil); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	select {
	case code := <-runDone:
		if code != 0 {
			t.Fatalf("run() exit code = %d, want 0; stderr=%s", code, stderrBuf.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not exit after shutdown")
	}
}

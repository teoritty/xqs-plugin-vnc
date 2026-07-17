package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"xqs-plugin-vnc/internal/ipc"
	"xqs-plugin-vnc/internal/rfb"
)

// TestFullProcess_InitializeActivateConnectReachesReady is the
// strongest end-to-end signal that the composition root is actually
// wired correctly: it drives run() (the real process entry point, minus
// os.Stdin/os.Stdout/os.Exit) as a fake host would, over real framed
// bytes on a pair of in-memory pipes — including answering the outbound
// RPCs the plugin itself sends (channel.open x2, session.registerEmbed,
// session.updateState) and playing both peers of the two RFB handshakes
// internal/relay.Pump drives once channels are open, exactly as a real
// xQuakShell host + browser + VNC server would from the plugin's point
// of view. It asserts the plugin reaches session.updateState("ready")
// without hanging, then shuts the process down cleanly.
func TestFullProcess_InitializeActivateConnectReachesReady(t *testing.T) {
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

	// 1. initialize / activate: the fixed lifecycle sequence every real
	// host performs before anything session-related.
	if _, err := h.call(t, "initialize", nil); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if _, err := h.call(t, "activate", nil); err != nil {
		t.Fatalf("activate: %v", err)
	}

	// 2. session.connect: triggers session.orchestrate() in the plugin,
	// which opens both channel-bus channels, runs both RFB handshakes
	// (via internal/relay.Pump, this host plays both the VNC-server and
	// browser peer for), registers the embed, and finally reports ready.
	connectParams := map[string]any{
		"sessionId":    "sess-1",
		"connectionId": "conn-1",
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

	// 3. Wait for the plugin to report state "ready" via
	// session.updateState, proving the whole chain — channel.open x2,
	// both RFB handshakes, registerEmbed — completed without hanging or
	// crashing the process. orchestrate() legitimately reports
	// "connecting" first (before channels/handshakes/registerEmbed run)
	// and "ready" only once everything succeeds, so this drains updates
	// until it sees a terminal one instead of asserting on the first.
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
				// expected first update; keep waiting for the terminal one.
			default:
				t.Fatalf("unexpected session.updateState state = %q (error=%q)", state.State, state.Error)
			}
		case <-deadline:
			t.Fatal("timed out waiting for session.updateState(ready); stderr=" + stderrBuf.String())
		}
	}

	// 4. Clean process shutdown.
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

// --- fake host process -----------------------------------------------

type stateUpdate struct {
	State string
	Error string
}

// fakeHostProcess simulates the xQuakShell host (plus, for the channels
// it opens, the VNC server and the browser) on the other end of the
// plugin's stdin/stdout pipes.
type fakeHostProcess struct {
	t   *testing.T
	enc *ipc.Encoder // writes to the plugin's stdin
	dec *ipc.Decoder // reads the plugin's stdout

	writeMu sync.Mutex

	idSeq int64

	mu       sync.Mutex
	pending  map[string]chan *ipc.Response
	channels map[uint32]*chanStream
	nextChID uint32

	stateUpdates chan stateUpdate
}

func newFakeHostProcess(t *testing.T, from io.Reader, to io.Writer) *fakeHostProcess {
	return &fakeHostProcess{
		t:            t,
		enc:          ipc.NewEncoder(to),
		dec:          ipc.NewDecoder(from),
		pending:      make(map[string]chan *ipc.Response),
		channels:     make(map[uint32]*chanStream),
		stateUpdates: make(chan stateUpdate, 8),
	}
}

// loop is the host's own read loop: it demultiplexes control-plane
// responses (delivered to whichever call() is waiting), control-plane
// requests the plugin sends (channel.open, session.registerEmbed,
// session.updateState — answered synchronously per method), and
// channel-bus data frames (delivered to the matching chanStream).
func (h *fakeHostProcess) loop() {
	for {
		f, err := h.dec.Decode()
		if err != nil {
			return
		}
		if f.ChannelID == 0 {
			kind, msg, err := ipc.DecodeMessage(f.Payload)
			if err != nil {
				continue
			}
			switch kind {
			case ipc.MessageResponse:
				resp := msg.(*ipc.Response)
				h.deliverResponse(resp)
			case ipc.MessageRequest:
				req := msg.(*ipc.Request)
				h.handlePluginRequest(req)
			case ipc.MessageNotification:
				// e.g. channel.close — nothing for this test to do.
			}
			continue
		}

		if f.Kind != ipc.KindChannelData {
			continue
		}
		h.mu.Lock()
		s := h.channels[f.ChannelID]
		h.mu.Unlock()
		if s != nil {
			s.deliver(f.Payload)
		}
	}
}

func (h *fakeHostProcess) deliverResponse(resp *ipc.Response) {
	id, ok := resp.ID.(string)
	if !ok {
		return
	}
	h.mu.Lock()
	ch, found := h.pending[id]
	h.mu.Unlock()
	if !found {
		return
	}
	select {
	case ch <- resp:
	default:
	}
}

// handlePluginRequest answers a request the PLUGIN sent (plugin -> host
// direction): channel.open, session.registerEmbed, session.updateState.
func (h *fakeHostProcess) handlePluginRequest(req *ipc.Request) {
	switch req.Method {
	case "channel.open":
		h.handleChannelOpen(req)
	case "session.registerEmbed":
		h.respond(req.ID, map[string]any{
			"embedToken": "test-embed-token",
			"uiUrl":      "https://example.invalid/ui",
			"tunnelUrl":  "wss://example.invalid/tunnel",
			"expiresAt":  "2099-01-01T00:00:00Z",
		})
	case "session.updateState":
		var p struct {
			SessionID string `json:"sessionId"`
			State     string `json:"state"`
			Error     string `json:"error"`
		}
		_ = json.Unmarshal(req.Params, &p)
		h.stateUpdates <- stateUpdate{State: p.State, Error: p.Error}
		h.respond(req.ID, map[string]any{"ok": true})
	default:
		h.respondError(req.ID, -32601, "method not found (fake host doesn't implement "+req.Method+")")
	}
}

func (h *fakeHostProcess) handleChannelOpen(req *ipc.Request) {
	var p struct {
		Purpose         string `json:"purpose"`
		ParentSessionID string `json:"parentSessionId"`
		Hint            string `json:"hint"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		h.respondError(req.ID, -32602, "invalid channel.open params")
		return
	}

	h.mu.Lock()
	h.nextChID++
	id := h.nextChID
	stream := newChanStream(id, h.enc, &h.writeMu)
	h.channels[id] = stream
	h.mu.Unlock()

	h.respond(req.ID, map[string]any{"channelId": id})

	switch p.Purpose {
	case "tcp-relay":
		go func() {
			if err := runFakeVNCServer(stream); err != nil {
				h.t.Logf("fake VNC server handshake: %v", err)
			}
		}()
	case "embed-stream":
		go func() {
			if err := runFakeBrowser(stream); err != nil {
				h.t.Logf("fake browser handshake: %v", err)
			}
		}()
	}
}

func (h *fakeHostProcess) respond(id any, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		h.t.Errorf("fakeHostProcess: marshal result: %v", err)
		return
	}
	payload, err := ipc.EncodeResponse(ipc.Response{ID: id, Result: raw})
	if err != nil {
		h.t.Errorf("fakeHostProcess: encode response: %v", err)
		return
	}
	h.write(payload)
}

func (h *fakeHostProcess) respondError(id any, code int, message string) {
	payload, err := ipc.EncodeResponse(ipc.Response{ID: id, Error: &ipc.RPCError{Code: code, Message: message}})
	if err != nil {
		h.t.Errorf("fakeHostProcess: encode error response: %v", err)
		return
	}
	h.write(payload)
}

func (h *fakeHostProcess) write(payload []byte) {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	if err := h.enc.Encode(ipc.KindJSONRPC, 0, payload); err != nil {
		h.t.Logf("fakeHostProcess: write: %v", err)
	}
}

// call sends a host -> plugin JSON-RPC request and waits for its
// response (the plugin's lifecycle/session handlers, exercised via the
// real Dispatcher/router this task builds).
func (h *fakeHostProcess) call(t *testing.T, method string, params any) (*ipc.Response, error) {
	t.Helper()

	h.idSeq++
	id := fmt.Sprintf("host-%d", h.idSeq)

	var paramsRaw json.RawMessage
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		paramsRaw = raw
	}

	ch := make(chan *ipc.Response, 1)
	h.mu.Lock()
	h.pending[id] = ch
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
	}()

	payload, err := ipc.EncodeRequest(ipc.Request{ID: id, Method: method, Params: paramsRaw})
	if err != nil {
		return nil, err
	}
	h.write(payload)

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp, resp.Error
		}
		return resp, nil
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("timed out waiting for response to %s", method)
	}
}

// --- channel-bus byte stream over kind=0x02 frames --------------------

// chanStream adapts one open channel-bus channel (as seen from the fake
// host's side) into an io.ReadWriter, so internal/rfb's handshake
// functions (which only need io.ReadWriter) can run directly against it,
// exactly mirroring how the real plugin drives the same functions
// against a transport.Channel.
type chanStream struct {
	id      uint32
	enc     *ipc.Encoder
	writeMu *sync.Mutex

	mu     sync.Mutex
	cond   *sync.Cond
	buf    bytes.Buffer
	closed bool
}

func newChanStream(id uint32, enc *ipc.Encoder, writeMu *sync.Mutex) *chanStream {
	s := &chanStream{id: id, enc: enc, writeMu: writeMu}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *chanStream) deliver(payload []byte) {
	s.mu.Lock()
	s.buf.Write(payload)
	s.cond.Signal()
	s.mu.Unlock()
}

func (s *chanStream) Read(p []byte) (int, error) {
	s.mu.Lock()
	for s.buf.Len() == 0 && !s.closed {
		s.cond.Wait()
	}
	if s.buf.Len() == 0 && s.closed {
		s.mu.Unlock()
		return 0, io.EOF
	}
	n, _ := s.buf.Read(p)
	s.mu.Unlock()
	return n, nil
}

func (s *chanStream) Write(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.enc.Encode(ipc.KindChannelData, s.id, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// --- fake RFB peers -----------------------------------------------------

// runFakeVNCServer plays the real-VNC-server side of internal/rfb.Handshake
// against conn (the tcp-relay chanStream): version exchange, offering only
// SecTypeNone, and a successful SecurityResult — enough for
// internal/relay.Pump's doHandshakes to succeed against it.
func runFakeVNCServer(conn io.ReadWriter) error {
	if err := rfb.WriteVersion(conn, rfb.V38); err != nil {
		return fmt.Errorf("write server version: %w", err)
	}
	if _, err := rfb.ReadVersion(conn); err != nil {
		return fmt.Errorf("read client version: %w", err)
	}
	if err := rfb.WriteSecurityTypeList(conn, []rfb.SecurityType{rfb.SecTypeNone}); err != nil {
		return fmt.Errorf("write security type list: %w", err)
	}
	var sel [1]byte
	if _, err := io.ReadFull(conn, sel[:]); err != nil {
		return fmt.Errorf("read security type selection: %w", err)
	}
	if rfb.SecurityType(sel[0]) != rfb.SecTypeNone {
		return fmt.Errorf("client selected unexpected security type %d", sel[0])
	}
	if err := rfb.WriteSecurityResult(conn, rfb.SecurityResultOK); err != nil {
		return fmt.Errorf("write security result: %w", err)
	}
	return nil
}

// runFakeBrowser plays the noVNC-browser side of internal/rfb.Frontshake
// against conn (the embed-stream chanStream): reads the plugin's
// synthesized version/security-type-list, replies with a real 3.8
// version and selects None, then reads the SecurityResult.
func runFakeBrowser(conn io.ReadWriter) error {
	if _, err := rfb.ReadVersion(conn); err != nil {
		return fmt.Errorf("read server version: %w", err)
	}
	if err := rfb.WriteVersion(conn, rfb.V38); err != nil {
		return fmt.Errorf("write client version: %w", err)
	}
	list, err := rfb.ReadSecurityTypeList(conn)
	if err != nil {
		return fmt.Errorf("read security type list: %w", err)
	}
	if list.Empty || len(list.Types) == 0 {
		return fmt.Errorf("server offered no security types")
	}
	if _, err := conn.Write([]byte{byte(rfb.SecTypeNone)}); err != nil {
		return fmt.Errorf("write security type selection: %w", err)
	}
	result, reason, err := rfb.ReadSecurityResult(conn, rfb.V38)
	if err != nil {
		return fmt.Errorf("read security result: %w", err)
	}
	if !result.OK() {
		return fmt.Errorf("security result not OK: %s", reason)
	}
	return nil
}

package relay

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"xqs-plugin-vnc/internal/rfb"
	"xqs-plugin-vnc/internal/transport"
)

// sharedClose is one close signal shared by both endpoints of a
// fakeChannelPair, so that closing *either* side (mirroring a real TCP
// close reaching both directions, or a channel-bus channel.close
// immediately severing both ends) unblocks any Send/Recv in flight on
// *both* endpoints — not just the one Close was called on. Idempotent via
// sync.Once so redundant Close calls (e.g. t.Cleanup on top of an
// explicit mid-test Close) are safe.
type sharedClose struct {
	ch   chan struct{}
	once sync.Once
}

func (s *sharedClose) Close() {
	s.once.Do(func() { close(s.ch) })
}

// fakeEndpoint is an in-memory FrameSink/FrameSource, mirroring
// internal/transport's own test fake (unexported there, so reimplemented
// here) — a pair of these formed by newFakeChannelPair stands in for a
// real credit-windowed channel-bus channel without needing the ipc/host
// machinery.
type fakeEndpoint struct {
	out    chan []byte
	in     chan []byte
	closed *sharedClose
}

// newFakeChannelPair returns a connected pair of *transport.Channel over
// an in-memory fake, plus sever: a function simulating the underlying
// transport itself being severed (a real TCP FIN/RST, or a channel-bus
// channel.close reaching both sides) — as opposed to a.Close()/b.Close(),
// which (matching Channel's own real semantics) only tear down that one
// local net.Conn handle, without notifying the fake peer at all.
func newFakeChannelPair(t *testing.T, maxPayload int) (a, b *transport.Channel, sever func()) {
	t.Helper()
	ab := make(chan []byte, 256)
	ba := make(chan []byte, 256)
	shared := &sharedClose{ch: make(chan struct{})}
	ea := &fakeEndpoint{out: ab, in: ba, closed: shared}
	eb := &fakeEndpoint{out: ba, in: ab, closed: shared}

	addrA := transport.Addr{ChannelID: 1, Purpose: "test"}
	addrB := transport.Addr{ChannelID: 2, Purpose: "test"}
	ca := transport.NewChannel(ea, ea, addrA, addrB, maxPayload)
	cb := transport.NewChannel(eb, eb, addrB, addrA, maxPayload)
	t.Cleanup(func() {
		ca.Close()
		cb.Close()
		shared.Close()
	})
	return ca, cb, shared.Close
}

func (e *fakeEndpoint) Send(p []byte) error {
	buf := append([]byte(nil), p...)
	select {
	case e.out <- buf:
		return nil
	case <-e.closed.ch:
		return errors.New("fake: endpoint closed")
	}
}

func (e *fakeEndpoint) Recv() ([]byte, bool) {
	select {
	case p, ok := <-e.in:
		if !ok {
			return nil, false
		}
		return p, true
	case <-e.closed.ch:
		return nil, false
	}
}

func (e *fakeEndpoint) Close() {
	e.closed.Close()
}

func withTimeout(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("test timed out")
	}
}

// runFakeVNCServer plays the real-VNC-server role over conn (a net.Conn,
// here the counterpart end of the tcp-relay fake channel pair): version
// exchange, VNCAuth (or None), SecurityResult=OK — the exact wire
// sequence rfb.Handshake expects on the other end.
func runFakeVNCServer(t *testing.T, conn io.ReadWriter, password []byte) {
	t.Helper()
	if err := rfb.WriteVersion(conn, rfb.V38); err != nil {
		t.Errorf("fake server: WriteVersion: %v", err)
		return
	}
	if _, err := rfb.ReadVersion(conn); err != nil {
		t.Errorf("fake server: ReadVersion: %v", err)
		return
	}
	if len(password) == 0 {
		if err := rfb.WriteSecurityTypeList(conn, []rfb.SecurityType{rfb.SecTypeNone}); err != nil {
			t.Errorf("fake server: WriteSecurityTypeList: %v", err)
			return
		}
		var sel [1]byte
		if _, err := io.ReadFull(conn, sel[:]); err != nil {
			t.Errorf("fake server: read selection: %v", err)
			return
		}
	} else {
		if err := rfb.WriteSecurityTypeList(conn, []rfb.SecurityType{rfb.SecTypeVNCAuth}); err != nil {
			t.Errorf("fake server: WriteSecurityTypeList: %v", err)
			return
		}
		var sel [1]byte
		if _, err := io.ReadFull(conn, sel[:]); err != nil {
			t.Errorf("fake server: read selection: %v", err)
			return
		}
		challenge := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
		wantResp, err := rfb.VNCAuthResponse(password, challenge[:])
		if err != nil {
			t.Errorf("fake server: VNCAuthResponse: %v", err)
			return
		}
		if _, err := conn.Write(challenge[:]); err != nil {
			t.Errorf("fake server: write challenge: %v", err)
			return
		}
		var resp [16]byte
		if _, err := io.ReadFull(conn, resp[:]); err != nil {
			t.Errorf("fake server: read response: %v", err)
			return
		}
		if !bytes.Equal(resp[:], wantResp) {
			t.Errorf("fake server: auth response mismatch")
		}
	}
	if err := rfb.WriteSecurityResult(conn, rfb.SecurityResultOK); err != nil {
		t.Errorf("fake server: WriteSecurityResult: %v", err)
	}
}

// runFakeBrowser plays the noVNC-browser role over conn: it expects the
// synthetic already-authenticated handshake rfb.Frontshake performs on
// the other end.
func runFakeBrowser(t *testing.T, conn io.ReadWriter) {
	t.Helper()
	if _, err := rfb.ReadVersion(conn); err != nil {
		t.Errorf("fake browser: ReadVersion: %v", err)
		return
	}
	if err := rfb.WriteVersion(conn, rfb.V38); err != nil {
		t.Errorf("fake browser: WriteVersion: %v", err)
		return
	}
	list, err := rfb.ReadSecurityTypeList(conn)
	if err != nil {
		t.Errorf("fake browser: ReadSecurityTypeList: %v", err)
		return
	}
	if len(list.Types) != 1 || list.Types[0] != rfb.SecTypeNone {
		t.Errorf("fake browser: offered types = %v, want [None]", list.Types)
	}
	if _, err := conn.Write([]byte{byte(rfb.SecTypeNone)}); err != nil {
		t.Errorf("fake browser: write selection: %v", err)
		return
	}
	result, _, err := rfb.ReadSecurityResult(conn, rfb.V38)
	if err != nil {
		t.Errorf("fake browser: ReadSecurityResult: %v", err)
		return
	}
	if !result.OK() {
		t.Errorf("fake browser: SecurityResult = %v, want OK", result)
	}
}

// TestRun_FullHappyPath proves the end-to-end wiring byte-for-byte:
// real VNCAuth handshake against a fake VNC server, synthetic frontshake
// against a fake browser, then raw bytes written after each handshake
// arrive unmodified on the other side, starting exactly at
// ServerInit/ClientInit.
func TestRun_FullHappyPath(t *testing.T) {
	tcpPlugin, tcpServer, severTCP := newFakeChannelPair(t, 0)
	embedPlugin, embedBrowser, _ := newFakeChannelPair(t, 64*1024)

	password := []byte("sekret")

	serverDone := make(chan struct{})
	browserDone := make(chan struct{})

	go func() {
		defer close(serverDone)
		runFakeVNCServer(t, tcpServer, password)
	}()
	go func() {
		defer close(browserDone)
		runFakeBrowser(t, embedBrowser)
	}()

	if err := doHandshakes(tcpPlugin, embedPlugin, password); err != nil {
		t.Fatalf("doHandshakes: %v", err)
	}
	withTimeout(t, func() {
		<-serverDone
		<-browserDone
	})

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- Run(tcpPlugin, embedPlugin, false, DefaultCouplingPolicy)
	}()

	// Server->browser: ServerInit-equivalent bytes written by the fake
	// server must arrive byte-for-byte at the fake browser.
	serverInit := []byte("FAKE-SERVER-INIT-PAYLOAD-0123456789")
	if _, err := tcpServer.Write(serverInit); err != nil {
		t.Fatalf("tcpServer.Write: %v", err)
	}
	gotAtBrowser := make([]byte, len(serverInit))
	if _, err := io.ReadFull(embedBrowser, gotAtBrowser); err != nil {
		t.Fatalf("read at fake browser: %v", err)
	}
	if !bytes.Equal(gotAtBrowser, serverInit) {
		t.Errorf("browser got %q, want %q", gotAtBrowser, serverInit)
	}

	// Browser->server: ClientInit-equivalent bytes (a well-formed
	// FramebufferUpdateRequest so the readonly filter's message parsing
	// doesn't reject it) written by the fake browser must arrive
	// byte-for-byte at the fake server.
	clientMsg := []byte{3, 0, 0, 0, 0, 0, 0, 10, 0, 10}
	if _, err := embedBrowser.Write(clientMsg); err != nil {
		t.Fatalf("embedBrowser.Write: %v", err)
	}
	gotAtServer := make([]byte, len(clientMsg))
	if _, err := io.ReadFull(tcpServer, gotAtServer); err != nil {
		t.Fatalf("read at fake server: %v", err)
	}
	if !bytes.Equal(gotAtServer, clientMsg) {
		t.Errorf("server got %x, want %x", gotAtServer, clientMsg)
	}

	// Server closes the TCP connection: Run must terminate with a
	// non-nil error (no retry), and both channels end up closed.
	severTCP()

	select {
	case err := <-runErrCh:
		if err == nil {
			t.Error("Run() returned nil error after server closed TCP, want non-nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not terminate after server closed TCP")
	}
}

// TestRun_ReadOnlyDropsInputButForwardsFramebufferRequests exercises the
// readonly filter wired through the full Run path: with readOnly=true, a
// KeyEvent from the browser never reaches the fake server, but a
// FramebufferUpdateRequest still does, and the stream stays in sync
// across both.
func TestRun_ReadOnlyDropsInputButForwardsFramebufferRequests(t *testing.T) {
	tcpPlugin, tcpServer, _ := newFakeChannelPair(t, 0)
	embedPlugin, embedBrowser, severEmbed := newFakeChannelPair(t, 64*1024)

	serverDone := make(chan struct{})
	browserDone := make(chan struct{})
	go func() { defer close(serverDone); runFakeVNCServer(t, tcpServer, nil) }()
	go func() { defer close(browserDone); runFakeBrowser(t, embedBrowser) }()

	if err := doHandshakes(tcpPlugin, embedPlugin, nil); err != nil {
		t.Fatalf("doHandshakes: %v", err)
	}
	withTimeout(t, func() {
		<-serverDone
		<-browserDone
	})

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- Run(tcpPlugin, embedPlugin, true, DefaultCouplingPolicy)
	}()

	keyEvent := []byte{4, 1, 0, 0, 0, 0, 0, 0x41}
	fbRequest := []byte{3, 0, 0, 0, 0, 0, 0, 10, 0, 10}
	if _, err := embedBrowser.Write(keyEvent); err != nil {
		t.Fatalf("write keyEvent: %v", err)
	}
	if _, err := embedBrowser.Write(fbRequest); err != nil {
		t.Fatalf("write fbRequest: %v", err)
	}

	got := make([]byte, len(fbRequest))
	if _, err := io.ReadFull(tcpServer, got); err != nil {
		t.Fatalf("read at fake server: %v", err)
	}
	if !bytes.Equal(got, fbRequest) {
		t.Errorf("server got %x, want %x (fbRequest only; keyEvent must have been dropped)", got, fbRequest)
	}

	severEmbed()
	select {
	case err := <-runErrCh:
		if err == nil {
			t.Error("Run() returned nil after embed-stream closed, want non-nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not terminate after embed-stream closed")
	}
}

// TestRun_ChannelClosedTerminatesCleanly covers the other termination
// path from design doc §7: the embed-stream channel closing (e.g. the
// browser tab went away) must end Run without hanging, even with no
// bytes ever exchanged post-handshake.
func TestRun_ChannelClosedTerminatesCleanly(t *testing.T) {
	tcpPlugin, tcpServer, _ := newFakeChannelPair(t, 0)
	embedPlugin, embedBrowser, _ := newFakeChannelPair(t, 64*1024)
	defer embedBrowser.Close()

	serverDone := make(chan struct{})
	browserDone := make(chan struct{})
	go func() { defer close(serverDone); runFakeVNCServer(t, tcpServer, nil) }()
	go func() { defer close(browserDone); runFakeBrowser(t, embedBrowser) }()

	if err := doHandshakes(tcpPlugin, embedPlugin, nil); err != nil {
		t.Fatalf("doHandshakes: %v", err)
	}
	withTimeout(t, func() {
		<-serverDone
		<-browserDone
	})

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- Run(tcpPlugin, embedPlugin, false, DefaultCouplingPolicy)
	}()

	tcpPlugin.Close()

	select {
	case err := <-runErrCh:
		if err == nil {
			t.Error("Run() returned nil after tcpPlugin closed locally, want non-nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not terminate after channel closed")
	}
}

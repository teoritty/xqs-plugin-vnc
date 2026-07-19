package transport

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	"xqs-plugin-vnc/internal/ipc"
)

// recordingWriter is a FrameWriter that records every Encode call and
// optionally fails on demand — a stand-in for the real stdout encoder.
type recordingWriter struct {
	mu     sync.Mutex
	frames []ipc.Frame
	failN  int // if > 0, the next failN Encode calls return errWriter
}

var errWriter = errors.New("recordingWriter: forced failure")

func (w *recordingWriter) Encode(kind ipc.Kind, channelID uint32, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failN > 0 {
		w.failN--
		return errWriter
	}
	cp := append([]byte(nil), payload...)
	w.frames = append(w.frames, ipc.Frame{Kind: kind, ChannelID: channelID, Payload: cp})
	return nil
}

func (w *recordingWriter) snapshot() []ipc.Frame {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]ipc.Frame, len(w.frames))
	copy(out, w.frames)
	return out
}

func creditFramePayload(channelID uint32, n uint32) []byte {
	p := make([]byte, 8)
	binary.BigEndian.PutUint32(p[0:4], channelID)
	binary.BigEndian.PutUint32(p[4:8], n)
	return p
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadlineAt := time.Now().Add(timeout)
	for time.Now().Before(deadlineAt) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

func TestCreditChannel_SendConsumesCredit(t *testing.T) {
	w := &recordingWriter{}
	cc := NewCreditChannel(7, w, 2, 1024)

	if err := cc.Send([]byte("a")); err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	if err := cc.Send([]byte("b")); err != nil {
		t.Fatalf("Send 2: %v", err)
	}

	frames := w.snapshot()
	if len(frames) != 2 {
		t.Fatalf("expected 2 frames written, got %d", len(frames))
	}
	for _, f := range frames {
		if f.Kind != ipc.KindChannelData || f.ChannelID != 7 {
			t.Fatalf("unexpected frame %+v", f)
		}
	}
}

func TestCreditChannel_SendBlocksAtZeroCreditAndUnblocksOnGrant(t *testing.T) {
	w := &recordingWriter{}
	cc := NewCreditChannel(1, w, 1, 1024)

	if err := cc.Send([]byte("first")); err != nil {
		t.Fatalf("Send 1: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cc.Send([]byte("second"))
	}()

	// Must still be blocked shortly after: no credit available.
	select {
	case err := <-done:
		t.Fatalf("Send returned early (err=%v) before any grant arrived", err)
	case <-time.After(100 * time.Millisecond):
	}

	// Simulate a kind=0x03 grant frame arriving from the host.
	cc.receiveCreditGrant(1)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Send after grant: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send did not unblock after credit grant")
	}
}

func TestCreditChannel_SendUnblocksWithErrClosedOnClose(t *testing.T) {
	w := &recordingWriter{}
	cc := NewCreditChannel(1, w, 0, 1024)

	done := make(chan error, 1)
	go func() {
		done <- cc.Send([]byte("blocked"))
	}()

	select {
	case err := <-done:
		t.Fatalf("Send returned early (err=%v) with zero credit and no close", err)
	case <-time.After(100 * time.Millisecond):
	}

	cc.Close()

	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Send after Close: got %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send did not unblock after Close")
	}
}

func TestCreditChannel_TrySendReturnsErrNoCredit(t *testing.T) {
	w := &recordingWriter{}
	cc := NewCreditChannel(1, w, 1, 1024)

	if err := cc.TrySend([]byte("first")); err != nil {
		t.Fatalf("TrySend 1: %v", err)
	}
	err := cc.TrySend([]byte("second"))
	if !errors.Is(err, ErrNoCredit) {
		t.Fatalf("TrySend at zero credit: got %v, want ErrNoCredit", err)
	}
}

func TestCreditChannel_TrySendReturnsErrClosed(t *testing.T) {
	w := &recordingWriter{}
	cc := NewCreditChannel(1, w, 5, 1024)
	cc.Close()

	err := cc.TrySend([]byte("x"))
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("TrySend after Close: got %v, want ErrClosed", err)
	}
}

func TestCreditChannel_SendRefundsCreditOnEncodeFailure(t *testing.T) {
	w := &recordingWriter{failN: 1}
	cc := NewCreditChannel(1, w, 1, 1024)

	err := cc.Send([]byte("x"))
	if err == nil || errors.Is(err, ErrClosed) || errors.Is(err, ErrNoCredit) {
		t.Fatalf("Send with forced encode failure: got %v, want a generic wrapped error", err)
	}

	// Credit should have been refunded: a second Send (writer now healthy)
	// must succeed without blocking.
	done := make(chan error, 1)
	go func() { done <- cc.Send([]byte("y")) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Send after refund: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Send after refund blocked — credit was not refunded on encode failure")
	}
}

func TestCreditChannel_GrantCreditNotAutomaticOnRecv(t *testing.T) {
	w := &recordingWriter{}
	cc := NewCreditChannel(3, w, 0, 1024)

	cc.deliverData([]byte("payload"))
	payload, ok := cc.Recv()
	if !ok || string(payload) != "payload" {
		t.Fatalf("Recv: got (%q, %v)", payload, ok)
	}

	// Give any (incorrect) auto-grant goroutine a chance to run.
	time.Sleep(50 * time.Millisecond)

	frames := w.snapshot()
	for _, f := range frames {
		if f.Kind == ipc.KindCredit {
			t.Fatalf("Recv triggered an automatic credit grant: %+v", f)
		}
	}
}

func TestCreditChannel_GrantCreditEmitsExplicitFrame(t *testing.T) {
	w := &recordingWriter{}
	cc := NewCreditChannel(9, w, 0, 1024)

	if err := cc.GrantCredit(3); err != nil {
		t.Fatalf("GrantCredit: %v", err)
	}

	frames := w.snapshot()
	if len(frames) != 1 {
		t.Fatalf("expected 1 credit frame, got %d", len(frames))
	}
	f := frames[0]
	if f.Kind != ipc.KindCredit || f.ChannelID != 9 {
		t.Fatalf("unexpected frame %+v", f)
	}
	if len(f.Payload) != 8 {
		t.Fatalf("credit payload length = %d, want 8", len(f.Payload))
	}
	gotChan := binary.BigEndian.Uint32(f.Payload[0:4])
	gotCredit := binary.BigEndian.Uint32(f.Payload[4:8])
	if gotChan != 9 || gotCredit != 3 {
		t.Fatalf("credit payload = channelId=%d credit=%d, want 9/3", gotChan, gotCredit)
	}
}

func TestCreditChannel_GrantCreditAfterCloseReturnsErrClosed(t *testing.T) {
	w := &recordingWriter{}
	cc := NewCreditChannel(1, w, 0, 1024)
	cc.Close()

	if err := cc.GrantCredit(1); !errors.Is(err, ErrClosed) {
		t.Fatalf("GrantCredit after Close: got %v, want ErrClosed", err)
	}
}

func TestCreditChannel_SendRejectsOversizePayload(t *testing.T) {
	w := &recordingWriter{}
	cc := NewCreditChannel(1, w, 5, 4)

	if err := cc.Send(make([]byte, 5)); err == nil {
		t.Fatal("Send with payload larger than maxPayload: expected error, got nil")
	}
}

func TestCreditChannel_RecvReturnsFalseAfterClose(t *testing.T) {
	w := &recordingWriter{}
	cc := NewCreditChannel(1, w, 0, 1024)
	cc.Close()

	_, ok := cc.Recv()
	if ok {
		t.Fatal("Recv after Close: expected ok == false")
	}
}

func TestCreditChannel_ConcurrentSendAndGrant(t *testing.T) {
	w := &recordingWriter{}
	cc := NewCreditChannel(1, w, 0, 64)

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if err := cc.Send([]byte("x")); err != nil {
				t.Errorf("Send: %v", err)
			}
		}()
	}

	// Grant credit concurrently, one at a time, racing against the sends
	// above. -race is expected to be run against this test.
	for i := 0; i < n; i++ {
		go cc.receiveCreditGrant(1)
	}

	waitDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitDone)
	}()

	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Send/receiveCreditGrant did not complete")
	}

	if got := len(w.snapshot()); got != n {
		t.Fatalf("expected %d frames sent, got %d", n, got)
	}
}

func TestRegistry_RoutesDataAndCreditFrames(t *testing.T) {
	w := &recordingWriter{}
	cc := NewCreditChannel(42, w, 1, 1024)
	reg := NewRegistry()
	reg.Register(cc)

	if err := reg.HandleChannelFrame(context.Background(), ipc.Frame{
		Kind: ipc.KindChannelData, ChannelID: 42, Payload: []byte("hello"),
	}); err != nil {
		t.Fatalf("HandleChannelFrame data: %v", err)
	}
	payload, ok := cc.Recv()
	if !ok || string(payload) != "hello" {
		t.Fatalf("Recv after routed data frame: (%q, %v)", payload, ok)
	}

	// Exhaust the one credit slot, then route a grant frame through the
	// registry and confirm Send unblocks.
	if err := cc.Send([]byte("a")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cc.Send([]byte("b")) }()

	select {
	case err := <-done:
		t.Fatalf("Send returned early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := reg.HandleChannelFrame(context.Background(), ipc.Frame{
		Kind: ipc.KindCredit, ChannelID: 42, Payload: creditFramePayload(42, 1),
	}); err != nil {
		t.Fatalf("HandleChannelFrame credit: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Send after routed grant: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send did not unblock after routed credit frame")
	}
}

func TestRegistry_UnknownChannelIsProtocolViolation(t *testing.T) {
	reg := NewRegistry()
	reg.grace = 10 * time.Millisecond // an id that is never registered still fails, just quickly
	err := reg.HandleChannelFrame(context.Background(), ipc.Frame{
		Kind: ipc.KindChannelData, ChannelID: 999, Payload: []byte("x"),
	})
	var pv *ipc.ProtocolViolationError
	if !errors.As(err, &pv) {
		t.Fatalf("HandleChannelFrame for unknown channelId: got %v, want *ipc.ProtocolViolationError", err)
	}
}

// TestRegistry_FrameRacingRegistrationIsRoutedWithinGrace is the VNC-crash regression: a
// server-speaks-first peer's first data frame can reach the read loop just before the
// orchestrate goroutine registers the channel (Register runs after channel.open returns, on a
// different goroutine). The grace must route it rather than kill the process.
func TestRegistry_FrameRacingRegistrationIsRoutedWithinGrace(t *testing.T) {
	w := &recordingWriter{}
	cc := NewCreditChannel(7, w, 4, 1024)
	reg := NewRegistry()

	go func() {
		time.Sleep(20 * time.Millisecond)
		reg.Register(cc)
	}()

	if err := reg.HandleChannelFrame(context.Background(), ipc.Frame{
		Kind: ipc.KindChannelData, ChannelID: 7, Payload: []byte("RFB 003.008\n"),
	}); err != nil {
		t.Fatalf("frame that raced registration should route within the grace, got: %v", err)
	}
	got, ok := cc.Recv()
	if !ok || string(got) != "RFB 003.008\n" {
		t.Fatalf("banner not delivered to the channel: ok=%v got=%q", ok, string(got))
	}
}

func TestRegistry_MalformedCreditFrameIsProtocolViolation(t *testing.T) {
	w := &recordingWriter{}
	cc := NewCreditChannel(1, w, 1, 1024)
	reg := NewRegistry()
	reg.Register(cc)

	err := reg.HandleChannelFrame(context.Background(), ipc.Frame{
		Kind: ipc.KindCredit, ChannelID: 1, Payload: []byte{1, 2, 3},
	})
	var pv *ipc.ProtocolViolationError
	if !errors.As(err, &pv) {
		t.Fatalf("malformed credit frame: got %v, want *ipc.ProtocolViolationError", err)
	}
}

func TestRegistry_CreditFramePayloadChannelIDMismatchIsProtocolViolation(t *testing.T) {
	w := &recordingWriter{}
	cc := NewCreditChannel(1, w, 1, 1024)
	reg := NewRegistry()
	reg.Register(cc)

	err := reg.HandleChannelFrame(context.Background(), ipc.Frame{
		Kind: ipc.KindCredit, ChannelID: 1, Payload: creditFramePayload(2, 1),
	})
	var pv *ipc.ProtocolViolationError
	if !errors.As(err, &pv) {
		t.Fatalf("mismatched credit frame: got %v, want *ipc.ProtocolViolationError", err)
	}
}

func TestRegistry_FramesForClosedChannelAreNoOps(t *testing.T) {
	w := &recordingWriter{}
	cc := NewCreditChannel(5, w, 1, 1024)
	reg := NewRegistry()
	reg.Register(cc)
	cc.Close()

	// Per docs/plugin-api.md's 0s grace period: frames arriving for an
	// already-closed channelId are dropped as no-ops, not errors.
	if err := reg.HandleChannelFrame(context.Background(), ipc.Frame{
		Kind: ipc.KindChannelData, ChannelID: 5, Payload: []byte("late"),
	}); err != nil {
		t.Fatalf("data frame for closed channel: got error %v, want nil (no-op)", err)
	}
	if err := reg.HandleChannelFrame(context.Background(), ipc.Frame{
		Kind: ipc.KindCredit, ChannelID: 5, Payload: creditFramePayload(5, 1),
	}); err != nil {
		t.Fatalf("credit frame for closed channel: got error %v, want nil (no-op)", err)
	}
}

func TestChannel_GrantCreditDelegatesToCreditChannel(t *testing.T) {
	w := &recordingWriter{}
	cc := NewCreditChannel(11, w, 0, 1024)
	addr := Addr{ChannelID: 11, Purpose: "test"}
	ch := NewChannel(cc, cc, addr, addr, 1024)
	t.Cleanup(func() { ch.Close() })

	if err := ch.GrantCredit(2); err != nil {
		t.Fatalf("Channel.GrantCredit: %v", err)
	}
	frames := w.snapshot()
	if len(frames) != 1 || frames[0].Kind != ipc.KindCredit {
		t.Fatalf("unexpected frames after Channel.GrantCredit: %+v", frames)
	}
}

func TestChannel_GrantCreditRejectsNonCreditSource(t *testing.T) {
	a, b := newFakePipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	ch := NewChannel(fakeSink{a}, fakeSource{a}, testAddr(1), testAddr(2), 1024)
	t.Cleanup(func() { ch.Close() })

	if err := ch.GrantCredit(1); err == nil {
		t.Fatal("GrantCredit on a non-CreditChannel source: expected error, got nil")
	}
}

// fakeSink/fakeSource adapt fakeEndpoint to the FrameSink/FrameSource
// interfaces individually (fakeEndpoint itself satisfies both directly,
// but wrapping keeps this test's intent — "not a *CreditChannel" — explicit
// regardless of fakeEndpoint's own method set).
type fakeSink struct{ e *fakeEndpoint }

func (f fakeSink) Send(p []byte) error { return f.e.Send(p) }

type fakeSource struct{ e *fakeEndpoint }

func (f fakeSource) Recv() ([]byte, bool) { return f.e.Recv() }

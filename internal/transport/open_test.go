package transport

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"xqs-plugin-vnc/internal/ipc"
)

// fakeRPCCaller is a fake RPCCaller for testing OpenChannel/CloseChannel
// without a real host: it just hands back a fixed channelId for
// channel.open and records channel.close notifications.
type fakeRPCCaller struct {
	nextChannelID uint32
	openErr       error

	lastMethod string
	lastParams any

	notifications []struct {
		method string
		params any
	}
	notifyErr error
}

func (f *fakeRPCCaller) Call(ctx context.Context, method string, params any, result any) error {
	f.lastMethod = method
	f.lastParams = params
	if f.openErr != nil {
		return f.openErr
	}
	switch r := result.(type) {
	case *channelOpenResult:
		r.ChannelID = f.nextChannelID
	default:
		// Round-trip through JSON to mimic a real client, for methods this
		// fake doesn't special-case.
		b, err := json.Marshal(struct {
			ChannelID uint32 `json:"channelId"`
		}{f.nextChannelID})
		if err != nil {
			return err
		}
		return json.Unmarshal(b, result)
	}
	return nil
}

func (f *fakeRPCCaller) Notify(ctx context.Context, method string, params any) error {
	f.notifications = append(f.notifications, struct {
		method string
		params any
	}{method, params})
	return f.notifyErr
}

func TestOpenChannel_TCPRelay_WiresCorrectCreditAndMaxPayload(t *testing.T) {
	caller := &fakeRPCCaller{nextChannelID: 5}
	w := &recordingWriter{}
	reg := NewRegistry()

	ch, err := OpenChannel(context.Background(), caller, w, reg, PurposeTCPRelay, "sess-1", "")
	if err != nil {
		t.Fatalf("OpenChannel: %v", err)
	}
	t.Cleanup(func() { ch.Close() })

	if caller.lastMethod != "channel.open" {
		t.Fatalf("method = %q, want channel.open", caller.lastMethod)
	}
	params, ok := caller.lastParams.(channelOpenParams)
	if !ok {
		t.Fatalf("params type = %T, want channelOpenParams", caller.lastParams)
	}
	if params.Purpose != "tcp-relay" || params.ParentSessionID != "sess-1" {
		t.Fatalf("unexpected params: %+v", params)
	}

	if ch.maxPayload != 1024*1024 {
		t.Fatalf("tcp-relay maxPayload = %d, want exactly 1048576 (1 MiB)", ch.maxPayload)
	}

	cc, ok := ch.source.(*CreditChannel)
	if !ok {
		t.Fatalf("channel source type = %T, want *CreditChannel", ch.source)
	}
	if cc.sendCred != 4 {
		t.Fatalf("tcp-relay initial credit = %d, want exactly 4", cc.sendCred)
	}
	if cc.id != 5 {
		t.Fatalf("channelId = %d, want 5", cc.id)
	}
}

func TestOpenChannel_EmbedStream_WiresCorrectCreditAndMaxPayload(t *testing.T) {
	caller := &fakeRPCCaller{nextChannelID: 9}
	w := &recordingWriter{}
	reg := NewRegistry()

	ch, err := OpenChannel(context.Background(), caller, w, reg, PurposeEmbedStream, "sess-2", "hint")
	if err != nil {
		t.Fatalf("OpenChannel: %v", err)
	}
	t.Cleanup(func() { ch.Close() })

	if ch.maxPayload != 64*1024 {
		t.Fatalf("embed-stream maxPayload = %d, want exactly 65536 (64 KiB, per API-FINDINGS.md F-8)", ch.maxPayload)
	}

	cc, ok := ch.source.(*CreditChannel)
	if !ok {
		t.Fatalf("channel source type = %T, want *CreditChannel", ch.source)
	}
	if cc.sendCred != 8 {
		t.Fatalf("embed-stream initial credit = %d, want exactly 8", cc.sendCred)
	}
}

func TestOpenChannel_RegistersWithRegistry(t *testing.T) {
	caller := &fakeRPCCaller{nextChannelID: 3}
	w := &recordingWriter{}
	reg := NewRegistry()

	ch, err := OpenChannel(context.Background(), caller, w, reg, PurposeTCPRelay, "", "")
	if err != nil {
		t.Fatalf("OpenChannel: %v", err)
	}
	t.Cleanup(func() { ch.Close() })

	// A data frame routed through the registry should reach the channel's
	// Read side, proving OpenChannel actually registered the CreditChannel
	// it built (not a disconnected one).
	if err := reg.HandleChannelFrame(context.Background(), ipc.Frame{
		Kind: ipc.KindChannelData, ChannelID: 3, Payload: []byte("hi"),
	}); err != nil {
		t.Fatalf("HandleChannelFrame: %v", err)
	}
	buf := make([]byte, 2)
	n, err := ch.Read(buf)
	if err != nil || n != 2 || string(buf) != "hi" {
		t.Fatalf("Read after registered frame: n=%d err=%v buf=%q", n, err, buf)
	}
}

func TestOpenChannel_UnknownPurposeRejected(t *testing.T) {
	caller := &fakeRPCCaller{nextChannelID: 1}
	w := &recordingWriter{}

	_, err := OpenChannel(context.Background(), caller, w, nil, "video-stream", "", "")
	if err == nil {
		t.Fatal("OpenChannel with unknown purpose: expected error, got nil")
	}
	if caller.lastMethod != "" {
		t.Fatalf("channel.open should not have been called for an unknown purpose; lastMethod = %q", caller.lastMethod)
	}
}

func TestOpenChannel_PropagatesRPCError(t *testing.T) {
	wantErr := errors.New("host: capability denied")
	caller := &fakeRPCCaller{openErr: wantErr}
	w := &recordingWriter{}

	_, err := OpenChannel(context.Background(), caller, w, nil, PurposeTCPRelay, "", "")
	if !errors.Is(err, wantErr) {
		t.Fatalf("OpenChannel error = %v, want wrapping %v", err, wantErr)
	}
}

func TestCloseChannel_SendsNotificationAndClosesLocally(t *testing.T) {
	caller := &fakeRPCCaller{nextChannelID: 7}
	w := &recordingWriter{}
	reg := NewRegistry()

	ch, err := OpenChannel(context.Background(), caller, w, reg, PurposeTCPRelay, "", "")
	if err != nil {
		t.Fatalf("OpenChannel: %v", err)
	}

	if err := CloseChannel(context.Background(), caller, ch, "done", "bye"); err != nil {
		t.Fatalf("CloseChannel: %v", err)
	}

	if len(caller.notifications) != 1 || caller.notifications[0].method != "channel.close" {
		t.Fatalf("notifications = %+v, want one channel.close", caller.notifications)
	}
	params, ok := caller.notifications[0].params.(channelCloseParams)
	if !ok {
		t.Fatalf("notification params type = %T", caller.notifications[0].params)
	}
	if params.ChannelID != 7 || params.Reason != "done" || params.Message != "bye" {
		t.Fatalf("unexpected close params: %+v", params)
	}

	// Idempotent: closing again must not error or send a second notification
	// via the underlying Channel.Close (CloseChannel itself would send a
	// second channel.close notification if called again, which is fine per
	// docs — "Close is idempotent regardless of which side initiates it or
	// how many times it arrives" — but the local Channel/CreditChannel must
	// not double-close or panic).
	if err := ch.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestCloseChannel_StillClosesLocallyIfNotifyFails(t *testing.T) {
	caller := &fakeRPCCaller{nextChannelID: 2, notifyErr: errors.New("stdio gone")}
	w := &recordingWriter{}
	reg := NewRegistry()

	ch, err := OpenChannel(context.Background(), caller, w, reg, PurposeTCPRelay, "", "")
	if err != nil {
		t.Fatalf("OpenChannel: %v", err)
	}

	_ = CloseChannel(context.Background(), caller, ch, "", "")

	// The channel must be locally closed regardless of the notify failure:
	// Read should return promptly (ErrClosed) rather than blocking forever.
	buf := make([]byte, 1)
	if _, err := ch.Read(buf); !errors.Is(err, ErrClosed) {
		t.Fatalf("Read after CloseChannel (notify failed) = %v, want ErrClosed", err)
	}
	if err := ch.Close(); err != nil {
		t.Fatalf("Close after CloseChannel: %v", err)
	}
}

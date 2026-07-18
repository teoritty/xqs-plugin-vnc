package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"xqs-plugin-vnc/internal/ipc"
)

// decodeOneResponse decodes a single framed JSON-RPC response written to
// buf and returns it.
func decodeOneResponse(t *testing.T, buf *bytes.Buffer) ipc.Response {
	t.Helper()
	dec := ipc.NewDecoder(buf)
	f, err := dec.Decode()
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if f.ChannelID != 0 || f.Kind != ipc.KindJSONRPC {
		t.Fatalf("unexpected frame: kind=%v channelId=%d", f.Kind, f.ChannelID)
	}
	kind, msg, err := ipc.DecodeMessage(f.Payload)
	if err != nil {
		t.Fatalf("DecodeMessage: %v", err)
	}
	if kind != ipc.MessageResponse {
		t.Fatalf("kind = %v, want MessageResponse", kind)
	}
	resp, ok := msg.(*ipc.Response)
	if !ok {
		t.Fatalf("msg type = %T, want *ipc.Response", msg)
	}
	return *resp
}

// dispatchRequest builds a Dispatcher over a fresh Handler/Encoder pair,
// encodes req as a JSON-RPC request frame, and dispatches it, returning
// the handler, the output buffer, and any Dispatch error.
func dispatchRequest(t *testing.T, method string, id any, params json.RawMessage) (*Handler, *bytes.Buffer, error) {
	t.Helper()
	var out bytes.Buffer
	h := NewHandler(ipc.NewEncoder(&out))
	d := ipc.NewDispatcher(h, nil)

	payload, err := ipc.EncodeRequest(ipc.Request{ID: id, Method: method, Params: params})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	f := ipc.Frame{Kind: ipc.KindJSONRPC, ChannelID: 0, Payload: payload}
	err = d.Dispatch(context.Background(), f)
	return h, &out, err
}

func TestInitializeRespondsOK(t *testing.T) {
	_, out, err := dispatchRequest(t, MethodInitialize, float64(1), json.RawMessage(`{"pluginId":"com.xquakshell.vnc"}`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	resp := decodeOneResponse(t, out)
	if resp.Error != nil {
		t.Fatalf("unexpected error response: %+v", resp.Error)
	}
	if string(resp.Result) != `{"ok":true}` {
		t.Errorf("result = %s, want {\"ok\":true}", resp.Result)
	}
	if resp.ID != float64(1) {
		t.Errorf("id = %v, want 1", resp.ID)
	}
}

func TestActivateRespondsOK(t *testing.T) {
	_, out, err := dispatchRequest(t, MethodActivate, "abc", json.RawMessage(`{"reason":"onProtocol:vnc"}`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	resp := decodeOneResponse(t, out)
	if resp.Error != nil {
		t.Fatalf("unexpected error response: %+v", resp.Error)
	}
	if resp.ID != "abc" {
		t.Errorf("id = %v, want abc", resp.ID)
	}
}

func TestPingRespondsPong(t *testing.T) {
	_, out, err := dispatchRequest(t, MethodPing, float64(2), nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	resp := decodeOneResponse(t, out)
	if resp.Error != nil {
		t.Fatalf("unexpected error response: %+v", resp.Error)
	}
	if string(resp.Result) != `{"pong":"ok"}` {
		t.Errorf("result = %s, want {\"pong\":\"ok\"}", resp.Result)
	}
}

func TestShutdownRespondsOKAndSignalsShutdown(t *testing.T) {
	h, out, err := dispatchRequest(t, MethodShutdown, float64(3), nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	resp := decodeOneResponse(t, out)
	if resp.Error != nil {
		t.Fatalf("unexpected error response: %+v", resp.Error)
	}
	if string(resp.Result) != `{"ok":true}` {
		t.Errorf("result = %s, want {\"ok\":true}", resp.Result)
	}
	select {
	case <-h.ShutdownRequested():
	default:
		t.Fatal("ShutdownRequested channel not closed after shutdown request")
	}
}

func TestUnknownMethodReturnsMethodNotFound(t *testing.T) {
	_, out, err := dispatchRequest(t, "session.connect", float64(4), nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	resp := decodeOneResponse(t, out)
	if resp.Error == nil {
		t.Fatal("expected error response for unknown method, got nil")
	}
	if resp.Error.Code != errMethodNotFound {
		t.Errorf("error code = %d, want %d", resp.Error.Code, errMethodNotFound)
	}
}

func TestDeactivateNotificationProducesNoResponseAndNoShutdown(t *testing.T) {
	var out bytes.Buffer
	h := NewHandler(ipc.NewEncoder(&out))
	d := ipc.NewDispatcher(h, nil)

	payload, err := ipc.EncodeNotification(ipc.Notification{Method: MethodDeactivate})
	if err != nil {
		t.Fatalf("EncodeNotification: %v", err)
	}
	f := ipc.Frame{Kind: ipc.KindJSONRPC, ChannelID: 0, Payload: payload}
	if err := d.Dispatch(context.Background(), f); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for a notification, got %d bytes", out.Len())
	}
	select {
	case <-h.ShutdownRequested():
		t.Fatal("deactivate must not trigger shutdown on its own")
	default:
	}
}

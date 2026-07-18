package ipc

import (
	"encoding/json"
	"testing"
)

func TestEncodeDecodeRequestRoundTrip(t *testing.T) {
	req := Request{ID: float64(1), Method: "ping", Params: json.RawMessage(`{"a":1}`)}
	b, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}

	kind, msg, err := DecodeMessage(b)
	if err != nil {
		t.Fatalf("DecodeMessage: %v", err)
	}
	if kind != MessageRequest {
		t.Fatalf("kind = %v, want MessageRequest", kind)
	}
	got, ok := msg.(*Request)
	if !ok {
		t.Fatalf("msg type = %T, want *Request", msg)
	}
	if got.Method != "ping" {
		t.Errorf("Method = %q, want ping", got.Method)
	}
	if got.ID != float64(1) {
		t.Errorf("ID = %v, want 1", got.ID)
	}
}

func TestEncodeDecodeNotificationRoundTrip(t *testing.T) {
	n := Notification{Method: "session.updateState", Params: json.RawMessage(`{"state":"ready"}`)}
	b, err := EncodeNotification(n)
	if err != nil {
		t.Fatalf("EncodeNotification: %v", err)
	}

	kind, msg, err := DecodeMessage(b)
	if err != nil {
		t.Fatalf("DecodeMessage: %v", err)
	}
	if kind != MessageNotification {
		t.Fatalf("kind = %v, want MessageNotification", kind)
	}
	got, ok := msg.(*Notification)
	if !ok {
		t.Fatalf("msg type = %T, want *Notification", msg)
	}
	if got.Method != "session.updateState" {
		t.Errorf("Method = %q", got.Method)
	}
}

func TestNotificationHasNoIDField(t *testing.T) {
	n := Notification{Method: "ping"}
	b, err := EncodeNotification(n)
	if err != nil {
		t.Fatalf("EncodeNotification: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, present := raw["id"]; present {
		t.Errorf("notification payload has an id field, want none: %s", b)
	}
}

func TestEncodeDecodeResponseRoundTrip(t *testing.T) {
	resp := Response{ID: float64(2), Result: json.RawMessage(`{"ok":true}`)}
	b, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}

	kind, msg, err := DecodeMessage(b)
	if err != nil {
		t.Fatalf("DecodeMessage: %v", err)
	}
	if kind != MessageResponse {
		t.Fatalf("kind = %v, want MessageResponse", kind)
	}
	got, ok := msg.(*Response)
	if !ok {
		t.Fatalf("msg type = %T, want *Response", msg)
	}
	if got.Error != nil {
		t.Errorf("Error = %v, want nil", got.Error)
	}
}

func TestResponseErrorShape(t *testing.T) {
	resp := Response{ID: float64(3), Error: &RPCError{Code: -32001, Message: "capability denied"}}
	b, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}

	kind, msg, err := DecodeMessage(b)
	if err != nil {
		t.Fatalf("DecodeMessage: %v", err)
	}
	if kind != MessageResponse {
		t.Fatalf("kind = %v, want MessageResponse", kind)
	}
	got := msg.(*Response)
	if got.Error == nil {
		t.Fatal("Error = nil, want set")
	}
	if got.Error.Code != -32001 || got.Error.Message != "capability denied" {
		t.Errorf("Error = %+v", got.Error)
	}
	if got.Result != nil {
		t.Errorf("Result = %s, want nil", got.Result)
	}
}

func TestDecodeMessageRejectsInvalidJSON(t *testing.T) {
	_, _, err := DecodeMessage([]byte(`not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestDecodeMessageRejectsMissingMethodOnRequestShape(t *testing.T) {
	_, _, err := DecodeMessage([]byte(`{"jsonrpc":"2.0","id":1}`))
	if err == nil {
		t.Fatal("expected error for message with id but no method, got nil")
	}
}

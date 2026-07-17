// This file (rpc.go) implements JSON-RPC 2.0 envelope types and their
// marshal/unmarshal to and from raw bytes — the payload half of an
// ipc.Frame with Kind == KindJSONRPC, not the frame itself. See codec.go
// for the frame layer this rides on top of.
//
// Per docs/plugin-api.md and JSON-RPC 2.0 (https://www.jsonrpc.org/specification):
//   - A Request carries an "id" and expects a Response.
//   - A Notification omits "id" and expects no response.
//   - A Response carries exactly one of "result" or "error", never both.
package ipc

import (
	"encoding/json"
	"errors"
	"fmt"
)

// protocolVersion is the fixed "jsonrpc" field value for JSON-RPC 2.0.
const protocolVersion = "2.0"

// RPCError matches the JSON-RPC 2.0 error object shape.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("ipc: rpc error %d: %s", e.Code, e.Message)
}

// Request is a JSON-RPC 2.0 request: it carries an ID and expects a
// Response from the peer.
type Request struct {
	ID     any             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response: exactly one of Result or Error is
// set, matching the ID of the Request it answers.
type Response struct {
	ID     any             `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// Notification is a JSON-RPC 2.0 notification: it has no ID and expects
// no response.
type Notification struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// envelope is the wire shape shared by all three message kinds; it is
// used only to sniff which concrete type a raw payload decodes to.
type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// ErrInvalidMessage indicates a payload that isn't well-formed JSON-RPC
// 2.0, or one whose shape doesn't unambiguously match Request,
// Notification, or Response.
var ErrInvalidMessage = errors.New("ipc: invalid JSON-RPC message")

// MessageKind identifies which concrete JSON-RPC shape a decoded payload
// turned out to be.
type MessageKind int

const (
	MessageUnknown MessageKind = iota
	MessageRequest
	MessageNotification
	MessageResponse
)

// EncodeRequest marshals r into a JSON-RPC 2.0 request payload.
func EncodeRequest(r Request) ([]byte, error) {
	return json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}{protocolVersion, r.ID, r.Method, r.Params})
}

// EncodeNotification marshals n into a JSON-RPC 2.0 notification payload.
func EncodeNotification(n Notification) ([]byte, error) {
	return json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}{protocolVersion, n.Method, n.Params})
}

// EncodeResponse marshals r into a JSON-RPC 2.0 response payload.
func EncodeResponse(r Response) ([]byte, error) {
	return json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id"`
		Result  json.RawMessage `json:"result,omitempty"`
		Error   *RPCError       `json:"error,omitempty"`
	}{protocolVersion, r.ID, r.Result, r.Error})
}

// DecodeMessage inspects payload and returns the concrete JSON-RPC value
// it represents (one of *Request, *Notification, *Response) along with a
// MessageKind tag identifying which. It distinguishes a Request from a
// Notification by presence of the "id" key (a Notification omits it
// entirely; a JSON null id is still considered present per JSON-RPC 2.0
// and therefore treated as a Request with a null ID). A payload carrying
// "result" or "error" is a Response.
func DecodeMessage(payload []byte) (MessageKind, any, error) {
	var env envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return MessageUnknown, nil, fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}

	// Response: has a result or error field.
	if env.Result != nil || env.Error != nil {
		var id any
		if env.ID != nil {
			if err := json.Unmarshal(env.ID, &id); err != nil {
				return MessageUnknown, nil, fmt.Errorf("%w: bad id: %v", ErrInvalidMessage, err)
			}
		}
		return MessageResponse, &Response{ID: id, Result: env.Result, Error: env.Error}, nil
	}

	if env.Method == "" {
		return MessageUnknown, nil, fmt.Errorf("%w: missing method", ErrInvalidMessage)
	}

	if env.ID == nil {
		return MessageNotification, &Notification{Method: env.Method, Params: env.Params}, nil
	}

	var id any
	if err := json.Unmarshal(env.ID, &id); err != nil {
		return MessageUnknown, nil, fmt.Errorf("%w: bad id: %v", ErrInvalidMessage, err)
	}
	return MessageRequest, &Request{ID: id, Method: env.Method, Params: env.Params}, nil
}

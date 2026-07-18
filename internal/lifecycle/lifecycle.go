// Package lifecycle implements the host->plugin RPC handlers for the
// fixed process lifecycle sequence described in docs/plugin-api.md
// ("Lifecycle RPC" table and "Shutdown sequence"): initialize, activate,
// ping, deactivate (notification), shutdown. It is the only thing this
// plugin can answer correctly before any VNC/session logic exists — see
// docs/superpowers/specs/2026-07-16-vnc-plugin-design.md §8 "Фаза 1".
//
// Handler implements ipc.RPCHandler and writes responses directly to an
// ipc.Encoder wired to the process's stdout, framed as kind=KindJSONRPC
// on channelId 0 (the control plane), per internal/ipc/dispatch.go.
package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"xqs-plugin-vnc/internal/ipc"
)

// Method names this handler answers. Anything else on a Request gets a
// JSON-RPC "method not found" error response; anything else on a
// Notification is silently ignored (notifications have no response
// channel to report an error on, and the host only ever sends documented
// notification methods).
const (
	MethodInitialize = "initialize"
	MethodActivate   = "activate"
	MethodPing       = "ping"
	MethodShutdown   = "shutdown"
	MethodDeactivate = "deactivate"
)

// errMethodNotFound is the JSON-RPC 2.0 standard code for an unknown
// method, per docs/plugin-api.md's RPC error code table.
const errMethodNotFound = -32601

// Handler answers the lifecycle RPC methods and tracks shutdown so the
// composition root (cmd/xqs-vnc/main.go) knows when to stop its read
// loop and exit the process.
type Handler struct {
	enc *ipc.Encoder

	mu       sync.Mutex
	shutdown chan struct{}
	once     sync.Once
}

// NewHandler returns a Handler that writes responses via enc.
func NewHandler(enc *ipc.Encoder) *Handler {
	return &Handler{enc: enc, shutdown: make(chan struct{})}
}

// ShutdownRequested returns a channel that is closed once this Handler
// has processed a "shutdown" request and written its response. The
// caller (main's read loop) should stop reading further frames and exit
// the process once this channel is closed, per the shutdown sequence in
// docs/plugin-api.md: deactivate notification, then shutdown RPC, then
// stdin closes.
func (h *Handler) ShutdownRequested() <-chan struct{} {
	return h.shutdown
}

// HandleRPC implements ipc.RPCHandler.
func (h *Handler) HandleRPC(ctx context.Context, kind ipc.MessageKind, msg any) error {
	switch kind {
	case ipc.MessageRequest:
		req, ok := msg.(*ipc.Request)
		if !ok {
			return fmt.Errorf("lifecycle: unexpected message type %T for MessageRequest", msg)
		}
		return h.handleRequest(req)
	case ipc.MessageNotification:
		n, ok := msg.(*ipc.Notification)
		if !ok {
			return fmt.Errorf("lifecycle: unexpected message type %T for MessageNotification", msg)
		}
		h.handleNotification(n)
		return nil
	default:
		// Responses (and MessageUnknown, which DecodeMessage never
		// actually returns without an error) aren't expected from the
		// host on the control plane at this phase; ignore rather than
		// fail the whole process over a message we don't act on.
		return nil
	}
}

func (h *Handler) handleRequest(req *ipc.Request) error {
	switch req.Method {
	case MethodInitialize, MethodActivate:
		// Response shape is "any JSON" per docs/plugin-api.md; {"ok":true}
		// is the simplest valid acknowledgement.
		return h.respondOK(req.ID)
	case MethodPing:
		return h.respond(req.ID, json.RawMessage(`{"pong":"ok"}`))
	case MethodShutdown:
		if err := h.respondOK(req.ID); err != nil {
			return err
		}
		h.once.Do(func() { close(h.shutdown) })
		return nil
	default:
		return h.respondError(req.ID, errMethodNotFound, "method not found")
	}
}

func (h *Handler) handleNotification(n *ipc.Notification) {
	// deactivate carries no response; it exists so, in a future phase,
	// this handler can start unwinding session/channel state before the
	// shutdown RPC arrives. At this phase there is nothing to unwind.
	// Unknown notification methods are ignored for the same reason: a
	// notification has no error channel back to the host.
	_ = n
}

func (h *Handler) respondOK(id any) error {
	return h.respond(id, json.RawMessage(`{"ok":true}`))
}

func (h *Handler) respond(id any, result json.RawMessage) error {
	payload, err := ipc.EncodeResponse(ipc.Response{ID: id, Result: result})
	if err != nil {
		return fmt.Errorf("lifecycle: encode response: %w", err)
	}
	return h.write(payload)
}

func (h *Handler) respondError(id any, code int, message string) error {
	payload, err := ipc.EncodeResponse(ipc.Response{ID: id, Error: &ipc.RPCError{Code: code, Message: message}})
	if err != nil {
		return fmt.Errorf("lifecycle: encode error response: %w", err)
	}
	return h.write(payload)
}

// write serializes access to the shared Encoder: HandleRPC is called
// from the single-goroutine read loop today, but serializing here keeps
// this handler safe if that ever changes.
func (h *Handler) write(payload []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.enc.Encode(ipc.KindJSONRPC, 0, payload)
}

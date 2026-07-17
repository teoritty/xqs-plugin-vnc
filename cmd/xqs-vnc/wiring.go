// This file (wiring.go) is the composition root's real construction
// logic, kept out of main.go so main.go itself stays a thin
// stdin/stdout/exit-code shim (per docs/superpowers/specs/2026-07-16-vnc-plugin-design.md
// §2's "main.go: только проводка"). It wires together every package
// built by prior tasks into one running process:
//
//   - internal/ipc: frame codec (Decoder/Encoder), the JSON-RPC Client
//     this task adds (client.go) for calls the plugin makes TO the host.
//   - internal/lifecycle: initialize/activate/ping/deactivate/shutdown.
//   - internal/session: session.connect and friends.
//   - internal/transport: channel-bus Registry (routes kind=0x02/0x03
//     frames to open channels) and OpenChannel/CloseChannel.
//   - internal/relay: the real RFB relay (session.RelayStarter).
//
// router (below) is the piece that ties the read loop's decoded
// JSON-RPC messages to the right destination: MessageResponse values go
// to the Client (answering an outstanding plugin -> host Call);
// MessageRequest/MessageNotification values are routed by method name to
// either the lifecycle.Handler or the session.Handler, matching each
// package's own documented method set exactly (no guessing/overlap).
package main

import (
	"context"
	"fmt"
	"io"
	"sync"

	"xqs-plugin-vnc/internal/ipc"
	"xqs-plugin-vnc/internal/lifecycle"
	"xqs-plugin-vnc/internal/relay"
	"xqs-plugin-vnc/internal/session"
	"xqs-plugin-vnc/internal/transport"
)

// embedEntry must match plugin.json's contributions.connectionProtocols[0].embedEntry
// exactly — internal/session.NewHandler's doc comment requires this, and
// session.registerEmbed's uiEntry param is sourced straight from it.
const embedEntry = "ui/vnc.html"

// syncWriter makes an io.Writer safe for concurrent Write calls by
// serializing them behind a mutex. Combined with ipc.Encoder.Encode's
// single-Write-per-frame guarantee (see codec.go), this is sufficient to
// make one shared *ipc.Encoder safe for concurrent callers: the plugin
// has (at minimum) three independent goroutines that end up encoding
// frames onto stdout — the main dispatch loop's synchronous RPC
// responses (lifecycle/session), the outbound ipc.Client (session
// orchestration's registerEmbed/updateState, channel.open/close), and
// the relay pump's channel-bus data/credit frames — and each Encode call
// must land on the wire as one atomic write, not interleaved with
// another goroutine's frame.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func newSyncWriter(w io.Writer) *syncWriter { return &syncWriter{w: w} }

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// plugin bundles every handler + the RPC client the composition root
// wires together, plus the channel-bus Registry the read loop's
// Dispatcher routes non-zero-channelId frames to.
type plugin struct {
	lifecycleHandler *lifecycle.Handler
	sessionHandler   *session.Handler
	client           *ipc.Client
	registry         *transport.Registry
}

// buildPlugin constructs every piece described in this file's package
// doc comment. enc must already be wired to a concurrency-safe
// underlying writer (see newSyncWriter) — buildPlugin hands the exact
// same *ipc.Encoder to every handler/client/channel it builds so all of
// them share one physical stdout stream safely, rather than each getting
// its own Encoder instance over a separately-wrapped writer (which would
// defeat syncWriter's mutual exclusion: two different mutexes guarding
// the same underlying io.Writer don't serialize against each other).
func buildPlugin(enc *ipc.Encoder) *plugin {
	client := ipc.NewClient(enc)
	registry := transport.NewRegistry()

	lifecycleHandler := lifecycle.NewHandler(enc)
	sessionHandler := session.NewHandler(enc, client, enc, registry, embedEntry, relay.Pump{})

	return &plugin{
		lifecycleHandler: lifecycleHandler,
		sessionHandler:   sessionHandler,
		client:           client,
		registry:         registry,
	}
}

// router implements ipc.RPCHandler for the control plane: it forwards
// Response messages to the outbound Client (answering a plugin -> host
// Call), and routes Request/Notification messages by method name to
// exactly one of lifecycle.Handler or session.Handler — the two method
// sets are disjoint by construction (see the const blocks in
// internal/lifecycle/lifecycle.go and internal/session/lifecycle.go), so
// there is no ambiguity about which handler owns a given method.
type router struct {
	client    *ipc.Client
	lifecycle *lifecycle.Handler
	session   *session.Handler
	enc       *ipc.Encoder
	encMu     sync.Mutex
}

func newRouter(p *plugin, enc *ipc.Encoder) *router {
	return &router{client: p.client, lifecycle: p.lifecycleHandler, session: p.sessionHandler, enc: enc}
}

var lifecycleMethods = map[string]bool{
	lifecycle.MethodInitialize: true,
	lifecycle.MethodActivate:   true,
	lifecycle.MethodPing:       true,
	lifecycle.MethodShutdown:   true,
	lifecycle.MethodDeactivate: true,
}

var sessionMethods = map[string]bool{
	session.MethodSessionConnect:       true,
	session.MethodSessionDisconnect:    true,
	session.MethodSessionEmbedViewport: true,
	session.MethodSessionEmbedActivity: true,
}

// HandleRPC implements ipc.RPCHandler.
func (r *router) HandleRPC(ctx context.Context, kind ipc.MessageKind, msg any) error {
	switch kind {
	case ipc.MessageResponse:
		resp, ok := msg.(*ipc.Response)
		if !ok {
			return fmt.Errorf("router: unexpected message type %T for MessageResponse", msg)
		}
		r.client.Deliver(resp)
		return nil
	case ipc.MessageRequest:
		req, ok := msg.(*ipc.Request)
		if !ok {
			return fmt.Errorf("router: unexpected message type %T for MessageRequest", msg)
		}
		return r.routeRequest(ctx, req)
	case ipc.MessageNotification:
		n, ok := msg.(*ipc.Notification)
		if !ok {
			return fmt.Errorf("router: unexpected message type %T for MessageNotification", msg)
		}
		return r.routeNotification(ctx, n)
	default:
		return nil
	}
}

func (r *router) routeRequest(ctx context.Context, req *ipc.Request) error {
	switch {
	case lifecycleMethods[req.Method]:
		return r.lifecycle.HandleRPC(ctx, ipc.MessageRequest, req)
	case sessionMethods[req.Method]:
		return r.session.HandleRPC(ctx, ipc.MessageRequest, req)
	default:
		return r.respondMethodNotFound(req.ID)
	}
}

func (r *router) routeNotification(ctx context.Context, n *ipc.Notification) error {
	switch {
	case lifecycleMethods[n.Method]:
		return r.lifecycle.HandleRPC(ctx, ipc.MessageNotification, n)
	case sessionMethods[n.Method]:
		return r.session.HandleRPC(ctx, ipc.MessageNotification, n)
	default:
		// Unknown notification methods are silently ignored, matching the
		// convention every handler in this plugin uses: a notification has
		// no error channel back to the host.
		return nil
	}
}

// errMethodNotFound is the JSON-RPC 2.0 standard code for an unknown
// method, per docs/plugin-api.md's RPC error code table — duplicated
// from internal/lifecycle since that constant is unexported there and
// this is the one place outside either handler that needs to answer a
// request neither handler recognizes.
const errMethodNotFound = -32601

func (r *router) respondMethodNotFound(id any) error {
	payload, err := ipc.EncodeResponse(ipc.Response{ID: id, Error: &ipc.RPCError{Code: errMethodNotFound, Message: "method not found"}})
	if err != nil {
		return fmt.Errorf("router: encode error response: %w", err)
	}
	r.encMu.Lock()
	defer r.encMu.Unlock()
	return r.enc.Encode(ipc.KindJSONRPC, 0, payload)
}

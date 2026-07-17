// Package session implements the session-level orchestration for a VNC
// embed session: session.connect -> session.registerEmbed -> (both
// channels open) -> session.updateState("ready"), per
// docs/superpowers/specs/2026-07-16-vnc-plugin-design.md §2's
// internal/session/ file list and §7's edge-case table.
//
// This package deliberately does NOT know anything about RFB message
// semantics or the actual VNC TCP dial (that's internal/rfb and Phase
// 3d's internal/relay, wired in via the RelayStarter extension point
// this file defines) — it only opens the two channel-bus channels
// (tcp-relay, embed-stream) via internal/transport.OpenChannel, calls
// session.registerEmbed, and tracks/reports session state.
package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"sync"

	"xqs-plugin-vnc/internal/ipc"
	"xqs-plugin-vnc/internal/transport"
)

// Method names this Handler answers, per docs/plugin-api.md's
// "Session embed lifecycle" and "Host -> plugin notifications (embed)".
const (
	MethodSessionConnect       = "session.connect"
	MethodSessionDisconnect    = "session.disconnect"
	MethodSessionEmbedViewport = "session.embedViewport"
	MethodSessionEmbedActivity = "session.embedActivity"
)

// JSON-RPC 2.0 standard error codes used by this package, per
// docs/plugin-api.md's RPC error code table.
const (
	errInvalidParams = -32602
	errMethodNotFound = -32601
)

// tunnelIDMain is the single tunnel id this plugin ever uses. Per
// docs/superpowers/specs/2026-07-16-vnc-plugin-design.md §10 F-6:
// "hint = tunnelId (default main)" — the channel-bus embed-stream
// channel's channel.open hint and session.registerEmbed's tunnelIds both
// carry this same value.
const tunnelIDMain = "main"

// RelayStarter is the extension point Phase 3d's relay pump (RFB
// handshake + bidirectional tcp-relay<->embed-stream pump) implements.
// This task stubs it: if nil, orchestrate skips straight to
// session.updateState("ready") once both channels are open and
// registerEmbed has succeeded, with no actual VNC connectivity —
// implementing the RFB handshake or the relay itself is explicitly out
// of scope here (see docs/superpowers/specs/... §8 Phase 3d).
type RelayStarter interface {
	// StartRelay is invoked once per session, after both channels are
	// open and registerEmbed has succeeded, with the session's connect
	// fields (host/port/password/readOnly) available via s. Implementers
	// own everything from here: RFB handshake, read-only input filtering,
	// credit-window coupling between the two channels, and reporting
	// state via s once the framebuffer stream is actually usable.
	StartRelay(ctx context.Context, s *Session, tcpChannel, embedChannel *transport.Channel) error
}

// Session tracks one session.connect's worth of state: the connect
// fields (host/port/password/readOnly), the two channel-bus channels
// once open, and the current lifecycle state. A fresh Session is built
// for every session.connect call, including crash-recovery re-connects —
// per design doc §7 "Crash-recovery: состояние процесса нулевое, никакого
// кэша поверх перезапуска" — so nothing here is reused across restarts.
type Session struct {
	id          string
	caller      transport.RPCCaller
	frameWriter transport.FrameWriter
	registry    *transport.Registry
	embedEntry  string
	relay       RelayStarter

	mu           sync.Mutex
	state        State
	password     []byte
	readOnly     bool
	host         string
	port         int
	tcpChannel   *transport.Channel
	embedChannel *transport.Channel
	torndown     bool
	viewport     EmbedViewport
	active       bool
	embedToken   registerEmbedResult
}

// ID returns the sessionId this Session was constructed for.
func (s *Session) ID() string { return s.id }

// ReadOnly reports the readOnly connect field. Safe for concurrent use.
func (s *Session) ReadOnly() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readOnly
}

// HostPort returns the target host/port from the connect fields.
func (s *Session) HostPort() (string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.host, s.port
}

// Password returns the raw password buffer captured from session.connect's
// fields. The returned slice is the Session's own backing array — callers
// (Phase 3d's relay pump) must not retain it past teardown, since
// teardown.go zeroes it in place. Returns nil if the connection has no
// password field (fields declared it optional, or protocol doesn't need
// auth).
func (s *Session) Password() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.password
}

// orchestrate runs the full connect -> registerEmbed -> (channels open)
// -> ready sequence in the background, per docs/plugin-api.md "Plugin
// returns {"accepted":true} quickly. Long-running work ... must run in a
// goroutine." Any panic anywhere in this sequence is recovered and routed
// to an error-state transition (design doc §7: "Панику в горутине. Ядро
// не увидит её как ошибку сессии. Recover + updateState(error), иначе
// вкладка виснет в connecting навсегда.") instead of crashing the plugin
// process.
func (s *Session) orchestrate(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			// A panic mid-sequence means we can no longer trust any
			// captured secret state; clear it defensively even though
			// teardown (triggered by session.disconnect or process exit)
			// will also attempt it.
			s.clearPassword()
			s.updateState(ctx, StateError, fmt.Sprintf("internal error: %v", r))
		}
	}()

	s.updateState(ctx, StateConnecting, "")

	// tcp-relay's hint is the dial target: per docs/plugin-api.md's
	// purpose table, "tcp-relay: Dials a target through the existing
	// TunnelDialProxy allowlist/dial policy" — the host needs host:port
	// to know what to dial, since the plugin process never gets a raw
	// socket itself. Once open, this channel *is* the TCP connection to
	// the VNC server (transport.Channel satisfies net.Conn) — Phase 3d's
	// relay pump (internal/relay) reads/writes it directly; there is no
	// separate net.Dial anywhere in this plugin.
	host, port := s.HostPort()
	tcpHint := net.JoinHostPort(host, strconv.Itoa(port))
	tcpCh, err := transport.OpenChannel(ctx, s.caller, s.frameWriter, s.registry, transport.PurposeTCPRelay, s.id, tcpHint)
	if err != nil {
		s.updateState(ctx, StateError, "failed to open tcp-relay channel")
		return
	}

	embedCh, err := transport.OpenChannel(ctx, s.caller, s.frameWriter, s.registry, transport.PurposeEmbedStream, s.id, tunnelIDMain)
	if err != nil {
		_ = transport.CloseChannel(ctx, s.caller, tcpCh, "setup-failed", "")
		s.updateState(ctx, StateError, "failed to open embed-stream channel")
		return
	}

	s.mu.Lock()
	s.tcpChannel = tcpCh
	s.embedChannel = embedCh
	s.mu.Unlock()

	if _, err := s.registerEmbed(ctx); err != nil {
		s.updateState(ctx, StateError, "session.registerEmbed failed")
		return
	}

	if s.relay != nil {
		if err := s.relay.StartRelay(ctx, s, tcpCh, embedCh); err != nil {
			s.updateState(ctx, StateError, "relay start failed")
			return
		}
	}

	s.updateState(ctx, StateReady, "")
}

// Handler answers the session.* RPC methods (session.connect) and
// notifications (session.disconnect, session.embedViewport,
// session.embedActivity), following the same ipc.RPCHandler /
// respond-via-Encoder pattern internal/lifecycle.Handler established.
// One Handler is shared process-wide (isolation: per-session means one
// session per process in practice, but nothing here assumes only one
// session is ever tracked).
type Handler struct {
	enc         *ipc.Encoder
	caller      transport.RPCCaller
	frameWriter transport.FrameWriter
	registry    *transport.Registry
	embedEntry  string
	relay       RelayStarter

	mu       sync.Mutex
	sessions map[string]*Session

	encMu sync.Mutex
}

// NewHandler returns a Handler that writes JSON-RPC responses via enc,
// places outbound RPC/notifications (session.registerEmbed,
// session.updateState, channel.open/channel.close) via caller, opens
// channel-bus channels via frameWriter/registry (see
// internal/transport.OpenChannel), advertises embedEntry as the uiEntry
// for session.registerEmbed (must match the manifest's connectionProtocol
// embedEntry, e.g. "ui/vnc.html"), and hands off to relay (nil is a valid
// no-op stub) once a session's channels are open and registered.
func NewHandler(enc *ipc.Encoder, caller transport.RPCCaller, frameWriter transport.FrameWriter, registry *transport.Registry, embedEntry string, relay RelayStarter) *Handler {
	return &Handler{
		enc:         enc,
		caller:      caller,
		frameWriter: frameWriter,
		registry:    registry,
		embedEntry:  embedEntry,
		relay:       relay,
		sessions:    make(map[string]*Session),
	}
}

// HandleRPC implements ipc.RPCHandler.
func (h *Handler) HandleRPC(ctx context.Context, kind ipc.MessageKind, msg any) error {
	switch kind {
	case ipc.MessageRequest:
		req, ok := msg.(*ipc.Request)
		if !ok {
			return fmt.Errorf("session: unexpected message type %T for MessageRequest", msg)
		}
		return h.handleRequest(ctx, req)
	case ipc.MessageNotification:
		n, ok := msg.(*ipc.Notification)
		if !ok {
			return fmt.Errorf("session: unexpected message type %T for MessageNotification", msg)
		}
		h.handleNotification(ctx, n)
		return nil
	default:
		return nil
	}
}

func (h *Handler) handleRequest(ctx context.Context, req *ipc.Request) error {
	switch req.Method {
	case MethodSessionConnect:
		return h.handleConnect(ctx, req)
	default:
		return h.respondError(req.ID, errMethodNotFound, "method not found")
	}
}

func (h *Handler) handleNotification(ctx context.Context, n *ipc.Notification) {
	switch n.Method {
	case MethodSessionDisconnect:
		h.handleDisconnect(ctx, n)
	case MethodSessionEmbedViewport:
		h.handleEmbedViewport(n)
	case MethodSessionEmbedActivity:
		h.handleEmbedActivity(n)
	default:
		// Unknown notification methods are ignored, matching
		// internal/lifecycle.Handler's convention: a notification has no
		// error channel back to the host.
	}
}

func (h *Handler) session(id string) *Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[id]
}

func (h *Handler) setSession(id string, s *Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions[id] = s
}

func (h *Handler) respondOK(id any) error {
	return h.respond(id, json.RawMessage(`{"ok":true}`))
}

func (h *Handler) respond(id any, result json.RawMessage) error {
	payload, err := ipc.EncodeResponse(ipc.Response{ID: id, Result: result})
	if err != nil {
		return fmt.Errorf("session: encode response: %w", err)
	}
	return h.write(payload)
}

func (h *Handler) respondError(id any, code int, message string) error {
	payload, err := ipc.EncodeResponse(ipc.Response{ID: id, Error: &ipc.RPCError{Code: code, Message: message}})
	if err != nil {
		return fmt.Errorf("session: encode error response: %w", err)
	}
	return h.write(payload)
}

// write serializes access to the shared Encoder, matching
// internal/lifecycle.Handler.write's rationale.
func (h *Handler) write(payload []byte) error {
	h.encMu.Lock()
	defer h.encMu.Unlock()
	return h.enc.Encode(ipc.KindJSONRPC, 0, payload)
}

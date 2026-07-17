// This file (client.go) implements a real JSON-RPC 2.0 client over an
// *Encoder wired to the plugin process's stdout: it sends requests with a
// fresh id, correlates the eventual matching *Response back to the
// caller via a pending-id map, and supports fire-and-forget notifications.
//
// This is the one piece of "real logic" the composition-root task
// (cmd/xqs-vnc/main.go) is allowed to introduce: every other package that
// needs to call OUT to the host (internal/transport's channel.open,
// internal/session's session.registerEmbed/session.updateState) defines
// only the seam it needs (transport.RPCCaller: Call/Notify) and is tested
// against a fake — per task 8/9's reports, no concrete implementation
// existed anywhere in the repo yet. Client satisfies that seam
// structurally (same Call/Notify method set), without this package
// importing internal/transport — avoiding an import cycle, since
// internal/transport already imports internal/ipc.
//
// Client itself does not read frames off stdin — that remains the
// composition root's single blocking read loop
// (internal/ipc/dispatch.go's Dispatcher). Whatever routes decoded
// MessageResponse values from that loop (a small router the composition
// root builds) must call Deliver for each one so a waiting Call can
// unblock.
package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultCallTimeout bounds how long Call waits for a matching Response
// when ctx carries no deadline of its own. Per docs/plugin-api.md the
// host's own synchronous RPC budget is 5 s (10 s for channel.open); this
// is deliberately more generous as a pure safety net against a
// permanently-hung call (a leaked goroutine parked forever) rather than a
// tight protocol timeout — callers that need a tighter bound should pass
// a ctx with their own deadline (as internal/transport.OpenChannel's
// callers may via ctx).
const DefaultCallTimeout = 30 * time.Second

// Client is a JSON-RPC 2.0 request/response client over a shared
// *Encoder. It is safe for concurrent use: multiple goroutines may call
// Call/Notify concurrently, and Deliver may be called concurrently with
// any of them (typically from the single frame-reading goroutine).
type Client struct {
	enc *Encoder

	idSeq int64

	mu      sync.Mutex
	pending map[string]chan *Response
}

// NewClient returns a Client that writes JSON-RPC requests/notifications
// via enc, framed as kind=KindJSONRPC on channelId 0 (the control plane),
// per internal/ipc/dispatch.go.
func NewClient(enc *Encoder) *Client {
	return &Client{enc: enc, pending: make(map[string]chan *Response)}
}

// Call sends method with params as a JSON-RPC request carrying a fresh
// id, and blocks until either a matching Response arrives (delivered via
// Deliver), ctx is done, or DefaultCallTimeout elapses (if ctx has no
// deadline of its own). On success, result (if non-nil) is decoded from
// the response's "result" field; a response carrying "error" is returned
// as *RPCError (which implements error).
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	id := c.nextID()

	paramsRaw, err := marshalParams(params)
	if err != nil {
		return fmt.Errorf("ipc: client: marshal params for %s: %w", method, err)
	}

	ch := make(chan *Response, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	payload, err := EncodeRequest(Request{ID: id, Method: method, Params: paramsRaw})
	if err != nil {
		return fmt.Errorf("ipc: client: encode request %s: %w", method, err)
	}
	if err := c.enc.Encode(KindJSONRPC, 0, payload); err != nil {
		return fmt.Errorf("ipc: client: send request %s: %w", method, err)
	}

	waitCtx, cancel := withDefaultTimeout(ctx)
	defer cancel()

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("ipc: client: decode result for %s: %w", method, err)
			}
		}
		return nil
	case <-waitCtx.Done():
		return fmt.Errorf("ipc: client: call %s: %w", method, waitCtx.Err())
	}
}

// Notify sends method with params as a JSON-RPC notification: no
// response is expected or awaited, matching docs/plugin-api.md's
// documented fire-and-forget calls (e.g. channel.close).
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	_ = ctx // no I/O blocking to bound; accepted for interface symmetry with Call.

	paramsRaw, err := marshalParams(params)
	if err != nil {
		return fmt.Errorf("ipc: client: marshal params for %s: %w", method, err)
	}
	payload, err := EncodeNotification(Notification{Method: method, Params: paramsRaw})
	if err != nil {
		return fmt.Errorf("ipc: client: encode notification %s: %w", method, err)
	}
	if err := c.enc.Encode(KindJSONRPC, 0, payload); err != nil {
		return fmt.Errorf("ipc: client: send notification %s: %w", method, err)
	}
	return nil
}

// Deliver routes an incoming Response (decoded by the composition root's
// read loop from the host's control-plane traffic) to the Call currently
// waiting on its id, if any. It returns false if no Call is waiting for
// resp's id — either it already timed out and gave up, or the response
// carries an id this Client never issued — which callers should treat as
// a harmless drop, not a protocol violation: a response racing a
// client-side timeout is an expected, ordinary occurrence, not a bug.
func (c *Client) Deliver(resp *Response) bool {
	id, ok := resp.ID.(string)
	if !ok {
		return false
	}

	c.mu.Lock()
	ch, found := c.pending[id]
	c.mu.Unlock()
	if !found {
		return false
	}

	select {
	case ch <- resp:
		return true
	default:
		// The buffered slot is already full (Deliver called twice for the
		// same id — shouldn't happen since the host answers each request
		// exactly once, but this keeps Deliver itself from ever blocking).
		return false
	}
}

// nextID returns a fresh, process-unique request id, encoded as a JSON
// string (not a bare number) so Deliver's id correlation never has to
// worry about JSON-RPC ids round-tripping as float64 through
// encoding/json's `any` decoding.
func (c *Client) nextID() string {
	n := atomic.AddInt64(&c.idSeq, 1)
	return "xqs-vnc-" + strconv.FormatInt(n, 10)
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	return json.Marshal(params)
}

// withDefaultTimeout wraps ctx with DefaultCallTimeout if ctx doesn't
// already carry its own deadline.
func withDefaultTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, DefaultCallTimeout)
}

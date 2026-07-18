package transport

import (
	"sync"
	"time"
)

// timeoutError is the net.Error returned when a deadline expires while a
// Read or Write is parked, or when a subsequent call is made after a
// deadline has already elapsed. It satisfies net.Error so callers that
// type-assert for Timeout() (as io.Copy loops, net/http, etc. commonly
// do) see the expected behavior.
type timeoutError struct{ op string }

func (e *timeoutError) Error() string   { return "transport: " + e.op + " deadline exceeded" }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return true }

// deadline is a resettable, one-shot alarm: wait() returns a channel that
// is closed exactly when the deadline expires. Per net.Conn's documented
// contract ("SetReadDeadline sets the deadline for future Read calls and
// any currently-blocked Read call... Once a deadline has been exceeded,
// the connection can be refreshed by setting a deadline in the future"),
// this type must satisfy three properties:
//
//  1. A call to set() while a Read/Write is already parked on wait()'s
//     previously-returned channel must be able to affect that parked
//     call — so the same channel instance is reused across set() calls
//     as long as it hasn't fired yet, rather than minting a fresh one
//     each time (which a parked goroutine, having already captured the
//     old reference, would never see).
//  2. Once expired, wait() keeps returning an already-closed channel —
//     so any subsequent Read/Write fails immediately without blocking —
//     until set() is called again with a time in the future, at which
//     point a fresh channel is minted and armed.
//  3. set() is safe to call concurrently with wait() and with the timer
//     firing.
//
// This mirrors the (unexported) pattern net.Pipe uses internally for the
// same purpose; reimplemented here since it isn't part of the public API.
type deadline struct {
	mu     sync.Mutex
	timer  *time.Timer
	cancel chan struct{} // closed exactly when the current deadline expires
}

// newDeadline returns a deadline with no expiry set (operations block
// forever until set() is called).
func newDeadline() *deadline {
	return &deadline{cancel: make(chan struct{})}
}

// set arms the deadline for time t. A zero t disables the deadline
// (operations block forever, as if no deadline had ever been set). A t in
// the past (or exactly now) expires the deadline immediately, so the next
// wait() call — and any call already parked on a previous wait() — sees
// it as elapsed right away.
func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Disarm any timer from a previous set() call. If it already fired
	// concurrently, drain the close it produced so we don't leak a
	// goroutine racing to close a channel we're about to replace.
	if d.timer != nil && !d.timer.Stop() {
		<-d.cancel
	}
	d.timer = nil

	// If the current cancel channel already expired, replace it with a
	// fresh, open one — reused by wait() until this new deadline (if any)
	// itself expires. If it hasn't expired, keep it: any goroutine already
	// parked on it (from a previous wait() call) must observe this new
	// deadline through that same channel.
	select {
	case <-d.cancel:
		d.cancel = make(chan struct{})
	default:
	}

	if t.IsZero() {
		return
	}

	if dur := time.Until(t); dur > 0 {
		cancel := d.cancel
		d.timer = time.AfterFunc(dur, func() {
			close(cancel)
		})
		return
	}

	// Deadline is already in the past: expire immediately.
	close(d.cancel)
}

// wait returns the channel that will be closed when the current deadline
// (if any) expires. It is safe to call repeatedly; between two calls to
// set(), it always returns the same channel.
func (d *deadline) wait() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cancel
}

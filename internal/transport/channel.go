package transport

import (
	"io"
	"net"
	"sync"
	"time"
)

// Channel is a net.Conn implementation over a binary channel-bus channel.
// It is the concrete body for the port described in conn.go: conn.go
// defines the seam (FrameSink/FrameSource) a real credit-windowed channel
// must implement (Phase 3b); Channel is the structural/mechanical piece
// this task builds — Read/Write buffering, frame splitting, deadlines,
// and Close — wired to that seam. There is no separate exported
// interface for "the port" beyond net.Conn itself: net.Conn already says
// everything callers (internal/rfb, crypto/tls) need, so introducing a
// parallel transport-specific interface would just be indirection without
// a distinct contract.
//
// Internally, Channel runs one background goroutine that continuously
// pulls payloads from FrameSource.Recv (the "read pump") and one that
// dequeues pending writes and pushes them to FrameSink.Send (the "write
// pump"). This decouples the blocking, cancellation-unaware Send/Recv
// calls from Read/Write's deadline and Close handling: Read and Write
// each select between "the pump made progress", "the deadline fired", and
// "Close happened", rather than trying to interrupt a blocking Send/Recv
// call directly (which the seam doesn't support — Phase 3b's real
// implementation is expected to make its own Send/Recv unblock on the
// channel being closed out from under it, not on anything Channel does).
type Channel struct {
	sink   FrameSink
	source FrameSource

	local, remote net.Addr
	maxPayload    int

	readDeadline  *deadline
	writeDeadline *deadline

	closeOnce sync.Once
	closed    chan struct{}

	// readCh delivers payloads from the read pump to Read. It is closed
	// by the read pump when FrameSource.Recv reports ok == false (source
	// exhausted/closed).
	readCh chan []byte

	readMu  sync.Mutex // serializes Read calls, per net.Conn's single-reader expectation
	readBuf []byte     // leftover bytes from a payload not fully consumed yet

	writeMu    sync.Mutex // serializes Write calls, per net.Conn's single-writer expectation
	writeReqCh chan writeReq
}

type writeReq struct {
	payload []byte
	done    chan error
}

// NewChannel builds a net.Conn over sink/source. maxPayload bounds how
// large a single chunk handed to sink.Send can be; Write splits larger
// application writes into multiple chunks of at most maxPayload bytes
// each (see frame.go's splitPayload). A maxPayload <= 0 falls back to
// DefaultMaxFramePayload.
func NewChannel(sink FrameSink, source FrameSource, local, remote net.Addr, maxPayload int) *Channel {
	if maxPayload <= 0 {
		maxPayload = DefaultMaxFramePayload
	}
	c := &Channel{
		sink:          sink,
		source:        source,
		local:         local,
		remote:        remote,
		maxPayload:    maxPayload,
		readDeadline:  newDeadline(),
		writeDeadline: newDeadline(),
		closed:        make(chan struct{}),
		readCh:        make(chan []byte),
		writeReqCh:    make(chan writeReq),
	}
	go c.readPump()
	go c.writePump()
	return c
}

func (c *Channel) readPump() {
	defer close(c.readCh)
	for {
		payload, ok := c.source.Recv()
		if !ok {
			return
		}
		if len(payload) == 0 {
			// Nothing to deliver; keep pumping rather than handing Read a
			// zero-length chunk (which would look like a spurious wakeup).
			continue
		}
		select {
		case c.readCh <- payload:
		case <-c.closed:
			return
		}
	}
}

func (c *Channel) writePump() {
	for {
		select {
		case req := <-c.writeReqCh:
			err := c.sink.Send(req.payload)
			req.done <- err
		case <-c.closed:
			return
		}
	}
}

// Read implements net.Conn. It buffers any unconsumed remainder of a
// payload across calls, so a caller asking for fewer bytes than one
// payload's length gets a short read of exactly what it asked for rather
// than losing the rest.
func (c *Channel) Read(b []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	if len(c.readBuf) == 0 {
		select {
		case <-c.closed:
			return 0, ErrClosed
		default:
		}
		select {
		case payload, ok := <-c.readCh:
			if !ok {
				return 0, io.EOF
			}
			c.readBuf = payload
		case <-c.readDeadline.wait():
			return 0, &timeoutError{op: "read"}
		case <-c.closed:
			return 0, ErrClosed
		}
	}

	n := copy(b, c.readBuf)
	c.readBuf = c.readBuf[n:]
	return n, nil
}

// Write implements net.Conn. It splits b into chunks of at most
// c.maxPayload bytes and hands each to the write pump in order; a
// deadline or Close aborts the write, and the returned byte count
// reflects exactly how many bytes were fully handed off before that
// happened, per net.Conn's Write contract.
func (c *Channel) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	chunks := splitPayload(b, c.maxPayload)
	total := 0
	for _, chunk := range chunks {
		done := make(chan error, 1)
		select {
		case c.writeReqCh <- writeReq{payload: chunk, done: done}:
		case <-c.writeDeadline.wait():
			return total, &timeoutError{op: "write"}
		case <-c.closed:
			return total, ErrClosed
		}

		select {
		case err := <-done:
			if err != nil {
				return total, err
			}
			total += len(chunk)
		case <-c.writeDeadline.wait():
			return total, &timeoutError{op: "write"}
		case <-c.closed:
			return total, ErrClosed
		}
	}
	return total, nil
}

// Close implements net.Conn. It is idempotent — repeated calls return nil
// and have no further effect — and unblocks any Read or Write currently
// parked on this Channel.
func (c *Channel) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
	})
	return nil
}

// Done returns a channel that is closed once c.Close() has been called
// (locally) or the underlying transport has been severed. Callers use this
// as a cancellation signal for anything that would otherwise block
// indefinitely against this Channel's lifetime — e.g. internal/relay's
// server->browser pump waiting on a session.tunnelBackpressure gate must
// not leak a goroutine if the channel closes while backpressured and no
// matching tunnelResume ever arrives.
func (c *Channel) Done() <-chan struct{} { return c.closed }

// LocalAddr implements net.Conn.
func (c *Channel) LocalAddr() net.Addr { return c.local }

// RemoteAddr implements net.Conn.
func (c *Channel) RemoteAddr() net.Addr { return c.remote }

// SetDeadline implements net.Conn.
func (c *Channel) SetDeadline(t time.Time) error {
	c.readDeadline.set(t)
	c.writeDeadline.set(t)
	return nil
}

// SetReadDeadline implements net.Conn.
func (c *Channel) SetReadDeadline(t time.Time) error {
	c.readDeadline.set(t)
	return nil
}

// SetWriteDeadline implements net.Conn.
func (c *Channel) SetWriteDeadline(t time.Time) error {
	c.writeDeadline.set(t)
	return nil
}

var _ net.Conn = (*Channel)(nil)

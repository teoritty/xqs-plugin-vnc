package transport

import (
	"bytes"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeEndpoint is an in-memory stand-in for a real credit-windowed
// channel: two fakeEndpoints created together by newFakePipe form a pair,
// each one's outgoing queue is the other's incoming queue. Both Send and
// Recv block (on unbuffered Go channels) until the peer is ready or the
// endpoint is closed, which is a reasonable stand-in for a real channel
// bus where Send can block on credit and Recv blocks until a frame
// arrives.
type fakeEndpoint struct {
	out  chan []byte
	in   chan []byte
	done chan struct{}
	once sync.Once
}

func newFakePipe() (*fakeEndpoint, *fakeEndpoint) {
	ab := make(chan []byte)
	ba := make(chan []byte)
	a := &fakeEndpoint{out: ab, in: ba, done: make(chan struct{})}
	b := &fakeEndpoint{out: ba, in: ab, done: make(chan struct{})}
	return a, b
}

func (e *fakeEndpoint) Send(p []byte) error {
	buf := append([]byte(nil), p...)
	select {
	case e.out <- buf:
		return nil
	case <-e.done:
		return errors.New("fake: endpoint closed")
	}
}

func (e *fakeEndpoint) Recv() ([]byte, bool) {
	select {
	case p, ok := <-e.in:
		if !ok {
			return nil, false
		}
		return p, true
	case <-e.done:
		return nil, false
	}
}

func (e *fakeEndpoint) Close() {
	e.once.Do(func() { close(e.done) })
}

func testAddr(id uint32) net.Addr { return Addr{ChannelID: id, Purpose: "test"} }

func newTestChannelPair(maxPayload int) (*Channel, *Channel, func()) {
	a, b := newFakePipe()
	ca := NewChannel(a, a, testAddr(1), testAddr(2), maxPayload)
	cb := NewChannel(b, b, testAddr(2), testAddr(1), maxPayload)
	cleanup := func() {
		ca.Close()
		cb.Close()
		a.Close()
		b.Close()
	}
	return ca, cb, cleanup
}

func TestChannel_RoundTrip(t *testing.T) {
	ca, cb, cleanup := newTestChannelPair(0)
	defer cleanup()

	msg := []byte("hello, rfb")
	writeErr := make(chan error, 1)
	go func() {
		_, err := ca.Write(msg)
		writeErr <- err
	}()

	buf := make([]byte, len(msg))
	if _, err := readFull(cb, buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(buf, msg) {
		t.Fatalf("got %q, want %q", buf, msg)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func TestChannel_ReadSmallerThanPayload(t *testing.T) {
	ca, cb, cleanup := newTestChannelPair(0)
	defer cleanup()

	msg := []byte("0123456789")
	writeErr := make(chan error, 1)
	go func() {
		_, err := ca.Write(msg)
		writeErr <- err
	}()

	var got []byte
	small := make([]byte, 3)
	for len(got) < len(msg) {
		n, err := cb.Read(small)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		got = append(got, small[:n]...)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("got %q, want %q", got, msg)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func TestChannel_WriteSplitsAcrossMaxPayload(t *testing.T) {
	a, b := newFakePipe()
	const maxPayload = 4
	ca := NewChannel(a, a, testAddr(1), testAddr(2), maxPayload)
	cb := NewChannel(b, b, testAddr(2), testAddr(1), maxPayload)
	defer func() {
		ca.Close()
		cb.Close()
		a.Close()
		b.Close()
	}()

	msg := []byte("0123456789") // 10 bytes -> 3 chunks of <=4
	writeErr := make(chan error, 1)
	go func() {
		_, err := ca.Write(msg)
		writeErr <- err
	}()

	var chunks [][]byte
	var got []byte
	for len(got) < len(msg) {
		buf := make([]byte, 64)
		n, err := cb.Read(buf)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		chunks = append(chunks, buf[:n])
		got = append(got, buf[:n]...)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("got %q, want %q", got, msg)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(chunks) < 3 {
		t.Fatalf("expected write to be split into >=3 chunks (maxPayload=%d), got %d chunks: %v", maxPayload, len(chunks), chunks)
	}
	for _, c := range chunks {
		if len(c) > maxPayload {
			t.Fatalf("chunk %q exceeds maxPayload %d", c, maxPayload)
		}
	}
}

func TestChannel_CloseIsIdempotent(t *testing.T) {
	ca, _, cleanup := newTestChannelPair(0)
	defer cleanup()

	if err := ca.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := ca.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestChannel_CloseUnblocksParkedRead(t *testing.T) {
	ca, _, cleanup := newTestChannelPair(0)
	defer cleanup()

	errCh := make(chan error, 1)
	go func() {
		_, err := ca.Read(make([]byte, 16))
		errCh <- err
	}()

	time.Sleep(20 * time.Millisecond) // let Read park
	ca.Close()

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Read after Close: got %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not unblock after Close")
	}
}

func TestChannel_CloseUnblocksParkedWrite(t *testing.T) {
	a, b := newFakePipe()
	// maxPayload=1 with no reader draining forces Write to park on the
	// unbuffered fake channel after the write pump tries (and fails, since
	// nothing reads) to hand the chunk to the peer.
	ca := NewChannel(a, a, testAddr(1), testAddr(2), 1)
	defer func() {
		ca.Close()
		a.Close()
		b.Close()
	}()

	errCh := make(chan error, 1)
	go func() {
		_, err := ca.Write([]byte("xy"))
		errCh <- err
	}()

	time.Sleep(20 * time.Millisecond) // let Write park
	ca.Close()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Write after Close: got nil error, want non-nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write did not unblock after Close")
	}
}

func TestChannel_ReadDeadlineExpires(t *testing.T) {
	ca, _, cleanup := newTestChannelPair(0)
	defer cleanup()

	if err := ca.SetReadDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	_, err := ca.Read(make([]byte, 16))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("Read after deadline: got %v, want a net.Error with Timeout()==true", err)
	}

	// A subsequent call must fail immediately, without blocking, until the
	// deadline is refreshed.
	start := time.Now()
	_, err = ca.Read(make([]byte, 16))
	elapsed := time.Since(start)
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("second Read after expired deadline: got %v, want Timeout net.Error", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("second Read after expired deadline blocked for %v, want near-immediate failure", elapsed)
	}

	// Refreshing the deadline into the future lets a subsequent Read block
	// (and succeed) normally again.
	if err := ca.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetReadDeadline (refresh): %v", err)
	}
}

func TestChannel_WriteDeadlineExpires(t *testing.T) {
	a, b := newFakePipe()
	ca := NewChannel(a, a, testAddr(1), testAddr(2), 0)
	defer func() {
		ca.Close()
		a.Close()
		b.Close()
	}()
	_ = b

	if err := ca.SetWriteDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatalf("SetWriteDeadline: %v", err)
	}

	// Nothing ever reads from a's out channel, so this Write can only
	// complete via the deadline firing.
	_, err := ca.Write([]byte("no reader"))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("Write after deadline: got %v, want a net.Error with Timeout()==true", err)
	}
}

func TestChannel_DeadlineInThePastFailsImmediately(t *testing.T) {
	ca, _, cleanup := newTestChannelPair(0)
	defer cleanup()

	if err := ca.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	start := time.Now()
	_, err := ca.Read(make([]byte, 16))
	elapsed := time.Since(start)
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("Read with past deadline: got %v, want Timeout net.Error", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("Read with past deadline blocked for %v, want near-immediate failure", elapsed)
	}
}

func TestChannel_ImplementsNetConn(t *testing.T) {
	var _ net.Conn = (*Channel)(nil)
}

func readFull(c net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := c.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

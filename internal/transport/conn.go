package transport

import "errors"

// FrameSink sends one application-level payload as a single unit of
// channel data (conceptually, one kind=0x02 frame, though the actual
// header encoding happens below this seam — see frame.go). Send blocks
// until the payload has been handed off to the underlying channel (which,
// for a real credit-windowed channel, may mean blocking until credit is
// available); it returns an error if the send cannot complete, e.g.
// because the channel was closed out from under it.
//
// Phase 3b's channel.open/credit implementation is expected to implement
// this interface against a real, credit-windowed channel. This task only
// defines the seam and exercises it against an in-memory fake (see
// channel_test.go).
type FrameSink interface {
	Send(payload []byte) error
}

// FrameSource receives channel-data payloads in order. Recv blocks until a
// payload is available or the source is closed/exhausted, in which case it
// returns ok == false. There is no separate error return: per the design
// doc's "Дисциплина отказа", a channel-bus failure is not something this
// seam distinguishes from a clean close — both simply stop producing
// payloads. (A real implementation that needs to surface *why* the source
// stopped can do so out of band, e.g. by recording the reason and letting
// the caller inspect it after Recv returns false; Channel.Read reports a
// plain io.EOF in that case.)
type FrameSource interface {
	Recv() (payload []byte, ok bool)
}

// ErrClosed is returned by Channel's methods once Close has been called
// and no further I/O can occur in that direction.
var ErrClosed = errors.New("transport: use of closed channel")

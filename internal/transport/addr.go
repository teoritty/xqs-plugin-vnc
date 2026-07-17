package transport

import "fmt"

// Addr is a synthetic net.Addr for a channel-bus connection: there is no
// socket, host, or port underneath it, only a channel id and the purpose
// it was opened for (e.g. "tcp-relay" or "embed-stream", per
// docs/plugin-api.md). It exists purely so Channel can satisfy net.Conn's
// LocalAddr/RemoteAddr methods with something identifying rather than
// nil.
type Addr struct {
	// ChannelID is the channel-bus channel id this connection rides on.
	ChannelID uint32
	// Purpose identifies what the channel was opened for. Left as a plain
	// string (rather than an enum) since this package doesn't otherwise
	// need to know the set of valid purposes — that's Phase 3b's
	// open.go/credit.go concern.
	Purpose string
}

// Network returns a fixed, synthetic network name — there is no
// dial/listen network these addresses belong to.
func (a Addr) Network() string { return "xqs-channel" }

// String identifies the channel by purpose and id, e.g.
// "xqs-channel:tcp-relay#3".
func (a Addr) String() string {
	return fmt.Sprintf("xqs-channel:%s#%d", a.Purpose, a.ChannelID)
}

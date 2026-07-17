package transport

import "testing"

func TestAddr(t *testing.T) {
	a := Addr{ChannelID: 3, Purpose: "tcp-relay"}
	if got, want := a.Network(), "xqs-channel"; got != want {
		t.Errorf("Network() = %q, want %q", got, want)
	}
	if got, want := a.String(), "xqs-channel:tcp-relay#3"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

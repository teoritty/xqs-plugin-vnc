package rfb

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// pipe returns two connected in-process net.Conn endpoints, closed at test
// cleanup.
func pipe(t *testing.T) (client, server net.Conn) {
	t.Helper()
	c, s := net.Pipe()
	t.Cleanup(func() {
		c.Close()
		s.Close()
	})
	return c, s
}

func withTimeout(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("test timed out")
	}
}

func TestHandshake_VNCAuthSuccess(t *testing.T) {
	client, server := pipe(t)
	password := []byte("sekret")
	challenge := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	wantResponse, err := VNCAuthResponse(password, challenge[:])
	if err != nil {
		t.Fatalf("VNCAuthResponse: %v", err)
	}

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- func() error {
			if err := WriteVersion(server, V38); err != nil {
				return err
			}
			if _, err := ReadVersion(server); err != nil {
				return err
			}
			if err := WriteSecurityTypeList(server, []SecurityType{SecTypeVNCAuth}); err != nil {
				return err
			}
			var sel [1]byte
			if _, err := io.ReadFull(server, sel[:]); err != nil {
				return err
			}
			if SecurityType(sel[0]) != SecTypeVNCAuth {
				t.Errorf("client selected %v, want VNCAuth", SecurityType(sel[0]))
			}
			if _, err := server.Write(challenge[:]); err != nil {
				return err
			}
			var resp [16]byte
			if _, err := io.ReadFull(server, resp[:]); err != nil {
				return err
			}
			for i := range resp {
				if resp[i] != wantResponse[i] {
					t.Errorf("response mismatch at byte %d: got %x want %x", i, resp[i], wantResponse[i])
					break
				}
			}
			return WriteSecurityResult(server, SecurityResultOK)
		}()
	}()

	withTimeout(t, func() {
		result, err := Handshake(client, password)
		if err != nil {
			t.Fatalf("Handshake: %v", err)
		}
		if result.SecurityType != SecTypeVNCAuth {
			t.Errorf("SecurityType = %v, want VNCAuth", result.SecurityType)
		}
		if result.ServerVersion != V38 {
			t.Errorf("ServerVersion = %v, want %v", result.ServerVersion, V38)
		}
	})

	if err := <-serverErrCh; err != nil {
		t.Fatalf("server goroutine: %v", err)
	}
}

func TestHandshake_NoneAuthSuccess(t *testing.T) {
	client, server := pipe(t)

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- func() error {
			if err := WriteVersion(server, V38); err != nil {
				return err
			}
			if _, err := ReadVersion(server); err != nil {
				return err
			}
			if err := WriteSecurityTypeList(server, []SecurityType{SecTypeNone}); err != nil {
				return err
			}
			var sel [1]byte
			if _, err := io.ReadFull(server, sel[:]); err != nil {
				return err
			}
			if SecurityType(sel[0]) != SecTypeNone {
				t.Errorf("client selected %v, want None", SecurityType(sel[0]))
			}
			return WriteSecurityResult(server, SecurityResultOK)
		}()
	}()

	withTimeout(t, func() {
		result, err := Handshake(client, nil)
		if err != nil {
			t.Fatalf("Handshake: %v", err)
		}
		if result.SecurityType != SecTypeNone {
			t.Errorf("SecurityType = %v, want None", result.SecurityType)
		}
	})

	if err := <-serverErrCh; err != nil {
		t.Fatalf("server goroutine: %v", err)
	}
}

func TestHandshake_EmptySecurityTypeListRejection(t *testing.T) {
	client, server := pipe(t)

	go func() {
		_ = WriteVersion(server, V38)
		_, _ = ReadVersion(server)
		// n=0 list, followed by reason string.
		_, _ = server.Write([]byte{0})
		_ = WriteReasonString(server, "too many auth attempts")
	}()

	withTimeout(t, func() {
		_, err := Handshake(client, nil)
		var rejected *ErrSecurityRejected
		if !errors.As(err, &rejected) {
			t.Fatalf("Handshake err = %v, want *ErrSecurityRejected", err)
		}
		if rejected.Reason != "too many auth attempts" {
			t.Errorf("Reason = %q, want %q", rejected.Reason, "too many auth attempts")
		}
	})
}

func TestHandshake_UnsupportedSecurityTypeOnly(t *testing.T) {
	client, server := pipe(t)

	go func() {
		_ = WriteVersion(server, V38)
		_, _ = ReadVersion(server)
		_ = WriteSecurityTypeList(server, []SecurityType{SecTypeVeNCrypt})
	}()

	withTimeout(t, func() {
		_, err := Handshake(client, nil)
		var noType *ErrNoSupportedSecurityType
		if !errors.As(err, &noType) {
			t.Fatalf("Handshake err = %v, want *ErrNoSupportedSecurityType", err)
		}
	})
}

func TestHandshake_SecurityResultFailureWithReason(t *testing.T) {
	client, server := pipe(t)

	go func() {
		_ = WriteVersion(server, V38)
		_, _ = ReadVersion(server)
		_ = WriteSecurityTypeList(server, []SecurityType{SecTypeNone})
		var sel [1]byte
		_, _ = io.ReadFull(server, sel[:])
		_ = WriteSecurityResultFailed(server, "authentication failed")
	}()

	withTimeout(t, func() {
		_, err := Handshake(client, nil)
		var rejected *ErrSecurityRejected
		if !errors.As(err, &rejected) {
			t.Fatalf("Handshake err = %v, want *ErrSecurityRejected", err)
		}
		if rejected.Reason != "authentication failed" {
			t.Errorf("Reason = %q, want %q", rejected.Reason, "authentication failed")
		}
	})
}

func TestHandshake_TruncatedAtVersion(t *testing.T) {
	client, server := pipe(t)
	go func() {
		_, _ = server.Write([]byte("RFB 003.0")) // short, then close
		server.Close()
	}()
	withTimeout(t, func() {
		_, err := Handshake(client, nil)
		var trunc *ErrTruncated
		if !errors.As(err, &trunc) {
			t.Fatalf("Handshake err = %v, want *ErrTruncated", err)
		}
	})
}

func TestHandshake_TruncatedAtSecurityTypeList(t *testing.T) {
	client, server := pipe(t)
	go func() {
		_ = WriteVersion(server, V38)
		_, _ = ReadVersion(server)
		server.Close() // never sends the count byte
	}()
	withTimeout(t, func() {
		_, err := Handshake(client, nil)
		var trunc *ErrTruncated
		if !errors.As(err, &trunc) {
			t.Fatalf("Handshake err = %v, want *ErrTruncated", err)
		}
	})
}

func TestHandshake_TruncatedAtChallenge(t *testing.T) {
	client, server := pipe(t)
	go func() {
		_ = WriteVersion(server, V38)
		_, _ = ReadVersion(server)
		_ = WriteSecurityTypeList(server, []SecurityType{SecTypeVNCAuth})
		var sel [1]byte
		_, _ = io.ReadFull(server, sel[:])
		_, _ = server.Write([]byte{1, 2, 3}) // short challenge
		server.Close()
	}()
	withTimeout(t, func() {
		_, err := Handshake(client, []byte("pw"))
		var trunc *ErrTruncated
		if !errors.As(err, &trunc) {
			t.Fatalf("Handshake err = %v, want *ErrTruncated", err)
		}
	})
}

func TestHandshake_TruncatedAtSecurityResult(t *testing.T) {
	client, server := pipe(t)
	go func() {
		_ = WriteVersion(server, V38)
		_, _ = ReadVersion(server)
		_ = WriteSecurityTypeList(server, []SecurityType{SecTypeNone})
		var sel [1]byte
		_, _ = io.ReadFull(server, sel[:])
		_, _ = server.Write([]byte{0, 0}) // short result
		server.Close()
	}()
	withTimeout(t, func() {
		_, err := Handshake(client, nil)
		var trunc *ErrTruncated
		if !errors.As(err, &trunc) {
			t.Fatalf("Handshake err = %v, want *ErrTruncated", err)
		}
	})
}

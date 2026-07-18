package rfb

import (
	"fmt"
	"io"
)

// HandshakeResult carries the outcome of a successful real handshake with a
// VNC server, in case a caller wants it for logging/updateState purposes.
// Handshake deliberately stops before ClientInit/ServerInit — see the
// package doc and design plan §1: those belong to the later relay phase.
type HandshakeResult struct {
	// ServerVersion is the version the server announced (always >= 3.8;
	// ReadVersion enforces the floor).
	ServerVersion Version
	// SecurityType is the security type this handshake negotiated and
	// completed.
	SecurityType SecurityType
}

// Handshake performs the real RFB handshake with a VNC server: version
// exchange, security-type negotiation, and (for VNCAuth) the DES
// challenge/response, ending with a successful SecurityResult. conn is any
// io.ReadWriter (real tests use net.Pipe(); the production caller wraps a
// transport.Conn). password is only consulted if the negotiated security
// type is VNCAuth; callers remain responsible for zeroing it afterwards
// (this function does not retain it).
//
// Handshake does not read or write ClientInit/ServerInit. Callers own the
// connection for that once this returns successfully.
func Handshake(conn io.ReadWriter, password []byte) (*HandshakeResult, error) {
	serverVersion, err := ReadVersion(conn)
	if err != nil {
		return nil, err
	}
	// The client always claims 3.8, regardless of what the server sent
	// (already enforced to be >= 3.8 by ReadVersion).
	if err := WriteVersion(conn, V38); err != nil {
		return nil, err
	}

	list, err := ReadSecurityTypeList(conn)
	if err != nil {
		return nil, err
	}
	if list.Empty {
		reason, err := ReadReasonString(conn)
		if err != nil {
			return nil, err
		}
		return nil, &ErrSecurityRejected{Reason: reason}
	}

	secType, err := SelectSecurityType(list.Types, SupportedSecurityTypes)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write([]byte{byte(secType)}); err != nil {
		return nil, fmt.Errorf("rfb: write security type selection: %w", err)
	}

	if secType == SecTypeVNCAuth {
		var challenge [vncChallengeLen]byte
		if _, err := io.ReadFull(conn, challenge[:]); err != nil {
			return nil, wrapTruncated("VNC auth challenge", err)
		}
		response, err := VNCAuthResponse(password, challenge[:])
		if err != nil {
			return nil, err
		}
		if _, err := conn.Write(response); err != nil {
			return nil, fmt.Errorf("rfb: write VNC auth response: %w", err)
		}
	}

	result, reason, err := ReadSecurityResult(conn, serverVersion)
	if err != nil {
		return nil, err
	}
	if !result.OK() {
		return nil, &ErrSecurityRejected{Reason: reason}
	}

	return &HandshakeResult{ServerVersion: serverVersion, SecurityType: secType}, nil
}

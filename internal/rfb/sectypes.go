package rfb

import (
	"fmt"
	"io"
)

// SecurityType is an RFB security-type identifier (RFC 6143 §7.1.2).
type SecurityType uint8

// Known security types. VeNCrypt is a recognized constant from Phase 2
// onward, but is not yet selectable by SelectSecurityType — VeNCrypt
// sub-negotiation is implemented in a later phase (§4 of the design plan).
const (
	SecTypeInvalid  SecurityType = 0
	SecTypeNone     SecurityType = 1
	SecTypeVNCAuth  SecurityType = 2
	SecTypeVeNCrypt SecurityType = 19
)

func (t SecurityType) String() string {
	switch t {
	case SecTypeInvalid:
		return "Invalid(0)"
	case SecTypeNone:
		return "None(1)"
	case SecTypeVNCAuth:
		return "VNCAuth(2)"
	case SecTypeVeNCrypt:
		return "VeNCrypt(19)"
	default:
		return fmt.Sprintf("Unknown(%d)", uint8(t))
	}
}

// SupportedSecurityTypes is the set of security types this plugin (v1) can
// actually complete a handshake for. VeNCrypt is deliberately absent —
// SelectSecurityType must never choose it until that phase lands.
var SupportedSecurityTypes = []SecurityType{SecTypeNone, SecTypeVNCAuth}

// SecurityTypeList is the security-type list a server offers during the
// RFB 3.8 handshake: "[n][type,type,...]" per RFC 6143 §7.1.2.
//
// n=0 is not merely an empty slice — it is itself a form of rejection: the
// server sends no types at all and instead follows immediately with a
// length-prefixed reason string (the same shape as a failed
// SecurityResult). Callers must check Empty and, when true, read the
// reason via ReadReasonString before doing anything else with the stream.
type SecurityTypeList struct {
	Types []SecurityType
	Empty bool
}

// ReadSecurityTypeList reads the security-type list from r. It does NOT
// consume the reason string that follows an empty (n=0) list — callers
// must do that themselves via ReadReasonString once they observe
// list.Empty, since the two concerns (list shape vs. reason text) are kept
// separate for testability.
func ReadSecurityTypeList(r io.Reader) (SecurityTypeList, error) {
	var nBuf [1]byte
	if _, err := io.ReadFull(r, nBuf[:]); err != nil {
		return SecurityTypeList{}, wrapTruncated("security type list count", err)
	}
	n := int(nBuf[0])
	if n == 0 {
		return SecurityTypeList{Empty: true}, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return SecurityTypeList{}, wrapTruncated("security type list entries", err)
	}
	types := make([]SecurityType, n)
	for i, b := range buf {
		types[i] = SecurityType(b)
	}
	return SecurityTypeList{Types: types}, nil
}

// WriteSecurityTypeList writes the security-type list wire format for the
// given types. Passing an empty slice writes n=0 (the caller is then
// responsible for writing a reason string immediately after, per RFC
// 6143 §7.1.2 / §7.1.1).
func WriteSecurityTypeList(w io.Writer, types []SecurityType) error {
	if len(types) > 255 {
		return fmt.Errorf("rfb: too many security types to encode: %d", len(types))
	}
	buf := make([]byte, 1+len(types))
	buf[0] = byte(len(types))
	for i, t := range types {
		buf[1+i] = byte(t)
	}
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("rfb: write security type list: %w", err)
	}
	return nil
}

// SelectSecurityType picks one security type from offered that this
// plugin supports, preferring the strongest supported match: VNCAuth over
// None, when both are offered. Returns ErrNoSupportedSecurityType (naming
// both what was offered and what this plugin supports) when no match
// exists.
func SelectSecurityType(offered []SecurityType, supported []SecurityType) (SecurityType, error) {
	supportedSet := make(map[SecurityType]bool, len(supported))
	for _, t := range supported {
		supportedSet[t] = true
	}

	// Prefer VNCAuth over None if both are offered and both are supported,
	// since it actually authenticates the connection.
	preference := []SecurityType{SecTypeVNCAuth, SecTypeNone}
	offeredSet := make(map[SecurityType]bool, len(offered))
	for _, t := range offered {
		offeredSet[t] = true
	}
	for _, pref := range preference {
		if supportedSet[pref] && offeredSet[pref] {
			return pref, nil
		}
	}

	// Fall back to first offered type that's supported and not already in
	// the preference list (keeps this correct even if SupportedSecurityTypes
	// grows before preference does).
	for _, t := range offered {
		if supportedSet[t] {
			return t, nil
		}
	}

	return SecTypeInvalid, &ErrNoSupportedSecurityType{Offered: offered, Supported: supported}
}

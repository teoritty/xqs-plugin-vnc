package rfb

import (
	"crypto/des"
	"fmt"
)

// vncChallengeLen is the fixed length of the server's VNC Authentication
// challenge (and, correspondingly, the client's response), per RFC 6143
// §7.2.2.
const vncChallengeLen = 16

// reverseByteBits reverses the bit order within a single byte (bit 0
// becomes bit 7, bit 1 becomes bit 6, and so on).
func reverseByteBits(b byte) byte {
	var r byte
	for i := 0; i < 8; i++ {
		if b&(1<<uint(i)) != 0 {
			r |= 1 << uint(7-i)
		}
	}
	return r
}

// deriveDESKey derives the 8-byte DES key VNC Authentication uses from a
// password, per RFC 6143 §7.2.2: the password is truncated or zero-padded
// to exactly 8 bytes, and then the bits of each of those 8 bytes are
// reversed. This bit reversal is the classic VNC/RFB quirk — standard DES
// key scheduling expects MSB-first bit order per byte, but VNC's
// password-derived key uses the opposite order.
//
// password is not mutated or retained by this function.
func deriveDESKey(password []byte) [8]byte {
	var padded [8]byte
	n := copy(padded[:], password) // copies at most 8 bytes; zero-pads the rest
	_ = n

	var key [8]byte
	for i, b := range padded {
		key[i] = reverseByteBits(b)
	}
	return key
}

// VNCAuthResponse computes the client's response to a VNC Authentication
// (security type 2) challenge, per RFC 6143 §7.2.2: the DES key is derived
// from password via deriveDESKey, and the 16-byte challenge is encrypted as
// two independent 8-byte blocks under that same key (ECB style — no
// chaining, no IV).
//
// password and challenge are read but not mutated; callers remain
// responsible for zeroing the password after use (this package does not do
// so itself — see the design plan's password-handling section).
//
// challenge must be exactly 16 bytes; any other length is an error.
func VNCAuthResponse(password []byte, challenge []byte) ([]byte, error) {
	if len(challenge) != vncChallengeLen {
		return nil, fmt.Errorf("rfb: VNC auth challenge must be %d bytes, got %d", vncChallengeLen, len(challenge))
	}

	key := deriveDESKey(password)
	block, err := des.NewCipher(key[:])
	if err != nil {
		// des.NewCipher only fails on wrong key length, which cannot happen
		// here since key is a fixed [8]byte.
		return nil, fmt.Errorf("rfb: VNC auth: %w", err)
	}

	response := make([]byte, vncChallengeLen)
	block.Encrypt(response[0:8], challenge[0:8])
	block.Encrypt(response[8:16], challenge[8:16])
	return response, nil
}

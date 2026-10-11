package pairing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

// The pairing proof formula is wire protocol: changing it breaks
// interoperability and requires a protocol version bump.
//
//	codeKey        = SHA-256("filesync-pair-v1:" || code)
//	proofMsg(tag)  = tag || 0x00 || nonceB || 0x00 || nonceA || 0x00 ||
//	                 initiatorID || 0x00 || acceptorID
//	proof_a        = HMAC-SHA256(codeKey, proofMsg("initiator"))
//	proof_b        = HMAC-SHA256(codeKey, proofMsg("acceptor"))
//
// Binding both nonces and both device IDs into every proof makes a
// proof captured in one session useless in any other session or for
// any other pair of devices.
const codeKeyDomain = "filesync-pair-v1:"

func codeKey(code string) []byte {
	sum := sha256.Sum256([]byte(codeKeyDomain + code))
	return sum[:]
}

func proofFor(key []byte, tag string, nonceB, nonceA []byte, initiatorID, acceptorID string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(tag))
	mac.Write([]byte{0})
	mac.Write(nonceB)
	mac.Write([]byte{0})
	mac.Write(nonceA)
	mac.Write([]byte{0})
	mac.Write([]byte(initiatorID))
	mac.Write([]byte{0})
	mac.Write([]byte(acceptorID))
	return mac.Sum(nil)
}

// verifyProof compares a presented proof against the expected one in
// constant time (hmac.Equal).
func verifyProof(key []byte, tag string, nonceB, nonceA []byte, initiatorID, acceptorID string, presented []byte) bool {
	return hmac.Equal(proofFor(key, tag, nonceB, nonceA, initiatorID, acceptorID), presented)
}

func genNonce(r io.Reader) ([]byte, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, fmt.Errorf("pairing: generate nonce: %w", err)
	}
	return b, nil
}

func genSessionID(r io.Reader) (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("pairing: generate session id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

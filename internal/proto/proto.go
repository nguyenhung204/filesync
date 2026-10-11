// Package proto defines FileSync's wire format: a length-prefixed frame
// carrying a JSON header and an optional binary payload, plus strict
// validation of everything a peer sends. Peer input is never trusted:
// sizes are checked before allocation and every message is validated
// field by field against the rules for its type (see frame.go for the
// read/write path and the check order).
package proto

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"filesync/internal/chunk"
	"filesync/internal/identity"
	"filesync/internal/store"
	"filesync/internal/validate"
)

// ProtocolVersion is the only wire protocol version this build speaks.
// Changing frame semantics requires bumping it.
const ProtocolVersion = 2

// Size limits. A peer-supplied length never controls allocation beyond
// these bounds (threat 3 in docs/threats.md).
const (
	// MaxHeaderBytes is capped at 0xFFFF (not 64 KiB): the frame
	// carries headerLen in a uint16 field, so a 65536-byte header
	// could never be represented — it would wrap to 0 on write
	// (round-2 fix; the limit is a comment's width below 64 KiB and
	// real headers are ≲10 KiB).
	MaxHeaderBytes       = 0xFFFF
	MaxIndexPayloadBytes = 4 << 20 // 4 MiB
	MaxFrameBytes        = 2 + MaxHeaderBytes + MaxIndexPayloadBytes
	MaxIndexBatch        = 1000 // entries per Index message
	MaxChunkIndexes      = 1024 // indexes per ChunkRequest
)

// Error codes: the closed set allowed in Error and PairReject messages.
const (
	CodeProtocolVersion     = "protocol_version"
	CodeMessageTooLarge     = "message_too_large"
	CodeBadMessage          = "bad_message"
	CodeUnpaired            = "unpaired"
	CodePairingUnavailable  = "pairing_unavailable"
	CodePairingExpired      = "pairing_expired"
	CodeBadProof            = "bad_proof"
	CodeAttemptsExceeded    = "attempts_exceeded"
	CodeFingerprintRejected = "fingerprint_rejected"
	CodeRevoked             = "revoked"
	CodeInternal            = "internal"
)

// ValidCode reports whether code belongs to the closed error-code set.
func ValidCode(code string) bool {
	switch code {
	case CodeProtocolVersion, CodeMessageTooLarge, CodeBadMessage, CodeUnpaired,
		CodePairingUnavailable, CodePairingExpired, CodeBadProof,
		CodeAttemptsExceeded, CodeFingerprintRejected, CodeRevoked, CodeInternal:
		return true
	}
	return false
}

// Type is a message type tag.
type Type string

// Message types.
const (
	TypeHello         Type = "hello"
	TypeIndex         Type = "index"
	TypeIndexRequest  Type = "index_request"
	TypeChunkRequest  Type = "chunk_request"
	TypeChunkData     Type = "chunk_data"
	TypeAck           Type = "ack"
	TypeError         Type = "error"
	TypePairRequest   Type = "pair_request"
	TypePairChallenge Type = "pair_challenge"
	TypePairProof     Type = "pair_proof"
	TypePairAccept    Type = "pair_accept"
	TypePairConfirm   Type = "pair_confirm"
	TypePairFinal     Type = "pair_final"
	TypePairReject    Type = "pair_reject"
	TypePing          Type = "ping"
	TypePong          Type = "pong"
)

func validType(t Type) bool {
	switch t {
	case TypeHello, TypeIndex, TypeIndexRequest, TypeChunkRequest, TypeChunkData,
		TypeAck, TypeError, TypePairRequest, TypePairChallenge, TypePairProof,
		TypePairAccept, TypePairConfirm, TypePairFinal, TypePairReject,
		TypePing, TypePong:
		return true
	}
	return false
}

func isPairType(t Type) bool {
	switch t {
	case TypePairRequest, TypePairChallenge, TypePairProof, TypePairAccept,
		TypePairConfirm, TypePairFinal, TypePairReject:
		return true
	}
	return false
}

// Header is the single JSON header shape for every frame. Fields that do
// not apply to a message type stay empty (omitempty).
type Header struct {
	Type     Type   `json:"type"`
	Protocol int    `json:"protocol,omitempty"`  // hello only: ProtocolVersion
	DeviceID string `json:"device_id,omitempty"` // hello, pair_request
	Name     string `json:"name,omitempty"`      // hello, pair_request (≤ 128 bytes)
	// index
	More bool `json:"more,omitempty"` // more Index batches follow
	// chunk
	Path     string `json:"path,omitempty"`     // chunk_request, chunk_data
	Indexes  []int  `json:"indexes,omitempty"`  // chunk_request (≤ MaxChunkIndexes, ≥ 0, unique)
	Index    int    `json:"index,omitempty"`    // chunk_data (≥ 0)
	Size     int    `json:"size,omitempty"`     // chunk_data (raw size; == len(payload) when Encoding is "")
	SHA256   string `json:"sha256,omitempty"`   // chunk_data (lowercase hex, 64 chars, of the raw chunk)
	Encoding string `json:"encoding,omitempty"` // chunk_data only: "" (raw) | "gzip"
	// pairing
	SessionID string `json:"session_id,omitempty"` // all Pair* except pair_request
	Nonce     string `json:"nonce,omitempty"`      // pair_challenge, pair_proof (base64, 32 bytes)
	Proof     string `json:"proof,omitempty"`      // pair_proof, pair_accept (base64, 32 bytes)
	// error / ack
	Code    string `json:"code,omitempty"`    // error, pair_reject (closed set)
	Message string `json:"message,omitempty"` // error (≤ 512 bytes, never a secret)
	AckType Type   `json:"ack_type,omitempty"`
}

// Message is one wire frame: a header plus an optional payload. Only
// TypeIndex (JSON) and TypeChunkData (binary) may carry a payload.
type Message struct {
	Header  Header
	Payload []byte
}

func badf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBadMessage, fmt.Sprintf(format, args...))
}

// Validate checks m against every rule for its type. Both the read path
// and the write path run it: a local caller must not be able to emit a
// frame that a peer would reject, and a peer must not be able to make us
// accept one either.
func (m Message) Validate() error {
	h := &m.Header
	if !validType(h.Type) {
		return badf("unknown type %q", h.Type)
	}
	if len(h.Name) > 128 {
		return badf("name too long: %d bytes", len(h.Name))
	}
	if len(h.Message) > 512 {
		return badf("error message too long: %d bytes", len(h.Message))
	}
	if h.Code != "" && !ValidCode(h.Code) {
		return badf("unknown error code %q", h.Code)
	}
	// Payload discipline: only index and chunk_data may carry one.
	if len(m.Payload) > 0 && h.Type != TypeIndex && h.Type != TypeChunkData {
		return badf("type %s must not carry a payload", h.Type)
	}
	// Encoding discipline: only chunk_data may declare one.
	if h.Encoding != "" && h.Type != TypeChunkData {
		return badf("type %s must not carry an encoding", h.Type)
	}
	// Pairing fields shared by all Pair* types. PairReject is exempt
	// from the session_id requirement: rejections also occur before a
	// session exists (unknown/expired code at the PairRequest step,
	// spec Flows B and C).
	if isPairType(h.Type) {
		if h.Type != TypePairRequest && h.Type != TypePairReject && h.SessionID == "" {
			return badf("type %s requires session_id", h.Type)
		}
		if err := checkB64Field("nonce", h.Nonce); err != nil {
			return err
		}
		if err := checkB64Field("proof", h.Proof); err != nil {
			return err
		}
	}

	switch h.Type {
	case TypeHello:
		// Protocol value is deliberately NOT checked here: the
		// transport layer owns the version handshake (spec Flow F)
		// and must be able to read a Hello{protocol: 99} in order to
		// answer it with Error{protocol_version}.
		if _, err := identity.ParseDeviceID(h.DeviceID); err != nil {
			return badf("hello: bad device_id: %v", err)
		}
	case TypeIndex:
		if _, err := m.Entries(); err != nil {
			return err
		}
	case TypeChunkRequest:
		if err := validatePath(h.Path); err != nil {
			return err
		}
		if len(h.Indexes) < 1 || len(h.Indexes) > MaxChunkIndexes {
			return badf("chunk_request: %d indexes, want 1..%d", len(h.Indexes), MaxChunkIndexes)
		}
		seen := make(map[int]struct{}, len(h.Indexes))
		for _, ix := range h.Indexes {
			if ix < 0 {
				return badf("chunk_request: negative index %d", ix)
			}
			if _, dup := seen[ix]; dup {
				return badf("chunk_request: duplicate index %d", ix)
			}
			seen[ix] = struct{}{}
		}
	case TypeChunkData:
		if err := validatePath(h.Path); err != nil {
			return err
		}
		if h.Index < 0 {
			return badf("chunk_data: negative index %d", h.Index)
		}
		// Size always describes the RAW chunk. For raw payloads it
		// must equal the payload length; for gzip payloads the
		// compressed length is bounded by MaxGzipPayloadBytes
		// instead (the raw length is re-checked after decompression
		// in DecompressChunkPayload).
		switch h.Encoding {
		case "":
			if h.Size != len(m.Payload) {
				return badf("chunk_data: size %d != payload %d bytes", h.Size, len(m.Payload))
			}
		case EncodingGzip:
			if len(m.Payload) < 1 || len(m.Payload) > MaxGzipPayloadBytes {
				return badf("chunk_data: gzip payload %d bytes out of range 1..%d", len(m.Payload), MaxGzipPayloadBytes)
			}
		default:
			return badf("chunk_data: unknown encoding %q", h.Encoding)
		}
		if h.Size < 1 || h.Size > chunk.ChunkSize {
			return badf("chunk_data: size %d out of range 1..%d", h.Size, chunk.ChunkSize)
		}
		if err := checkSHA256Hex(h.SHA256); err != nil {
			return err
		}
	case TypePairRequest:
		if _, err := identity.ParseDeviceID(h.DeviceID); err != nil {
			return badf("pair_request: bad device_id: %v", err)
		}
	case TypeError, TypePairReject:
		if h.Code == "" {
			return badf("type %s requires a code", h.Type)
		}
	}
	return nil
}

func checkB64Field(name, v string) error {
	if v == "" {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return badf("%s: invalid base64: %v", name, err)
	}
	if len(raw) != 32 {
		return badf("%s: decoded %d bytes, want 32", name, len(raw))
	}
	return nil
}

func checkSHA256Hex(s string) error {
	if len(s) != 64 {
		return badf("sha256: %d chars, want 64", len(s))
	}
	if s != strings.ToLower(s) {
		return badf("sha256: must be lowercase hex")
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return badf("sha256: invalid hex: %v", err)
	}
	if len(raw) != 32 {
		return badf("sha256: decoded %d bytes, want 32", len(raw))
	}
	return nil
}

func validatePath(p string) error {
	if _, err := validate.ValidateRelPath(p, validate.DefaultMaxLen); err != nil {
		return badf("bad path: %v", err)
	}
	return nil
}

// Entries decodes the payload of a TypeIndex message, enforcing the
// batch and per-entry path rules. It is an error to call it on any
// other message type.
func (m Message) Entries() ([]store.Entry, error) {
	if m.Header.Type != TypeIndex {
		return nil, badf("Entries called on type %s", m.Header.Type)
	}
	if len(m.Payload) > MaxIndexPayloadBytes {
		return nil, fmt.Errorf("%w: index payload %d bytes exceeds %d", ErrTooLarge, len(m.Payload), MaxIndexPayloadBytes)
	}
	var entries []store.Entry
	if err := json.Unmarshal(m.Payload, &entries); err != nil {
		return nil, badf("index payload: invalid JSON: %v", err)
	}
	if len(entries) > MaxIndexBatch {
		return nil, fmt.Errorf("%w: index batch %d entries exceeds %d", ErrTooLarge, len(entries), MaxIndexBatch)
	}
	for i := range entries {
		if err := validatePath(entries[i].Path); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

// IndexMessage builds a TypeIndex message from entries and validates it
// against all limits, so a sender fails locally instead of emitting a
// frame the peer would reject.
func IndexMessage(entries []store.Entry, more bool) (Message, error) {
	payload, err := json.Marshal(entries)
	if err != nil {
		return Message{}, fmt.Errorf("proto: encode index: %w", err)
	}
	m := Message{Header: Header{Type: TypeIndex, More: more}, Payload: payload}
	if err := m.Validate(); err != nil {
		return Message{}, err
	}
	return m, nil
}

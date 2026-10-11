package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Frame layout (fixed; changing it requires a protocol version bump):
//
//	[4B BE uint32 frameLen][2B BE uint16 headerLen][header JSON][payload]
//	frameLen = 2 + headerLen + len(payload)   (excludes the 4 length bytes)
var (
	ErrTooLarge   = errors.New("proto: message too large")
	ErrBadMessage = errors.New("proto: bad message")
)

// WriteMessage validates m, then writes the whole frame in a single
// Write call so frames from concurrent writers cannot interleave when
// the caller serializes per connection (transport.Conn does).
func WriteMessage(w io.Writer, m Message) error {
	if err := m.Validate(); err != nil {
		return err
	}
	hdr, err := json.Marshal(m.Header)
	if err != nil {
		return fmt.Errorf("proto: encode header: %w", err)
	}
	if len(hdr) > MaxHeaderBytes {
		// Header-size violations are ErrBadMessage, matching the
		// read-path check order in the spec (step 3); ErrTooLarge is
		// reserved for the frame length itself.
		return fmt.Errorf("%w: header %d bytes exceeds %d", ErrBadMessage, len(hdr), MaxHeaderBytes)
	}
	frameLen := 2 + len(hdr) + len(m.Payload)
	if frameLen > MaxFrameBytes {
		return fmt.Errorf("%w: frame %d bytes exceeds %d", ErrTooLarge, frameLen, MaxFrameBytes)
	}
	buf := make([]byte, 4+frameLen)
	binary.BigEndian.PutUint32(buf, uint32(frameLen))
	binary.BigEndian.PutUint16(buf[4:], uint16(len(hdr)))
	copy(buf[6:], hdr)
	copy(buf[6+len(hdr):], m.Payload)
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("proto: write frame: %w", err)
	}
	return nil
}

// ReadMessage reads exactly one frame from r. The check order is a
// security property: the peer-supplied frame length is compared with
// MaxFrameBytes BEFORE any allocation sized by it, so a hostile length
// prefix can never drive a huge allocation (threat 3). A clean EOF at a
// frame boundary is returned as io.EOF.
func ReadMessage(r io.Reader) (Message, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return Message{}, err // io.EOF / io.ErrUnexpectedEOF pass through
	}
	frameLen := binary.BigEndian.Uint32(lenBuf[:])
	if frameLen < 2 || frameLen > MaxFrameBytes {
		return Message{}, fmt.Errorf("%w: frame length %d", ErrTooLarge, frameLen)
	}
	buf := make([]byte, frameLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		return Message{}, fmt.Errorf("proto: read frame body: %w", err)
	}
	headerLen := int(binary.BigEndian.Uint16(buf))
	if headerLen > MaxHeaderBytes {
		return Message{}, badf("header length %d exceeds %d", headerLen, MaxHeaderBytes)
	}
	if 2+headerLen > len(buf) {
		return Message{}, badf("header length %d exceeds frame %d", headerLen, len(buf))
	}
	var h Header
	if err := json.Unmarshal(buf[2:2+headerLen], &h); err != nil {
		return Message{}, badf("invalid header JSON: %v", err)
	}
	m := Message{Header: h, Payload: append([]byte(nil), buf[2+headerLen:]...)}
	if err := m.Validate(); err != nil {
		return Message{}, err
	}
	return m, nil
}

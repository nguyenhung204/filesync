package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func rawFrame(headerJSON string, payload []byte) []byte {
	frameLen := 2 + len(headerJSON) + len(payload)
	buf := make([]byte, 4+frameLen)
	binary.BigEndian.PutUint32(buf, uint32(frameLen))
	binary.BigEndian.PutUint16(buf[4:], uint16(len(headerJSON)))
	copy(buf[6:], headerJSON)
	copy(buf[6+len(headerJSON):], payload)
	return buf
}

func TestReadMessageRejectsOversizedFrame(t *testing.T) {
	// Only the 4-byte length prefix exists: the reader must refuse
	// before trying to allocate or read a 1 GiB body.
	var buf bytes.Buffer
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], 1<<30)
	buf.Write(lb[:])
	_, err := ReadMessage(&buf)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestReadMessageRejectsBadHeaders(t *testing.T) {
	cases := map[string][]byte{
		"header length beyond frame": func() []byte {
			b := rawFrame(`{"type":"ping"}`, nil)
			binary.BigEndian.PutUint16(b[4:], 60000)
			return b
		}(),
		"header not json": rawFrame(`{nope`, nil),
		"unknown type":    rawFrame(`{"type":"teleport"}`, nil),
		"unknown code":    rawFrame(`{"type":"error","code":"whatever"}`, nil),
		"payload on ping": rawFrame(`{"type":"ping"}`, []byte{1}),
	}
	for name, frame := range cases {
		if _, err := ReadMessage(bytes.NewReader(frame)); !errors.Is(err, ErrBadMessage) && !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s: err = %v, want ErrBadMessage/ErrTooLarge", name, err)
		}
	}
}

func TestReadMessageCleanEOF(t *testing.T) {
	if _, err := ReadMessage(bytes.NewReader(nil)); err != io.EOF {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	// Two frames back to back read as two messages.
	var buf bytes.Buffer
	m := Message{Header: Header{Type: TypePing}}
	if err := WriteMessage(&buf, m); err != nil {
		t.Fatal(err)
	}
	if err := WriteMessage(&buf, m); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got, err := ReadMessage(&buf)
		if err != nil || got.Header.Type != TypePing {
			t.Fatalf("frame %d: got %v, err %v", i, got.Header.Type, err)
		}
	}
	if _, err := ReadMessage(&buf); err != io.EOF {
		t.Fatalf("after frames: err = %v, want io.EOF", err)
	}
}

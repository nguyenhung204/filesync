package proto

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"filesync/internal/chunk"
	"filesync/internal/identity"
	"filesync/internal/store"
	"filesync/internal/vclock"
)

func testDeviceID(seed string) string {
	return identity.DeviceIDFromCertDER([]byte(seed))
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func chunkDataMsg(payload []byte, index int) Message {
	sum := sha256.Sum256(payload)
	return Message{Header: Header{
		Type: TypeChunkData, Path: "dir/file.bin", Index: index,
		Size: len(payload), SHA256: hex.EncodeToString(sum[:]),
	}, Payload: payload}
}

func validMessages(t *testing.T) []Message {
	t.Helper()
	id := testDeviceID("cert-a")
	nonce := b64(bytes.Repeat([]byte{7}, 32))
	proof := b64(bytes.Repeat([]byte{9}, 32))
	return []Message{
		{Header: Header{Type: TypeHello, Protocol: ProtocolVersion, DeviceID: id, Name: "node-a"}},
		{Header: Header{Type: TypeIndexRequest}},
		{Header: Header{Type: TypePing}},
		{Header: Header{Type: TypePong}},
		{Header: Header{Type: TypeAck, AckType: TypeIndex}},
		{Header: Header{Type: TypeChunkRequest, Path: "a/b.txt", Indexes: []int{0, 2, 5}}},
		chunkDataMsg([]byte{0, 255, 1, 2}, 0),
		{Header: Header{Type: TypeError, Code: CodeBadMessage, Message: "nope"}},
		{Header: Header{Type: TypePairRequest, DeviceID: id, Name: "node-a"}},
		{Header: Header{Type: TypePairChallenge, SessionID: "sess", Nonce: nonce}},
		{Header: Header{Type: TypePairProof, SessionID: "sess", Nonce: nonce, Proof: proof}},
		{Header: Header{Type: TypePairAccept, SessionID: "sess", Proof: proof}},
		{Header: Header{Type: TypePairConfirm, SessionID: "sess"}},
		{Header: Header{Type: TypePairFinal, SessionID: "sess"}},
		{Header: Header{Type: TypePairReject, SessionID: "sess", Code: CodeBadProof}},
	}
}

func TestFrameRoundTrip(t *testing.T) {
	msgs := validMessages(t)
	// Index with real entries.
	idx, err := IndexMessage([]store.Entry{
		{Path: "a.txt", Size: 3, Vector: vclock.VersionVector{}},
		{Path: "d/b.txt", Size: 0, Vector: vclock.VersionVector{}},
	}, true)
	if err != nil {
		t.Fatalf("IndexMessage: %v", err)
	}
	msgs = append(msgs, idx)
	// Chunk data at exactly ChunkSize with binary content.
	big := make([]byte, chunk.ChunkSize)
	for i := range big {
		big[i] = byte(i * 31)
	}
	msgs = append(msgs, chunkDataMsg(big, 3))

	for _, want := range msgs {
		var buf bytes.Buffer
		if err := WriteMessage(&buf, want); err != nil {
			t.Fatalf("WriteMessage(%s): %v", want.Header.Type, err)
		}
		got, err := ReadMessage(&buf)
		if err != nil {
			t.Fatalf("ReadMessage(%s): %v", want.Header.Type, err)
		}
		if got.Header.Type != want.Header.Type {
			t.Errorf("type = %s, want %s", got.Header.Type, want.Header.Type)
		}
		if !bytes.Equal(got.Payload, want.Payload) {
			t.Errorf("%s: payload mismatch (%d vs %d bytes)", want.Header.Type, len(got.Payload), len(want.Payload))
		}
		if buf.Len() != 0 {
			t.Errorf("%s: %d unread bytes left", want.Header.Type, buf.Len())
		}
	}
	// Index entries decode back.
	entries, err := idx.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 2 || entries[0].Path != "a.txt" || entries[1].Path != "d/b.txt" {
		t.Errorf("entries = %+v", entries)
	}
}

func TestChunkDataValidation(t *testing.T) {
	good := chunkDataMsg([]byte("hello"), 0)
	cases := []struct {
		name   string
		mutate func(m *Message)
	}{
		{"size mismatch", func(m *Message) { m.Header.Size = 99 }},
		{"size too big", func(m *Message) {
			m.Payload = make([]byte, chunk.ChunkSize+1)
			m.Header.Size = len(m.Payload)
			sum := sha256.Sum256(m.Payload)
			m.Header.SHA256 = hex.EncodeToString(sum[:])
		}},
		{"empty chunk", func(m *Message) {
			m.Payload = nil
			m.Header.Size = 0
			m.Header.SHA256 = hex.EncodeToString(make([]byte, 32))
		}},
		{"sha short", func(m *Message) { m.Header.SHA256 = "abcd" }},
		{"sha uppercase", func(m *Message) { m.Header.SHA256 = strings.ToUpper(m.Header.SHA256) }},
		{"sha not hex", func(m *Message) { m.Header.SHA256 = strings.Repeat("z", 64) }},
		{"negative index", func(m *Message) { m.Header.Index = -1 }},
		{"bad path", func(m *Message) { m.Header.Path = "../escape" }},
	}
	for _, tc := range cases {
		m := good
		m.Payload = append([]byte(nil), good.Payload...)
		tc.mutate(&m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s: Validate passed, want error", tc.name)
		}
	}
	if err := good.Validate(); err != nil {
		t.Errorf("good message rejected: %v", err)
	}
}

func TestChunkRequestRejectsBadPath(t *testing.T) {
	for _, p := range []string{"../x", "/abs", `C:\x`, "a/../../b", "CON.txt", "a/b.txt:stream"} {
		m := Message{Header: Header{Type: TypeChunkRequest, Path: p, Indexes: []int{0}}}
		if err := m.Validate(); err == nil {
			t.Errorf("path %q accepted, want rejection", p)
		}
	}
	ok := Message{Header: Header{Type: TypeChunkRequest, Path: "a/b/c.txt", Indexes: []int{0}}}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid nested path rejected: %v", err)
	}
	// Index count / duplicate rules.
	for _, idxs := range [][]int{nil, {}, {-1}, {0, 0}, make([]int, MaxChunkIndexes+1)} {
		m := Message{Header: Header{Type: TypeChunkRequest, Path: "a", Indexes: idxs}}
		if err := m.Validate(); err == nil {
			t.Errorf("indexes %v accepted, want rejection", idxs)
		}
	}
}

func TestIndexBatchLimits(t *testing.T) {
	many := make([]store.Entry, MaxIndexBatch+1)
	for i := range many {
		many[i] = store.Entry{Path: "f.txt", Vector: vclock.VersionVector{}}
	}
	if _, err := IndexMessage(many, false); err == nil {
		t.Error("IndexMessage accepted more than MaxIndexBatch entries")
	}
	oversize := Message{Header: Header{Type: TypeIndex}, Payload: make([]byte, MaxIndexPayloadBytes+1)}
	if err := oversize.Validate(); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversize index payload: err = %v, want ErrTooLarge", err)
	}
	badEntry := Message{Header: Header{Type: TypeIndex}, Payload: []byte(`[{"Path":"../evil"}]`)}
	if err := badEntry.Validate(); err == nil {
		t.Error("index entry with traversal path accepted")
	}
}

func TestWriteMessageRejectsInvalid(t *testing.T) {
	m := chunkDataMsg([]byte("hello"), 0)
	m.Header.Size = 12345 // != payload
	var buf bytes.Buffer
	if err := WriteMessage(&buf, m); err == nil {
		t.Fatal("WriteMessage accepted an invalid message")
	}
	if buf.Len() != 0 {
		t.Errorf("writer received %d bytes for an invalid message", buf.Len())
	}
}

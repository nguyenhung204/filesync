package proto

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand"
	"testing"

	"filesync/internal/chunk"
)

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(raw); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func gzipChunkMsg(raw, payload []byte) Message {
	sum := sha256.Sum256(raw)
	return Message{Header: Header{
		Type: TypeChunkData, Path: "dir/file.bin", Index: 0,
		Size: len(raw), SHA256: hex.EncodeToString(sum[:]), Encoding: EncodingGzip,
	}, Payload: payload}
}

func TestCompressChunkRoundTrip(t *testing.T) {
	// One full chunk of highly repetitive data.
	raw := bytes.Repeat([]byte("filesync-chunk-"), chunk.ChunkSize/len("filesync-chunk-")+1)[:chunk.ChunkSize]
	payload, enc := CompressChunkPayload(raw)
	if enc != EncodingGzip {
		t.Fatalf("encoding = %q, want %q", enc, EncodingGzip)
	}
	if len(payload) >= len(raw)/10 {
		t.Fatalf("compressed payload %d bytes, want < %d (raw/10)", len(payload), len(raw)/10)
	}
	msg := gzipChunkMsg(raw, payload)

	// The frame survives the wire unchanged.
	var buf bytes.Buffer
	if err := WriteMessage(&buf, msg); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	got, err := ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if got.Header.Type != msg.Header.Type || got.Header.Path != msg.Header.Path ||
		got.Header.Index != msg.Header.Index || got.Header.Size != msg.Header.Size ||
		got.Header.SHA256 != msg.Header.SHA256 || got.Header.Encoding != msg.Header.Encoding {
		t.Errorf("header = %+v, want %+v", got.Header, msg.Header)
	}
	if !bytes.Equal(got.Payload, payload) {
		t.Errorf("payload changed on the wire (%d vs %d bytes)", len(got.Payload), len(payload))
	}

	// Decompression returns the exact raw chunk, verifiable against
	// the chunk map hash of the raw data (ADR-002 unchanged).
	back, err := DecompressChunkPayload(got, chunk.ChunkSize)
	if err != nil {
		t.Fatalf("DecompressChunkPayload: %v", err)
	}
	if !bytes.Equal(back, raw) {
		t.Fatal("decompressed data differs from the original chunk")
	}
	sum := sha256.Sum256(raw)
	meta := chunk.ChunkMeta{Index: 0, SHA256: sum, Size: len(raw)}
	if !chunk.VerifyChunk(back, meta) {
		t.Error("VerifyChunk rejected the decompressed chunk")
	}
}

func TestCompressIncompressibleReturnsRaw(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	data := make([]byte, 64<<10)
	if _, err := rng.Read(data); err != nil {
		t.Fatalf("rng: %v", err)
	}
	payload, enc := CompressChunkPayload(data)
	if enc != "" {
		t.Errorf("encoding = %q, want raw for incompressible data", enc)
	}
	if !bytes.Equal(payload, data) {
		t.Error("raw fallback must return the original bytes")
	}
	if payload, enc := CompressChunkPayload(nil); enc != "" || len(payload) != 0 {
		t.Errorf("empty input: payload %d bytes, encoding %q, want raw empty", len(payload), enc)
	}
}

func TestDecompressZipBombRejected(t *testing.T) {
	// A tiny gzip payload that expands past one chunk.
	big := make([]byte, chunk.ChunkSize+1)
	m := gzipChunkMsg(big, gzipBytes(t, big))
	if len(m.Payload) > 1<<14 {
		t.Fatalf("test premise broken: bomb payload is %d bytes", len(m.Payload))
	}
	if _, err := DecompressChunkPayload(m, chunk.ChunkSize); !errors.Is(err, ErrTooLarge) {
		t.Errorf("zip bomb: err = %v, want ErrTooLarge", err)
	}
	// The cap comes from maxOut, not from any declared size.
	small := gzipBytes(t, make([]byte, 4096))
	m2 := gzipChunkMsg(make([]byte, 4096), small)
	if _, err := DecompressChunkPayload(m2, 1024); !errors.Is(err, ErrTooLarge) {
		t.Errorf("maxOut=1024: err = %v, want ErrTooLarge", err)
	}
	// Raw payloads are capped by maxOut too.
	m3 := Message{Header: Header{Type: TypeChunkData, Path: "f", Size: 4096}, Payload: make([]byte, 4096)}
	if _, err := DecompressChunkPayload(m3, 1024); !errors.Is(err, ErrTooLarge) {
		t.Errorf("raw over maxOut: err = %v, want ErrTooLarge", err)
	}
}

func TestDecompressCorruptGzip(t *testing.T) {
	raw := []byte("hello world")
	// Not gzip at all.
	m := gzipChunkMsg(raw, []byte("this is not a gzip stream"))
	if _, err := DecompressChunkPayload(m, chunk.ChunkSize); !errors.Is(err, ErrBadMessage) {
		t.Errorf("non-gzip payload: err = %v, want ErrBadMessage", err)
	}
	// Truncated gzip.
	full := gzipBytes(t, raw)
	m = gzipChunkMsg(raw, full[:len(full)/2])
	if _, err := DecompressChunkPayload(m, chunk.ChunkSize); !errors.Is(err, ErrBadMessage) {
		t.Errorf("truncated gzip: err = %v, want ErrBadMessage", err)
	}
	// Declared raw size does not match the decompressed length.
	m = gzipChunkMsg(raw, gzipBytes(t, raw))
	m.Header.Size = len(raw) + 1
	if _, err := DecompressChunkPayload(m, chunk.ChunkSize); !errors.Is(err, ErrBadMessage) {
		t.Errorf("size mismatch: err = %v, want ErrBadMessage", err)
	}
	// Defense in depth: the helper re-checks type and encoding even
	// for callers that skipped Validate.
	m = gzipChunkMsg(raw, gzipBytes(t, raw))
	m.Header.Encoding = "zstd"
	if _, err := DecompressChunkPayload(m, chunk.ChunkSize); !errors.Is(err, ErrBadMessage) {
		t.Errorf("unknown encoding: err = %v, want ErrBadMessage", err)
	}
	m = Message{Header: Header{Type: TypePing}}
	if _, err := DecompressChunkPayload(m, chunk.ChunkSize); !errors.Is(err, ErrBadMessage) {
		t.Errorf("non-chunk type: err = %v, want ErrBadMessage", err)
	}
	// Raw message whose Size lies about the payload length.
	m = Message{Header: Header{Type: TypeChunkData, Path: "f", Size: 99}, Payload: []byte("short")}
	if _, err := DecompressChunkPayload(m, chunk.ChunkSize); !errors.Is(err, ErrBadMessage) {
		t.Errorf("raw size mismatch: err = %v, want ErrBadMessage", err)
	}
}

func TestProtocolVersionIs2(t *testing.T) {
	// Literal assert on purpose: the next protocol bump must update
	// this test deliberately, not drift through the constant.
	if ProtocolVersion != 2 {
		t.Fatalf("ProtocolVersion = %d, want 2", ProtocolVersion)
	}
	m := Message{Header: Header{Type: TypeHello, Protocol: ProtocolVersion, DeviceID: testDeviceID("cert-a")}}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, m); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	got, err := ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if got.Header.Protocol != 2 {
		t.Errorf("hello on the wire carries protocol %d, want 2", got.Header.Protocol)
	}
}

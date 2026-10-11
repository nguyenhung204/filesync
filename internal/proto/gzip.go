package proto

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"

	"filesync/internal/chunk"
)

// EncodingGzip is the Header.Encoding value marking a gzip-compressed
// chunk_data payload. The empty encoding always means raw payload.
const EncodingGzip = "gzip"

// MaxGzipPayloadBytes caps the COMPRESSED payload of a chunk_data
// message. Gzip of incompressible data grows it by only ~100 bytes at
// 1 MiB (framing plus stored-block headers), so ChunkSize + 1 KiB is a
// generous allowance that still rejects abnormally bloated payloads
// before any decompression happens.
const MaxGzipPayloadBytes = chunk.ChunkSize + 1024

// CompressChunkPayload gzip-compresses one raw chunk for the wire. It
// returns the compressed payload with encoding EncodingGzip only when
// compression actually shrinks the data; otherwise (including empty
// input) it returns data unchanged with encoding "". Callers must
// pass a valid chunk (1..chunk.ChunkSize bytes); the header's Size and
// SHA256 always describe the raw data, never the compressed payload.
func CompressChunkPayload(data []byte) (payload []byte, encoding string) {
	if len(data) == 0 {
		return data, ""
	}
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.DefaultCompression)
	if err != nil {
		return data, "" // unreachable for DefaultCompression
	}
	if _, err := w.Write(data); err != nil {
		return data, ""
	}
	if err := w.Close(); err != nil {
		return data, ""
	}
	if buf.Len() < len(data) {
		return buf.Bytes(), EncodingGzip
	}
	return data, ""
}

// DecompressChunkPayload returns the raw chunk carried by m, which
// must be a chunk_data message. It re-checks the type and encoding
// itself so callers that skipped Validate are still safe. Decompression
// is hard-capped: at most maxOut+1 bytes are ever read, so a zip bomb
// is rejected with ErrTooLarge without allocating for its declared
// size. The decompressed length must equal Header.Size (the raw size)
// or ErrBadMessage is returned. All errors wrap ErrBadMessage or
// ErrTooLarge for errors.Is.
func DecompressChunkPayload(m Message, maxOut int) ([]byte, error) {
	h := m.Header
	if h.Type != TypeChunkData {
		return nil, badf("decompress: type %s, want chunk_data", h.Type)
	}
	switch h.Encoding {
	case "":
		if len(m.Payload) > maxOut {
			return nil, fmt.Errorf("%w: raw payload %d bytes exceeds limit %d", ErrTooLarge, len(m.Payload), maxOut)
		}
		raw := append([]byte(nil), m.Payload...)
		if len(raw) != h.Size {
			return nil, badf("chunk_data: raw size %d != header size %d", len(raw), h.Size)
		}
		return raw, nil
	case EncodingGzip:
		zr, err := gzip.NewReader(bytes.NewReader(m.Payload))
		if err != nil {
			return nil, badf("chunk_data: gzip header: %v", err)
		}
		defer zr.Close()
		raw, err := io.ReadAll(io.LimitReader(zr, int64(maxOut)+1))
		if err != nil {
			return nil, badf("chunk_data: gzip read: %v", err)
		}
		if len(raw) > maxOut {
			return nil, fmt.Errorf("%w: decompressed data exceeds limit %d", ErrTooLarge, maxOut)
		}
		if len(raw) != h.Size {
			return nil, badf("chunk_data: decompressed size %d != header size %d", len(raw), h.Size)
		}
		return raw, nil
	default:
		return nil, badf("chunk_data: unknown encoding %q", h.Encoding)
	}
}

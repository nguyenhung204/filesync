// Package chunk implements fixed-size 1MiB chunking (ADR-002) with
// SHA-256 hashes per chunk, chunk diffing, and per-chunk verification.
package chunk

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// ChunkSize is the fixed chunk size: 1 MiB.
const ChunkSize = 1 << 20

// ChunkMeta describes one chunk of a file.
type ChunkMeta struct {
	Index  int      // 0-based
	SHA256 [32]byte // SHA-256 of exactly Size bytes of chunk data
	Size   int      // 1..ChunkSize; the last chunk may be shorter
}

// Offset returns the byte offset of this chunk within its file.
func (m ChunkMeta) Offset() int64 {
	return int64(m.Index) * ChunkSize
}

// HexHash returns the lowercase hex encoding of the chunk hash.
func (m ChunkMeta) HexHash() string {
	return hex.EncodeToString(m.SHA256[:])
}

// HashChunks reads r sequentially and returns the chunk map. An empty input
// yields an empty, non-nil slice. A mid-stream read error returns nil plus a
// wrapped error; a partial chunk list is never returned.
func HashChunks(r io.Reader) ([]ChunkMeta, error) {
	metas := []ChunkMeta{}
	buf := make([]byte, ChunkSize)
	index := 0
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			metas = append(metas, ChunkMeta{
				Index:  index,
				SHA256: sha256.Sum256(buf[:n]),
				Size:   n,
			})
			index++
		}
		switch err {
		case nil:
			continue
		case io.EOF, io.ErrUnexpectedEOF:
			return metas, nil
		default:
			return nil, fmt.Errorf("chunk: read chunk %d: %w", index, err)
		}
	}
}

// HashFile hashes a file from disk without loading it into memory.
func HashFile(path string) (metas []ChunkMeta, totalSize int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("chunk: open %s: %w", path, err)
	}
	defer f.Close()
	metas, err = HashChunks(f)
	if err != nil {
		return nil, 0, fmt.Errorf("chunk: hash %s: %w", path, err)
	}
	for _, m := range metas {
		totalSize += int64(m.Size)
	}
	return metas, totalSize, nil
}

// Diff returns the indexes (from want) of the chunks that must be
// transferred to turn have into want: positions missing in have or whose
// hash, size, or index differ. Indexes present only in have (file shrank)
// are not returned. The result is ascending and non-nil.
func Diff(have, want []ChunkMeta) []int {
	out := []int{}
	for i, w := range want {
		if i >= len(have) || have[i].SHA256 != w.SHA256 || have[i].Size != w.Size || have[i].Index != w.Index {
			out = append(out, w.Index)
		}
	}
	return out
}

// VerifyChunk reports whether data is exactly the chunk described by meta:
// same length and matching SHA-256. It never panics, including on nil data.
func VerifyChunk(data []byte, meta ChunkMeta) bool {
	if len(data) != meta.Size {
		return false
	}
	sum := sha256.Sum256(data)
	return bytes.Equal(sum[:], meta.SHA256[:])
}

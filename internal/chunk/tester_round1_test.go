package chunk

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Tester round 1: end-to-end resume scenario at primitive level.
// A file >2MiB is hashed from disk, the receiver gets chunks 0 and 2,
// the connection drops, the bitmap is persisted (MarshalBinary) and
// restored (as a journal on restart would), and Missing() must name
// exactly the chunk still owed. Every received chunk must pass
// VerifyChunk; a corrupted copy must not.
func TestTesterResume_BitmapRoundTripFileOver2MiB(t *testing.T) {
	data := testData(2*ChunkSize + 345) // 3 chunks: 1MiB, 1MiB, 345
	path := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	metas, total, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if total != int64(len(data)) || len(metas) != 3 {
		t.Fatalf("HashFile: total=%d chunks=%d, want %d/3", total, len(metas), len(data))
	}

	chunkBytes := func(m ChunkMeta) []byte {
		return data[m.Offset() : m.Offset()+int64(m.Size)]
	}

	// Receiver got chunks 0 and 2 before the drop.
	bm := NewBitmap(len(metas))
	for _, idx := range []int{0, 2} {
		if !VerifyChunk(chunkBytes(metas[idx]), metas[idx]) {
			t.Fatalf("received chunk %d fails VerifyChunk", idx)
		}
		bm.Set(idx)
	}
	// A corrupted copy of chunk 0 must be rejected (engine re-requests it).
	corrupt := append([]byte{}, chunkBytes(metas[0])...)
	corrupt[123] ^= 0xFF
	if VerifyChunk(corrupt, metas[0]) {
		t.Fatal("corrupted chunk passed VerifyChunk")
	}

	// Persist + restore the bitmap, as resume-after-restart does.
	enc, err := bm.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if len(enc) != 8+1 { // 8-byte header + ceil(3/8)
		t.Fatalf("encoded bitmap len = %d, want 9", len(enc))
	}
	var restored Bitmap
	if err := restored.UnmarshalBinary(enc); err != nil {
		t.Fatal(err)
	}
	if restored.Len() != 3 || restored.Count() != 2 {
		t.Fatalf("restored: Len=%d Count=%d, want 3/2", restored.Len(), restored.Count())
	}
	if want := []int{1}; !reflect.DeepEqual(restored.Missing(), want) {
		t.Fatalf("restored Missing = %v, want %v (only the owed chunk)", restored.Missing(), want)
	}
	// Diff against an empty have-list agrees every chunk is owed; Diff
	// against the full local set owes nothing — resume must use the bitmap.
	if got := Diff(nil, metas); !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Fatalf("Diff(nil, metas) = %v, want [0 1 2]", got)
	}
	if got := Diff(metas, metas); len(got) != 0 {
		t.Fatalf("Diff(metas, metas) = %v, want empty", got)
	}

	// Deliver the missing chunk: bitmap completes and verifies.
	if !VerifyChunk(chunkBytes(metas[1]), metas[1]) {
		t.Fatal("missing chunk 1 fails VerifyChunk on arrival")
	}
	restored.Set(1)
	if !restored.All() {
		t.Fatal("bitmap not All after final chunk")
	}
	if m := restored.Missing(); m == nil || len(m) != 0 {
		t.Fatalf("Missing after completion = %v, want empty non-nil", m)
	}

	// Round-trip through bytes.Reader must agree with HashFile exactly.
	again, err := HashChunks(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, metas) {
		t.Fatal("HashChunks and HashFile disagree on >2MiB file")
	}
}

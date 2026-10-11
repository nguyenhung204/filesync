package chunk

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// deterministic content: byte i = i*31 mod 251
func testData(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i * 31) % 251)
	}
	return b
}

func TestHashChunks_Sizes(t *testing.T) {
	cases := []struct {
		size      int
		wantCount int
		lastSize  int
	}{
		{0, 0, 0},
		{1, 1, 1},
		{ChunkSize, 1, ChunkSize},
		{ChunkSize + 1, 2, 1},
		{2*ChunkSize + 7, 3, 7},
	}
	for _, tc := range cases {
		data := testData(tc.size)
		metas, err := HashChunks(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("size %d: %v", tc.size, err)
		}
		if metas == nil {
			t.Fatalf("size %d: metas is nil", tc.size)
		}
		if len(metas) != tc.wantCount {
			t.Fatalf("size %d: got %d chunks, want %d", tc.size, len(metas), tc.wantCount)
		}
		for i, m := range metas {
			if m.Index != i {
				t.Fatalf("size %d: chunk %d has Index %d", tc.size, i, m.Index)
			}
			if m.Offset() != int64(i)*ChunkSize {
				t.Fatalf("size %d: chunk %d Offset = %d", tc.size, i, m.Offset())
			}
			wantSize := ChunkSize
			if i == len(metas)-1 {
				wantSize = tc.lastSize
			}
			if m.Size != wantSize {
				t.Fatalf("size %d: chunk %d Size = %d, want %d", tc.size, i, m.Size, wantSize)
			}
		}
	}
}

func TestHashChunks_KnownVector(t *testing.T) {
	data := []byte("hello filesync")
	metas, err := HashChunks(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 {
		t.Fatalf("got %d chunks, want 1", len(metas))
	}
	want := sha256.Sum256(data)
	if metas[0].SHA256 != want {
		t.Fatalf("hash = %x, want %x", metas[0].SHA256, want)
	}
	if wantHex := hex.EncodeToString(want[:]); metas[0].HexHash() != wantHex {
		t.Fatalf("HexHash = %q, want %q", metas[0].HexHash(), wantHex)
	}
}

func TestHashFile_RoundTrip(t *testing.T) {
	data := testData(ChunkSize + 123)
	path := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	metas, total, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := HashChunks(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(metas, want) {
		t.Fatalf("HashFile metas differ from HashChunks")
	}
	if total != int64(len(data)) {
		t.Fatalf("totalSize = %d, want %d", total, len(data))
	}
	if _, _, err := HashFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file: expected error")
	}
}

func cloneMetas(in []ChunkMeta) []ChunkMeta {
	out := make([]ChunkMeta, len(in))
	copy(out, in)
	return out
}

func TestDiff(t *testing.T) {
	base, _ := HashChunks(bytes.NewReader(testData(3 * ChunkSize)))
	if got := Diff(base, base); got == nil || len(got) != 0 {
		t.Fatalf("Diff(x,x) = %v, want empty non-nil", got)
	}

	changed := cloneMetas(base)
	changed[1].SHA256[0] ^= 0xFF
	if got := Diff(base, changed); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("hash change: Diff = %v, want [1]", got)
	}

	longer := append(cloneMetas(base), ChunkMeta{Index: 3, Size: 10, SHA256: sha256.Sum256([]byte("0123456789"))})
	if got := Diff(base, longer); !reflect.DeepEqual(got, []int{3}) {
		t.Fatalf("want longer: Diff = %v, want [3]", got)
	}

	if got := Diff(base, base[:2]); got == nil || len(got) != 0 {
		t.Fatalf("want shorter: Diff = %v, want empty", got)
	}

	shifted := cloneMetas(base)
	shifted[2].Index = 99 // defensive: position carries wrong index
	// want[2].Index is 2; a have with mismatched Index at that position counts as different.
	if got := Diff(shifted, base); !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("index mismatch: Diff = %v, want [2]", got)
	}

	resized := cloneMetas(base)
	resized[0].Size = 5
	if got := Diff(base, resized); !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("size change: Diff = %v, want [0]", got)
	}

	if got := Diff(nil, base); !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Fatalf("have=nil: Diff = %v, want [0 1 2]", got)
	}
}

func TestVerifyChunk(t *testing.T) {
	data := []byte("chunk payload")
	meta := ChunkMeta{Index: 0, SHA256: sha256.Sum256(data), Size: len(data)}
	if !VerifyChunk(data, meta) {
		t.Fatal("valid chunk rejected")
	}
	tampered := append([]byte{}, data...)
	tampered[0] ^= 0xFF
	if VerifyChunk(tampered, meta) {
		t.Fatal("tampered chunk accepted")
	}
	if VerifyChunk(data[:len(data)-1], meta) {
		t.Fatal("short chunk accepted")
	}
	if VerifyChunk(append(append([]byte{}, data...), 0), meta) {
		t.Fatal("long chunk accepted")
	}
	if VerifyChunk(nil, meta) {
		t.Fatal("nil chunk accepted")
	}
}

package chunk

import (
	"encoding/binary"
	"math"
	"testing"
)

// Job 002c, mục 3 (GATE): a hostile n must never overflow the expected
// length computation or yield a negative Len(). With n =
// 0xFFFFFFFFFFFFFFF9 the old code wrapped (n+7)/8 to 0, accepted an
// 8-byte payload, and produced Len() == -7.
func TestBitmap_UnmarshalRejectsOverflowingN(t *testing.T) {
	b := NewBitmap(3)
	b.Set(1)

	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data[:8], 0xFFFFFFFFFFFFFFF9)
	if err := b.UnmarshalBinary(data); err == nil {
		t.Fatal("UnmarshalBinary(n=0xFFFFFFFFFFFFFFF9): expected error, got nil")
	}
	// A failed decode must leave the destination bitmap untouched.
	if b.Len() != 3 {
		t.Errorf("after failed decode: Len() = %d, want 3", b.Len())
	}
	if missing := b.Missing(); len(missing) != 2 || missing[0] != 0 || missing[1] != 2 {
		t.Errorf("after failed decode: Missing() = %v, want [0 2]", missing)
	}

	// n at the platform int ceiling with a short payload is rejected too.
	data2 := make([]byte, 16)
	binary.BigEndian.PutUint64(data2[:8], uint64(math.MaxInt))
	if err := b.UnmarshalBinary(data2); err == nil {
		t.Fatal("UnmarshalBinary(n=MaxInt, short payload): expected error, got nil")
	}

	// n = 0 with a header-only payload stays valid.
	var empty Bitmap
	data3 := make([]byte, 8)
	if err := empty.UnmarshalBinary(data3); err != nil {
		t.Fatalf("UnmarshalBinary(n=0): err = %v, want nil", err)
	}
	if empty.Len() != 0 {
		t.Errorf("UnmarshalBinary(n=0): Len() = %d, want 0", empty.Len())
	}
}

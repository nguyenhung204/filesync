package chunk

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
)

// Bitmap tracks which chunks of a file have been received, for resume after
// an interrupted transfer.
//
// Binary format (fixed; the resume journal persists it):
// 8-byte big-endian uint64 n, then exactly ceil(n/8) bytes; bit i lives in
// byte i/8 at position i%8, LSB-first.
type Bitmap struct {
	words []uint64
	n     int
}

// NewBitmap creates a bitmap for n chunks. Negative n is treated as 0.
func NewBitmap(n int) *Bitmap {
	if n < 0 {
		n = 0
	}
	return &Bitmap{words: make([]uint64, (n+63)/64), n: n}
}

// Len returns the total number of chunks tracked.
func (b *Bitmap) Len() int {
	if b == nil {
		return 0
	}
	return b.n
}

// Set marks chunk i as received. Out-of-range indexes are ignored: indexes
// come from untrusted peers and must never panic.
func (b *Bitmap) Set(i int) {
	if b == nil || i < 0 || i >= b.n {
		return
	}
	b.words[i/64] |= 1 << (uint(i) % 64)
}

// Has reports whether chunk i has been received.
func (b *Bitmap) Has(i int) bool {
	if b == nil || i < 0 || i >= b.n {
		return false
	}
	return b.words[i/64]&(1<<(uint(i)%64)) != 0
}

// Count returns the number of received chunks in [0, n).
func (b *Bitmap) Count() int {
	if b == nil {
		return 0
	}
	count := 0
	for _, w := range b.words {
		count += bits.OnesCount64(w)
	}
	return count
}

// Missing returns the ascending indexes of chunks not yet received.
// A complete bitmap yields an empty, non-nil slice.
func (b *Bitmap) Missing() []int {
	out := []int{}
	if b == nil {
		return out
	}
	for i := 0; i < b.n; i++ {
		if !b.Has(i) {
			out = append(out, i)
		}
	}
	return out
}

// All reports whether every chunk has been received. n == 0 is complete.
func (b *Bitmap) All() bool {
	if b == nil {
		return false
	}
	return b.Count() == b.n
}

// MarshalBinary encodes the bitmap in the fixed format documented on Bitmap.
func (b *Bitmap) MarshalBinary() ([]byte, error) {
	n := 0
	if b != nil {
		n = b.n
	}
	out := make([]byte, 8+(n+7)/8)
	binary.BigEndian.PutUint64(out[:8], uint64(n))
	if b != nil {
		for i := 0; i < n; i++ {
			if b.Has(i) {
				out[8+i/8] |= 1 << (uint(i) % 8)
			}
		}
	}
	return out, nil
}

// UnmarshalBinary decodes data produced by MarshalBinary. The total length
// must match the declared n exactly; bits beyond n are ignored.
func (b *Bitmap) UnmarshalBinary(data []byte) error {
	if len(data) < 8 {
		return fmt.Errorf("chunk: bitmap too short: %d bytes", len(data))
	}
	n64 := binary.BigEndian.Uint64(data[:8])
	// Guard BEFORE the length arithmetic: for n64 near 2^64 the (n64+7)
	// below would wrap around and let a hostile header pass the length
	// check, and int(n64) would go negative. Anything beyond the
	// platform int range is rejected outright.
	if n64 > uint64(math.MaxInt) {
		return fmt.Errorf("chunk: bitmap length %d exceeds platform int range", n64)
	}
	wantLen := 8 + (n64+7)/8
	if uint64(len(data)) != wantLen {
		return fmt.Errorf("chunk: bitmap length mismatch: n=%d needs %d bytes, got %d", n64, wantLen, len(data))
	}
	n := int(n64)
	fresh := NewBitmap(n)
	for i := 0; i < n; i++ {
		if data[8+i/8]&(1<<(uint(i)%8)) != 0 {
			fresh.Set(i)
		}
	}
	b.words = fresh.words
	b.n = n
	return nil
}

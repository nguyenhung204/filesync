package chunk

import (
	"reflect"
	"testing"
)

func TestBitmap_SetHasMissingCount(t *testing.T) {
	b := NewBitmap(10)
	if b.Len() != 10 {
		t.Fatalf("Len = %d, want 10", b.Len())
	}
	for _, i := range []int{0, 3, 9} {
		b.Set(i)
	}
	for _, i := range []int{0, 3, 9} {
		if !b.Has(i) {
			t.Fatalf("Has(%d) = false", i)
		}
	}
	for _, i := range []int{1, 2, 4, 10, -1} {
		if b.Has(i) {
			t.Fatalf("Has(%d) = true", i)
		}
	}
	if b.Count() != 3 {
		t.Fatalf("Count = %d, want 3", b.Count())
	}
	if want := []int{1, 2, 4, 5, 6, 7, 8}; !reflect.DeepEqual(b.Missing(), want) {
		t.Fatalf("Missing = %v, want %v", b.Missing(), want)
	}
	if b.All() {
		t.Fatal("All = true before completion")
	}
	b.Set(-1)
	b.Set(10)
	b.Set(1000) // out of range: ignored, must not panic
	if b.Count() != 3 {
		t.Fatalf("out-of-range Set changed Count to %d", b.Count())
	}
	for i := 0; i < 10; i++ {
		b.Set(i)
	}
	if !b.All() {
		t.Fatal("All = false after setting every chunk")
	}
	if m := b.Missing(); m == nil || len(m) != 0 {
		t.Fatalf("Missing after complete = %v, want empty non-nil", m)
	}
	if !NewBitmap(0).All() {
		t.Fatal("empty bitmap must be All")
	}
	if NewBitmap(-3).Len() != 0 {
		t.Fatal("negative n must be treated as 0")
	}
}

func TestBitmap_MarshalRoundTrip(t *testing.T) {
	b := NewBitmap(13)
	for _, i := range []int{0, 5, 12} {
		b.Set(i)
	}
	data, err := b.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 8+2 {
		t.Fatalf("encoded length = %d, want 10 (8 + ceil(13/8))", len(data))
	}
	var back Bitmap
	if err := back.UnmarshalBinary(data); err != nil {
		t.Fatal(err)
	}
	if back.Len() != b.Len() || back.Count() != b.Count() ||
		!reflect.DeepEqual(back.Missing(), b.Missing()) {
		t.Fatalf("round trip mismatch: len %d/%d count %d/%d missing %v/%v",
			back.Len(), b.Len(), back.Count(), b.Count(), back.Missing(), b.Missing())
	}
}

func TestBitmap_UnmarshalRejectsBadLength(t *testing.T) {
	b := NewBitmap(16)
	data, err := b.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	var out Bitmap
	if err := out.UnmarshalBinary(data[:len(data)-1]); err == nil {
		t.Fatal("truncated data accepted")
	}
	if err := out.UnmarshalBinary(append(append([]byte{}, data...), 0)); err == nil {
		t.Fatal("extra byte accepted")
	}
	if err := out.UnmarshalBinary([]byte{1, 2, 3}); err == nil {
		t.Fatal("short header accepted")
	}
}

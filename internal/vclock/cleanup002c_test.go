package vclock

import "testing"

// Job 002c, mục 5 (NIT): Canonical drops the empty-device element so a
// hand-written vector can always round-trip through Marshal/Unmarshal,
// while UnmarshalJSON keeps rejecting an empty key from the wire.
func TestVClock_CanonicalDropsEmptyKey(t *testing.T) {
	v := VersionVector{"": 5, "dev": 2}
	c := v.Canonical()
	if _, ok := c[""]; ok {
		t.Errorf("Canonical() kept the empty key: %v", c)
	}
	if c.Get("dev") != 2 || len(c) != 1 {
		t.Errorf("Canonical() = %v, want exactly {dev: 2}", c)
	}

	data, err := v.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var back VersionVector
	if err := back.UnmarshalJSON(data); err != nil {
		t.Fatalf("UnmarshalJSON(%s): %v", data, err)
	}
	if len(back) != 1 || back.Get("dev") != 2 {
		t.Errorf("round trip = %v, want exactly {dev: 2}", back)
	}

	var wire VersionVector
	if err := wire.UnmarshalJSON([]byte(`{"":5}`)); err == nil {
		t.Error("UnmarshalJSON with empty key: expected error, got nil")
	}
}

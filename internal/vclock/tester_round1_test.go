package vclock

import (
	"encoding/json"
	"testing"
)

// Tester round 1: canonical JSON must hold after an unmarshal->marshal
// round (input keys unsorted, explicit zero entries present), after a
// Merge that introduces a zero entry, and Canonical() must be a
// non-mutating clone.
func TestTesterVersionVector_MarshalCanonicalAfterUnmarshal(t *testing.T) {
	var v VersionVector
	if err := json.Unmarshal([]byte(`{"devB":0,"devA":2,"devC":1}`), &v); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"devA":2,"devC":1}` {
		t.Fatalf("re-marshal = %s, want canonical %s (sorted, zeros dropped)", out, `{"devA":2,"devC":1}`)
	}
	out2, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if string(out2) != string(out) {
		t.Fatalf("marshal not deterministic: %s vs %s", out, out2)
	}
}

func TestTesterVersionVector_MergeZeroEntryMarshalsCanonically(t *testing.T) {
	merged := VersionVector{"A": 1}.Merge(VersionVector{"B": 0})
	out, err := json.Marshal(merged)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"A":1}` {
		t.Fatalf("marshal(merged) = %s, want %s (zero entry from merge dropped)", out, `{"A":1}`)
	}
	if merged.Compare(VersionVector{"A": 1}) != Equal {
		t.Fatalf("merged = %v, want equal to {A:1}", merged)
	}
}

func TestTesterVersionVector_CanonicalNonMutating(t *testing.T) {
	v := VersionVector{"A": 0, "B": 3}
	c := v.Canonical()
	if _, ok := c["A"]; ok {
		t.Fatalf("Canonical kept zero entry: %v", c)
	}
	if c.Get("B") != 3 || len(c) != 1 {
		t.Fatalf("Canonical = %v, want {B:3}", c)
	}
	// Original must be untouched by Canonical and by mutating its result.
	if len(v) != 2 || v.Get("B") != 3 {
		t.Fatalf("Canonical mutated the original: %v", v)
	}
	c["B"] = 99
	if v.Get("B") != 3 {
		t.Fatalf("mutating Canonical result leaked into original: %v", v)
	}
}

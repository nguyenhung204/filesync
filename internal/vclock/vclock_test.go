package vclock

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestVersionVector_IncrementAndGet(t *testing.T) {
	v := New()
	v = v.Increment("devA").Increment("devA").Increment("devB")
	if v.Get("devA") != 2 || v.Get("devB") != 1 {
		t.Fatalf("got A=%d B=%d, want 2/1", v.Get("devA"), v.Get("devB"))
	}
	if v.Get("unknown") != 0 {
		t.Fatalf("unknown device = %d, want 0", v.Get("unknown"))
	}
	var nilVec VersionVector
	nilVec = nilVec.Increment("devA")
	if nilVec.Get("devA") != 1 {
		t.Fatalf("increment on nil: got %d, want 1", nilVec.Get("devA"))
	}
	if out := v.Increment(""); out.Get("") != 0 {
		t.Fatal("empty device increment should be a no-op")
	}
}

func TestVersionVector_MergeTakesMaxAndDoesNotMutate(t *testing.T) {
	a := VersionVector{"A": 3, "B": 1}
	b := VersionVector{"A": 1, "B": 5, "C": 2}
	merged := a.Merge(b)
	want := VersionVector{"A": 3, "B": 5, "C": 2}
	if merged.Compare(want) != Equal {
		t.Fatalf("merged = %v, want %v", merged, want)
	}
	if a.Compare(VersionVector{"A": 3, "B": 1}) != Equal {
		t.Fatalf("merge mutated a: %v", a)
	}
	if b.Compare(VersionVector{"A": 1, "B": 5, "C": 2}) != Equal {
		t.Fatalf("merge mutated b: %v", b)
	}
	if got := a.Merge(nil); got.Compare(a) != Equal {
		t.Fatalf("Merge(nil) = %v, want clone of %v", got, a)
	}
}

func TestVersionVector_CompareTable(t *testing.T) {
	cases := []struct {
		name string
		v, o VersionVector
		want Relation
	}{
		{"both empty", New(), New(), Equal},
		{"zero equals absent", VersionVector{"A": 0}, New(), Equal},
		{"same", VersionVector{"A": 2}, VersionVector{"A": 2}, Equal},
		{"dominates", VersionVector{"A": 2, "B": 1}, VersionVector{"A": 1, "B": 1}, Dominates},
		{"dominated", VersionVector{"A": 1}, VersionVector{"A": 2}, Dominated},
		{"concurrent", VersionVector{"A": 2, "B": 1}, VersionVector{"A": 1, "B": 2}, Concurrent},
		{"concurrent disjoint", VersionVector{"A": 1}, VersionVector{"B": 1}, Concurrent},
		{"nil vs nil", nil, nil, Equal},
		{"extra zero key", VersionVector{"A": 1, "B": 0}, VersionVector{"A": 1}, Equal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.v.Compare(tc.o); got != tc.want {
				t.Fatalf("Compare = %v, want %v", got, tc.want)
			}
		})
	}
	if Equal.String() != "equal" || Dominates.String() != "dominates" ||
		Dominated.String() != "dominated" || Concurrent.String() != "concurrent" {
		t.Fatal("Relation.String values wrong")
	}
}

func TestVersionVector_ConcurrentEditsScenario(t *testing.T) {
	base := New().Increment("origin")
	a := base.Clone().Increment("devA")
	b := base.Clone().Increment("devB")
	if a.Compare(b) != Concurrent || b.Compare(a) != Concurrent {
		t.Fatalf("expected concurrent both ways, got %v / %v", a.Compare(b), b.Compare(a))
	}
}

func TestVersionVector_HappenedBeforeAndDominates(t *testing.T) {
	old := VersionVector{"A": 1}
	newer := VersionVector{"A": 2}
	if !old.HappenedBefore(newer) || old.Dominates(newer) {
		t.Fatal("HappenedBefore/Dominates wrong for dominated pair")
	}
	if !newer.Dominates(old) || newer.HappenedBefore(old) {
		t.Fatal("HappenedBefore/Dominates wrong for dominating pair")
	}
	conc := VersionVector{"B": 1}
	if old.HappenedBefore(conc) || old.Dominates(conc) {
		t.Fatal("concurrent vectors must be neither happened-before nor dominating")
	}
}

func TestVersionVector_CloneIndependent(t *testing.T) {
	v := VersionVector{"A": 1}
	c := v.Clone()
	c["A"] = 99
	c["B"] = 5
	if v.Get("A") != 1 || v.Get("B") != 0 {
		t.Fatalf("clone mutation leaked into original: %v", v)
	}
	if got := (VersionVector)(nil).Clone(); got == nil || len(got) != 0 {
		t.Fatalf("clone of nil = %v, want empty non-nil", got)
	}
}

func TestVersionVector_JSONRoundTrip(t *testing.T) {
	v := VersionVector{"B": 2, "A": 1, "Z": 0}
	data1, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	data2, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data1, data2) {
		t.Fatalf("marshal not deterministic: %s vs %s", data1, data2)
	}
	if string(data1) != `{"A":1,"B":2}` {
		t.Fatalf("marshal = %s, want canonical without zero entries", data1)
	}
	var back VersionVector
	if err := json.Unmarshal(data1, &back); err != nil {
		t.Fatal(err)
	}
	if back.Compare(VersionVector{"A": 1, "B": 2}) != Equal {
		t.Fatalf("round trip = %v", back)
	}
}

func TestVersionVector_UnmarshalRejectsInvalid(t *testing.T) {
	for _, data := range []string{`{"A":-1}`, `{"A":1.5}`, `{"":3}`, `[]`, `"x"`} {
		var v VersionVector
		if err := json.Unmarshal([]byte(data), &v); err == nil {
			t.Fatalf("Unmarshal(%s): expected error, got %v", data, v)
		}
	}
	var v VersionVector
	if err := json.Unmarshal([]byte(`null`), &v); err != nil {
		t.Fatalf("null: unexpected error %v", err)
	}
	if v == nil || len(v) != 0 {
		t.Fatalf("null: got %v, want empty non-nil", v)
	}
}
